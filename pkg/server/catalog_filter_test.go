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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/filter"
)

// filterProvider serves a small catalogue with categories: two live
// categories (one adult-flagged channel), uncategorized movies named with a
// prefix, and one category without any kept entry once filtered.
func filterProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("action") {
		case "get_live_categories":
			_, _ = w.Write([]byte(`[{"category_id":"10","category_name":"A| News","parent_id":0},` +
				`{"category_id":"20","category_name":"B| Sport","parent_id":0},` +
				`{"category_id":"30","category_name":"A| Empty","parent_id":0}]`))
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"stream_id":1,"name":"A| One","category_id":"10","epg_channel_id":"one.a","is_adult":0},` +
				`{"stream_id":2,"name":"B| Two","category_id":"20","epg_channel_id":"two.b","is_adult":0},` +
				`{"stream_id":3,"name":"A| Night","category_id":"10","epg_channel_id":"night.a","is_adult":1},` +
				`{"stream_id":4,"name":"A| Late","category_id":"30","epg_channel_id":"late.a","is_adult":"1"}]`))
		case "get_vod_categories":
			_, _ = w.Write([]byte(`[]`))
		case "get_vod_streams":
			_, _ = w.Write([]byte(`[{"stream_id":"7","name":"A| Film","category_id":""},` +
				`{"stream_id":"8","name":"C| Film","category_id":""}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func filteredTestConfig(t *testing.T, baseURL string, cfg filter.Config) *Config {
	t.Helper()
	c := playerAPITestConfig(baseURL)
	set, err := filter.NewSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.catalogFilter = set
	return c
}

func ids(items []map[string]interface{}, field string) string {
	var out []string
	for _, m := range items {
		out = append(out, catalogString(m[field]))
	}
	return strings.Join(out, ",")
}

var aOnlyNoAdult = filter.Config{All: filter.Patterns{Group: `^A\|`, Name: `^A\|`}, ExcludeAdult: true}

func TestPlayerAPIFiltersLiveListAndKeepsNamesFromFullList(t *testing.T) {
	invalidateCatalogCache()
	c := filteredTestConfig(t, filterProvider(t).URL, aOnlyNoAdult)

	got := decodeList(t, callPlayerAPI(t, c, "action=get_live_streams"))
	if s := ids(got, "stream_id"); s != "1" {
		t.Fatalf("players should get only stream 1, got %s", s)
	}
	// The channel-name index (for /status and logs) comes from the full list.
	for _, id := range []string{"1", "2", "3"} {
		if _, ok := lookupAPIChannelName(id); !ok {
			t.Fatalf("channel %s missing from the name index", id)
		}
	}
	// The cached catalogue (used by /watch) stays unfiltered.
	full, err := c.getCachedLiveStreamsCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(full.([]interface{})); n != 4 {
		t.Fatalf("cached catalogue has %d entries, want the provider's 4", n)
	}
}

func TestPlayerAPIFiltersCategoriesAndPrunesEmptyOnes(t *testing.T) {
	invalidateCatalogCache()
	c := filteredTestConfig(t, filterProvider(t).URL, aOnlyNoAdult)

	// Without the stream list cached, categories are filtered by name only.
	if s := ids(decodeList(t, callPlayerAPI(t, c, "action=get_live_categories")), "category_id"); s != "10,30" {
		t.Fatalf("categories by name = %s, want 10,30", s)
	}
	// Once the list is cached, a category left empty (30: only an adult
	// channel) disappears. The rendered answer is memoised per cached
	// entry, so drop the cache to render again.
	invalidateCatalogCache()
	decodeList(t, callPlayerAPI(t, c, "action=get_live_streams"))
	if s := ids(decodeList(t, callPlayerAPI(t, c, "action=get_live_categories")), "category_id"); s != "10" {
		t.Fatalf("categories after pruning = %s, want 10", s)
	}
}

func TestPlayerAPIFiltersUncategorizedMoviesByName(t *testing.T) {
	invalidateCatalogCache()
	c := filteredTestConfig(t, filterProvider(t).URL, filter.Config{All: filter.Patterns{Group: `^A\|`, Name: `^A\|`}})

	if s := ids(decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams")), "stream_id"); s != "7" {
		t.Fatalf("movies = %s, want 7 (judged on name, no category)", s)
	}
}

func TestPlayerAPIUnfilteredWithoutRules(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig(filterProvider(t).URL)
	if s := ids(decodeList(t, callPlayerAPI(t, c, "action=get_live_streams")), "stream_id"); s != "1,2,3,4" {
		t.Fatalf("no rules must pass everything, got %s", s)
	}
}

func TestFilteredLiveStreamIsNotFound(t *testing.T) {
	invalidateCatalogCache()
	liveKeptMem = nil
	c := filteredTestConfig(t, filterProvider(t).URL, aOnlyNoAdult)

	// No catalogue cached yet: fail open.
	if c.liveStreamFiltered("2") {
		t.Fatal("unknown catalogue must not block streams")
	}
	if _, err := c.getCachedLiveStreamsCatalog(); err != nil {
		t.Fatal(err)
	}
	if !c.liveStreamFiltered("2") || !c.liveStreamFiltered("3.ts") {
		t.Fatal("filtered channels must be refused")
	}
	if c.liveStreamFiltered("1") || c.liveStreamFiltered("999") {
		t.Fatal("kept and unknown channels must be allowed")
	}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/live/u/p/2.ts", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "2.ts"}}
	c.xtreamStreamLive(ctx)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestFilteredEPGPath(t *testing.T) {
	invalidateCatalogCache()
	liveKeptMem = nil
	dir := t.TempDir()
	raw := filepath.Join(dir, "xmltv.xml")
	guide := `<tv><channel id="one.a"></channel><channel id="two.b"></channel>` +
		`<programme channel="one.a"/><programme channel="two.b"/></tv>`
	if err := os.WriteFile(raw, []byte(guide), 0o644); err != nil {
		t.Fatal(err)
	}
	c := filteredTestConfig(t, filterProvider(t).URL, aOnlyNoAdult)

	// No live catalogue cached: serve the provider's guide as is.
	if p := c.filteredEPGPath(raw); p != raw {
		t.Fatalf("without a live catalogue the raw guide is served, got %s", p)
	}
	if _, err := c.getCachedLiveStreamsCatalog(); err != nil {
		t.Fatal(err)
	}
	p := c.filteredEPGPath(raw)
	if p == raw {
		t.Fatal("a filtered guide should be built")
	}
	got, _ := os.ReadFile(p)
	if want := `<tv><channel id="one.a"></channel><programme channel="one.a"/></tv>`; string(got) != want {
		t.Fatalf("filtered guide = %q, want %q", got, want)
	}
	if again := c.filteredEPGPath(raw); again != p {
		t.Fatal("the filtered guide must be reused while nothing changed")
	}
	if b, _ := os.ReadFile(raw); string(b) != guide {
		t.Fatal("the provider's guide must be left untouched")
	}

	// No live rule: the raw guide is served.
	c2 := filteredTestConfig(t, "", filter.Config{Movie: filter.Patterns{Name: "x"}})
	if p := c2.filteredEPGPath(raw); p != raw {
		t.Fatalf("movie-only rules must not filter the guide, got %s", p)
	}
}

func benchLiveList(n int) []interface{} {
	items := make([]interface{}, n)
	for i := range items {
		cat := "10"
		if i%3 == 0 {
			cat = "20"
		}
		items[i] = map[string]interface{}{"stream_id": i, "name": "A| Channel", "category_id": cat, "is_adult": 0, "epg_channel_id": "x"}
	}
	return items
}

func BenchmarkFilterLiveList50k(b *testing.B) {
	c := &Config{}
	c.catalogFilter, _ = filter.NewSet(aOnlyNoAdult)
	items := benchLiveList(50_000)
	names := map[string]string{"10": "A| News", "20": "B| Sport"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.filterEntries(filter.Live, items, names)
	}
}
