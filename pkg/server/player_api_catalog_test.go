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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/config"
)

// fakeCatalogProvider is a player_api.php stand-in that counts requests per
// action+category and can be switched to failing.
type fakeCatalogProvider struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	fail  atomic.Bool
	delay time.Duration
}

func newFakeCatalogProvider(t *testing.T) *fakeCatalogProvider {
	t.Helper()
	p := &fakeCatalogProvider{hits: map[string]int{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		cat := r.URL.Query().Get("category_id")
		p.mu.Lock()
		p.hits[action+"|"+cat]++
		p.mu.Unlock()
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
		if p.fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case cat != "":
			_, _ = fmt.Fprintf(w, `[{"stream_id":"9","name":"Only %s","tv_archive":0}]`, cat)
		default:
			_, _ = w.Write([]byte(`[{"stream_id":"1","name":"One","tv_archive":0},{"stream_id":"2","name":"Two","tv_archive":1}]`))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeCatalogProvider) count(action, cat string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[action+"|"+cat]
}

func playerAPITestConfig(baseURL string) *Config {
	gin.SetMode(gin.TestMode)
	return &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: baseURL, XtreamUser: "prov", XtreamPassword: "secret"}}
}

func callPlayerAPI(t *testing.T, c *Config, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php?"+rawQuery, nil)
	c.xtreamPlayerAPI(ctx, q)
	return w
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not a JSON list: %v (%s)", err, w.Body.String())
	}
	return out
}

func TestPlayerAPICatalogServedFromCache(t *testing.T) {
	invalidateCatalogCache()
	p := newFakeCatalogProvider(t)
	c := playerAPITestConfig(p.srv.URL)

	for i := 0; i < 3; i++ {
		if got := decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams")); len(got) != 2 {
			t.Fatalf("call %d: %d items, want 2", i, len(got))
		}
	}
	if n := p.count("get_vod_streams", ""); n != 1 {
		t.Fatalf("provider hit %d times, want 1", n)
	}
	// The /watch search shares the same cache entry.
	if _, err := c.getCachedVODCatalog(); err != nil {
		t.Fatal(err)
	}
	if n := p.count("get_vod_streams", ""); n != 1 {
		t.Fatalf("search re-fetched the catalogue: %d provider hits", n)
	}
}

func TestPlayerAPICategoryNotServedFromFullCatalogue(t *testing.T) {
	invalidateCatalogCache()
	p := newFakeCatalogProvider(t)
	c := playerAPITestConfig(p.srv.URL)

	decodeList(t, callPlayerAPI(t, c, "action=get_live_streams"))
	got := decodeList(t, callPlayerAPI(t, c, "action=get_live_streams&category_id=42"))
	if len(got) != 1 || got[0]["name"] != "Only 42" {
		t.Fatalf("category request got %v, want the provider's filtered list", got)
	}
	decodeList(t, callPlayerAPI(t, c, "action=get_live_streams&category_id=42"))
	if a, b := p.count("get_live_streams", ""), p.count("get_live_streams", "42"); a != 1 || b != 1 {
		t.Fatalf("provider hits: full=%d category=%d, want 1 and 1", a, b)
	}
}

func TestPlayerAPIUnknownParamsBypassCache(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig("http://provider.invalid")
	if _, ok := c.playerAPICatalogKey("get_vod_streams", url.Values{"limit": {"10"}}); ok {
		t.Fatal("request with an unknown parameter must not use the catalog cache")
	}
	if _, ok := c.playerAPICatalogKey("get_vod_info", url.Values{"vod_id": {"1"}}); ok {
		t.Fatal("per-item actions must not use the catalog cache")
	}
}

