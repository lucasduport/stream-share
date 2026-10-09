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
	"os"
	"path/filepath"
	"testing"
)

func writeTempM3U(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.m3u")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFindEntryInM3UReturnsExtAndTitle is the contract the extension and title
// lookups share: one pass must surface both the file extension and the
// #EXTINF title attached to the matching URL.
func TestFindEntryInM3UReturnsExtAndTitle(t *testing.T) {
	m3u := writeTempM3U(t, `#EXTM3U
#EXTINF:-1,Movie One
http://provider.example/movie/user/pass/123.mp4
#EXTINF:-1,Series Pilot
http://provider.example/series/user/pass/456.mkv
`)
	entry := findEntryInM3U(m3u, "movie", "123")
	if entry.ext != ".mp4" {
		t.Fatalf("ext = %q, want .mp4", entry.ext)
	}
	if entry.title != "Movie One" {
		t.Fatalf("title = %q, want %q", entry.title, "Movie One")
	}
	entry = findEntryInM3U(m3u, "series", "456")
	if entry.ext != ".mkv" || entry.title != "Series Pilot" {
		t.Fatalf("series entry = %+v", entry)
	}
}

// TestFindEntryInM3UMissReturnsZero guards the memoisation contract: a miss
// must be the zero value so the cache stores an expiring miss, and a wrong
// basePath or unknown ID must not match anything.
func TestFindEntryInM3UMissReturnsZero(t *testing.T) {
	m3u := writeTempM3U(t, `#EXTM3U
#EXTINF:-1,Movie One
http://provider.example/movie/user/pass/123.mp4
`)
	if got := findEntryInM3U(m3u, "series", "123"); got != (vodEntry{}) {
		t.Fatalf("wrong basePath matched: %+v", got)
	}
	if got := findEntryInM3U(m3u, "movie", "999"); got != (vodEntry{}) {
		t.Fatalf("unknown id matched: %+v", got)
	}
	// An id that is a prefix of another id must not match the longer one.
	if got := findEntryInM3U(m3u, "movie", "12"); got != (vodEntry{}) {
		t.Fatalf("prefix id matched: %+v", got)
	}
}

// TestFindEntryInM3UResetsTitleBetweenEntries: the title of a previous entry
// must not leak onto a later URL that does not match.
func TestFindEntryInM3UResetsTitleBetweenEntries(t *testing.T) {
	m3u := writeTempM3U(t, `#EXTM3U
#EXTINF:-1,Unrelated
http://provider.example/movie/user/pass/111.mp4
#EXTINF:-1,Target
http://provider.example/movie/user/pass/222.mkv
`)
	// 111 is scanned (title captured, then reset), 222 matches with its title.
	entry := findEntryInM3U(m3u, "movie", "222")
	if entry.title != "Target" {
		t.Fatalf("title = %q, want %q", entry.title, "Target")
	}
}

// TestDefaultVODExt pins the per-type defaults used when no extension can be
// resolved synchronously.
func TestDefaultVODExt(t *testing.T) {
	if got := defaultVODExt("movie"); got != ".mp4" {
		t.Fatalf("movie default = %q, want .mp4", got)
	}
	if got := defaultVODExt("series"); got != ".mkv" {
		t.Fatalf("series default = %q, want .mkv", got)
	}
}

// TestVODFallbackTitle pins the placeholder-title precedence: series metadata
// first, then the caller-supplied title, then a constant.
func TestVODFallbackTitle(t *testing.T) {
	if got := vodFallbackTitle("series", "Ep title", "My Series", 2, 4); got != "My Series — S02E04" {
		t.Fatalf("series fallback = %q", got)
	}
	if got := vodFallbackTitle("movie", "  Some Movie  ", "", 0, 0); got != "Some Movie" {
		t.Fatalf("movie fallback = %q", got)
	}
	if got := vodFallbackTitle("movie", "", "", 0, 0); got != "Unknown title" {
		t.Fatalf("empty fallback = %q", got)
	}
	// A series without episode context falls through to the plain title.
	if got := vodFallbackTitle("series", "Ep title", "My Series", 0, 0); got != "Ep title" {
		t.Fatalf("series no-context fallback = %q", got)
	}
}
