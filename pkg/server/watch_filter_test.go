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
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/filter"
	"github.com/lucasduport/stream-share/pkg/types"
)

// watchProvider serves movies and series in two "languages" (A and C), plus an
// adult-flagged movie, as a provider's catalogue would.
func watchProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("action") {
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":"7","name":"A - Some Film","category_id":"","container_extension":"mkv"},` +
				`{"stream_id":"8","name":"C - Some Film","category_id":"","container_extension":"mkv"},` +
				`{"stream_id":"9","name":"A - Some Film Night","category_id":"","container_extension":"mkv","is_adult":1}]`))
		case "get_series":
			_, _ = w.Write([]byte(`[{"series_id":"70","name":"A - Some Show","category_id":""},` +
				`{"series_id":"80","name":"C - Some Show","category_id":""}]`))
		case "get_series_info":
			id := r.URL.Query().Get("series_id")
			_, _ = w.Write([]byte(`{"info":{"name":"series ` + id + `"},"episodes":{"1":[` +
				`{"id":"` + id + `1","episode_num":1,"season":1,"title":"S01E01","container_extension":"mkv"}]}}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resultTitles(rs []types.VODResult) string {
	var out []string
	for _, r := range rs {
		t := r.Title
		if r.StreamType == "series" {
			t = r.SeriesTitle
		}
		out = append(out, t)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

// /watch searches what players are offered: the catalogue filter (language
// rules and the adult flag) applies to its movie and series results.
func TestWatchSearchFollowsCatalogueFilter(t *testing.T) {
	invalidateCatalogCache()
	rules := filter.Config{
		Movie:        filter.Patterns{Any: `^A - `},
		Series:       filter.Patterns{Any: `^A - `},
		ExcludeAdult: true,
	}
	c := filteredTestConfig(t, watchProvider(t).URL, rules)

	movies, err := c.searchXtreamMovies("some film")
	if err != nil {
		t.Fatal(err)
	}
	if got := resultTitles(movies); got != "A - Some Film" {
		t.Fatalf("movies = %q, want only the kept, non-adult one", got)
	}

	series, err := c.searchXtreamSeries("some show")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range series {
		if !strings.HasPrefix(r.SeriesTitle, "A - ") {
			t.Fatalf("series result %q is filtered out for players", r.SeriesTitle)
		}
	}
	if len(series) == 0 {
		t.Fatal("the kept series should still be found")
	}
}

// Without rules, /watch searches the whole catalogue as before.
func TestWatchSearchUnfilteredWithoutRules(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig(watchProvider(t).URL)

	movies, err := c.searchXtreamMovies("some film")
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 3 {
		t.Fatalf("movies = %q, want all 3 without rules", resultTitles(movies))
	}
	series, err := c.searchXtreamSeries("some show")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range series {
		seen[r.SeriesTitle[:1]] = true
	}
	if !seen["A"] || !seen["C"] {
		t.Fatalf("series = %q, want both A and C without rules", resultTitles(series))
	}
}
