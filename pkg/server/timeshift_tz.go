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
	"fmt"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the Alpine image ships no zoneinfo

	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

const timeshiftLayout = "2006-01-02:15-04"

// tzState memoises zone lookups and remembers what was last logged, so the
// per-request path does no I/O and logs only when the answer changes.
type tzState struct {
	mu         sync.Mutex
	zones      map[string]*time.Location // nil value = known-unknown name
	lastZone   string
	lastSource string
}

// zone resolves an IANA name once; first reports a name seen for the first time.
func (s *tzState) zone(name string) (loc *time.Location, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	loc, ok := s.zones[name]
	if !ok {
		loc, _ = time.LoadLocation(name)
		if s.zones == nil {
			s.zones = map[string]*time.Location{}
		}
		s.zones[name] = loc
	}
	return loc, !ok
}

// providerLocation is the zone the provider expects catchup start times in:
// the setting, else the provider's reported zone, else its clock offset, else
// UTC. It reads only the cached account snapshot and never calls the provider.
func (c *Config) providerLocation() *time.Location {
	loc, source := c.resolveProviderLocation()
	name := loc.String()
	c.tz.mu.Lock()
	changed := c.tz.lastZone != name || c.tz.lastSource != source
	c.tz.lastZone, c.tz.lastSource = name, source
	c.tz.mu.Unlock()
	if changed {
		utils.InfoLog("Catchup: provider catchup times in %s (from %s)", name, source)
	}
	return loc
}

func (c *Config) resolveProviderLocation() (*time.Location, string) {
	if v := strings.TrimSpace(c.CatchupProviderTimezone); v != "" {
		if loc, _ := c.tz.zone(v); loc != nil {
			return loc, "setting"
		}
	}
	var info *xtreamapi.AccountInfo
	if st := c.providerInfo; st != nil {
		st.mu.Lock()
		info = st.info
		st.mu.Unlock()
	}
	if info == nil {
		return time.UTC, "default"
	}
	if name := info.ServerTimezone; name != "" {
		loc, first := c.tz.zone(name)
		if loc != nil {
			return loc, "provider timezone"
		}
		if first {
			utils.WarnLog("Catchup: provider reports unknown time zone %q; falling back to its clock", name)
		}
	}
	if info.ServerTime != "" && info.ServerTimestamp != 0 {
		if local, err := time.Parse("2006-01-02 15:04:05", info.ServerTime); err == nil {
			const quarter = 15 * 60
			off := local.Unix() - info.ServerTimestamp
			off = (off + quarter/2) / quarter * quarter // rounds half up; clocks drift by seconds
			if off == 0 {
				return time.UTC, "provider clock"
			}
			h, m := off/3600, off%3600/60
			if m < 0 {
				m = -m
			}
			return time.FixedZone(fmt.Sprintf("provider%+03d:%02d", h, m), int(off)), "provider clock"
		}
	}
	return time.UTC, "default"
}

// providerTimeshiftStart re-expresses a UTC "YYYY-MM-DD:HH-MM" start in loc,
// the zone the provider reads it in. Unix timestamps and anything unparseable
// pass through unchanged.
func providerTimeshiftStart(start string, loc *time.Location) string {
	t, err := time.ParseInLocation(timeshiftLayout, start, time.UTC)
	if err != nil {
		return start
	}
	return t.In(loc).Format(timeshiftLayout)
}
