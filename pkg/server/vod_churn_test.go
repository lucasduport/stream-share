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
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
)

// laggyProvider models a provider that allows max connections per account but
// tracks them lazily, at the TCP level: a closed connection keeps counting for
// linger after it closed, and the limit is enforced asynchronously, enforce
// after each new connection, by cutting the oldest connection that is still
// open while the count is over the limit. A burst of reconnects therefore gets
// the live connection cut even though the client never held two at once,
// which is what production showed. Only connections that served the media
// file count (API calls are not media connections).
type laggyProvider struct {
	files   *fakeVODProvider
	max     int
	linger  time.Duration
	enforce time.Duration

	mu    sync.Mutex
	conns map[net.Conn]*laggyConn
	media int // media connections opened
	cuts  int
}

type laggyConn struct {
	opened time.Time
	closed time.Time // zero while open
	cut    bool
}

type laggyConnKey struct{}

func newLaggyProvider(files *fakeVODProvider, linger, enforce time.Duration) *laggyProvider {
	return &laggyProvider{files: files, max: 1, linger: linger, enforce: enforce, conns: map[net.Conn]*laggyConn{}}
}

// start runs the provider on an httptest server wired to track connections.
func (p *laggyProvider) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(p)
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, laggyConnKey{}, c)
	}
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s != http.StateClosed && s != http.StateHijacked {
			return
		}
		p.mu.Lock()
		if lc, ok := p.conns[c]; ok && lc.closed.IsZero() {
			lc.closed = time.Now()
		}
		p.mu.Unlock()
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func (p *laggyProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/series/") {
		if c, ok := r.Context().Value(laggyConnKey{}).(net.Conn); ok {
			p.mu.Lock()
			if _, known := p.conns[c]; !known {
				p.conns[c] = &laggyConn{opened: time.Now()}
				p.media++
				time.AfterFunc(p.enforce, p.enforceLimit)
			}
			p.mu.Unlock()
		}
	}
	p.files.ServeHTTP(w, r)
}

// enforceLimit cuts the oldest open media connection while the count (open
// connections plus closed ones still lingering) is over the limit.
func (p *laggyProvider) enforceLimit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for {
		counted := 0
		var oldest net.Conn
		var oldestLC *laggyConn
		for c, lc := range p.conns {
			if lc.cut {
				continue
			}
			if lc.closed.IsZero() {
				counted++
				if oldestLC == nil || lc.opened.Before(oldestLC.opened) {
					oldest, oldestLC = c, lc
				}
			} else if now.Sub(lc.closed) < p.linger {
				counted++
			}
		}
		if counted <= p.max || oldest == nil {
			return
		}
		oldestLC.cut = true
		oldestLC.closed = now
		p.cuts++
		_ = oldest.Close()
	}
}

func (p *laggyProvider) stats() (media, cuts int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.media, p.cuts
}

// laggyTestServer is progressiveTestServer in front of a laggyProvider.
func laggyTestServer(t *testing.T, p *laggyProvider) (*Config, *httptest.Server) {
	t.Helper()
	t.Setenv("CACHE_FOLDER", t.TempDir())
	gin.SetMode(gin.TestMode)
	up := p.start(t)
	c := &Config{
		ProxyConfig: &config.ProxyConfig{XtreamBaseURL: up.URL, XtreamUser: "user", XtreamPassword: "pass", VODCacheEnabled: true},
		db:          &database.DBManager{},
	}
	r := gin.New()
	h := func(ctx *gin.Context) { c.streamVODWithCache(ctx, "series", ".mkv") }
	r.GET("/series/:id", h)
	r.HEAD("/series/:id", h)
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	vodCacheClient.CloseIdleConnections()
	return c, front
}

