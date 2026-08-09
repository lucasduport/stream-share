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
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

const (
	// providerInfoDefaultRefreshMinutes is how long a subscription snapshot is
	// considered current. Subscription facts change on the order of days, so a
	// slow refresh is plenty and keeps provider traffic negligible.
	providerInfoDefaultRefreshMinutes = 15

	// providerInfoMinInterval is the floor between two real provider requests,
	// including forced ones. A dashboard polling the endpoint (or several
	// dashboards polling at once) must never turn into a login flood — that is
	// exactly the behavior that gets an egress IP blocked.
	providerInfoMinInterval = 30 * time.Second

	// providerInfoTimeout bounds a single provider request.
	providerInfoTimeout = 15 * time.Second
)

// providerInfoState caches the last subscription snapshot read from the upstream
// provider. Reads are served from here so the frequently-polled dashboard
// endpoints never contact the provider on the request path.
type providerInfoState struct {
	mu          sync.Mutex
	info        *xtreamapi.AccountInfo
	lastErr     string    // most recent fetch failure, cleared on success
	lastAttempt time.Time // when a real request last went out, for rate-limiting
	inFlight    bool
}

// providerInfoSnapshot is an immutable copy of the cache for rendering.
type providerInfoSnapshot struct {
	Info *xtreamapi.AccountInfo
	Err  string
	// Throttled reports that a refresh was wanted but suppressed to protect the
	// provider, so Info may be older than the refresh interval.
	Throttled bool
}

func (s *providerInfoState) snapshotLocked() providerInfoSnapshot {
	return providerInfoSnapshot{Info: s.info, Err: s.lastErr}
}

// providerInfoTTL is how long a snapshot stays fresh before a refresh is due.
func (c *Config) providerInfoTTL() time.Duration {
	mins := c.ProviderInfoRefreshMinutes
	if mins <= 0 {
		mins = providerInfoDefaultRefreshMinutes
	}
	return time.Duration(mins) * time.Minute
}

// providerAccount returns the cached subscription snapshot, refreshing it from
// the provider when the cache is empty, older than the refresh interval, or
// force is set. Refreshes are spaced at least providerInfoMinInterval apart and
// never run concurrently.
func (c *Config) providerAccount(force bool) providerInfoSnapshot {
	st := c.providerInfo
	if st == nil {
		return providerInfoSnapshot{}
	}

	st.mu.Lock()
	stale := st.info == nil || time.Since(st.info.FetchedAt) >= c.providerInfoTTL()
	if !stale && !force {
		snap := st.snapshotLocked()
		st.mu.Unlock()
		return snap
	}
	if st.inFlight || (!st.lastAttempt.IsZero() && time.Since(st.lastAttempt) < providerInfoMinInterval) {
		snap := st.snapshotLocked()
		snap.Throttled = true
		st.mu.Unlock()
		return snap
	}
	st.inFlight = true
	st.lastAttempt = time.Now()
	st.mu.Unlock()

	info, err := c.fetchProviderAccount()

	st.mu.Lock()
	if err != nil {
		st.lastErr = err.Error()
		utils.WarnLog("Provider info: refresh failed: %v", err)
	} else {
		st.info, st.lastErr = info, ""
		utils.DebugLog("Provider info: refreshed (status=%q, connections=%d/%d)",
			info.Status, info.ActiveConnections, info.MaxConnections)
	}
	st.inFlight = false
	snap := st.snapshotLocked()
	st.mu.Unlock()
	return snap
}

// providerAccountCached returns whatever is already cached without ever
// contacting the provider. Endpoints that a dashboard polls on a tight loop use
// this so adding subscription data to them costs nothing upstream.
func (c *Config) providerAccountCached() providerInfoSnapshot {
	st := c.providerInfo
	if st == nil {
		return providerInfoSnapshot{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.snapshotLocked()
}

// fetchProviderAccount performs one real account-info request upstream.
func (c *Config) fetchProviderAccount() (*xtreamapi.AccountInfo, error) {
	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, "")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerInfoTimeout)
	defer cancel()
	return client.AccountInfo(ctx)
}

// getProviderInfo GET /api/internal/provider — the upstream IPTV provider's own
// view of the subscription behind this instance: whether it is active, when it
// expires, and how many of its allowed connections are in use. Served from cache
// (see startProviderInfoRefresher); `?refresh=true` asks for a fresh read,
// subject to the same rate limit as every other refresh.
func (c *Config) getProviderInfo(ctx *gin.Context) {
	if c.providerInfo == nil {
		ctx.JSON(http.StatusOK, types.APIResponse{
			Success: true,
			Data: map[string]interface{}{
				"configured": false,
				"reason":     "no Xtream provider is configured for this instance",
			},
		})
		return
	}

	snap := c.providerAccount(isTruthyQuery(ctx.Query("refresh")))
	if snap.Info == nil {
		// Nothing has ever been read successfully — report the failure rather
		// than an empty subscription that a dashboard would render as "unknown".
		err := snap.Err
		if err == "" {
			err = "provider subscription info is not available yet"
		}
		ctx.JSON(http.StatusBadGateway, types.APIResponse{Success: false, Error: err})
		return
	}

	data := c.renderProviderInfo(snap)
	if snap.Throttled {
		ctx.Header("X-Provider-Info-Throttled", "true")
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: data})
}

