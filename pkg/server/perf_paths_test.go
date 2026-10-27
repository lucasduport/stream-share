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
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

func serveFile(t *testing.T, p, acceptEncoding, rng string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/f", nil)
	if acceptEncoding != "" {
		ctx.Request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	if rng != "" {
		ctx.Request.Header.Set("Range", rng)
	}
	serveFileCompressed(ctx, p, "application/xml")
	return w
}

func waitForFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A gzip-accepting client gets the plain file until the compressed sibling is
// built, then the sibling; a Range request and a client without gzip always
// get the plain file; a changed file is not served from an older sibling.
func TestServeFileCompressed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "guide.xml")
	body := strings.Repeat("<programme channel=\"c1\"/>\n", 5000)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	w := serveFile(t, p, "gzip, deflate", "")
	if w.Header().Get("Content-Encoding") != "" || w.Body.String() != body {
		t.Fatal("first request must get the plain file while the sibling builds")
	}
	waitForFile(t, p+".gz")
	// The rename lands before the build is unregistered; let it finish.
	time.Sleep(50 * time.Millisecond)

	w = serveFile(t, p, "gzip", "")
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected the gzip sibling once built")
	}
	if w.Body.Len() >= len(body)/4 {
		t.Fatalf("compressed body %d bytes, plain %d", w.Body.Len(), len(body))
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatal("decompressed body differs")
	}
	if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatal("missing Vary: Accept-Encoding")
	}

	if w := serveFile(t, p, "", ""); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("a client without gzip must get the plain file")
	}
	if w := serveFile(t, p, "gzip;q=0", ""); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip;q=0 refuses gzip")
	}
	if w := serveFile(t, p, "gzip", "bytes=0-9"); w.Header().Get("Content-Encoding") != "" || w.Body.Len() != 10 {
		t.Fatal("a Range request must get the plain file's bytes")
	}

	// A new version: the old sibling no longer matches and is not served.
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(p, []byte("<tv/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, later, later)
	if w := serveFile(t, p, "gzip", ""); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "<tv/>" {
		t.Fatal("a sibling of an older version must not be served")
	}
}

func getPHPTestConfig(baseURL string) *Config {
	u, _ := url.Parse(baseURL + "/get.php?username=provuser&password=provpass")
	return &Config{ProxyConfig: &config.ProxyConfig{
		RemoteURL:          u,
		XtreamBaseURL:      baseURL,
		XtreamUser:         "provuser",
		XtreamPassword:     "provpass",
		User:               "localuser",
		Password:           "localpass",
		M3UFileName:        "playlist.m3u",
		M3UCacheExpiration: 1,
	}}
}

func serveGetPHP(t *testing.T, c *Config) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php?username=localuser&password=localpass&type=m3u_plus", nil)
	c.xtreamGet(ctx)
	return w
}

// get.php waits for the provider only when nothing is cached; an expired copy
// is served at once while one background refresh replaces it, the copy
// survives a restart, and a complete playlist doubles as the VOD M3U.
func TestXtreamGetServesStaleWhileRefreshing(t *testing.T) {
	t.Setenv("CACHE_FOLDER", t.TempDir())
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n > 1 {
			<-release
		}
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,Movie " + string(rune('0'+n)) + "\nhttp://p/movie/provuser/provpass/7.mkv\n"))
	}))
	defer srv.Close()
	defer close(release)
	c := getPHPTestConfig(srv.URL)

	w := serveGetPHP(t, c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Movie 1") ||
		!strings.Contains(w.Body.String(), "/movie/localuser/localpass/7.mkv") {
		t.Fatalf("cold get.php: %d %q", w.Code, w.Body.String())
	}
	// The complete playlist was installed as the VOD M3U.
	if ext, title, ok := lookupStreamIndex(vodM3UCachePath(), "movie", "7"); !ok || ext != ".mkv" || title != "Movie 1" {
		t.Fatalf("VOD M3U from get.php: %q %q %v", ext, title, ok)
	}

	// Expire the copy, as after a restart: the meta is rebuilt from disk.
	xtreamM3uCacheLock.Lock()
	for k := range xtreamM3uCache {
		p := xtreamM3uCache[k].string
		old := time.Now().Add(-2 * time.Hour)
		_ = os.Chtimes(p, old, old)
		delete(xtreamM3uCache, k)
	}
	xtreamM3uCacheLock.Unlock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serveGetPHP(t, c) }()
	select {
	case w = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("an expired copy must be served without waiting for the provider")
	}
	if !strings.Contains(w.Body.String(), "Movie 1") {
		t.Fatalf("stale copy expected, got %q", w.Body.String())
	}
	// A second request during the refresh starts no second download.
	_ = serveGetPHP(t, c)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&hits) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("provider hit %d times, want 2 (one background refresh)", n)
	}
}

// The proxified M3U's tracks are served through one route: the index picks
// the track and the file name must match it.
func TestTrackProxyRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("media:" + r.URL.Path))
	}))
	defer upstream.Close()

	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			XtreamUser:     "u",
			XtreamPassword: "p",
		},
		endpointAntiColision: "anti",
		playlist: &m3u.Playlist{Tracks: []m3u.Track{
			{Name: "A", URI: upstream.URL + "/live/x/y/1.ts"},
			{Name: "B", URI: upstream.URL + "/hls/index.m3u8"},
		}},
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	c.m3uRoutes(r.Group("/"))

	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		return w
	}
	if w := get("/anti/u/p/0/1.ts"); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("/live/x/y/1.ts")) {
		t.Fatalf("track 0: %d %q", w.Code, w.Body.String())
	}
	if w := get("/anti/u/p/1/seg42.ts"); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("/hls/seg42.ts")) {
		t.Fatalf("hls track: %d %q", w.Code, w.Body.String())
	}
	for _, p := range []string{"/anti/u/p/0/other.ts", "/anti/u/p/2/1.ts", "/anti/u/p/x/1.ts", "/anti/u/p/1/a/b.ts"} {
		if w := get(p); w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", p, w.Code)
		}
	}
}
