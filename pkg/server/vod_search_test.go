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

package server

import (
	"testing"
)

func TestParseQueryTokens(t *testing.T) {
	cases := []struct {
		q                 string
		wantTokens        []string
		wantSeason        int
		wantEpisode       int
	}{
		{"the office s02e04", []string{"the", "office"}, 2, 4},
		{"the office s2e4", []string{"the", "office"}, 2, 4},
		{"the office s02 e04", []string{"the", "office"}, 2, 4},
		{"inception", []string{"inception"}, 0, 0},
		{"  spaced  query  ", []string{"spaced", "query"}, 0, 0},
		{"", nil, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			tokens, season, episode := parseQueryTokens(tc.q)
			if season != tc.wantSeason {
				t.Errorf("season = %d, want %d", season, tc.wantSeason)
			}
			if episode != tc.wantEpisode {
				t.Errorf("episode = %d, want %d", episode, tc.wantEpisode)
			}
			if len(tokens) != len(tc.wantTokens) {
				t.Fatalf("tokens = %v, want %v", tokens, tc.wantTokens)
			}
			for i := range tokens {
				if tokens[i] != tc.wantTokens[i] {
					t.Errorf("token[%d] = %q, want %q", i, tokens[i], tc.wantTokens[i])
				}
			}
		})
	}
}

func TestAllTokensIn(t *testing.T) {
	if !allTokensIn([]string{"office", "s02"}, "The Office S02") {
		t.Error("expected match (case-insensitive)")
	}
	if allTokensIn([]string{"office", "s03"}, "The Office S02") {
		t.Error("expected no match")
	}
	if !allTokensIn(nil, "anything") {
		t.Error("empty tokens should match anything")
	}
}

func TestToInt(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int
	}{
		{42, 42},
		{int64(7), 7},
		{float64(3.9), 3},
		{"15", 15},
		{nil, 0},
		{"abc", 0},
	}
	for _, tc := range cases {
		if got := toInt(tc.in); got != tc.want {
			t.Errorf("toInt(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
