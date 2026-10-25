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
	"bytes"
	"encoding/json"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
)

func TestFormatEpisodeTitle(t *testing.T) {
	cases := []struct {
		series        string
		season, ep    int
		epTitle, want string
	}{
		{"Show", 1, 2, "", "Show — S01E02"},
		{"Show", 1, 2, "Pilot", "Show — S01E02 — Pilot"},
		// Providers often repeat the series and number as the episode title.
		{"Show", 1, 2, "Show S01E02", "Show — S01E02"},
		{"Show", 1, 2, "Show - S01 E02", "Show — S01E02"},
		{"Show", 1, 2, "Episode 2", "Show — S01E02"},
		{"Show", 10, 12, "The End", "Show — S10E12 — The End"},
		{"", 1, 2, "Lone", "Lone"},
	}
	for _, tc := range cases {
		if got := formatEpisodeTitle(tc.series, tc.season, tc.ep, tc.epTitle); got != tc.want {
			t.Errorf("formatEpisodeTitle(%q,%d,%d,%q) = %q, want %q", tc.series, tc.season, tc.ep, tc.epTitle, got, tc.want)
		}
	}
}

// decodeLikeXtream decodes JSON the way xtream.Client.Action does (UseNumber),
// so ids and numbers arrive as json.Number like in production.
func decodeLikeXtream(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var v interface{}
	d := json.NewDecoder(bytes.NewReader([]byte(s)))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.(map[string]interface{})
}

func resetVODNameState(t *testing.T) {
	t.Helper()
	vodNameMu.Lock()
	vodNameIndex = map[string]string{}
	vodNameMu.Unlock()
	titleResolvedMu.Lock()
	titleResolvedIndex = map[string]titleAttempt{}
	titleResolvedMu.Unlock()
}

const seriesInfoJSON = `{
  "info": {"name": "Dark"},
  "episodes": {
    "1": [
      {"id": "1001", "episode_num": 1, "season": 1, "title": "Dark S01E01", "container_extension": "mkv"},
      {"id": 1002, "episode_num": "2", "title": "Lies"}
    ],
    "2": [
      {"id": "2001", "episode_num": 1, "season": 2, "title": "Beginnings and Endings"}
    ]
  }
}`

// Every episode of a get_series_info answer gets a title, not only those a
// search query happened to match.
func TestHarvestSeriesEpisodeTitles(t *testing.T) {
	resetVODNameState(t)
	c := &Config{ProxyConfig: &config.ProxyConfig{}}

	if n := c.harvestSeriesEpisodeTitles("", decodeLikeXtream(t, seriesInfoJSON)); n != 3 {
		t.Fatalf("harvested %d titles, want 3", n)
	}
	want := map[string]string{
		"1001": "Dark — S01E01",
		"1002": "Dark — S01E02 — Lies", // season taken from the map key
		"2001": "Dark — S02E01 — Beginnings and Endings",
	}
	for id, w := range want {
		if got, ok := vodNameCached(id); !ok || got != w {
			t.Errorf("title of %s = %q (%v), want %q", id, got, ok, w)
		}
	}
}

// An explicit series name (from the search, which knows it) wins over info.name.
func TestHarvestSeriesEpisodeTitlesUsesGivenSeriesName(t *testing.T) {
	resetVODNameState(t)
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	c.harvestSeriesEpisodeTitles("Dark (2017)", decodeLikeXtream(t, seriesInfoJSON))
	if got, _ := vodNameCached("1001"); got != "Dark (2017) — S01E01" {
		t.Fatalf("title = %q", got)
	}
}

func TestHarvestSeriesEpisodeTitlesIgnoresUnusableAnswers(t *testing.T) {
	resetVODNameState(t)
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	for _, s := range []string{`{}`, `{"info": {"name": "X"}}`, `{"episodes": {"1": [{"id": "5"}]}}`, `{"info": {"name": "X"}, "episodes": []}`} {
		if n := c.harvestSeriesEpisodeTitles("", decodeLikeXtream(t, s)); n != 0 {
			t.Errorf("%s: harvested %d, want 0", s, n)
		}
	}
	if n := c.harvestSeriesEpisodeTitles("", nil); n != 0 {
		t.Errorf("nil: harvested %d", n)
	}
}

// A playback that started before the series was browsed memoises a miss; the
// harvest must clear it so the next lookup resolves instead of waiting out
// titleMissRetryAfter.
func TestHarvestClearsMemoisedSeriesMiss(t *testing.T) {
	resetVODNameState(t)
	t.Setenv("CACHE_FOLDER", t.TempDir())
	c := &Config{ProxyConfig: &config.ProxyConfig{}}

	titleResolvedStore("1001", "") // earlier miss
	if title, done := titleResolvedLookup("1001"); !done || title != "" {
		t.Fatalf("precondition: miss not memoised (%q, %v)", title, done)
	}

	c.harvestSeriesEpisodeTitles("", decodeLikeXtream(t, seriesInfoJSON))

	if _, done := titleResolvedLookup("1001"); done {
		t.Fatal("memoised miss survived the harvest")
	}
	if title, ok := c.resolveTitleAtStart("1001", "series"); !ok || title != "Dark — S01E01" {
		t.Fatalf("resolveTitleAtStart = %q, %v", title, ok)
	}
}
