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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
)

// fakeVODProvider serves one file with Range support and controllable pacing.
type fakeVODProvider struct {
	data        []byte
	chunk       int           // bytes per write
	delay       time.Duration // pause after each write
	stallAt     int64         // absolute offset at which to pause stallFor (once), 0 = never
	stallFor    time.Duration
	abortAt     int64 // absolute offset at which every response is cut, 0 = never
	noLength    bool  // send no Content-Length (chunked)
	ignoreRange bool  // answer every request with the whole file (200)

	requests  int32 // connections opened for the file itself
	stallOnce sync.Once
	paths     sync.Map

	mu     sync.Mutex
	ranges []string    // Range header of each file request ("" for none)
	opened []time.Time // when each file connection opened
	closed []time.Time // when it closed (zero while open)
}

// maxConcurrent returns the most file connections the provider had open at
// once, ignoring overlaps shorter than slack: a client closing one connection
// and opening the next is seen a few milliseconds late by the server, which
// is not two connections in use.
func (p *fakeVODProvider) maxConcurrent(slack time.Duration) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	best := 0
	for _, s := range p.opened {
		n := 0
		for j, o := range p.opened {
			c := p.closed[j]
			if !o.After(s) && (c.IsZero() || c.Add(-slack).After(s)) {
				n++
			}
		}
		if n > best {
			best = n
		}
	}
	return best
}

// rangesSeen returns the Range headers received so far.
func (p *fakeVODProvider) rangesSeen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ranges...)
}

func (p *fakeVODProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.paths.Store(r.URL.Path, true)
	if !strings.HasPrefix(r.URL.Path, "/series/") {
		http.NotFound(w, r) // API calls (titles): not the file
		return
	}
	atomic.AddInt32(&p.requests, 1)
	p.mu.Lock()
	p.ranges = append(p.ranges, r.Header.Get("Range"))
	p.mu.Unlock()
	// A connection counts as open until the client closes it (the request
	// context ends then), which is what a one-connection provider enforces.
	p.mu.Lock()
	idx := len(p.opened)
	p.opened = append(p.opened, time.Now())
	p.closed = append(p.closed, time.Time{})
	p.mu.Unlock()
	go func() {
		<-r.Context().Done()
		p.mu.Lock()
		p.closed[idx] = time.Now()
		p.mu.Unlock()
	}()

	total := int64(len(p.data))
	start, last := int64(0), total-1
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") && !p.ignoreRange {
		spec := strings.TrimPrefix(rg, "bytes=")
		a, b, _ := strings.Cut(spec, "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			if e, err := strconv.ParseInt(b, 10, 64); err == nil && e < last {
				last = e
			}
		}
		if p.noLength {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, last))
		} else {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, last, total))
		}
		if !p.noLength {
			w.Header().Set("Content-Length", strconv.FormatInt(last-start+1, 10))
		}
		w.WriteHeader(http.StatusPartialContent)
	} else {
		if !p.noLength {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		}
		w.WriteHeader(http.StatusOK)
	}
	fl, _ := w.(http.Flusher)
	for off := start; off <= last; {
		if p.abortAt > 0 && off >= p.abortAt {
			panic(http.ErrAbortHandler)
		}
		if p.stallAt > 0 && off >= p.stallAt {
			p.stallOnce.Do(func() { time.Sleep(p.stallFor) })
		}
		end := off + int64(p.chunk)
		if p.abortAt > 0 && off < p.abortAt && end > p.abortAt {
			end = p.abortAt
		}
		if p.stallAt > 0 && off < p.stallAt && end > p.stallAt {
			end = p.stallAt
		}
		if end > last+1 {
			end = last + 1
		}
		if _, err := w.Write(p.data[off:end]); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		off = end
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
	}
}

