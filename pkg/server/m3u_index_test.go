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

import "testing"

// TestStreamNamesHashStableAcrossIteration is the property the skip-unchanged
// optimisation depends on: Go randomises map iteration order, so a hash that
// walked the map directly would differ run to run and never skip anything.
func TestStreamNamesHashStableAcrossIteration(t *testing.T) {
	names := map[string]string{}
	epg := map[string]string{}
	for i := 0; i < 200; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i%10))
		names[id] = "Channel " + id
		epg[id] = id + ".tv"
	}

	want := streamNamesHash(names, epg)
	for i := 0; i < 20; i++ {
		if got := streamNamesHash(names, epg); got != want {
			t.Fatalf("hash changed between calls on identical input: %s != %s", got, want)
		}
	}
}

func TestStreamNamesHashDetectsChanges(t *testing.T) {
	base := map[string]string{"1": "One", "2": "Two"}
	baseEPG := map[string]string{"1": "one.tv"}
	want := streamNamesHash(base, baseEPG)

	cases := []struct {
		name  string
		names map[string]string
		epg   map[string]string
	}{
		{"renamed channel", map[string]string{"1": "Uno", "2": "Two"}, baseEPG},
		{"added channel", map[string]string{"1": "One", "2": "Two", "3": "Three"}, baseEPG},
		{"removed channel", map[string]string{"1": "One"}, baseEPG},
		{"changed epg id", base, map[string]string{"1": "uno.tv"}},
		{"removed epg id", base, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamNamesHash(tc.names, tc.epg); got == want {
				t.Fatal("hash did not change, so a real update would be skipped")
			}
		})
	}
}

// TestStreamNamesHashNoFieldBleed guards the delimiter choice: without
// separators, {"ab":"c"} and {"a":"bc"} would hash identically and a rename
// could be silently skipped.
func TestStreamNamesHashNoFieldBleed(t *testing.T) {
	a := streamNamesHash(map[string]string{"ab": "c"}, nil)
	b := streamNamesHash(map[string]string{"a": "bc"}, nil)
	if a == b {
		t.Fatal("adjacent fields collide; separators are not doing their job")
	}
}
