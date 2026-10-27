/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package discord

import (
	"testing"
	"time"
)

func parisNow(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	now, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

func TestParseShareWindow(t *testing.T) {
	now := parisNow(t, "2026-10-27 20:30") // a Tuesday, Europe/Paris (CET)
	cases := []struct {
		start, until       string
		wantStart, wantEnd string
	}{
		{"", "2h", "2026-10-27 20:30", "2026-10-27 22:30"},
		{"now", "90m", "2026-10-27 20:30", "2026-10-27 22:00"},
		{"", "1h30", "2026-10-27 20:30", "2026-10-27 22:00"},
		{"21:00", "23:00", "2026-10-27 21:00", "2026-10-27 23:00"},
		{"21h", "23:30", "2026-10-27 21:00", "2026-10-27 23:30"},
		{"22:00", "01:00", "2026-10-27 22:00", "2026-10-28 01:00"}, // over midnight
		{"19:00", "21:00", "2026-10-28 19:00", "2026-10-28 21:00"}, // past today: tomorrow
		{"demain 21:00", "2h", "2026-10-28 21:00", "2026-10-28 23:00"},
		{"tomorrow 21h30", "tomorrow 23:00", "2026-10-28 21:30", "2026-10-28 23:00"},
		{"2026-11-02 18:00", "2026-11-02 20:00", "2026-11-02 18:00", "2026-11-02 20:00"},
		{"20:30", "45", "2026-10-27 20:30", "2026-10-27 21:15"},
	}
	for _, tc := range cases {
		start, end, err := parseShareWindow(tc.start, tc.until, now)
		if err != nil {
			t.Errorf("(%q, %q): %v", tc.start, tc.until, err)
			continue
		}
		if got := start.Format("2006-01-02 15:04"); got != tc.wantStart {
			t.Errorf("(%q, %q) start = %s, want %s", tc.start, tc.until, got, tc.wantStart)
		}
		if got := end.Format("2006-01-02 15:04"); got != tc.wantEnd {
			t.Errorf("(%q, %q) end = %s, want %s", tc.start, tc.until, got, tc.wantEnd)
		}
		if start.Location().String() != "Europe/Paris" {
			t.Errorf("(%q, %q) not in the server zone: %s", tc.start, tc.until, start.Location())
		}
	}
}

func TestParseShareWindowErrors(t *testing.T) {
	now := parisNow(t, "2026-10-27 20:30")
	cases := []struct{ start, until string }{
		{"", ""},                                 // end required
		{"", "soon"},                             // unreadable end
		{"later", "2h"},                          // unreadable start
		{"", "25h"},                              // over the 24h cap
		{"21:00", "2026-10-27 20:00"},            // end before start
		{"2026-10-20 18:00", "2026-10-20 20:00"}, // already over
		{"demain 25:00", "2h"},                   // invalid clock
		{"2026-10-27 21:00", "2026-10-28 22:00"}, // 25h window
	}
	for _, tc := range cases {
		if s, e, err := parseShareWindow(tc.start, tc.until, now); err == nil {
			t.Errorf("(%q, %q) = %s → %s, want an error", tc.start, tc.until, s, e)
		}
	}
}

// Across the daylight-saving change (Europe/Paris goes back on the last Sunday
// of October), wall-clock times stay what the user typed.
func TestParseShareWindowDST(t *testing.T) {
	now := parisNow(t, "2026-10-24 22:00") // Saturday before the change
	start, end, err := parseShareWindow("demain 01:00", "05:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format("15:04") != "01:00" || end.Format("15:04") != "05:00" || end.Sub(start) != 5*time.Hour {
		t.Fatalf("window = %s → %s (%s)", start, end, end.Sub(start))
	}
}