// assertSinglePass checks the file was downloaded in one pass over a single
// connection at a time: the header request, then at most one request for the
// rest from where the header ended, with only bounded window requests in
// between. Nothing of the sequential pass is downloaded twice.
func assertSinglePass(t *testing.T, p *fakeVODProvider) {
	t.Helper()
	rs := p.rangesSeen()
	head := fmt.Sprintf("bytes=0-%d", vodHeadSize-1)
	rest := fmt.Sprintf("bytes=%d-", vodHeadSize)
	ok := len(rs) == 1 && (rs[0] == "" || rs[0] == head)
	if len(rs) >= 2 && rs[0] == head && rs[len(rs)-1] == rest {
		ok = true
		for _, r := range rs[1 : len(rs)-1] {
			if strings.HasSuffix(r, "-") { // only bounded windows in between
				ok = false
			}
		}
	}
	if !ok {
		t.Fatalf("provider requests %q, want the header then the rest, once", rs)
	}
	if n := p.maxConcurrent(50 * time.Millisecond); n != 1 {
		t.Fatalf("provider had %d connections open at once, want 1", n)
	}
}

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// progressiveTestServer runs the VOD handler in front of provider, the way
// production routes it.
func progressiveTestServer(t *testing.T, provider http.Handler, cacheEnabled bool) (*Config, *httptest.Server, *httptest.Server) {
	t.Helper()
	t.Setenv("CACHE_FOLDER", t.TempDir())
	gin.SetMode(gin.TestMode)
	// These tests are not about connection spacing (see vod_churn_test.go):
	// keep it short so a test does not wait on the previous one's close.
	prevSpacing := vodConnPacer.setSpacing(20 * time.Millisecond)
	t.Cleanup(func() { vodConnPacer.setSpacing(prevSpacing) })
	vodCacheClient.CloseIdleConnections()
	up := httptest.NewServer(provider)
	t.Cleanup(up.Close)
	c := &Config{
		ProxyConfig: &config.ProxyConfig{XtreamBaseURL: up.URL, XtreamUser: "user", XtreamPassword: "pass", VODCacheEnabled: cacheEnabled},
		db:          &database.DBManager{},
	}
	r := gin.New()
	h := func(ctx *gin.Context) { c.streamVODWithCache(ctx, "series", ".mkv") }
	r.GET("/series/:id", h)
	r.HEAD("/series/:id", h)
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return c, up, front
}

func getRange(t *testing.T, url, rng string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

// waitDownloadDone waits for the item's download to leave the registry.
func waitDownloadDone(t *testing.T, c *Config, id string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, busy := c.inProgressDownloads.Load(id); !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("download did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A fresh item is served from its cache download: one provider connection,
// byte-identical content, and the cached file in place once done.
func TestProgressiveServesFromDownloadWithOneUpstreamConnection(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 3<<20), chunk: 64 << 10, delay: 3 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)

	resp, body, err := getRange(t, front.URL+"/series/101.mkv", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Length") != strconv.Itoa(len(p.data)) || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("status=%d len=%q ranges=%q", resp.StatusCode, resp.Header.Get("Content-Length"), resp.Header.Get("Accept-Ranges"))
	}
	if !bytes.Equal(body, p.data) {
		t.Fatalf("body differs from the source (%d vs %d bytes)", len(body), len(p.data))
	}
	waitDownloadDone(t, c, "101")
	assertSinglePass(t, p)
	got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "101.mkv"))
	if err != nil || !bytes.Equal(got, p.data) {
		t.Fatalf("cached file wrong: %v", err)
	}
}

// HEAD gives the total size; a Range inside the downloaded part is served at
// once; a Range far ahead is served once the download gets there; a Range past
// the end is refused. All from the single download.
func TestProgressiveRangesAndHead(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 4<<20), chunk: 64 << 10, delay: 5 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/102.mkv"

	req, _ := http.NewRequest("HEAD", url, nil)
	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = hresp.Body.Close()
	if hresp.Header.Get("Content-Length") != strconv.Itoa(len(p.data)) {
		t.Fatalf("HEAD Content-Length = %q, want %d", hresp.Header.Get("Content-Length"), len(p.data))
	}

	resp, body, err := getRange(t, url, "bytes=100-199")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, p.data[100:200]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(p.data)) {
		t.Fatalf("early range: status=%d cr=%q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}

	tail := len(p.data) - 1000
	resp, body, err = getRange(t, url, fmt.Sprintf("bytes=%d-", tail))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, p.data[tail:]) {
		t.Fatalf("tail range: status=%d len=%d", resp.StatusCode, len(body))
	}

	resp, _, err = getRange(t, url, fmt.Sprintf("bytes=%d-", len(p.data)+10))
	if err == nil && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		// The download may have finished by now; the file path answers 416 too.
		t.Fatalf("past-end range: status=%d, want 416", resp.StatusCode)
	}
	waitDownloadDone(t, c, "102")
	assertSinglePass(t, p)
}

