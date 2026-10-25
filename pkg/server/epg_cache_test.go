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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

// resetEPGState isolates a test from the package-level download state and
// points the cache root at a temp dir.
func resetEPGState(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_FOLDER", t.TempDir())
	epgMu.Lock()
	epgInFlight, epgLastErr, epgFailedAt = nil, nil, time.Time{}
	epgMu.Unlock()
}

func epgTestConfig(baseURL string) *Config {
	return &Config{ProxyConfig: &config.ProxyConfig{
		XtreamBaseURL:      baseURL,
		XtreamUser:         "provuser",
		XtreamPassword:     "provpass",
		M3UCacheExpiration: 1,
	}}
}

// bigGuide builds an XMLTV document of at least n bytes.
func bigGuide(n int) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<tv>\n")
	for i := 0; b.Len() < n; i++ {
		b.WriteString(`  <programme start="20261025000000 +0000" channel="c1"><title>Show</title></programme>` + "\n")
	}
	b.WriteString("</tv>\n")
	return b.Bytes()
}

func serveXMLTV(t *testing.T, c *Config) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/xmltv.php", nil)
	c.xtreamXMLTV(ctx)
	return w
}

// A guide far larger than the old 100 MB-style buffer cap must come through
// whole, the provider must receive the Xtream credentials, and later requests
// must be served from the cache without another provider round trip.
func TestXMLTVServesWholeGuideAndCaches(t *testing.T) {
	resetEPGState(t)
	guide := bigGuide(3 << 20)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/xmltv.php" || r.URL.Query().Get("username") != "provuser" || r.URL.Query().Get("password") != "provpass" {
			http.Error(w, "bad request", http.StatusForbidden)
			return
		}
		_, _ = w.Write(guide)
	}))
	defer srv.Close()
	c := epgTestConfig(srv.URL)

	for i := 0; i < 3; i++ {
		w := serveXMLTV(t, c)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), guide) {
			t.Fatalf("request %d: body is %d bytes, want %d identical bytes", i, w.Body.Len(), len(guide))
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
			t.Fatalf("Content-Type = %q", ct)
		}
		if strings.Contains(w.Body.String(), "provpass") {
			t.Fatal("provider password reached the client")
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("provider hit %d times, want 1", n)
	}
}

// A provider that gzips the guide itself (no Content-Encoding) must still
// produce a plain XML cache.
func TestXMLTVInflatesGzipBody(t *testing.T) {
	resetEPGState(t)
	guide := bigGuide(256 << 10)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(guide)
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(gz.Bytes())
	}))
	defer srv.Close()

	w := serveXMLTV(t, epgTestConfig(srv.URL))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), guide) {
		t.Fatalf("status %d, body %d bytes, want %d plain XML bytes", w.Code, w.Body.Len(), len(guide))
	}
}

// A body that stops sending data must be aborted by the stall timeout, and
// must not leave a cache file or a .part behind.
func TestRefreshEPGAbortsOnStall(t *testing.T) {
	resetEPGState(t)
	old := epgStallTimeout
	epgStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { epgStallTimeout = old })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<?xml version=\"1.0\"?>\n<tv>\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	dest := epgCachePath()
	start := time.Now()
	err := epgTestConfig(srv.URL).refreshEPG(dest)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v, want a stall error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stall detection took %v", d)
	}
	for _, p := range []string{dest, dest + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", filepath.Base(p), err)
		}
	}
}

// A truncated download or an error page must not replace the cached guide,
// and errors must not carry the provider credentials.
func TestRefreshEPGKeepsGoodCacheOnBadDownload(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"truncated": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write([]byte("<?xml version=\"1.0\"?>\n<tv>\n<programme"))
		},
		"error page": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html><body>Account expired</body></html>"))
		},
		"http error": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			resetEPGState(t)
			srv := httptest.NewServer(h)
			defer srv.Close()

			dest := epgCachePath()
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			good := bigGuide(1024)
			if err := os.WriteFile(dest, good, 0o644); err != nil {
				t.Fatal(err)
			}
			err := epgTestConfig(srv.URL).refreshEPG(dest)
			if err == nil {
				t.Fatal("refreshEPG succeeded on a bad download")
			}
			if strings.Contains(err.Error(), "provpass") {
				t.Fatalf("error leaks the provider password: %v", err)
			}
			got, _ := os.ReadFile(dest)
			if !bytes.Equal(got, good) {
				t.Fatal("cached guide was replaced")
			}
			if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
				t.Fatalf(".part left behind: %v", err)
			}
		})
	}
}

// When the cached guide is stale and the provider fails, players keep getting
// the last good guide, and the failure is not retried on every request.
func TestXMLTVServesStaleGuideWhenProviderFails(t *testing.T) {
	resetEPGState(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := epgTestConfig(srv.URL)

	dest := epgCachePath()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	good := bigGuide(1024)
	if err := os.WriteFile(dest, good, 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dest, stale, stale); err != nil {
		t.Fatal(err)
	}

	w := serveXMLTV(t, c)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), good) {
		t.Fatalf("status %d, want the stale guide", w.Code)
	}
	// Let the background refresh fail, then check it is not retried.
	if f := c.startEPGRefresh(); f != nil {
		<-f.done
	}
	for i := 0; i < 3; i++ {
		if w := serveXMLTV(t, c); w.Code != http.StatusOK {
			t.Fatalf("status %d after provider failure", w.Code)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("provider hit %d times, want 1 (backoff)", n)
	}
}

// With no cached guide, a request whose client goes away stops waiting, while
// the shared download carries on and serves the next request.
func TestXMLTVColdWaitHonoursClientCancel(t *testing.T) {
	resetEPGState(t)
	guide := bigGuide(1024)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write(guide)
	}))
	defer srv.Close()
	c := epgTestConfig(srv.URL)

	rctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.waitEPGCache(rctx); err == nil {
		t.Fatal("cold wait returned before the guide existed")
	}
	close(release)
	p, err := c.waitEPGCache(context.Background())
	if err != nil {
		t.Fatalf("waitEPGCache: %v", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, guide) {
		t.Fatal("guide not cached after the shared download finished")
	}
}
