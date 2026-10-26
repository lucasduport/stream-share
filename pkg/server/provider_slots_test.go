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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/provider"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

// oneConnProvider serves several files, one per stream id, and behaves like a
// provider allowing a single connection per account: when a connection opens
// while another is still open, the older one is cut (after a short grace, so
// a client that just closed it is not counted). Cuts are what the arbiter must
// prevent.
type oneConnProvider struct {
	files map[string][]byte // stream id -> content
	chunk int
	delay time.Duration
	allow int // connections allowed at once; 0 = no enforcement

	mu      sync.Mutex
	open    map[int]chan struct{} // connection id -> cut signal
	nextID  int
	maxOpen int
	ranges  map[string][]string // stream id -> Range headers seen
	cuts    int32
}

func newOneConnProvider(files map[string][]byte) *oneConnProvider {
	return &oneConnProvider{files: files, chunk: 64 << 10, delay: 8 * time.Millisecond, allow: 1,
		open: map[int]chan struct{}{}, ranges: map[string][]string{}}
}

func (p *oneConnProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(path.Base(r.URL.Path), path.Ext(r.URL.Path))
	data, ok := p.files[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	p.ranges[id] = append(p.ranges[id], r.Header.Get("Range"))
	older := make([]chan struct{}, 0, len(p.open))
	for _, c := range p.open {
		older = append(older, c)
	}
	p.mu.Unlock()

	// Enforce the limit like a real provider: older connections still open
	// after a moment are cut.
	if p.allow > 0 && len(older) >= p.allow {
		time.Sleep(100 * time.Millisecond)
		p.mu.Lock()
		for cid, c := range p.open {
			if len(p.open) < p.allow {
				break
			}
			close(c)
			delete(p.open, cid)
			atomic.AddInt32(&p.cuts, 1)
		}
		p.mu.Unlock()
	}

	cut := make(chan struct{})
	p.mu.Lock()
	cid := p.nextID
	p.nextID++
	p.open[cid] = cut
	if len(p.open) > p.maxOpen {
		p.maxOpen = len(p.open)
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.open, cid)
		p.mu.Unlock()
	}()

	total := int64(len(data))
	start, last := int64(0), total-1
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
		a, b, _ := strings.Cut(strings.TrimPrefix(rg, "bytes="), "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			if e, err := strconv.ParseInt(b, 10, 64); err == nil && e < last {
				last = e
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, last, total))
		w.Header().Set("Content-Length", strconv.FormatInt(last-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
	fl, _ := w.(http.Flusher)
	for off := start; off <= last; {
		select {
		case <-cut:
			panic(http.ErrAbortHandler) // the provider drops the connection
		case <-r.Context().Done():
			return
		default:
		}
		end := off + int64(p.chunk)
		if end > last+1 {
			end = last + 1
		}
		if _, err := w.Write(data[off:end]); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		off = end
		time.Sleep(p.delay)
	}
}

func (p *oneConnProvider) rangesFor(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ranges[id]...)
}

// slotTestServer is progressiveTestServer with a provider-slot arbiter of the
// given capacity, and a short reader grace so tests need not wait seconds.
func slotTestServer(t *testing.T, p http.Handler, capacity int) (*Config, *httptest.Server) {
	t.Helper()
	c, _, front := progressiveTestServer(t, p, true)
	c.providerSlots = provider.New(func() int { return capacity })
	old := vodReaderGrace
	vodReaderGrace = 100 * time.Millisecond
	t.Cleanup(func() { vodReaderGrace = old })
	return c, front
}

func cachedFile(t *testing.T, id string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", id+".mkv"))
	if err != nil {
		t.Fatalf("cached file %s: %v", id, err)
	}
	return b
}

// resumedFromMiddle reports whether one of the ranges resumes a download past
// its start (bytes=N- with N > 0), i.e. the download was paused and resumed.
func resumedFromMiddle(ranges []string) bool {
	for _, r := range ranges {
		if strings.HasPrefix(r, "bytes=") && strings.HasSuffix(r, "-") && r != "bytes=0-" {
			return true
		}
	}
	return false
}

