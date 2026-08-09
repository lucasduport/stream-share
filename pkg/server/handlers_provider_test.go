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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

// providerConfigWith builds a Config whose provider cache already holds info,
// so no test ever contacts a provider.
func providerConfigWith(info *xtreamapi.AccountInfo) *Config {
	return &Config{
		ProxyConfig:  &config.ProxyConfig{XtreamBaseURL: "http://provider.example"},
		providerInfo: &providerInfoState{info: info, lastAttempt: time.Now()},
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestRenderProviderInfoActiveSubscription(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	c := providerConfigWith(nil)
	info := &xtreamapi.AccountInfo{
		Auth: true, Status: "Active", ExpiresAt: timePtr(exp),
		ActiveConnections: 2, MaxConnections: 4,
		AllowedOutputFormats: []string{"m3u8", "ts"},
		FetchedAt:            time.Now(),
	}

	data := c.renderProviderInfo(providerInfoSnapshot{Info: info})

	if data["active"] != true {
		t.Errorf("active = %v, want true", data["active"])
	}
	if data["expired"] != false {
		t.Errorf("expired = %v, want false", data["expired"])
	}
	if got := data["days_remaining"]; got != 29 && got != 30 {
		t.Errorf("days_remaining = %v, want 29 or 30", got)
	}
	if data["connections_available"] != 2 {
		t.Errorf("connections_available = %v, want 2", data["connections_available"])
	}
	if data["max_connections"] != 4 {
		t.Errorf("max_connections = %v, want 4", data["max_connections"])
	}
	if data["stale"] != false {
		t.Errorf("stale = %v, want false", data["stale"])
	}
	if _, ok := data["unlimited"]; ok {
		t.Error("unlimited is set for an account that has an expiry date")
	}
}

func TestRenderProviderInfoExpiredAndOversubscribed(t *testing.T) {
	c := providerConfigWith(nil)
	info := &xtreamapi.AccountInfo{
		Auth: true, Status: "Active", ExpiresAt: timePtr(time.Now().Add(-24 * time.Hour)),
		// Providers do report more active connections than the limit allows.
		ActiveConnections: 5, MaxConnections: 4,
		FetchedAt: time.Now(),
	}

	data := c.renderProviderInfo(providerInfoSnapshot{Info: info})

	if data["expired"] != true {
		t.Errorf("expired = %v, want true", data["expired"])
	}
	if data["active"] != false {
		t.Errorf("active = %v, want false for an expired subscription", data["active"])
	}
	if data["connections_available"] != 0 {
		t.Errorf("connections_available = %v, want 0 (never negative)", data["connections_available"])
	}
}

func TestRenderProviderInfoUnlimitedAndUnreportedLimit(t *testing.T) {
	c := providerConfigWith(nil)
	info := &xtreamapi.AccountInfo{Auth: true, Status: "Active", FetchedAt: time.Now()}

	data := c.renderProviderInfo(providerInfoSnapshot{Info: info})

	if data["expires_at"] != nil {
		t.Errorf("expires_at = %v, want nil for an unlimited account", data["expires_at"])
	}
	if data["unlimited"] != true {
		t.Errorf("unlimited = %v, want true", data["unlimited"])
	}
	if _, ok := data["days_remaining"]; ok {
		t.Error("days_remaining is set for an account with no expiry date")
	}
	if _, ok := data["max_connections"]; ok {
		t.Error("max_connections is set even though the provider reported no limit")
	}
	if _, ok := data["connections_available"]; ok {
		t.Error("connections_available is set even though the provider reported no limit")
	}
}

func TestRenderProviderInfoRejectedCredentialsAreNotActive(t *testing.T) {
	c := providerConfigWith(nil)
	info := &xtreamapi.AccountInfo{Auth: false, Message: "Invalid credentials", FetchedAt: time.Now()}

	data := c.renderProviderInfo(providerInfoSnapshot{Info: info})

	if data["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", data["authenticated"])
	}
	if data["active"] != false {
		t.Errorf("active = %v, want false when the provider rejects our credentials", data["active"])
	}
	if data["message"] != "Invalid credentials" {
		t.Errorf("message = %v, want the provider's message", data["message"])
	}
}

func TestRenderProviderInfoMarksFailedRefreshStale(t *testing.T) {
	c := providerConfigWith(nil)
	info := &xtreamapi.AccountInfo{Auth: true, Status: "Active", FetchedAt: time.Now()}

	data := c.renderProviderInfo(providerInfoSnapshot{Info: info, Err: "provider returned HTTP 456"})

	if data["stale"] != true {
		t.Errorf("stale = %v, want true after a failed refresh", data["stale"])
	}
	if data["error"] != "provider returned HTTP 456" {
		t.Errorf("error = %v, want the refresh failure", data["error"])
	}
}

func TestProviderAccountServesFreshCacheWithoutFetching(t *testing.T) {
	info := &xtreamapi.AccountInfo{Auth: true, Status: "Active", FetchedAt: time.Now()}
	c := providerConfigWith(info)
	// An unreachable base URL: reaching the network at all would fail the test
	// by returning a nil Info instead of the cached one.
	c.XtreamBaseURL = "http://127.0.0.1:1"

	snap := c.providerAccount(false)

	if snap.Info != info {
		t.Fatalf("snapshot Info = %v, want the cached snapshot served without a fetch", snap.Info)
	}
	if snap.Throttled {
		t.Error("Throttled = true, want false for a plain cache hit")
	}
}

func TestProviderAccountThrottlesForcedRefresh(t *testing.T) {
	info := &xtreamapi.AccountInfo{Auth: true, Status: "Active", FetchedAt: time.Now().Add(-time.Hour)}
	c := providerConfigWith(info)
	c.XtreamBaseURL = "http://127.0.0.1:1"
	// A request went out moments ago, so a forced refresh must be suppressed
	// rather than hitting the provider again.
	c.providerInfo.lastAttempt = time.Now()

	snap := c.providerAccount(true)

	if !snap.Throttled {
		t.Error("Throttled = false, want true for a forced refresh inside the minimum interval")
	}
	if snap.Info != info {
		t.Error("a throttled refresh dropped the cached snapshot, want it served as-is")
	}
}

func TestProviderAccountFetchesAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"user_info": {"auth": 1, "status": "Active",
			"exp_date": "1793491200", "active_cons": "1", "max_connections": "3"}}`))
	}))
	defer srv.Close()

	c := providerConfigWith(nil)
	c.XtreamBaseURL = srv.URL
	c.XtreamUser, c.XtreamPassword = "u", "p"
	c.providerInfo.lastAttempt = time.Time{} // nothing fetched yet

	snap := c.providerAccount(false)
	if snap.Info == nil {
		t.Fatalf("first call returned no info (err=%q)", snap.Err)
	}
	if snap.Info.MaxConnections != 3 {
		t.Errorf("MaxConnections = %d, want 3", snap.Info.MaxConnections)
	}
	if hits != 1 {
		t.Fatalf("provider was called %d time(s) on the first read, want 1", hits)
	}

	// A second read inside the refresh interval must come from cache.
	if snap := c.providerAccount(false); snap.Info == nil {
		t.Fatal("second call returned no info")
	}
	if hits != 1 {
		t.Errorf("provider was called %d time(s) in total, want 1 — the second read should hit cache", hits)
	}
}

func TestProviderInfoTTLFallsBackToDefault(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	if got := c.providerInfoTTL(); got != providerInfoDefaultRefreshMinutes*time.Minute {
		t.Errorf("providerInfoTTL() = %v, want the %d-minute default", got, providerInfoDefaultRefreshMinutes)
	}
	c.ProviderInfoRefreshMinutes = 60
	if got := c.providerInfoTTL(); got != time.Hour {
		t.Errorf("providerInfoTTL() = %v, want 1h", got)
	}
}

func TestGetProviderInfoWithoutXtreamProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{}} // plain M3U deployment

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/provider", nil)
	c.getProviderInfo(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !body.Success || body.Data["configured"] != false {
		t.Errorf("body = %+v, want success with configured=false", body)
	}
}

func TestGetProviderInfoReportsFailureWhenNothingCached(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := providerConfigWith(nil)
	c.providerInfo.lastErr = "provider returned HTTP 456"

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/provider", nil)
	c.getProviderInfo(ctx)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "456") {
		t.Errorf("body = %s, want it to carry the provider failure", w.Body.String())
	}
}

func TestGetProviderInfoServesCachedSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := providerConfigWith(&xtreamapi.AccountInfo{
		Auth: true, Status: "Active", ExpiresAt: timePtr(time.Now().Add(48 * time.Hour)),
		ActiveConnections: 1, MaxConnections: 2, FetchedAt: time.Now(),
	})
	c.XtreamBaseURL = "http://127.0.0.1:1"

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/provider", nil)
	c.getProviderInfo(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !body.Success {
		t.Fatalf("success = false, body = %s", w.Body.String())
	}
	if body.Data["active"] != true || body.Data["max_connections"] != float64(2) {
		t.Errorf("data = %+v, want an active subscription with max_connections 2", body.Data)
	}
	if body.Data["local_upstream_connections"] != float64(0) {
		t.Errorf("local_upstream_connections = %v, want 0 with no session manager",
			body.Data["local_upstream_connections"])
	}
}

func TestProviderStatusLine(t *testing.T) {
	cases := []struct {
		name     string
		info     *xtreamapi.AccountInfo
		contains []string
		want     string // exact match when set
	}{
		{
			name: "nothing cached yet",
			info: nil,
			want: "",
		},
		{
			name: "active with expiry and connections",
			info: &xtreamapi.AccountInfo{
				Auth: true, Status: "Active", ExpiresAt: timePtr(time.Now().Add(10*24*time.Hour + time.Hour)),
				ActiveConnections: 1, MaxConnections: 3, FetchedAt: time.Now(),
			},
			contains: []string{"Subscription: Active", "10 day(s) left", "1/3 connection(s) in use"},
		},
		{
			name: "trial and expired",
			info: &xtreamapi.AccountInfo{
				Auth: true, Status: "Expired", IsTrial: true,
				ExpiresAt: timePtr(time.Now().Add(-time.Hour)), FetchedAt: time.Now(),
			},
			contains: []string{"Expired", "trial", "expired"},
		},
		{
			name:     "credentials rejected",
			info:     &xtreamapi.AccountInfo{Auth: false, Status: "Active", FetchedAt: time.Now()},
			contains: []string{"credentials rejected"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerConfigWith(tc.info).providerStatusLine()
			if tc.want != "" || len(tc.contains) == 0 {
				if got != tc.want {
					t.Fatalf("providerStatusLine() = %q, want %q", got, tc.want)
				}
				return
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("providerStatusLine() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestProviderStatusBlockIsCacheOnly(t *testing.T) {
	// No cached info and an unreachable provider: the block must be nil rather
	// than blocking /status on a provider request.
	c := providerConfigWith(nil)
	c.XtreamBaseURL = "http://127.0.0.1:1"
	if block := c.providerStatusBlock(); block != nil {
		t.Fatalf("providerStatusBlock() = %+v, want nil when nothing is cached", block)
	}

	c = providerConfigWith(&xtreamapi.AccountInfo{
		Auth: true, Status: "Active", ActiveConnections: 2, MaxConnections: 4, FetchedAt: time.Now(),
	})
	block := c.providerStatusBlock()
	if block == nil {
		t.Fatal("providerStatusBlock() = nil, want the cached summary")
	}
	if block["active"] != true || block["active_connections"] != 2 || block["max_connections"] != 4 {
		t.Errorf("block = %+v, want active with 2/4 connections", block)
	}
}

func TestIsTruthyQuery(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !isTruthyQuery(v) {
			t.Errorf("isTruthyQuery(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "maybe"} {
		if isTruthyQuery(v) {
			t.Errorf("isTruthyQuery(%q) = true, want false", v)
		}
	}
}