// Two players of the same uncached item share one download.
func TestProgressiveTwoPlayersShareOneDownload(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 2<<20), chunk: 32 << 10, delay: 3 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)

	var wg sync.WaitGroup
	bodies := make([][]byte, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, bodies[i], errs[i] = getRange(t, front.URL+"/series/103.mkv", "")
		}(i)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		if errs[i] != nil || !bytes.Equal(bodies[i], p.data) {
			t.Fatalf("player %d: err=%v len=%d", i, errs[i], len(bodies[i]))
		}
	}
	waitDownloadDone(t, c, "103")
	assertSinglePass(t, p)
}

// A download that stops making progress ends the player's response early (an
// interrupted transfer, so it reconnects) without stopping the download; the
// reconnecting player resumes at its position.
func TestProgressiveStallCutsResponseButDownloadContinues(t *testing.T) {
	prev := vodProgressiveStallTimeout
	vodProgressiveStallTimeout = 300 * time.Millisecond
	defer func() { vodProgressiveStallTimeout = prev }()

	p := &fakeVODProvider{data: randomBytes(t, 2<<20), chunk: 64 << 10, stallAt: 512 << 10, stallFor: time.Second}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/104.mkv"

	_, body, err := getRange(t, url, "")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("stalled response should end as an interrupted transfer, got err=%v len=%d", err, len(body))
	}
	if len(body) == 0 || !bytes.Equal(body, p.data[:len(body)]) {
		t.Fatalf("bytes before the cut are wrong (len=%d)", len(body))
	}

	// Reconnect at the current position like a player, until the download has
	// resumed and the rest arrives.
	got := body
	for try := 0; len(got) < len(p.data); try++ {
		if try > 20 {
			t.Fatalf("player never got the rest (%d/%d bytes)", len(got), len(p.data))
		}
		resp, more, _ := getRange(t, url, fmt.Sprintf("bytes=%d-", len(got)))
		if resp != nil && resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("resume: status %d", resp.StatusCode)
		}
		got = append(got, more...)
	}
	if !bytes.Equal(got, p.data) {
		t.Fatal("content reassembled across reconnects differs from the source")
	}
	waitDownloadDone(t, c, "104")
	assertSinglePass(t, p) // the stall must not restart the download
}

// A download that fails ends the in-flight response as interrupted, never
// promotes the partial file, and the next request starts a new download.
func TestProgressiveDownloadFailure(t *testing.T) {
	prevBackoff := vodCacheRetryBackoff
	vodCacheRetryBackoff = 5 * time.Millisecond
	defer func() { vodCacheRetryBackoff = prevBackoff }()

	p := &fakeVODProvider{data: randomBytes(t, 2<<20), chunk: 64 << 10, abortAt: 1 << 20}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/105.mkv"

	_, body, err := getRange(t, url, "")
	if err == nil {
		t.Fatalf("response of a failed download should be interrupted, got %d clean bytes", len(body))
	}
	if !bytes.Equal(body, p.data[:len(body)]) {
		t.Fatal("bytes served before the failure are wrong")
	}
	waitDownloadDone(t, c, "105")
	first := atomic.LoadInt32(&p.requests)
	if _, err := os.Stat(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "105.mkv")); !os.IsNotExist(err) {
		t.Fatalf("a failed download must not leave a cached file: %v", err)
	}

	_, _, _ = getRange(t, url, "bytes=0-10")
	if n := atomic.LoadInt32(&p.requests); n <= first {
		t.Fatalf("a new request after a failed download should start a new one (requests %d -> %d)", first, n)
	}
	waitDownloadDone(t, c, "105")
}

// A player still reading when the download completes (and the file is
// renamed into place) reaches the end with the full content.
func TestProgressiveCompletesDuringRead(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 2<<20), chunk: 128 << 10}
	c, _, front := progressiveTestServer(t, p, true)

	resp, err := http.Get(front.URL + "/series/106.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	waitDownloadDone(t, c, "106") // finished and renamed while the response is open
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := append(first, rest...); !bytes.Equal(got, p.data) {
		t.Fatalf("content differs after completion mid-read (%d vs %d bytes)", len(got), len(p.data))
	}
}

