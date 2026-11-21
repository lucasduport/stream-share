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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func throttleConfig(minutes int) *Config {
	return &Config{ProxyConfig: &config.ProxyConfig{
		LDAPEnabled:               true,
		LoginFailureThreshold:     5,
		LoginFailureWindowMinutes: minutes,
		LoginLockoutMinutes:       15,
	}}
}

func freshThrottle() {
	authThrottle = newLoginThrottle()
}

func TestThrottleConfigDefaults(t *testing.T) {
	freshThrottle()

	// Disabled entirely
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	if threshold, _, _ := c.throttleConfig(); threshold != 0 {
		t.Fatalf("expected throttling disabled, got threshold %d", threshold)
	}

	// Defaults for window and lockout
	c = &Config{ProxyConfig: &config.ProxyConfig{LoginFailureThreshold: 10}}
	_, window, lockout := c.throttleConfig()
	if window != 10*time.Minute || lockout != 15*time.Minute {
		t.Fatalf("expected default window/lockout 10m/15m, got %s/%s", window, lockout)
	}

	// Explicit values honored
	c = &Config{ProxyConfig: &config.ProxyConfig{
		LoginFailureThreshold:     3,
		LoginFailureWindowMinutes: 1,
		LoginLockoutMinutes:       2,
	}}
	threshold, window, lockout := c.throttleConfig()
	if threshold != 3 || window != time.Minute || lockout != 2*time.Minute {
		t.Fatalf("unexpected config: %d/%s/%s", threshold, window, lockout)
	}
}

func TestRecordFailedLoginLocksAtThreshold(t *testing.T) {
	freshThrottle()
	now := time.Now()
	const window = 10 * time.Minute
	const lockout = 15 * time.Minute

	lt := newLoginThrottle()
	for i := 1; i < 3; i++ {
		if got := lt.recordFailedLogin(now, "user:alice", 3, window, lockout); got != 0 {
			t.Fatalf("failure %d unexpectedly locked (%s)", i, got)
		}
		if _, locked := lt.loginLocked(now, "user:alice"); locked {
			t.Fatalf("should not be locked after failure %d", i)
		}
	}
	if got := lt.recordFailedLogin(now, "user:alice", 3, window, lockout); got != lockout {
		t.Fatalf("3rd failure should report lockout %s, got %s", lockout, got)
	}
	if _, locked := lt.loginLocked(now, "user:alice"); !locked {
		t.Fatal("expected lock after 3rd failure")
	}

	// Lock expires
	if _, locked := lt.loginLocked(now.Add(lockout+time.Second), "user:alice"); locked {
		t.Fatal("expected lock to have expired")
	}
}

func TestRecordFailedLoginWindowPrunes(t *testing.T) {
	freshThrottle()
	now := time.Now()
	const window = 10 * time.Minute

	lt := newLoginThrottle()
	lt.recordFailedLogin(now.Add(-window-time.Minute), "user:alice", 2, window, time.Minute)
	if got := lt.recordFailedLogin(now, "user:alice", 2, window, time.Minute); got != 0 {
		t.Fatalf("stale failure should not count towards the lock, got lockout %s", got)
	}
}

func TestResetLoginClearsState(t *testing.T) {
	freshThrottle()
	now := time.Now()

	lt := newLoginThrottle()
	lt.recordFailedLogin(now, "user:alice", 1, time.Minute, time.Minute)
	if _, locked := lt.loginLocked(now, "user:alice"); !locked {
		t.Fatal("alice should be locked")
	}
	lt.resetLogin("user:alice")
	if _, locked := lt.loginLocked(now, "user:alice"); locked {
		t.Fatal("reset should clear the lock")
	}
}

func TestSweepBoundsMemory(t *testing.T) {
	freshThrottle()
	now := time.Now()

	lt := newLoginThrottle()
	lt.recordFailedLogin(now.Add(-2*time.Hour), "user:old", 5, 10*time.Minute, time.Minute)
	lt.recordFailedLogin(now.Add(-2*time.Hour), "ip:old", 5, 10*time.Minute, time.Minute)
	lt.lockedUntil["user:old"] = now.Add(-time.Hour)
	lt.sweep(now, 10*time.Minute)

	if _, ok := lt.failures["user:old"]; ok {
		t.Fatal("sweep should drop expired failure history")
	}
	if _, ok := lt.lockedUntil["user:old"]; ok {
		t.Fatal("sweep should drop finished lockouts")
	}
}

