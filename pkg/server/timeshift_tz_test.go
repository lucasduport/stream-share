/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 * Copyright (C) 2026  x3n0n10
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
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

func TestProviderTimeshiftStart(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		loc  *time.Location
		in   string
		want string
	}{
		{time.UTC, "2026-11-10:19-00", "2026-11-10:19-00"},
		{berlin, "2026-11-10:19-00", "2026-11-10:20-00"},
		{berlin, "2026-07-10:19-00", "2026-07-10:21-00"},
		{berlin, "2026-10-25:00-30", "2026-10-25:02-30"}, // CEST
		{berlin, "2026-10-25:01-30", "2026-10-25:02-30"}, // CET
		{berlin, "2026-03-29:01-30", "2026-03-29:03-30"},
		{berlin, "1762801200", "1762801200"},
		{berlin, "garbage", "garbage"},
	}
	for _, tc := range cases {
		if got := providerTimeshiftStart(tc.in, tc.loc); got != tc.want {
			t.Errorf("%s in %s: got %q, want %q", tc.in, tc.loc, got, tc.want)
		}
	}
}

func TestProviderLocation(t *testing.T) {
	clock := &xtreamapi.AccountInfo{
		ServerTimezone:  "GMT+1",
		ServerTime:      "2026-11-10 17:21:24",
		ServerTimestamp: time.Date(2026, 11, 10, 16, 21, 24, 0, time.UTC).Unix(),
	}
	cases := []struct {
		name    string
		setting string
		info    *xtreamapi.AccountInfo
		want    string // zone name, or "+3600" for a fixed offset
	}{
		{"setting beats provider", "Europe/Amsterdam", &xtreamapi.AccountInfo{ServerTimezone: "Africa/Dakar"}, "Europe/Amsterdam"},
		{"unknown setting falls through", "Mars/Base", &xtreamapi.AccountInfo{ServerTimezone: "Africa/Dakar"}, "Africa/Dakar"},
		{"provider zone", "", &xtreamapi.AccountInfo{ServerTimezone: "Africa/Dakar"}, "Africa/Dakar"},
		{"provider clock", "", clock, "+3600"},
		{"no data", "", &xtreamapi.AccountInfo{}, "UTC"},
		{"no cache", "", nil, "UTC"},
	}
	for _, tc := range cases {
		c := providerConfigWith(tc.info)
		c.CatchupProviderTimezone = tc.setting
		loc := c.providerLocation()
		_, off := time.Date(2026, 11, 10, 12, 0, 0, 0, loc).Zone()
		got := loc.String()
		if tc.want == "+3600" {
			if off != 3600 {
				t.Errorf("%s: offset %d, want 3600", tc.name, off)
			}
		} else if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	// Empty cache, nil state, and nothing configured must never reach the provider.
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	if c.providerLocation() != time.UTC {
		t.Error("nil providerInfo should give UTC")
	}
}
