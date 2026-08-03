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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestHealthzReportsReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{}}

	call := func() int {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
		c.healthz(ctx)
		return w.Code
	}

	if got := call(); got != http.StatusServiceUnavailable {
		t.Errorf("before ready: got %d, want 503", got)
	}
	atomic.StoreInt32(&c.ready, 1)
	if got := call(); got != http.StatusOK {
		t.Errorf("after ready: got %d, want 200", got)
	}
}

func TestIsBlockedStatus(t *testing.T) {
	cases := []struct {
		name   string
		codes  string
		status int
		want   bool
	}{
		{"default matches 456", "", 456, true},
		{"default rejects 403", "", 403, false},
		{"custom single", "455", 455, true},
		{"custom rejects default when overridden", "455", 456, false},
		{"custom list with spaces", " 456 , 461 ", 461, true},
		{"zero status never blocked", "456", 0, false},
		{"malformed entries ignored", "abc,456", 456, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{ProxyConfig: &config.ProxyConfig{HealthCheckBlockedCodes: tc.codes}}
			if got := c.isBlockedStatus(tc.status); got != tc.want {
				t.Errorf("isBlockedStatus(%d) with codes %q = %v, want %v", tc.status, tc.codes, got, tc.want)
			}
		})
	}
}

func TestParseDailyTimes(t *testing.T) {
	got := parseDailyTimes(" 04:00, 16:00 ,,bad,25:00,12:61,08:30")
	want := []dailyTime{{4, 0}, {16, 0}, {8, 30}}
	if len(got) != len(want) {
		t.Fatalf("parseDailyTimes returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNextDailyTime(t *testing.T) {
	times := []dailyTime{{4, 0}, {16, 0}}
	loc := time.UTC

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "before first",
			now:  time.Date(2026, 8, 3, 2, 30, 0, 0, loc),
			want: time.Date(2026, 8, 3, 4, 0, 0, 0, loc),
		},
		{
			name: "between the two",
			now:  time.Date(2026, 8, 3, 9, 0, 0, 0, loc),
			want: time.Date(2026, 8, 3, 16, 0, 0, 0, loc),
		},
		{
			name: "after last rolls to tomorrow",
			now:  time.Date(2026, 8, 3, 20, 0, 0, 0, loc),
			want: time.Date(2026, 8, 4, 4, 0, 0, 0, loc),
		},
		{
			name: "exactly at a time picks the next one",
			now:  time.Date(2026, 8, 3, 4, 0, 0, 0, loc),
			want: time.Date(2026, 8, 3, 16, 0, 0, 0, loc),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextDailyTime(tc.now, times); !got.Equal(tc.want) {
				t.Errorf("nextDailyTime(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}
