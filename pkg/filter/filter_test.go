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

package filter

import (
	"strings"
	"testing"
)

func TestNilSetKeepsEverything(t *testing.T) {
	s, err := NewSet(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Fatal("an empty config must compile to a nil set")
	}
	if s.Active() || s.ActiveFor(Live) || s.ExcludesAdult() || s.Fingerprint() != "" {
		t.Fatal("nil set must report inactive")
	}
	if !s.Keep(Live, "any", "thing", true) || !s.KeepGroup(Movie, "any") {
		t.Fatal("nil set must keep everything")
	}
}

func TestIncludeExclude(t *testing.T) {
	r, err := New("filter", Patterns{Group: `^(A|B)\|`, NameExclude: `(?i)backup`})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		group, name string
		want        bool
	}{
		{"A| News", "One", true},
		{"B| Sport", "Two", true},
		{"C| Kids", "Three", false},
		{"A| News", "One BACKUP", false},
	}
	for _, c := range cases {
		if got := r.Keep(c.group, c.name); got != c.want {
			t.Errorf("Keep(%q, %q) = %v, want %v", c.group, c.name, got, c.want)
		}
	}
}

func TestEmptyGroupJudgedOnName(t *testing.T) {
	r, err := New("filter", Patterns{Group: `^A\|`, GroupExclude: `^X`, Name: `^(A|B)\|`})
	if err != nil {
		t.Fatal(err)
	}
	// No group: the group patterns do not apply, the name pattern does.
	if !r.Keep("", "A| Film") {
		t.Fatal("uncategorized entry with a kept name must be kept")
	}
	if r.Keep("", "C| Film") {
		t.Fatal("uncategorized entry with a dropped name must be dropped")
	}
	// With a group, both apply.
	if r.Keep("C| Group", "A| Film") {
		t.Fatal("categorized entry must pass the group pattern")
	}
	if !r.KeepGroup("") {
		t.Fatal("an empty group name is kept")
	}
}

func TestCaseInsensitiveOptIn(t *testing.T) {
	r, _ := New("filter", Patterns{NameExclude: `adult`})
	if !r.Keep("", "ADULT channel") {
		t.Fatal("patterns are case sensitive by default")
	}
	r, _ = New("filter", Patterns{NameExclude: `(?i)adult`})
	if r.Keep("", "ADULT channel") {
		t.Fatal("(?i) must make the pattern case insensitive")
	}
}

func TestInvalidRegexNamesOption(t *testing.T) {
	_, err := NewSet(Config{Movie: Patterns{GroupExclude: `(`}})
	if err == nil || !strings.Contains(err.Error(), "--filter-movie-group-exclude-regex") {
		t.Fatalf("error must name the option, got %v", err)
	}
}

func TestKindRulesCombineWithAll(t *testing.T) {
	s, err := NewSet(Config{
		All:  Patterns{NameExclude: `^DROP`},
		Live: Patterns{Group: `^KEEP`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Keep(Live, "KEEP group", "x", false) {
		t.Fatal("live entry passing both sets must be kept")
	}
	if s.Keep(Live, "OTHER", "x", false) {
		t.Fatal("live rules must apply to live entries")
	}
	if !s.Keep(Movie, "OTHER", "x", false) {
		t.Fatal("live rules must not apply to movies")
	}
	if s.Keep(Movie, "", "DROP me", false) {
		t.Fatal("rules for every kind must apply to movies")
	}
	if !s.ActiveFor(Movie) || !s.ActiveFor(Series) {
		t.Fatal("a rule for every kind makes every kind active")
	}
}

func TestActiveForOnlyConfiguredKinds(t *testing.T) {
	s, _ := NewSet(Config{Series: Patterns{Name: `x`}})
	if s.ActiveFor(Live) || s.ActiveFor(Movie) || !s.ActiveFor(Series) {
		t.Fatal("only series should be active")
	}
}

func TestExcludeAdult(t *testing.T) {
	s, err := NewSet(Config{ExcludeAdult: true})
	if err != nil || s == nil {
		t.Fatalf("adult-only config must be active: %v", err)
	}
	if s.Keep(Live, "g", "n", true) || !s.Keep(Live, "g", "n", false) {
		t.Fatal("adult flag must drop only flagged entries")
	}
}

func TestFingerprintTracksRules(t *testing.T) {
	a, _ := NewSet(Config{Live: Patterns{Group: "x"}})
	b, _ := NewSet(Config{Live: Patterns{Group: "x"}})
	c, _ := NewSet(Config{Movie: Patterns{Group: "x"}})
	d, _ := NewSet(Config{Live: Patterns{Group: "x"}, ExcludeAdult: true})
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("same rules must give the same fingerprint")
	}
	if a.Fingerprint() == c.Fingerprint() || a.Fingerprint() == d.Fingerprint() {
		t.Fatal("different rules must give different fingerprints")
	}
}

func TestKindFromPath(t *testing.T) {
	for p, want := range map[string]Kind{
		"/movie/u/p/1.mkv":  Movie,
		"/series/u/p/2.mp4": Series,
		"/live/u/p/3.ts":    Live,
		"/u/p/4":            Unknown,
	} {
		if got := KindFromPath(p); got != want {
			t.Errorf("KindFromPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAnyMatchesGroupOrName(t *testing.T) {
	r, err := New("filter", Patterns{Any: `^\|?(FR|EN)(\||\s)`})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		group, name string
		want        bool
	}{
		{"", "FR - Amélie", true},              // language in the title
		{"|FR| COMÉDIE/DRAME", "Amélie", true}, // language in the group only
		{"|TR| DIZI", "Yesilcam", false},       // neither
		{"", "Untagged title", false},          // no group, untagged name
		{"FOR ADULTS", "EN - Something", true}, // any is not an exclude
	}
	for _, tc := range cases {
		if got := r.Keep(tc.group, tc.name); got != tc.want {
			t.Errorf("Keep(%q, %q) = %v, want %v", tc.group, tc.name, got, tc.want)
		}
	}
	if _, err := New("filter-movie", Patterns{Any: "("}); err == nil || !strings.Contains(err.Error(), "filter-movie-any-regex") {
		t.Fatalf("invalid any regex error should name the option, got %v", err)
	}
}

func TestPlaylistKindDefaultsToLive(t *testing.T) {
	for in, want := range map[string]Kind{
		"http://h/movie/u/p/1.mkv":   Movie,
		"http://h/series/u/p/2.mp4":  Series,
		"http://h/live/u/p/3.ts":     Live,
		"http://h/u/p/4":             Live,
		"http://radio.example/s.mp3": Live,
	} {
		if got := PlaylistKind(in); got != want {
			t.Errorf("PlaylistKind(%q) = %v, want %v", in, got, want)
		}
	}
}
