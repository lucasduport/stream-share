/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// Health status values reported by the health endpoints.
const (
	healthStatusHealthy  = "healthy"  // provider served a live response
	healthStatusBlocked  = "blocked"  // provider returned 456: our egress IP is blocked
	healthStatusError    = "error"    // some other provider or transport failure
	healthStatusUnknown  = "unknown"  // no probe has completed yet
	healthStatusDisabled = "disabled" // the feature is turned off
)

// defaultBlockedCodes is the upstream status treated as "egress IP blocked" when
// the operator hasn't set HEALTHCHECK_BLOCKED_CODES. Many Xtream providers return
// 456, but it is outside the HTTP standard, so providers may use other codes —
// hence a configurable default rather than a hardcoded constant.
//
// A "blocked" verdict is deliberately the only condition an external watchdog
// should act on: other failures (provider outage, a bad probe channel id,
// transport errors) report as "error" so the watchdog does not churn the VPN
// over problems reconnecting cannot fix.
const defaultBlockedCodes = "456"

// isBlockedStatus reports whether an upstream status code is one the operator has
// designated as "egress IP blocked".
func (c *Config) isBlockedStatus(status int) bool {
	if status <= 0 {
		return false
	}
	spec := strings.TrimSpace(c.HealthCheckBlockedCodes)
	if spec == "" {
		spec = defaultBlockedCodes
	}
	for _, part := range strings.Split(spec, ",") {
		if code, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && code == status {
			return true
		}
	}
	return false
}

// healthState caches the most recent provider-probe result. /healthz reads it
// cheaply (so the frequent Docker HEALTHCHECK never contacts the provider) while
// only scheduled, startup, and forced probes refresh it.
type healthState struct {
	mu        sync.Mutex
	status    string
	code      string    // upstream status code or synthetic key, for diagnostics
	detail    string    // human-readable explanation
	checkedAt time.Time // when the cached result was produced (zero = never)
	lastProbe time.Time // when a real probe last ran, for rate-limiting
	inFlight  bool
}

// healthSnapshot is an immutable copy of the cached state for rendering.
type healthSnapshot struct {
	Status     string
	Code       string
	Detail     string
	CheckedAt  time.Time
	AgeSeconds int
}

func (h *healthState) snapshotLocked() healthSnapshot {
	snap := healthSnapshot{Status: h.status, Code: h.code, Detail: h.detail, CheckedAt: h.checkedAt}
	if snap.Status == "" {
		snap.Status = healthStatusUnknown
	}
	if !h.checkedAt.IsZero() {
		snap.AgeSeconds = int(time.Since(h.checkedAt).Seconds())
	}
	return snap
}

// probeHealth runs one real provider probe and updates the cache, returning the
// resulting snapshot. It respects HealthCheckMinIntervalSeconds: when called
// again within that window (or while a probe is already running) it skips the
// provider and returns the cached snapshot with throttled=true. This keeps the
// force-probe endpoint from being turned into a way to hammer the provider.
func (c *Config) probeHealth() (snap healthSnapshot, throttled bool) {
	h := c.health
	if h == nil {
		return healthSnapshot{Status: healthStatusDisabled}, false
	}

	minInterval := time.Duration(c.HealthCheckMinIntervalSeconds) * time.Second

	h.mu.Lock()
	if h.inFlight || (!h.lastProbe.IsZero() && time.Since(h.lastProbe) < minInterval) {
		snap = h.snapshotLocked()
		h.mu.Unlock()
		return snap, true
	}
	h.inFlight = true
	h.lastProbe = time.Now()
	h.mu.Unlock()

	status, code, detail := c.runProviderProbe()

	h.mu.Lock()
	h.status, h.code, h.detail = status, code, detail
	h.checkedAt = time.Now()
	h.inFlight = false
	snap = h.snapshotLocked()
	h.mu.Unlock()

	utils.InfoLog("Health check: provider probe -> %s (%s)", status, detail)
	return snap, false
}