func TestNoteLoginFailureLocksUsernameAndIP(t *testing.T) {
	freshThrottle()
	c := throttleConfig(10)

	// Threshold is 5 for the username: 5 failures lock it.
	for i := 0; i < 4; i++ {
		c.noteLoginFailure("alice", "203.0.113.10")
	}
	if _, locked := authThrottle.loginLocked(time.Now(), loginKeyUser("alice")); locked {
		t.Fatal("username should not be locked after 4 failures (threshold 5)")
	}
	c.noteLoginFailure("alice", "203.0.113.10")
	if _, locked := authThrottle.loginLocked(time.Now(), loginKeyUser("alice")); !locked {
		t.Fatal("username should be locked after 5 failures")
	}

	freshThrottle()
	// Same IP spraying many usernames locks at 3x the threshold (15).
	for i := 0; i < 14; i++ {
		c.noteLoginFailure("user"+string(rune('a'+i)), "203.0.113.10")
	}
	if _, locked := authThrottle.loginLocked(time.Now(), loginKeyIP("203.0.113.10")); locked {
		t.Fatal("IP should not be locked after 14 failures (threshold 15)")
	}
	c.noteLoginFailure("another", "203.0.113.10")
	if _, locked := authThrottle.loginLocked(time.Now(), loginKeyIP("203.0.113.10")); !locked {
		t.Fatal("IP should be locked after 15 failures")
	}
}

func TestNoteLoginSuccessClearsUsernameOnly(t *testing.T) {
	freshThrottle()
	now := time.Now()
	c := throttleConfig(10)

	c.noteLoginFailure("alice", "203.0.113.10")
	c.noteLoginSuccess("alice")
	if _, locked := authThrottle.loginLocked(now, loginKeyUser("alice")); locked {
		t.Fatal("success should clear the username lock state")
	}
	if len(authThrottle.failures[loginKeyIP("203.0.113.10")]) != 1 {
		t.Fatal("success must not clear the IP history")
	}
}

func TestLoginKeyUserIsCaseInsensitive(t *testing.T) {
	if loginKeyUser("Alice") != loginKeyUser("alice") {
		t.Fatal("capitalization should not create distinct throttle buckets")
	}
}

func TestRejectIfThrottledReturns429(t *testing.T) {
	freshThrottle()
	gin.SetMode(gin.TestMode)

	c := throttleConfig(5)
	c.noteLoginFailure("alice", "203.0.113.10")
	c.noteLoginFailure("alice", "203.0.113.10")
	c.noteLoginFailure("alice", "203.0.113.10")
	c.noteLoginFailure("alice", "203.0.113.10")
	c.noteLoginFailure("alice", "203.0.113.10")

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php?username=alice&password=x", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"

	if !c.rejectIfThrottled(ctx, "alice") {
		t.Fatal("locked user should be throttled")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header")
	}

	// A different, clean user is not affected by alice's lockout.
	rec2 := httptest.NewRecorder()
	ctx2, _ := gin.CreateTestContext(rec2)
	ctx2.Request = httptest.NewRequest(http.MethodGet, "/get.php?username=bob&password=x", nil)
	ctx2.Request.RemoteAddr = "203.0.113.10:1234"
	if c.rejectIfThrottled(ctx2, "bob") {
		t.Fatal("bob should not be throttled by alice's username lock")
	}
}

func TestAuthenticateThrottlesAfterFailures(t *testing.T) {
	freshThrottle()
	gin.SetMode(gin.TestMode)

	// Local credentials (LDAP disabled) so the whole flow runs offline.
	c := &Config{ProxyConfig: &config.ProxyConfig{
		LoginFailureThreshold:     2,
		LoginFailureWindowMinutes: 10,
		LoginLockoutMinutes:       5,
	}}
	c.User = config.CredentialString("bob")
	c.Password = config.CredentialString("secret")

	for i := 0; i < 2; i++ {
		func() {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php?username=bob&password=wrong", nil)
			ctx.Request.RemoteAddr = "203.0.113.20:1234"
			c.authenticate(ctx)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: expected 401, got %d", i+1, rec.Code)
			}
		}()
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php?username=bob&password=secret", nil)
	ctx.Request.RemoteAddr = "203.0.113.20:1234"
	c.authenticate(ctx)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after exhausting failed attempts, got %d", rec.Code)
	}

	// Once the throttle state is cleared (fresh process), the correct
	// credentials pass through authenticate without aborting.
	freshThrottle()
	rec2 := httptest.NewRecorder()
	ctx2, _ := gin.CreateTestContext(rec2)
	ctx2.Request = httptest.NewRequest(http.MethodGet, "/get.php?username=bob&password=secret", nil)
	ctx2.Request.RemoteAddr = "203.0.113.20:1234"
	c.authenticate(ctx2)
	if ctx2.IsAborted() {
		t.Fatalf("valid credentials should pass, got abort with status %d", rec2.Code)
	}
	if rec2.Code == http.StatusUnauthorized || rec2.Code == http.StatusTooManyRequests {
		t.Fatalf("valid credentials should not be rejected, got %d", rec2.Code)
	}
}