// Without a provider Content-Length the file is sent whole, chunked, and the
// download end ends it.
func TestProgressiveUnknownLength(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 1<<20), chunk: 64 << 10, delay: 2 * time.Millisecond, noLength: true}
	c, _, front := progressiveTestServer(t, p, true)

	resp, body, err := getRange(t, front.URL+"/series/107.mkv", "bytes=500-")
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, p.data) {
		t.Fatalf("unknown length: err=%v status=%v len=%d", err, resp.StatusCode, len(body))
	}
	waitDownloadDone(t, c, "107")
}

// With the VOD cache disabled requests are proxied to the provider as before.
func TestProgressiveCacheDisabledProxiesDirectly(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 512<<10), chunk: 64 << 10}
	_, _, front := progressiveTestServer(t, p, false)

	_, body, err := getRange(t, front.URL+"/series/108.mkv", "")
	if err != nil || !bytes.Equal(body, p.data) {
		t.Fatalf("direct proxy: err=%v len=%d", err, len(body))
	}
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod")); len(entries) != 0 {
		t.Fatalf("cache disabled must not write files, found %d", len(entries))
	}
	if _, ok := p.paths.Load("/series/user/pass/108.mkv"); !ok {
		t.Fatal("provider was not asked for the item directly")
	}
}

func TestParseSingleRange(t *testing.T) {
	const total = 1000
	cases := []struct {
		in         string
		start, end int64
		ok         bool
	}{
		{"bytes=0-99", 0, 99, true},
		{"bytes=900-", 900, 999, true},
		{"bytes=-100", 900, 999, true},
		{"bytes=990-5000", 990, 999, true},
		{"bytes=-5000", 0, 999, true},
		{"bytes=1000-", 0, 0, false},
		{"bytes=5-2", 0, 0, false},
		{"bytes=0-1,5-6", 0, 0, false},
		{"", 0, 0, false},
		{"items=0-1", 0, 0, false},
	}
	for _, tc := range cases {
		r, ok := parseSingleRange(tc.in, total)
		if ok != tc.ok || (ok && (r[0] != tc.start || r[1] != tc.end)) {
			t.Errorf("parseSingleRange(%q) = %v, %v; want [%d %d], %v", tc.in, r, ok, tc.start, tc.end, tc.ok)
		}
	}
}

// BenchmarkProgressiveRead compares serving from a download in progress (all
// bytes already written) with serving the finished file.
func BenchmarkProgressiveRead(b *testing.B) {
	gin.SetMode(gin.TestMode)
	dir := b.TempDir()
	data := randomBytes(b, 64<<20)
	dest := filepath.Join(dir, "x.mkv")
	if err := os.WriteFile(dest+".part", data, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		b.Fatal(err)
	}
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	run := func(b *testing.B, serve func(*gin.Context)) {
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest("GET", "/x", nil)
			serve(ctx)
			if w.Body.Len() != len(data) {
				b.Fatalf("served %d bytes", w.Body.Len())
			}
		}
	}
	b.Run("in-progress", func(b *testing.B) {
		dl := newVODDownload(dest)
		dl.setTotal(int64(len(data)))
		dl.advance(int64(len(data)))
		run(b, func(ctx *gin.Context) { c.serveVODDownload(ctx, dl, "x", "", false) })
	})
	b.Run("finished-file", func(b *testing.B) {
		run(b, func(ctx *gin.Context) { serveLocalFileRange(ctx, dest, "video/x-matroska", "", false) })
	})
}

// Every VOD response says "no-store": a CDN caching by file extension would
// otherwise fetch the whole file to fill its cache and make a player's Range
// requests wait for that fill, defeating progressive serving.
func TestVODResponsesAreNotStoredByIntermediaries(t *testing.T) {
	p := &fakeVODProvider{data: randomBytes(t, 2<<20), chunk: 64 << 10, delay: 2 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)

	resp, _, err := getRange(t, front.URL+"/series/101.mkv", "bytes=0-1023")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("in-progress response Cache-Control = %q, want no-store", got)
	}
	waitDownloadDone(t, c, "101")

	resp, _, err = getRange(t, front.URL+"/series/101.mkv", "bytes=0-1023")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cached-file response Cache-Control = %q, want no-store", got)
	}

	_, _, direct := progressiveTestServer(t, p, false)
	resp, _, err = getRange(t, direct.URL+"/series/102.mkv", "bytes=0-1023")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("direct-proxy response Cache-Control = %q, want no-store", got)
	}
}
