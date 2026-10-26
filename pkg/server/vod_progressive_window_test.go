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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- window bookkeeping -----------------------------------------------------

func TestVODDownloadReadableEnd(t *testing.T) {
	d := newVODDownload(filepath.Join(t.TempDir(), "x.mkv"))
	d.setTotal(100 << 20)
	d.advance(10)
	d.addWindow(50, 60)
	d.addWindow(60, 70) // adjacent: merged
	d.addWindow(80, 90)
	d.addWindow(85, 95) // overlapping: merged
	d.addWindow(5, 8)   // inside the prefix: ignored

	cases := []struct{ off, want int64 }{
		{0, 10},  // prefix
		{9, 10},  // prefix end
		{10, 10}, // nothing at the prefix end yet
		{30, 30}, // gap
		{50, 70}, // merged window
		{65, 70}, // inside it
		{70, 70}, // just after it
		{80, 95}, // overlapping windows merged
		{100, 100},
	}
	for _, tc := range cases {
		if got := d.readableEnd(tc.off); got != tc.want {
			t.Errorf("readableEnd(%d) = %d, want %d", tc.off, got, tc.want)
		}
	}
	if len(d.windows) != 2 {
		t.Fatalf("windows = %v, want 2 merged spans", d.windows)
	}

	// The prefix reaching a window absorbs it, and the windows past it chain.
	d.advance(45) // written = 55
	if got := d.readableEnd(0); got != 70 {
		t.Fatalf("prefix chained into the window: readableEnd(0) = %d, want 70", got)
	}
	d.advance(20) // written = 75: the first window is now inside the prefix
	if len(d.windows) != 1 || d.windows[0] != (byteSpan{80, 95}) {
		t.Fatalf("windows after the prefix passed one = %v", d.windows)
	}
}

func TestVODDownloadRequestWindow(t *testing.T) {
	d := newVODDownload(filepath.Join(t.TempDir(), "x.mkv"))

	d.requestWindow(50 << 20)
	if d.windowWanted() {
		t.Fatal("window requested before the total size is known")
	}
	d.setTotal(100 << 20)
	d.advance(1 << 20)

	d.requestWindow(2 << 20) // 1 MiB ahead: the sequential download brings it
	if d.windowWanted() {
		t.Fatal("window requested for a small gap")
	}

	d.requestWindow(90 << 20)
	d.requestWindow(91 << 20) // inside the pending window: deduplicated
	if len(d.wanted) != 1 || d.wanted[0] != (byteSpan{90 << 20, 90<<20 + vodWindowSize}) {
		t.Fatalf("wanted = %v", d.wanted)
	}
	select {
	case <-d.wake:
	default:
		t.Fatal("the downloader was not woken")
	}

	d.requestWindow(100<<20 - 10) // near the end: clamped to the total
	if w := d.wanted[1]; w.end != 100<<20 {
		t.Fatalf("window near EOF = %v, want end clamped to the total", w)
	}

	// Part of a window already fetched: only the rest is fetched.
	d.addWindow(90<<20, 91<<20)
	w, ok := d.nextWindow()
	if !ok || w.start != 91<<20 {
		t.Fatalf("nextWindow = %v, %v; want it to start after the fetched part", w, ok)
	}

	// The provider ignoring Range disables windows for good.
	d.windowFailed(true)
	if d.windowWanted() {
		t.Fatal("windows still wanted after the provider ignored Range")
	}
	d.requestWindow(60 << 20)
	if d.windowWanted() {
		t.Fatal("window requested after windows were disabled")
	}
}

func TestVODDownloadWindowFailuresDisable(t *testing.T) {
	d := newVODDownload(filepath.Join(t.TempDir(), "x.mkv"))
	d.setTotal(100 << 20)
	for i := 0; i < vodWindowMaxFailures-1; i++ {
		d.windowFailed(false)
		if d.noWindows {
			t.Fatalf("windows disabled after %d failure(s)", i+1)
		}
	}
	d.windowFailed(false)
	if !d.noWindows {
		t.Fatal("windows still enabled after the failure limit")
	}
}

func TestVODDownloadRestartClearsWindows(t *testing.T) {
	d := newVODDownload(filepath.Join(t.TempDir(), "x.mkv"))
	d.setTotal(100 << 20)
	d.advance(1 << 20)
	d.addWindow(50<<20, 51<<20)
	d.requestWindow(80 << 20)
	d.restart()
	if d.written != 0 || len(d.windows) != 0 || len(d.wanted) != 0 || !d.noWindows {
		t.Fatalf("after restart: written=%d windows=%v wanted=%v noWindows=%v", d.written, d.windows, d.wanted, d.noWindows)
	}
}

// --- end to end, through the real handler -----------------------------------

// readSome opens url with rng, reads up to n bytes (all when n < 0) and closes
// the connection, the way a demuxer reads a few bytes then seeks elsewhere.
func readSome(t *testing.T, url, rng string, n int64) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r io.Reader = resp.Body
	if n >= 0 {
		r = io.LimitReader(resp.Body, n)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading %s: %v", rng, err)
	}
	return resp, body
}

// downloadState returns the running download of id, nil once it is done.
func downloadState(c *Config, id string) *vodDownloadState {
	v, ok := c.inProgressDownloads.Load(id)
	if !ok {
		return nil
	}
	st := v.(*vodDownload).state()
	return &st
}