// The production case: a viewer leaves item A while its download runs, and
// starts item B. B must be served without a single provider cut; A pauses
// while B downloads and completes afterwards, byte-identical, without burning
// its retries.
func TestProviderSlotsSwitchingItemsPausesTheAbandonedDownload(t *testing.T) {
	a, b := randomBytes(t, 12<<20), randomBytes(t, 3<<20)
	p := newOneConnProvider(map[string][]byte{"201": a, "202": b})
	c, front := slotTestServer(t, p, 1)

	// Start watching A, then leave after the first bytes.
	resp, body, err := getRange(t, front.URL+"/series/201.mkv", "bytes=0-65535")
	if err != nil || resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, a[:65536]) {
		t.Fatalf("A first read: status=%v err=%v", resp, err)
	}
	time.Sleep(300 * time.Millisecond) // past the reader grace: A is in the background

	// Watch B from the start to the end.
	resp, body, err = getRange(t, front.URL+"/series/202.mkv", "")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("B: status=%v err=%v", resp, err)
	}
	if !bytes.Equal(body, b) {
		t.Fatalf("B differs from the source (%d vs %d bytes)", len(body), len(b))
	}

	waitDownloadDone(t, c, "202")
	waitDownloadDone(t, c, "201")
	if n := atomic.LoadInt32(&p.cuts); n != 0 {
		t.Fatalf("provider cut %d connection(s); arbitration must avoid every cut", n)
	}
	if !bytes.Equal(cachedFile(t, "201"), a) || !bytes.Equal(cachedFile(t, "202"), b) {
		t.Fatal("cached files differ from the sources")
	}
	if !resumedFromMiddle(p.rangesFor("201")) {
		t.Fatalf("A was never paused and resumed: ranges %v", p.rangesFor("201"))
	}
}

// A background download that is waiting for a slot gets a viewer: it moves to
// the foreground and takes the slot from the other background download.
func TestProviderSlotsReaderReturningToAParkedDownloadResumesIt(t *testing.T) {
	a, b := randomBytes(t, 12<<20), randomBytes(t, 2<<20)
	p := newOneConnProvider(map[string][]byte{"301": a, "302": b})
	c, front := slotTestServer(t, p, 1)

	exp := time.Now().Add(time.Hour)
	upA, destA, _ := c.resolveVODCacheURL("series", ".mkv", "301.mkv")
	upB, destB, _ := c.resolveVODCacheURL("series", ".mkv", "302.mkv")
	c.startBackgroundDownload(upA, destA, "301", "series", exp, nil)
	time.Sleep(150 * time.Millisecond) // A holds the slot
	c.startBackgroundDownload(upB, destB, "302", "series", exp, nil)
	time.Sleep(150 * time.Millisecond) // B waits behind A

	start := time.Now()
	resp, body, err := getRange(t, front.URL+"/series/302.mkv", "")
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, b) {
		t.Fatalf("B: err=%v len=%d", err, len(body))
	}
	// A alone needs ~1.5 s; B jumping ahead of it is served well before.
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("B took %v: it did not take the slot from the background download", d)
	}
	waitDownloadDone(t, c, "301")
	if n := atomic.LoadInt32(&p.cuts); n != 0 {
		t.Fatalf("provider cut %d connection(s)", n)
	}
	if !bytes.Equal(cachedFile(t, "301"), a) {
		t.Fatal("A's cached file differs from the source")
	}
}

// With arbitration off (capacity 0) downloads run side by side, as before.
func TestProviderSlotsDisabledKeepsOldBehaviour(t *testing.T) {
	a, b := randomBytes(t, 4<<20), randomBytes(t, 4<<20)
	p := newOneConnProvider(map[string][]byte{"401": a, "402": b})
	p.allow = 0 // count, but do not cut
	c, _ := slotTestServer(t, p, 0)

	exp := time.Now().Add(time.Hour)
	upA, destA, _ := c.resolveVODCacheURL("series", ".mkv", "401.mkv")
	upB, destB, _ := c.resolveVODCacheURL("series", ".mkv", "402.mkv")
	c.startBackgroundDownload(upA, destA, "401", "series", exp, nil)
	c.startBackgroundDownload(upB, destB, "402", "series", exp, nil)
	waitDownloadDone(t, c, "401")
	waitDownloadDone(t, c, "402")
	p.mu.Lock()
	maxOpen := p.maxOpen
	p.mu.Unlock()
	if maxOpen != 2 {
		t.Fatalf("with arbitration off both downloads should run at once, saw at most %d", maxOpen)
	}
	if !bytes.Equal(cachedFile(t, "401"), a) || !bytes.Equal(cachedFile(t, "402"), b) {
		t.Fatal("cached files differ from the sources")
	}
}

// providerSlotCapacity: forced, disabled, and auto from the cached account info.
func TestProviderSlotCapacity(t *testing.T) {
	c, _, _ := progressiveTestServer(t, http.NotFoundHandler(), true)
	c.ProviderMaxConnections = 3
	if got := c.providerSlotCapacity(); got != 3 {
		t.Fatalf("forced: %d", got)
	}
	c.ProviderMaxConnections = -1
	if got := c.providerSlotCapacity(); got != 0 {
		t.Fatalf("disabled: %d", got)
	}
	c.ProviderMaxConnections = 0
	if got := c.providerSlotCapacity(); got != 0 {
		t.Fatalf("auto without account info must not limit: %d", got)
	}
	c.providerInfo = &providerInfoState{info: &xtreamapi.AccountInfo{MaxConnections: 1}}
	if got := c.providerSlotCapacity(); got != 1 {
		t.Fatalf("auto with the provider reporting 1: %d", got)
	}
	c.ProviderMaxConnections = -1
	if got := c.providerSlotCapacity(); got != 0 {
		t.Fatalf("disabled overrides the reported limit: %d", got)
	}
}
