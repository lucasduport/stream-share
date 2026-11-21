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
	"encoding/json"
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
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/types"
)

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

// vodTestDB connects to the Postgres named by the DB_* variables, skipping
// when none is reachable (CI without a database).
func vodTestDB(t *testing.T) *database.DBManager {
	t.Helper()
	db, err := database.NewDBManager("")
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	return db
}

func TestCachedVODEntry(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{}, db: vodTestDB(t)}
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	put := func(id, p string) {
		t.Helper()
		e := &types.VODCacheEntry{StreamID: id, Type: "movie", FilePath: p, Status: "ready", ExpiresAt: time.Now().Add(time.Hour)}
		if err := c.db.UpsertVODCache(e); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []string{"c1-ok", "c1-gone", "c1-dir"} {
			_ = c.db.DeleteVODCacheEntry(id)
		}
	})
	put("c1-ok", file)
	put("c1-gone", filepath.Join(dir, "gone.mp4"))
	put("c1-dir", dir)

	if e := c.cachedVODEntry("c1-ok"); e == nil || e.FilePath != file {
		t.Fatalf("existing file = %+v", e)
	}
	for _, id := range []string{"c1-gone", "c1-dir", "c1-norow"} {
		if e := c.cachedVODEntry(id); e != nil {
			t.Fatalf("%s = %+v, want nil", id, e)
		}
	}
}

// Runs without a database, so CI covers the file rule.
func TestRegularFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want bool
	}{"file": {file, true}, "missing": {filepath.Join(dir, "gone"), false}, "directory": {dir, false}} {
		if got := regularFileExists(tc.path); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

// A ready row whose file is gone used to answer an error from the download
// link; it must fall back to proxying the provider.
func TestTemporaryLinkFallsBackWhenCachedFileMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("from-provider"))
	}))
	t.Cleanup(up.Close)

	db := vodTestDB(t)
	t.Cleanup(func() { _ = db.DeleteVODCacheEntry("c1-link") })
	e := &types.VODCacheEntry{StreamID: "c1-link", Type: "movie", FilePath: filepath.Join(t.TempDir(), "gone.mp4"), Status: "ready", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertVODCache(e); err != nil {
		t.Fatal(err)
	}
	c := &Config{ProxyConfig: &config.ProxyConfig{}, db: db, sessionManager: session.NewSessionManager(nil)}
	t.Cleanup(c.sessionManager.Stop)
	token, _ := c.sessionManager.GenerateTemporaryLink("u", "c1-link", "T", up.URL+"/movie/u/p/c1-link.mp4")

	r := gin.New()
	r.GET("/download/:token", c.handleTemporaryLink)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/download/"+token, nil))
	if w.Code != http.StatusOK || hits.Load() == 0 {
		t.Fatalf("status=%d provider hits=%d body=%q; want fallback to provider", w.Code, hits.Load(), w.Body.String())
	}
}

// startCacheFor posts a Discord-style cache request and returns the server
// config, with a provider slow enough that the download is still running.
func startCacheFor(t *testing.T, db *database.DBManager, streamID string) (*Config, map[string]interface{}) {
	t.Helper()
	t.Setenv("CACHE_FOLDER", t.TempDir())
	gin.SetMode(gin.TestMode)
	up := httptest.NewServer(&fakeVODProvider{data: randomBytes(t, 256<<10), chunk: 4 << 10, delay: 20 * time.Millisecond})
	t.Cleanup(up.Close)
	c := &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: up.URL, XtreamUser: "u", XtreamPassword: "p"}, db: db}
	t.Cleanup(func() { waitDownloadsIdle(c) }) // runs before up.Close
	r := gin.New()
	r.POST("/cache/start", c.startCache)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/cache/start", strings.NewReader(`{"stream_id":"`+streamID+`","type":"series","days":1}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var resp struct{ Data map[string]interface{} }
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q err=%v", w.Code, w.Body.String(), err)
	}
	return c, resp.Data
}

// A download started with an extension must register under the bare id a
// player request uses, or the two download the item twice. No database needed.
func TestStartCacheKeysDownloadByBareID(t *testing.T) {
	c, data := startCacheFor(t, nil, "123.mkv")
	if data["stream_id"] != "123.mkv" {
		t.Errorf("response stream_id = %v, want as requested", data["stream_id"])
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := c.inProgressDownloads.Load("123"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no in-progress download under the bare id")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := c.inProgressDownloads.Load("123.mkv"); ok {
		t.Error("download also registered under the extension-bearing id")
	}
}

func TestStartCacheStoresRowUnderBareID(t *testing.T) {
	db := vodTestDB(t)
	t.Cleanup(func() { _ = db.DeleteVODCacheEntry("c2-777"); _ = db.DeleteVODCacheEntry("c2-777.mkv") })
	startCacheFor(t, db, "c2-777.mkv")
	if e, err := db.GetVODCache("c2-777"); err != nil || e == nil {
		t.Fatalf("no row under the bare id: %v", err)
	}
	if e, err := db.GetVODCache("c2-777.mkv"); err == nil && e != nil {
		t.Error("row also stored under the extension-bearing id")
	}
}

// The Discord bot looks a download up by the id it started it with
// ("123.mkv"); the row is keyed by the bare id.
func TestCacheLookupsAcceptExtensionedID(t *testing.T) {
	db := vodTestDB(t)
	t.Cleanup(func() { _ = db.DeleteVODCacheEntry("c2-lookup") })
	if err := db.UpsertVODCache(&types.VODCacheEntry{StreamID: "c2-lookup", Type: "movie", Status: "downloading", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{}, db: db}
	r := gin.New()
	r.GET("/cache/by-stream/:streamid", c.getCacheByStream)
	r.GET("/cache/progress/:streamid", c.getCacheProgress)
	for _, u := range []string{"/cache/by-stream/c2-lookup.mkv", "/cache/progress/c2-lookup.mkv"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d body %q", u, w.Code, w.Body.String())
		}
	}
}

// waitDownloadsIdle waits for every in-progress download to finish. Where a
// test needs the caches to be fully settled, time is the only signal.
func waitDownloadsIdle(c *Config) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		c.inProgressDownloads.Range(func(_, _ interface{}) bool { busy = true; return false })
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
