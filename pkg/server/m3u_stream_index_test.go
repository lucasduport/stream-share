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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildStreamIndex(t *testing.T) {
	m3u := `#EXTM3U
#EXTINF:-1 tvg-id="ch1" tvg-name="Channel One" group-title="News",Channel One
http://provider.example.com/live/user/pass/12345.ts
#EXTINF:-1 tvg-id="mv1" tvg-name="Some Movie" group-title="Movies",Some Movie
http://provider.example.com/movie/user/pass/67890.mp4
#EXTINF:-1,Series Episode
http://provider.example.com/series/user/pass/11111.mkv
`
	idx := buildStreamIndex(strings.NewReader(m3u))

	// Live entry
	e, ok := idx["live\x0012345"]
	if !ok {
		t.Fatal("missing live entry")
	}
	if e.Ext != ".ts" {
		t.Fatalf("live ext = %q, want .ts", e.Ext)
	}
	if e.Title != "Channel One" {
		t.Fatalf("live title = %q, want %q", e.Title, "Channel One")
	}

	// Movie entry
	e, ok = idx["movie\x0067890"]
	if !ok {
		t.Fatal("missing movie entry")
	}
	if e.Ext != ".mp4" {
		t.Fatalf("movie ext = %q, want .mp4", e.Ext)
	}
	if e.Title != "Some Movie" {
		t.Fatalf("movie title = %q, want %q", e.Title, "Some Movie")
	}

	// Series entry
	e, ok = idx["series\x0011111"]
	if !ok {
		t.Fatal("missing series entry")
	}
	if e.Ext != ".mkv" {
		t.Fatalf("series ext = %q, want .mkv", e.Ext)
	}
	if e.Title != "Series Episode" {
		t.Fatalf("series title = %q, want %q", e.Title, "Series Episode")
	}
}

func TestExtractBasePath(t *testing.T) {
	cases := map[string]string{
		"/live/user/pass/123.ts":     "live",
		"/movie/user/pass/456.mp4":   "movie",
		"/series/user/pass/789.mkv":  "series",
		"/timeshift/u/p/1/2/3":       "timeshift",
		"/other/path/123":            "",
		"/user/pass/123.ts":          "",
	}
	for path, want := range cases {
		if got := extractBasePath(path); got != want {
			t.Errorf("extractBasePath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestGetStreamIndexCachesByMtime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.m3u")
	content := "#EXTM3U\n#EXTINF:-1,Test\nhttp://h/movie/u/p/1.mp4\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	idx1 := getStreamIndex(p)
	if idx1 == nil {
		t.Fatal("expected index")
	}
	if _, ok := idx1["movie\x001"]; !ok {
		t.Fatal("missing entry in first index")
	}

	// Same mtime -> same map instance (cached).
	idx2 := getStreamIndex(p)
	if idx1["movie\x001"].Ext != idx2["movie\x001"].Ext {
		t.Fatal("index entries differ between calls")
	}
}

func TestLookupStreamIndex(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lookup.m3u")
	content := "#EXTM3U\n#EXTINF:-1,My Movie\nhttp://h/movie/u/p/42.mp4\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ext, title, found := lookupStreamIndex(p, "movie", "42")
	if !found {
		t.Fatal("expected found")
	}
	if ext != ".mp4" {
		t.Fatalf("ext = %q, want .mp4", ext)
	}
	if title != "My Movie" {
		t.Fatalf("title = %q, want %q", title, "My Movie")
	}

	// Miss
	if _, _, found := lookupStreamIndex(p, "movie", "999"); found {
		t.Fatal("expected miss for unknown id")
	}
	// Wrong basePath
	if _, _, found := lookupStreamIndex(p, "series", "42"); found {
		t.Fatal("expected miss for wrong basePath")
	}
}