func TestPlayerAPIServesLastGoodCatalogueWhenProviderFails(t *testing.T) {
	invalidateCatalogCache()
	p := newFakeCatalogProvider(t)
	c := playerAPITestConfig(p.srv.URL)

	decodeList(t, callPlayerAPI(t, c, "action=get_series"))
	p.fail.Store(true)

	// Expire the entry: the next request must still answer, from the last
	// good copy, while a refresh is attempted.
	key := c.catalogKey("get_series", "")
	catalogCacheMu.Lock()
	catalogCache[key].fetchedAt = time.Now().Add(-2 * catalogCacheTTL())
	catalogCacheMu.Unlock()

	if got := decodeList(t, callPlayerAPI(t, c, "action=get_series")); len(got) != 2 {
		t.Fatalf("stale catalogue: %d items, want 2", len(got))
	}
	// A forced refresh failing also falls back to the last good copy.
	e, stale, err := fetchCatalogEntry(key, true, c.catalogFetcher("get_series", nil))
	if err != nil || !stale || e == nil {
		t.Fatalf("forced refresh on failure: entry=%v stale=%v err=%v, want last good copy", e != nil, stale, err)
	}
	waitFor(t, func() bool { return p.count("get_series", "") >= 2 })
}

func TestPlayerAPIEmptyAnswerNotCached(t *testing.T) {
	invalidateCatalogCache()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := playerAPITestConfig(srv.URL)

	for i := 0; i < 2; i++ {
		if got := decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams")); len(got) != 0 {
			t.Fatalf("got %d items, want the provider's empty list", len(got))
		}
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("provider hit %d times, want 2 (empty answers are not cached)", n)
	}
}

func TestPlayerAPIConcurrentRequestsCoalesce(t *testing.T) {
	invalidateCatalogCache()
	p := newFakeCatalogProvider(t)
	p.delay = 50 * time.Millisecond
	c := playerAPITestConfig(p.srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := callPlayerAPI(t, c, "action=get_vod_streams"); w.Code != http.StatusOK {
				t.Errorf("status = %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if n := p.count("get_vod_streams", ""); n != 1 {
		t.Fatalf("provider hit %d times, want 1", n)
	}
}

func TestPlayerAPICatchupFlagsDoNotMutateCachedCatalogue(t *testing.T) {
	invalidateCatalogCache()
	p := newFakeCatalogProvider(t)
	c := playerAPITestConfig(p.srv.URL)
	c.catchupManager = catchup.New(true, t.TempDir(), 3)

	for _, it := range decodeList(t, callPlayerAPI(t, c, "action=get_live_streams")) {
		if fmt.Sprint(it["tv_archive"]) != "1" {
			t.Fatalf("player answer not flagged for catchup: %v", it)
		}
	}
	if !c.catchupManager.HasUpstreamCatchup("2") || c.catchupManager.HasUpstreamCatchup("1") {
		t.Fatal("upstream catchup support must come from the provider's own tv_archive")
	}
	raw, ok := getCachedCatalog(c.catalogKey("get_live_streams", ""))
	if !ok {
		t.Fatal("catalogue not cached")
	}
	for _, it := range raw.([]interface{}) {
		m := it.(map[string]interface{})
		if m["stream_id"] == "1" && fmt.Sprint(m["tv_archive"]) != "0" {
			t.Fatalf("cached catalogue was mutated: %v", m)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// BenchmarkPlayerAPIVODStreams measures a cache hit on a 50k-item catalogue:
// the rendered JSON is reused, so a hit is a copy of bytes, not an encode.
func BenchmarkPlayerAPIVODStreams(b *testing.B) {
	invalidateCatalogCache()
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 50000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"num":%d,"name":"Movie %d (2021) 4K","stream_type":"movie","stream_id":%d,"stream_icon":"http://img.example/%d.jpg","rating":"7.1","added":"1690000000","category_id":"12","container_extension":"mkv"}`, i, i, i, i)
	}
	sb.WriteString("]")
	body := sb.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := playerAPITestConfig(srv.URL)
	q := url.Values{"action": {"get_vod_streams"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php?action=get_vod_streams", nil)
		c.xtreamPlayerAPI(ctx, q)
		if w.Code != http.StatusOK {
			b.Fatalf("status %d", w.Code)
		}
	}
}
