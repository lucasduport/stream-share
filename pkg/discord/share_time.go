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
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// shareMaxWindow mirrors the server's cap on how long a share link may last.
const shareMaxWindow = 24 * time.Hour

var (
	reClock    = regexp.MustCompile(`^(\d{1,2})(?:[:h](\d{2}))?h?$`)
	reDuration = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m?)?$`)
)

// parseShareWindow turns /share's start and until options into a window, in
// now's time zone (the server's TZ). start: empty or "now" for now, "HH:MM"
// (next occurrence, today unless already past), "tomorrow HH:MM" /
// "demain HH:MM", or "YYYY-MM-DD HH:MM". until: a duration ("2h", "90m",
// "1h30") from the start, "HH:MM" with a colon (next occurrence after the
// start),
// "tomorrow HH:MM", or "YYYY-MM-DD HH:MM". The window must end after the start
// and after now, and last at most shareMaxWindow.
func parseShareWindow(startArg, untilArg string, now time.Time) (time.Time, time.Time, error) {
	start, err := parseShareStart(startArg, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseShareEnd(untilArg, start, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	switch {
	case !end.After(start):
		return time.Time{}, time.Time{}, fmt.Errorf("the end (%s) must be after the start (%s)", shareFmt(end), shareFmt(start))
	case end.Sub(start) > shareMaxWindow:
		return time.Time{}, time.Time{}, fmt.Errorf("a share link can last at most %s", shareMaxWindow)
	case !end.After(now):
		return time.Time{}, time.Time{}, fmt.Errorf("that window is already over (ended %s)", shareFmt(end))
	}
	return start, end, nil
}

func parseShareStart(arg string, now time.Time) (time.Time, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "", "now", "maintenant":
		return now, nil
	}
	if t, ok, err := parseDayAndClock(arg, now); ok || err != nil {
		return t, err
	}
	if h, m, ok := parseClock(arg); ok {
		t := atClock(now, h, m)
		if t.Before(now.Add(-time.Minute)) {
			t = t.AddDate(0, 0, 1) // already past today: the next one
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("could not read the start %q (try `21:00`, `tomorrow 21:00` or `2026-11-02 21:00`)", arg)
}

func parseShareEnd(arg string, start, now time.Time) (time.Time, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		return time.Time{}, fmt.Errorf("an end time or a duration is required (e.g. `23:00` or `2h`)")
	}
	if t, ok, err := parseDayAndClock(arg, now); ok || err != nil {
		return t, err
	}
	// An end time needs a colon: "1h30" or "2h" are durations.
	if h, m, ok := parseClock(arg); ok && strings.Contains(arg, ":") {
		t := atClock(start, h, m)
		if !t.After(start) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	if d, ok := parseShareDuration(arg); ok {
		return start.Add(d), nil
	}
	return time.Time{}, fmt.Errorf("could not read the end %q (try `23:00`, `2h`, `90m` or `2026-11-02 23:00`)", arg)
}

// parseDayAndClock reads "tomorrow HH:MM", "demain HH:MM", "today HH:MM",
// "aujourd'hui HH:MM" and "YYYY-MM-DD HH:MM". ok is false when arg has none of
// these shapes.
func parseDayAndClock(arg string, now time.Time) (time.Time, bool, error) {
	parts := strings.Fields(arg)
	if len(parts) != 2 {
		return time.Time{}, false, nil
	}
	var day time.Time
	switch parts[0] {
	case "tomorrow", "demain":
		day = now.AddDate(0, 0, 1)
	case "today", "aujourd'hui", "aujourdhui":
		day = now
	default:
		d, err := time.ParseInLocation("2006-01-02", parts[0], now.Location())
		if err != nil {
			return time.Time{}, false, nil
		}
		day = d
	}
	h, m, ok := parseClock(parts[1])
	if !ok {
		return time.Time{}, true, fmt.Errorf("could not read the time %q (use HH:MM)", parts[1])
	}
	return atClock(day, h, m), true, nil
}

// parseClock reads "21:00", "21h00", "21h" or "21".
func parseClock(s string) (int, int, bool) {
	mm := reClock.FindStringSubmatch(s)
	if mm == nil {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(mm[1])
	m := 0
	if mm[2] != "" {
		m, _ = strconv.Atoi(mm[2])
	}
	if h > 23 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// parseShareDuration reads "2h", "90m", "1h30", "1h30m" (a bare number is
// minutes).
func parseShareDuration(s string) (time.Duration, bool) {
	mm := reDuration.FindStringSubmatch(s)
	if mm == nil || (mm[1] == "" && mm[2] == "") {
		return 0, false
	}
	h, _ := strconv.Atoi(mm[1])
	m, _ := strconv.Atoi(mm[2])
	d := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	return d, d > 0
}

func atClock(day time.Time, h, m int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
}

func shareFmt(t time.Time) string {
	return t.Format("Mon 02 Jan 15:04")
}