// The access pattern of ffmpeg opening a file whose index is at the end:
// read the header, jump near the end, jump back. The jump ahead must be
// served in moments (not after the whole download), over a single provider
// connection, and the cached file must still come out byte-identical.
func TestProgressiveSeekNearEndServedWithoutWaiting(t *testing.T) {
	data := randomBytes(t, 12<<20)
	// ~1.6 MB/s: the whole file takes ~8 s to download.
	p := &fakeVODProvider{data: data, chunk: 64 << 10, delay: 40 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/201.mkv"
	total := int64(len(data))

	_, head := readSome(t, url, "bytes=0-", 32<<10)
	if !bytes.Equal(head, data[:len(head)]) {
		t.Fatal("header bytes differ")
	}

	tailFrom := total - 300_000
	start := time.Now()
	resp, tail := readSome(t, url, fmt.Sprintf("bytes=%d-", tailFrom), -1)
	took := time.Since(start)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("tail status %d", resp.StatusCode)
	}
	if !bytes.Equal(tail, data[tailFrom:]) {
		t.Fatalf("tail bytes differ (%d bytes)", len(tail))
	}
	t.Logf("tail served in %v", took)
	if took > 3*time.Second {
		t.Fatalf("tail took %v; it must not wait for the download (~8 s)", took)
	}
	if st := downloadState(c, "201"); st == nil || st.written >= total {
		t.Fatal("the download already finished: the test did not exercise a read ahead of it")
	}

	_, back := readSome(t, url, "bytes=1000-", 64<<10)
	if !bytes.Equal(back, data[1000:1000+len(back)]) {
		t.Fatal("bytes after jumping back differ")
	}

	waitDownloadDone(t, c, "201")
	if n := p.maxConcurrent(50 * time.Millisecond); n != 1 {
		t.Fatalf("provider had %d connections open at once, want 1 (ranges %v)", n, p.rangesSeen())
	}
	bounded := false
	for _, r := range p.rangesSeen() {
		if strings.Count(r, "-") == 1 && !strings.HasSuffix(r, "-") {
			bounded = true
		}
	}
	if !bounded {
		t.Fatalf("no ranged window request reached the provider: %v", p.rangesSeen())
	}
	got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "201.mkv"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cached file differs from the source: %v", err)
	}
}

// A seek to the middle followed by continuous reading crosses several
// windows: every byte must be right, and still one provider connection.
func TestProgressiveSeekMiddleReadsAcrossWindows(t *testing.T) {
	prev := vodWindowSize
	vodWindowSize = 1 << 20
	defer func() { vodWindowSize = prev }()

	// ~2 MB/s: the sequential download needs ~8 s to reach 16 MiB.
	data := randomBytes(t, 24<<20)
	p := &fakeVODProvider{data: data, chunk: 64 << 10, delay: 30 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/202.mkv"

	_, _ = readSome(t, url, "bytes=0-", 16<<10)
	from := int64(16 << 20)
	start := time.Now()
	_, body := readSome(t, url, fmt.Sprintf("bytes=%d-", from), 2<<20)
	if !bytes.Equal(body, data[from:from+int64(len(body))]) || len(body) != 2<<20 {
		t.Fatalf("read %d bytes from the middle, content differs or short", len(body))
	}
	took := time.Since(start)
	t.Logf("2 MiB from the middle in %v", took)
	if took > 5*time.Second {
		t.Fatalf("reading 2 MiB from the middle took %v; the download alone needs ~8 s to get there", took)
	}

	waitDownloadDone(t, c, "202")
	if n := p.maxConcurrent(50 * time.Millisecond); n != 1 {
		t.Fatalf("provider had %d connections open at once, want 1", n)
	}
	got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "202.mkv"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cached file differs from the source: %v", err)
	}
}

// A provider that ignores Range cannot serve windows: readers fall back to
// waiting for the sequential download, and the content stays right.
func TestProgressiveSeekWhenProviderIgnoresRange(t *testing.T) {
	data := randomBytes(t, 6<<20)
	p := &fakeVODProvider{data: data, chunk: 64 << 10, delay: 10 * time.Millisecond, ignoreRange: true}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/203.mkv"
	total := int64(len(data))

	_, _ = readSome(t, url, "bytes=0-", 16<<10)
	_, tail := readSome(t, url, fmt.Sprintf("bytes=%d-", total-100_000), -1)
	if !bytes.Equal(tail, data[total-100_000:]) {
		t.Fatal("tail bytes differ")
	}

	waitDownloadDone(t, c, "203")
	if n := p.maxConcurrent(50 * time.Millisecond); n != 1 {
		t.Fatalf("provider had %d connections open at once, want 1", n)
	}
	got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", "203.mkv"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cached file differs from the source: %v", err)
	}
}

// A read just ahead of the download waits for it rather than pausing it for a
// window.
func TestProgressiveSmallGapUsesNoWindow(t *testing.T) {
	data := randomBytes(t, 4<<20)
	p := &fakeVODProvider{data: data, chunk: 64 << 10, delay: 5 * time.Millisecond}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/204.mkv"

	_, body := readSome(t, url, "bytes=1048576-", 64<<10)
	if !bytes.Equal(body, data[1<<20:1<<20+len(body)]) {
		t.Fatal("bytes differ")
	}
	waitDownloadDone(t, c, "204")
	for _, r := range p.rangesSeen() {
		if r != "" {
			t.Fatalf("a ranged request reached the provider for a small gap: %v", p.rangesSeen())
		}
	}
}
