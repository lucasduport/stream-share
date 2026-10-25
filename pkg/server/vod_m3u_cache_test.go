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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
)

// resetVODM3UState isolates a test from the package-level download state and
// points the cache root at a temp dir.
func resetVODM3UState(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_FOLDER", t.TempDir())
	vodM3UMu.Lock()
	vodM3UInFlight, vodM3ULastErr, vodM3UFailedAt = nil, nil, time.Time{}
	vodM3UMu.Unlock()
}

func vodM3UTestConfig(baseURL string) *Config {
	return &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: baseURL, M3UCacheExpiration: 1}}
}

// A cold cache must not block the caller, and concurrent callers must share a
// single provider download.
func TestEnsureVODM3UCacheColdIsNonBlockingAndCoalesced(t *testing.T) {
	resetVODM3UState(t)
	release := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,Movie\nhttp://p/movie/u/p/1.mkv\n"))
	}))
	defer srv.Close()
	c := vodM3UTestConfig(srv.URL)

	for i := 0; i < 5; i++ {
		if _, err := c.ensureVODM3UCache(); !errors.Is(err, errVODM3UNotReady) {
			t.Fatalf("cold call %d: err = %v, want errVODM3UNotReady", i, err)
		}
	}
	close(release)
	p, err := c.waitVODM3UCache()
	if err != nil {
		t.Fatalf("waitVODM3UCache: %v", err)
	}
	if ext, title, ok := lookupStreamIndex(p, "movie", "1"); !ok || ext != ".mkv" || title != "Movie" {
		t.Fatalf("lookup = %q %q %v", ext, title, ok)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("provider hit %d times, want 1", n)
	}
}

// An aborted download must leave the previous cache intact rather than a
// truncated file with a fresh mtime.
func TestRefreshVODM3UKeepsOldCacheOnFailure(t *testing.T) {
	resetVODM3UState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1,Trunc"))
		// Short body vs Content-Length: the client sees an unexpected EOF.
	}))
	defer srv.Close()
	c := vodM3UTestConfig(srv.URL)

	cacheFile := vodM3UCachePath()
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	const good = "#EXTM3U\n#EXTINF:-1,Good\nhttp://p/movie/u/p/2.mp4\n"
	if err := os.WriteFile(cacheFile, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.refreshVODM3U(cacheFile); err == nil {
		t.Fatal("refreshVODM3U succeeded on a truncated body")
	}
	got, err := os.ReadFile(cacheFile)
	if err != nil || string(got) != good {
		t.Fatalf("cache file = %q, %v; want previous content", got, err)
	}
	if _, err := os.Stat(cacheFile + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part file left behind: %v", err)
	}
}

// After a failure, lookups must not retrigger a download until the backoff
// elapses.
func TestStartVODM3URefreshBacksOffAfterFailure(t *testing.T) {
	resetVODM3UState(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := vodM3UTestConfig(srv.URL)

	if _, err := c.waitVODM3UCache(); err == nil {
		t.Fatal("expected failure")
	}
	for i := 0; i < 3; i++ {
		_, _ = c.ensureVODM3UCache()
	}
	if f := c.startVODM3URefresh(); f != nil {
		t.Fatal("refresh started during backoff")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("provider hit %d times, want 1", n)
	}
}