// runProviderProbe dials the configured probe channel once and classifies the
// result. It never mutates cache state; probeHealth owns that.
func (c *Config) runProviderProbe() (status, code, detail string) {
	if c.sessionManager == nil {
		return healthStatusError, "", "session manager not initialized"
	}
	target, err := c.healthProbeURL()
	if err != nil {
		return healthStatusError, "", "invalid probe URL: " + err.Error()
	}

	timeout := time.Duration(c.HealthCheckTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	uerr := c.sessionManager.ProbeUpstream(ctx, target)
	if uerr == nil {
		return healthStatusHealthy, "", "provider served a live response"
	}
	code = uerr.Key
	if c.isBlockedStatus(uerr.StatusCode) {
		return healthStatusBlocked, code, fmt.Sprintf("provider returned %d: egress IP is blocked", uerr.StatusCode)
	}
	return healthStatusError, code, uerr.Error()
}

// healthProbeURL builds the upstream URL for the configured probe channel,
// mirroring the live-stream path used by real playback.
func (c *Config) healthProbeURL() (*url.URL, error) {
	id := strings.Trim(strings.TrimSpace(c.HealthCheckStreamID), "/")
	if id == "" {
		return nil, fmt.Errorf("HEALTHCHECK_STREAM_ID is empty")
	}
	raw := fmt.Sprintf("%s/live/%s/%s/%s",
		strings.TrimRight(c.XtreamBaseURL, "/"),
		c.XtreamUser.PathEscape(), c.XtreamPassword.PathEscape(), id)
	return url.Parse(raw)
}

// healthz is the container readiness endpoint the Docker HEALTHCHECK polls. It
// returns 200 once startup has completed (the ready flag set at the end of
// Serve) and 503 before then. It reflects only whether THIS service is up —
// never provider/VPN state — so it is safe to gate compose startup ordering on
// (`depends_on: condition: service_healthy`). Provider/VPN health lives on the
// authenticated /api/internal/health endpoint instead.
func (c *Config) healthz(ctx *gin.Context) {
	if atomic.LoadInt32(&c.ready) == 1 {
		ctx.JSON(http.StatusOK, gin.H{"status": "ready"})
		return
	}
	ctx.JSON(http.StatusServiceUnavailable, gin.H{"status": "starting"})
}

// healthProbe forces a fresh provider probe (rate-limited) and returns the
// result. It lives under the authenticated internal API and is meant for an
// external watchdog to test a new egress IP right after reconnecting the VPN.
func (c *Config) healthProbe(ctx *gin.Context) {
	if c.health == nil {
		c.writeHealth(ctx, healthSnapshot{Status: healthStatusDisabled})
		return
	}
	snap, throttled := c.probeHealth()
	if throttled {
		ctx.Header("X-Health-Throttled", "true")
	}
	c.writeHealth(ctx, snap)
}

// writeHealth renders a snapshot as JSON and maps it to an HTTP status: 200 when
// healthy, disabled, or not-yet-probed (so a fresh container is not flagged
// during its start period); 503 when blocked, errored, or stale.
func (c *Config) writeHealth(ctx *gin.Context, snap healthSnapshot) {
	stale := c.HealthCheckStaleMinutes > 0 && !snap.CheckedAt.IsZero() &&
		time.Since(snap.CheckedAt) > time.Duration(c.HealthCheckStaleMinutes)*time.Minute

	httpStatus := http.StatusServiceUnavailable
	switch snap.Status {
	case healthStatusDisabled, healthStatusHealthy, healthStatusUnknown:
		httpStatus = http.StatusOK
	}
	if stale {
		httpStatus = http.StatusServiceUnavailable
	}

	body := gin.H{
		"status": snap.Status,
		"stale":  stale,
	}
	if snap.Code != "" {
		body["code"] = snap.Code
	}
	if snap.Detail != "" {
		body["detail"] = snap.Detail
	}
	if !snap.CheckedAt.IsZero() {
		body["checked_at"] = snap.CheckedAt.UTC().Format(time.RFC3339)
		body["age_seconds"] = snap.AgeSeconds
	}
	ctx.JSON(httpStatus, body)
}

// startHealthMonitor seeds the health cache shortly after startup and, when
// HealthCheckTimes is set, re-probes at those local wall-clock times. It never
// reconnects the VPN: acting on a bad result is deliberately an external concern.
func (c *Config) startHealthMonitor(stop <-chan struct{}) {
	if c.health == nil {
		return
	}

	// Seed once soon after startup so /healthz reflects reality quickly rather
	// than waiting for the first scheduled window (which could be hours away).
	go func() {
		select {
		case <-time.After(5 * time.Second):
		case <-stop:
			return
		}
		c.probeHealth()
	}()

	times := parseDailyTimes(c.HealthCheckTimes)
	if len(times) == 0 {
		utils.InfoLog("Health check: enabled (externally scheduled); probe channel %q", c.HealthCheckStreamID)
		return
	}
	utils.InfoLog("Health check: enabled; self-probing at %s (%s); probe channel %q",
		c.HealthCheckTimes, localZoneName(), c.HealthCheckStreamID)

	go func() {
		for {
			next := nextDailyTime(time.Now(), times)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
				c.probeHealth()
			}
		}
	}()
}

// dailyTime is a wall-clock hour:minute in the container's local timezone.
type dailyTime struct{ hour, min int }

// parseDailyTimes parses a comma-separated list of HH:MM entries, skipping
// malformed ones with a warning rather than failing startup.
func parseDailyTimes(spec string) []dailyTime {
	var out []dailyTime
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		hm := strings.SplitN(part, ":", 2)
		if len(hm) != 2 {
			utils.WarnLog("Health check: ignoring malformed time %q (want HH:MM)", part)
			continue
		}
		h, err1 := strconv.Atoi(strings.TrimSpace(hm[0]))
		m, err2 := strconv.Atoi(strings.TrimSpace(hm[1]))
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			utils.WarnLog("Health check: ignoring out-of-range time %q", part)
			continue
		}
		out = append(out, dailyTime{h, m})
	}
	return out
}

// nextDailyTime returns the soonest future instant matching one of times, in the
// local timezone (honoring the TZ the process was started with).
func nextDailyTime(now time.Time, times []dailyTime) time.Time {
	var best time.Time
	for _, t := range times {
		cand := time.Date(now.Year(), now.Month(), now.Day(), t.hour, t.min, 0, 0, now.Location())
		if !cand.After(now) {
			cand = cand.Add(24 * time.Hour)
		}
		if best.IsZero() || cand.Before(best) {
			best = cand
		}
	}
	if best.IsZero() {
		return now.Add(24 * time.Hour)
	}
	return best
}

// localZoneName reports the abbreviated name of the local timezone for logging.
func localZoneName() string {
	name, _ := time.Now().Zone()
	return name
}
