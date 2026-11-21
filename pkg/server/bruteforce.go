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
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// ipThresholdMultiplier is how much more tolerant the per-IP counter is than
// the per-username counter: a household legitimately shares one public IP,
// so the IP signal must be weaker to avoid locking out honest users.
const ipThresholdMultiplier = 3

// loginThrottle implements sliding-window failed-login throttling for the
// public player endpoints. Every failed authentication counts against both
// the username and the source IP. The username counter is the primary guard:
// it cannot be evaded by rotating source addresses, so a dictionary attack on
// one account stops at the threshold. The IP counter only dampens attacks
// spreading failures across many usernames.
//
// Caveats, by design:
//   - The IP key relies on gin's ClientIP(), which a client can influence
//     through forwarded headers when the app runs behind a proxy chain that
//     is not fully trusted. The username key is unaffected by that.
//   - A directory outage (LDAP unreachable) counts failures for every
//     authenticating username, so after an outage usernames may briefly be
//     locked out. Fail-closed was chosen over fail-open.
type loginThrottle struct {
	mu          sync.Mutex
	failures    map[string][]time.Time
	lockedUntil map[string]time.Time
}

// authThrottle is the process-wide failed-login tracker.
var authThrottle = newLoginThrottle()

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		failures:    make(map[string][]time.Time),
		lockedUntil: make(map[string]time.Time),
	}
}

// loginKeyUser namespaces a key by (case-folded) username. Case-folding
// prevents trivially bypassing the counter by alternating capitalization.
func loginKeyUser(username string) string {
	return "user:" + strings.ToLower(username)
}

// loginKeyIP namespaces a key by source address.
func loginKeyIP(ip string) string {
	return "ip:" + ip
}

// throttleConfig resolves the active (threshold, window, lockout) values.
// A threshold <= 0 disables throttling entirely; a positive threshold with
// unset (zero) window or lockout falls back to the built-in defaults.
func (c *Config) throttleConfig() (threshold int, window, lockout time.Duration) {
	threshold = c.LoginFailureThreshold
	window = time.Duration(c.LoginFailureWindowMinutes) * time.Minute
	lockout = time.Duration(c.LoginLockoutMinutes) * time.Minute
	if threshold > 0 {
		if window <= 0 {
			window = 10 * time.Minute
		}
		if lockout <= 0 {
			lockout = 15 * time.Minute
		}
	}
	return threshold, window, lockout
}

// loginLocked reports whether any key is currently locked, and for how long.
func (t *loginThrottle) loginLocked(now time.Time, keys ...string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	worst := time.Duration(0)
	locked := false
	for _, key := range keys {
		until, ok := t.lockedUntil[key]
		if !ok {
			continue
		}
		if remaining := until.Sub(now); remaining > 0 {
			locked = true
			if remaining > worst {
				worst = remaining
			}
		} else {
			delete(t.lockedUntil, key)
		}
	}
	return worst, locked
}

// recordFailedLogin adds one failure for key. When the number of failures
// within window reaches threshold, the key locks for lockout and its failure
// history is cleared. It returns the lockout duration when this call caused
// the lock, 0 otherwise.
func (t *loginThrottle) recordFailedLogin(now time.Time, key string, threshold int, window, lockout time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	recent := t.failures[key][:0]
	for _, ts := range t.failures[key] {
		if now.Sub(ts) < window {
			recent = append(recent, ts)
		}
	}
	recent = append(recent, now)
	t.failures[key] = recent

	if len(recent) >= threshold {
		delete(t.failures, key)
		t.lockedUntil[key] = now.Add(lockout)
		return lockout
	}
	return 0
}

// resetLogin drops all failure history and lockout state for the given keys
// (used on successful authentication).
func (t *loginThrottle) resetLogin(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, key := range keys {
		delete(t.failures, key)
		delete(t.lockedUntil, key)
	}
}

// sweep removes expired failure history and finished lockouts, bounding
// memory without a background goroutine. It is called opportunistically on
// the failure path; with throttling enabled the maps hold at most
// (usernames seen + source IPs) x threshold entries.
func (t *loginThrottle) sweep(now time.Time, window time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, until := range t.lockedUntil {
		if now.After(until) {
			delete(t.lockedUntil, key)
		}
	}
	for key, stamps := range t.failures {
		recent := stamps[:0]
		for _, ts := range stamps {
			if now.Sub(ts) < window {
				recent = append(recent, ts)
			}
		}
		if len(recent) == 0 {
			delete(t.failures, key)
		} else {
			t.failures[key] = recent
		}
	}
}

// rejectIfThrottled aborts the request with 429 Too Many Requests when the
// username or the source IP is currently locked out. It returns true when the
// request was aborted, so callers must stop processing.
func (c *Config) rejectIfThrottled(ctx *gin.Context, username string) bool {
	threshold, _, _ := c.throttleConfig()
	if threshold <= 0 {
		return false
	}
	ip := ctx.ClientIP()
	now := time.Now()
	if remaining, locked := authThrottle.loginLocked(now, loginKeyUser(username), loginKeyIP(ip)); locked {
		seconds := int(math.Ceil(remaining.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		ctx.Header("Retry-After", strconv.Itoa(seconds))
		utils.WarnLog("Login throttled: user=%s ip=%s locked for %s", username, ip, remaining.Round(time.Second))
		ctx.AbortWithStatus(http.StatusTooManyRequests)
		return true
	}
	return false
}

// noteLoginFailure records a failed authentication against the username and
// (with a more tolerant threshold) the source IP, locking whichever counter
// trips. Throttling disabled is a no-op.
func (c *Config) noteLoginFailure(username, ip string) {
	threshold, window, lockout := c.throttleConfig()
	if threshold <= 0 {
		return
	}
	now := time.Now()
	authThrottle.sweep(now, window)
	if lockoutFor := authThrottle.recordFailedLogin(now, loginKeyUser(username), threshold, window, lockout); lockoutFor > 0 {
		utils.WarnLog("Login throttling: user=%s locked for %s after %d failed login(s)", username, lockoutFor, threshold)
	}
	if lockoutFor := authThrottle.recordFailedLogin(now, loginKeyIP(ip), threshold*ipThresholdMultiplier, window, lockout); lockoutFor > 0 {
		utils.WarnLog("Login throttling: ip=%s locked for %s after %d failed login(s)", ip, lockoutFor, threshold*ipThresholdMultiplier)
	}
}

// noteLoginSuccess clears the username's failure history on a successful
// authentication. The IP history is deliberately kept: one account failing
// does not absolve its source address.
func (c *Config) noteLoginSuccess(username string) {
	if c.LoginFailureThreshold <= 0 {
		return
	}
	authThrottle.resetLogin(loginKeyUser(username))
}