// The production access pattern of a player opening a file whose index is at
// the end: header, index near EOF, back to the header area, index again,
// header area again, all within about two seconds. Against a provider that
// counts closed connections for a while and cuts the oldest open one when
// over its limit, the download must not be cut, must open (almost) no new
// connections, and the index reads must be served quickly.
func TestProgressiveIndexReadsDoNotChurnProviderConnections(t *testing.T) {
	data := randomBytes(t, 40<<20) // large enough for the tail fetch (vodTailMinFile)
	// ~8 MB/s: the whole file takes ~5 s.
	files := &fakeVODProvider{data: data, chunk: 64 << 10, delay: 8 * time.Millisecond}
	p := newLaggyProvider(files, 400*time.Millisecond, 150*time.Millisecond)
	c, front := laggyTestServer(t, p)
	url := front.URL + "/series/501.mkv"
	total := int64(len(data))

	check := func(rng string, from int64, n int64) time.Duration {
		t.Helper()
		start := time.Now()
		_, got := readSome(t, url, rng, n)
		took := time.Since(start)
		if !bytes.Equal(got, data[from:from+int64(len(got))]) || int64(len(got)) != n {
			t.Fatalf("%s: %d bytes, content differs or short", rng, len(got))
		}
		return took
	}

	check("bytes=0-", 0, 64<<10)
	idx := total - 300_000
	tookIdx := check(fmt.Sprintf("bytes=%d-", idx), idx, 300_000)
	check("bytes=6000-", 6000, 64<<10)
	idx2 := total - 306_139
	tookIdx2 := check(fmt.Sprintf("bytes=%d-", idx2), idx2, 200_000)
	check("bytes=6000-", 6000, 256<<10)
	t.Logf("index reads served in %v and %v", tookIdx, tookIdx2)

	waitDownloadDone(t, c, "501")
	media, cuts := p.stats()
	t.Logf("media connections opened: %d, cuts: %d", media, cuts)
	if cuts != 0 {
		t.Fatalf("provider cut %d connection(s) during the index reads", cuts)
	}
	if media > 2 {
		t.Fatalf("%d media connections opened; the index reads must not churn connections", media)
	}
	if tookIdx > time.Second || tookIdx2 > time.Second {
		t.Fatalf("index reads took %v / %v, want < 1 s", tookIdx, tookIdx2)
	}
	got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "501.mkv"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cached file differs from the source: %v", err)
	}
}

// A genuine seek to the middle needs bytes far ahead of the download: the
// download's connection is closed for a window, and a new one is unavoidable.
// Opened right away, the lazily-counting provider cuts it; spaced by
// vodConnPacer (longer than the provider's linger), nothing is cut. The
// seek is still served quickly and the cached file comes out byte-identical.
func TestProgressiveMidFileSeekSpacesNewConnections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spacing time.Duration
		wantCut bool
	}{
		{"no spacing", 0, true},
		{"spaced", 600 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := vodConnPacer.setSpacing(tc.spacing)
			t.Cleanup(func() { vodConnPacer.setSpacing(prev) })

			data := randomBytes(t, 32<<20)
			// ~4 MB/s: the whole file takes ~8 s.
			files := &fakeVODProvider{data: data, chunk: 64 << 10, delay: 16 * time.Millisecond}
			p := newLaggyProvider(files, 400*time.Millisecond, 150*time.Millisecond)
			c, front := laggyTestServer(t, p)
			url := front.URL + "/series/502.mkv"

			_, head := readSome(t, url, "bytes=0-", 64<<10)
			if !bytes.Equal(head, data[:len(head)]) {
				t.Fatal("header bytes differ")
			}
			// Let the header and the end of the file in, and the request for
			// the rest run: the seek below then interrupts that request.
			deadline := time.Now().Add(10 * time.Second)
			for {
				if st := downloadState(c, "502"); st != nil && st.written >= 6<<20 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the sequential download never got going")
				}
				time.Sleep(20 * time.Millisecond)
			}
			mid := int64(len(data)) * 3 / 4
			start := time.Now()
			_, got := readSome(t, url, fmt.Sprintf("bytes=%d-", mid), 1<<20)
			took := time.Since(start)
			if !bytes.Equal(got, data[mid:mid+int64(len(got))]) || len(got) != 1<<20 {
				t.Fatalf("mid-file read: %d bytes, content differs or short", len(got))
			}
			t.Logf("mid-file seek served in %v", took)
			if st := downloadState(c, "502"); st == nil || st.written >= mid {
				t.Fatal("the download reached the middle: the seek was not ahead of it")
			}
			if !tc.wantCut && took > 2*time.Second {
				t.Fatalf("mid-file seek took %v, want < 2 s", took)
			}

			waitDownloadDone(t, c, "502")
			media, cuts := p.stats()
			t.Logf("media connections opened: %d, cuts: %d", media, cuts)
			if tc.wantCut && cuts == 0 {
				t.Fatal("expected the provider model to cut an unspaced reconnect; the test no longer exercises the spacing")
			}
			if !tc.wantCut && cuts != 0 {
				t.Fatalf("provider cut %d connection(s) despite the spacing", cuts)
			}
			got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "502.mkv"))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("cached file differs from the source: %v", err)
			}
		})
	}
}