// renderProviderInfo turns a snapshot into the JSON body. Fields the provider
// did not report are omitted rather than sent as zeroes, so a dashboard can tell
// "unlimited"/"unknown" apart from a real value.
func (c *Config) renderProviderInfo(snap providerInfoSnapshot) map[string]interface{} {
	info := snap.Info
	data := map[string]interface{}{
		"configured":         true,
		"authenticated":      info.Auth,
		"status":             info.Status,
		"is_trial":           info.IsTrial,
		"expired":            info.Expired(),
		"active_connections": info.ActiveConnections,
		"checked_at":         info.FetchedAt.UTC().Format(time.RFC3339),
		"age_seconds":        int64(time.Since(info.FetchedAt).Seconds()),
	}

	// "active" is the single boolean a dashboard needs for a green/red light:
	// the provider says Active, it accepted our credentials, and the expiry
	// date (if any) has not passed.
	data["active"] = info.Auth && !info.Expired() &&
		(info.Status == "" || strings.EqualFold(info.Status, "Active"))

	if info.Message != "" {
		data["message"] = info.Message
	}
	if info.ExpiresAt != nil {
		exp := *info.ExpiresAt
		data["expires_at"] = exp.Format(time.RFC3339)
		data["expires_in_seconds"] = int64(time.Until(exp).Seconds())
		data["days_remaining"] = int(math.Floor(time.Until(exp).Hours() / 24))
	} else {
		data["expires_at"] = nil
		data["unlimited"] = true
	}
	if info.CreatedAt != nil {
		data["created_at"] = info.CreatedAt.Format(time.RFC3339)
	}
	if info.MaxConnections > 0 {
		available := info.MaxConnections - info.ActiveConnections
		if available < 0 {
			available = 0
		}
		data["max_connections"] = info.MaxConnections
		data["connections_available"] = available
	}
	// How many of those provider-side connections are ours: one per active
	// shared stream, however many viewers each is serving. Seeing this next to
	// active_connections is what makes the multiplexing visible — and tells an
	// operator whether a connection limit is being spent elsewhere.
	data["local_upstream_connections"] = c.activeStreamCount()

	if len(info.AllowedOutputFormats) > 0 {
		data["allowed_output_formats"] = info.AllowedOutputFormats
	}
	if info.ServerTimezone != "" {
		data["server_timezone"] = info.ServerTimezone
	}
	if info.ServerTime != "" {
		data["server_time"] = info.ServerTime
	}

	// A failure after an earlier success: keep serving the last known state, but
	// say so rather than passing stale numbers off as current.
	if snap.Err != "" {
		data["stale"] = true
		data["error"] = snap.Err
	} else {
		data["stale"] = time.Since(info.FetchedAt) >= 2*c.providerInfoTTL()
	}
	return data
}

// activeStreamCount reports how many shared streams are currently running, which
// is also how many connections this instance holds open to the provider.
func (c *Config) activeStreamCount() int {
	if c.sessionManager == nil {
		return 0
	}
	n := 0
	for _, s := range c.sessionManager.GetAllStreams() {
		if s.Active {
			n++
		}
	}
	return n
}

// providerStatusBlock is the compact subscription summary embedded in /status,
// built purely from cache. Returns nil when nothing has been read yet.
func (c *Config) providerStatusBlock() map[string]interface{} {
	snap := c.providerAccountCached()
	if snap.Info == nil {
		return nil
	}
	info := snap.Info
	block := map[string]interface{}{
		"status":             info.Status,
		"active":             info.Auth && !info.Expired() && (info.Status == "" || strings.EqualFold(info.Status, "Active")),
		"expired":            info.Expired(),
		"is_trial":           info.IsTrial,
		"active_connections": info.ActiveConnections,
		"checked_at":         info.FetchedAt.UTC().Format(time.RFC3339),
	}
	if info.ExpiresAt != nil {
		block["expires_at"] = info.ExpiresAt.Format(time.RFC3339)
		block["days_remaining"] = int(math.Floor(time.Until(*info.ExpiresAt).Hours() / 24))
	}
	if info.MaxConnections > 0 {
		block["max_connections"] = info.MaxConnections
	}
	return block
}

// providerStatusLine renders the subscription as one line for the text summary
// that /status returns and Discord's /status echoes. Empty when nothing is
// cached yet.
func (c *Config) providerStatusLine() string {
	snap := c.providerAccountCached()
	if snap.Info == nil {
		return ""
	}
	info := snap.Info

	state := info.Status
	if state == "" {
		state = "unknown"
	}
	if !info.Auth {
		state += " (credentials rejected)"
	}
	parts := []string{state}
	if info.IsTrial {
		parts = append(parts, "trial")
	}
	switch {
	case info.Expired():
		parts = append(parts, "expired "+info.ExpiresAt.Format("2006-01-02"))
	case info.ExpiresAt != nil:
		days := int(math.Floor(time.Until(*info.ExpiresAt).Hours() / 24))
		parts = append(parts, fmt.Sprintf("%d day(s) left", days))
	}
	if info.MaxConnections > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d connection(s) in use", info.ActiveConnections, info.MaxConnections))
	} else if info.ActiveConnections > 0 {
		parts = append(parts, fmt.Sprintf("%d connection(s) in use", info.ActiveConnections))
	}
	return "Subscription: " + strings.Join(parts, " — ")
}

// startProviderInfoRefresher keeps the subscription snapshot warm in the
// background so request handlers only ever read from cache. It seeds shortly
// after startup and then refreshes on the configured interval.
func (c *Config) startProviderInfoRefresher(stop <-chan struct{}) {
	if c.providerInfo == nil {
		return
	}
	interval := c.providerInfoTTL()
	utils.InfoLog("Provider subscription info: enabled (refreshing every %v)", interval)

	go func() {
		select {
		case <-time.After(10 * time.Second):
		case <-stop:
			return
		}
		c.providerAccount(false)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				c.providerAccount(false)
			}
		}
	}()
}

// isTruthyQuery interprets the usual spellings of a boolean query parameter.
func isTruthyQuery(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
