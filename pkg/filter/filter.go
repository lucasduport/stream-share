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

// Package filter decides which catalogue entries (live channels, movies,
// series) clients get to see, from regular expressions on an entry's group
// (category / group-title) and on its name.
//
// The semantics follow pierre-emmanuelJ/iptv-proxy's channel filters (v3.12):
// include patterns keep what they match, exclude patterns drop what they match
// even when it was kept, an entry is kept when it passes every pattern, and an
// empty pattern does not filter. Expressions are RE2 and case sensitive unless
// they start with (?i).
//
// Two extensions: rules can differ per kind (live, movie, series) on top of
// rules for every kind, and an entry without a group is not dropped by a group
// include pattern, since many providers leave movies and series uncategorized
// and carry their language or origin in the name only.
package filter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Kind is the type of catalogue entry a rule applies to.
type Kind int

// Entry kinds. Unknown is used where the type cannot be told (a plain M3U
// track); only the rules for every kind apply to it.
const (
	Unknown Kind = iota
	Live
	Movie
	Series
)

// String names the kind as it appears in option names.
func (k Kind) String() string {
	switch k {
	case Live:
		return "live"
	case Movie:
		return "movie"
	case Series:
		return "series"
	default:
		return "unknown"
	}
}

// Patterns are the regular expressions of one rule set, as configured. An
// empty pattern does not filter.
type Patterns struct {
	// Group and Name keep what they match.
	Group, Name string
	// GroupExclude and NameExclude drop what they match, even when it was
	// kept above.
	GroupExclude, NameExclude string
}

// Rules are compiled Patterns. A nil *Rules keeps everything.
type Rules struct {
	group, name               *regexp.Regexp
	groupExclude, nameExclude *regexp.Regexp
	desc                      string
}

// New compiles p. option prefixes the option names in errors (e.g.
// "filter-live" gives "filter-live-group-regex"). It returns nil when p
// filters nothing.
func New(option string, p Patterns) (*Rules, error) {
	r := &Rules{}
	for _, f := range []struct {
		name, pattern string
		into          **regexp.Regexp
	}{
		{"group-regex", p.Group, &r.group},
		{"name-regex", p.Name, &r.name},
		{"group-exclude-regex", p.GroupExclude, &r.groupExclude},
		{"name-exclude-regex", p.NameExclude, &r.nameExclude},
	} {
		if f.pattern == "" {
			continue
		}
		re, err := regexp.Compile(f.pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid --%s-%s %q: %w", option, f.name, f.pattern, err)
		}
		*f.into = re
	}
	if r.group == nil && r.name == nil && r.groupExclude == nil && r.nameExclude == nil {
		return nil, nil
	}
	r.desc = strings.Join([]string{p.Group, p.Name, p.GroupExclude, p.NameExclude}, "\x00")
	return r, nil
}

// Keep tells whether an entry with this group and name passes the rules.
//
// A group include pattern only applies to entries that have a group: an
// uncategorized entry is judged on its name alone. A group exclude pattern
// likewise only matches a non-empty group. Name patterns always apply.
func (r *Rules) Keep(group, name string) bool {
	if r == nil {
		return true
	}
	if group != "" {
		if r.group != nil && !r.group.MatchString(group) {
			return false
		}
		if r.groupExclude != nil && r.groupExclude.MatchString(group) {
			return false
		}
	}
	if r.name != nil && !r.name.MatchString(name) {
		return false
	}
	if r.nameExclude != nil && r.nameExclude.MatchString(name) {
		return false
	}
	return true
}

// KeepGroup tells whether a group (category) is kept, whatever the names of
// its entries: it passes the group patterns. Used for category lists.
func (r *Rules) KeepGroup(group string) bool {
	if r == nil || group == "" {
		return true
	}
	if r.group != nil && !r.group.MatchString(group) {
		return false
	}
	if r.groupExclude != nil && r.groupExclude.MatchString(group) {
		return false
	}
	return true
}

// Set holds the rules for every kind and per kind, plus the adult flag. A nil
// *Set keeps everything: callers check Active (or nil) once and skip all
// filtering work on the fast path.
type Set struct {
	all, live, movie, series *Rules
	excludeAdult             bool
}

// Config is the configured filter: the patterns for every kind and per kind.
type Config struct {
	All, Live, Movie, Series Patterns
	// ExcludeAdult drops entries the provider flags as adult (Xtream is_adult).
	ExcludeAdult bool
}

// NewSet compiles cfg. It returns nil (keep everything) when nothing is
// configured, and an error naming the option of an invalid expression.
func NewSet(cfg Config) (*Set, error) {
	s := &Set{excludeAdult: cfg.ExcludeAdult}
	var err error
	if s.all, err = New("filter", cfg.All); err != nil {
		return nil, err
	}
	if s.live, err = New("filter-live", cfg.Live); err != nil {
		return nil, err
	}
	if s.movie, err = New("filter-movie", cfg.Movie); err != nil {
		return nil, err
	}
	if s.series, err = New("filter-series", cfg.Series); err != nil {
		return nil, err
	}
	if !s.Active() {
		return nil, nil
	}
	return s, nil
}

// Active tells whether anything is filtered.
func (s *Set) Active() bool {
	return s != nil && (s.all != nil || s.live != nil || s.movie != nil || s.series != nil || s.excludeAdult)
}

// ActiveFor tells whether entries of kind k may be filtered out.
func (s *Set) ActiveFor(k Kind) bool {
	return s.Active() && (s.all != nil || s.excludeAdult || s.kind(k) != nil)
}

// ExcludesAdult tells whether adult-flagged entries are dropped.
func (s *Set) ExcludesAdult() bool {
	return s != nil && s.excludeAdult
}

func (s *Set) kind(k Kind) *Rules {
	switch k {
	case Live:
		return s.live
	case Movie:
		return s.movie
	case Series:
		return s.series
	}
	return nil
}

// Keep tells whether an entry of kind k is kept. adult is the provider's adult
// flag for the entry (false when unknown).
func (s *Set) Keep(k Kind, group, name string, adult bool) bool {
	if s == nil {
		return true
	}
	if adult && s.excludeAdult {
		return false
	}
	return s.all.Keep(group, name) && s.kind(k).Keep(group, name)
}

// KeepGroup tells whether a category of kind k is kept by the group patterns.
func (s *Set) KeepGroup(k Kind, group string) bool {
	if s == nil {
		return true
	}
	return s.all.KeepGroup(group) && s.kind(k).KeepGroup(group)
}

// Fingerprint identifies the rules: two sets with the same fingerprint keep
// the same entries. Empty for a nil set. Used to key filtered caches.
func (s *Set) Fingerprint() string {
	if !s.Active() {
		return ""
	}
	h := sha256.New()
	for _, r := range []*Rules{s.all, s.live, s.movie, s.series} {
		if r != nil {
			h.Write([]byte(r.desc))
		}
		h.Write([]byte{0xff})
	}
	if s.excludeAdult {
		h.Write([]byte("adult"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// KindFromPath tells the kind of a stream from its Xtream URL path
// ("/movie/u/p/1.mkv" is a movie, "/series/..." a series, "/live/..." or a
// bare "/u/p/1.ts" live). Unknown for anything else.
func KindFromPath(p string) Kind {
	switch {
	case strings.Contains(p, "/movie/"):
		return Movie
	case strings.Contains(p, "/series/"):
		return Series
	case strings.Contains(p, "/live/"):
		return Live
	}
	return Unknown
}
