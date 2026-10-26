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
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// Progressive VOD serving.
//
// When an uncached movie or episode is requested, the cache download is the
// only connection opened to the provider for it: players are served from the
// file being written, instead of being proxied to the provider in parallel.
// Providers commonly allow a single connection per account, and a second one
// for the same file made the provider cut the first, so the player and the
// cache download used to kill each other until one of them gave up.
//
// Players are answered as if the file were complete (its total size, Range
// support) and the body follows the download. A read just ahead of what has
// been written waits for it. A read far ahead (a seek, or a container index
// stored at the end of the file, which demuxers read before playing) asks the
// download for a window: the downloader pauses the sequential download, fetches
// that byte range from the provider with a Range request, then resumes where it
// stopped. The provider still sees a single connection at a time, and the
// player gets its bytes within moments instead of after the whole file.

var (
	// vodProgressiveHeaderWait bounds how long a request waits for the
	// download's first response headers (they carry the total size, needed to
	// answer Range requests). Providers answer within a second or two; a slow
	// one is still given time, and a player that times out first reconnects.
	vodProgressiveHeaderWait = 25 * time.Second
	// vodProgressiveStallTimeout ends a response when the download has made no
	// progress at all for this long. The body is cut short, so the player sees
	// an interrupted read and reconnects at its position, rather than holding
	// a silent connection forever. Players' own read timeouts (typically
	// 8–30 s) usually fire first, with the same outcome.
	vodProgressiveStallTimeout = 30 * time.Second
)

// vodProgressiveChunk is the size of each read from the cache file.
const vodProgressiveChunk = 256 * 1024

var (
	// vodWindowSize is how much a window fetches past the requested offset.
	// Readers get the window's bytes as they land, and ask for the next window
	// when they reach its end, so this only bounds how long the sequential
	// download is paused per window: 4 MiB is ~2 s at 2 MB/s. Index elements
	// at the end of a file (matroska cues and tags, an MP4 moov) are almost
	// always smaller.
	vodWindowSize int64 = 4 << 20
	// vodWindowMinGap and vodWindowHorizon decide when a read ahead is worth a
	// window rather than waiting: only when the sequential download would need
	// more than vodWindowHorizon to reach it at its measured rate, and never
	// for less than vodWindowMinGap.
	vodWindowMinGap  int64 = 2 << 20
	vodWindowHorizon       = 3 * time.Second
	// vodWindowMainRun is the least time the sequential download runs between
	// two windows, so readers asking for window after window cannot stall it.
	vodWindowMainRun = 500 * time.Millisecond
	// vodWindowMaxFailures disables windows for a download after this many
	// failed window fetches; readers then wait for the sequential download.
	vodWindowMaxFailures = 3
)

// byteSpan is the half-open byte range [start, end).
type byteSpan struct{ start, end int64 }

// vodDownload is the state of one cache download, shared by every request
// served from it. The download writes straight to the .part file (no
// buffering), so every byte counted in written is readable from it.
type vodDownload struct {
	part, dest string

	mu      sync.Mutex
	written int64 // bytes written to the .part file, from offset 0
	total   int64 // full size, 0 while unknown
	headers bool  // the provider has answered (total is known if it sent one)
	done    bool
	err     error
	changed chan struct{} // closed and replaced on every state change

	// Windows: byte ranges past written already fetched into the .part file
	// (sorted, disjoint), and the ones readers are waiting for. wake tells the
	// downloader that a window is wanted.
	windows        []byteSpan
	wanted         []byteSpan
	wake           chan struct{}
	noWindows      bool // the provider ignored a ranged request, or windows kept failing
	windowFailures int

	// Sequential download rate (bytes/s, smoothed), to judge whether a read
	// ahead is worth a window.
	rate     float64
	rateAt   time.Time
	rateFrom int64
}

func newVODDownload(dest string) *vodDownload {
	return &vodDownload{part: dest + ".part", dest: dest, changed: make(chan struct{}), wake: make(chan struct{}, 1)}
}

// notifyLocked wakes every waiter. The caller holds d.mu.
func (d *vodDownload) notifyLocked() {
	close(d.changed)
	d.changed = make(chan struct{})
}

// setTotal records that the provider answered, with the file's total size
// (0 when it did not say).
func (d *vodDownload) setTotal(total int64) {
	d.mu.Lock()
	if total > 0 {
		d.total = total
	}
	d.headers = true
	d.notifyLocked()
	d.mu.Unlock()
}

// advance records n more bytes written to the .part file by the sequential
// download.
func (d *vodDownload) advance(n int64) {
	d.mu.Lock()
	d.written += n
	now := time.Now()
	if d.rateAt.IsZero() {
		d.rateAt, d.rateFrom = now, d.written-n
	} else if dt := now.Sub(d.rateAt); dt >= 500*time.Millisecond {
		inst := float64(d.written-d.rateFrom) / dt.Seconds()
		if d.rate == 0 {
			d.rate = inst
		} else {
			d.rate = 0.7*d.rate + 0.3*inst
		}
		d.rateAt, d.rateFrom = now, d.written
	}
	// Windows now inside the sequential prefix are no longer needed.
	kept := d.windows[:0]
	for _, w := range d.windows {
		if w.end > d.written {
			kept = append(kept, w)
		}
	}
	d.windows = kept
	d.notifyLocked()
	d.mu.Unlock()
}

// resumeSequential resets the rate sample when the sequential download starts
// again after a pause (a window or a retry), so the pause is not counted as a
// slow transfer.
func (d *vodDownload) resumeSequential() {
	d.mu.Lock()
	d.rateAt = time.Time{}
	d.mu.Unlock()
}

// restart records that the .part file was truncated to rewrite it from the
// start (the provider ignored a resume Range). Fetched windows are gone with
// it, and a provider that ignores Range cannot serve windows either.
func (d *vodDownload) restart() {
	d.mu.Lock()
	d.written = 0
	d.windows, d.wanted, d.noWindows = nil, nil, true
	d.notifyLocked()
	d.mu.Unlock()
}

// readableEndLocked returns the end of the bytes readable from off without
// waiting: past the sequential prefix and any fetched windows contiguous with
// it. It returns off when nothing is readable there. The caller holds d.mu.
func (d *vodDownload) readableEndLocked(off int64) int64 {
	end := off
	if off < d.written {
		end = d.written
	}
	for _, w := range d.windows { // sorted, so contiguous windows chain
		if w.start <= end && w.end > end {
			end = w.end
		}
	}
	return end
}

// readableEnd is readableEndLocked for callers not holding d.mu.
func (d *vodDownload) readableEnd(off int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.readableEndLocked(off)
}

// requestWindow asks the downloader to fetch the bytes at off ahead of the
// sequential download, unless they are already readable, already asked for,
// or close enough for the sequential download to bring soon.
func (d *vodDownload) requestWindow(off int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.noWindows || d.done || d.total <= 0 || off >= d.total || d.readableEndLocked(off) > off {
		return
	}
	gap := off - d.written
	threshold := vodWindowMinGap
	if h := int64(d.rate * vodWindowHorizon.Seconds()); h > threshold {
		threshold = h
	}
	if gap <= threshold {
		return
	}
	for _, w := range d.wanted {
		if off >= w.start && off < w.end {
			return
		}
	}
	end := off + vodWindowSize
	if end > d.total {
		end = d.total
	}
	d.wanted = append(d.wanted, byteSpan{off, end})
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// windowWanted reports whether a reader waits for a window.
func (d *vodDownload) windowWanted() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.wanted) > 0 && !d.noWindows
}

// nextWindow pops the next window to fetch, skipping ones the download
// already covers.
func (d *vodDownload) nextWindow() (byteSpan, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.wanted) > 0 && !d.noWindows {
		w := d.wanted[0]
		d.wanted = d.wanted[1:]
		if r := d.readableEndLocked(w.start); r > w.start {
			if r >= w.end {
				continue
			}
			w.start = r // only the part still missing
		}
		return w, true
	}
	return byteSpan{}, false
}

// addWindow records that [start, end) has been written to the .part file.
func (d *vodDownload) addWindow(start, end int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if end <= start || end <= d.written {
		return
	}
	merged := make([]byteSpan, 0, len(d.windows)+1)
	nw := byteSpan{start, end}
	placed := false
	for _, w := range d.windows {
		switch {
		case w.end < nw.start:
			merged = append(merged, w)
		case w.start > nw.end:
			if !placed {
				merged = append(merged, nw)
				placed = true
			}
			merged = append(merged, w)
		default: // overlapping or adjacent: absorb
			if w.start < nw.start {
				nw.start = w.start
			}
			if w.end > nw.end {
				nw.end = w.end
			}
		}
	}
	if !placed {
		merged = append(merged, nw)
	}
	d.windows = merged
	d.notifyLocked()
}

// windowFailed counts a failed window fetch; past vodWindowMaxFailures, or
// immediately when the provider ignored the Range (rangeIgnored), windows are
// disabled and readers fall back to waiting for the sequential download.
func (d *vodDownload) windowFailed(rangeIgnored bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.windowFailures++
	if rangeIgnored || d.windowFailures >= vodWindowMaxFailures {
		d.noWindows = true
		d.wanted = nil
	}
	d.notifyLocked()
}

// finish records the end of the download: err is nil once the complete file
// is at dest.
func (d *vodDownload) finish(err error) {
	d.mu.Lock()
	d.done, d.err = true, err
	d.notifyLocked()
	d.mu.Unlock()
}

// vodDownloadState is a snapshot of a download.
type vodDownloadState struct {
	written, total int64
	headers, done  bool
	err            error
	changed        <-chan struct{}
}

func (d *vodDownload) state() vodDownloadState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return vodDownloadState{written: d.written, total: d.total, headers: d.headers, done: d.done, err: d.err, changed: d.changed}
}

// waitHeaders waits until the provider has answered (or the download ended),
// at most max. It returns false if gone is closed first.
func (d *vodDownload) waitHeaders(gone <-chan struct{}, max time.Duration) (vodDownloadState, bool) {
	timer := time.NewTimer(max)
	defer timer.Stop()
	for {
		st := d.state()
		if st.headers || st.done {
			return st, true
		}
		select {
		case <-st.changed:
		case <-timer.C:
			return d.state(), true
		case <-gone:
			return st, false
		}
	}
}

// failed reports whether the download ended without producing the file.
func (d *vodDownload) failed() bool {
	s := d.state()
	return s.done && s.err != nil
}

// openForRead opens the file being written, or the finished file when the
// download has completed and renamed it in the meantime.
func (d *vodDownload) openForRead() (*os.File, error) {
	if f, err := os.Open(d.part); err == nil {
		return f, nil
	}
	return os.Open(d.dest)
}

// serveVODDownload answers a request for a movie or episode from its cache
// download, while it runs. filename and asAttachment are as for
// serveLocalFileRange.
func (c *Config) serveVODDownload(ctx *gin.Context, dl *vodDownload, label, filename string, asAttachment bool) {
	reqCtx := ctx.Request.Context()
	contentType := contentTypeForPath(dl.dest)

	// Wait for the provider's answer, which carries the total size.
	st, ok := dl.waitHeaders(reqCtx.Done(), vodProgressiveHeaderWait)
	if !ok {
		return // client went away
	}
	if st.done && st.err == nil {
		serveLocalFileRange(ctx, dl.dest, contentType, filename, asAttachment)
		return
	}
	if st.done {
		ctx.AbortWithStatus(http.StatusBadGateway)
		return
	}

	f, err := dl.openForRead()
	if err != nil {
		if s := dl.state(); s.done && s.err == nil {
			serveLocalFileRange(ctx, dl.dest, contentType, filename, asAttachment)
			return
		}
		utils.WarnLog("Cache: cannot read in-progress download of %s: %v", label, err)
		ctx.AbortWithStatus(http.StatusBadGateway)
		return
	}
	defer func() { _ = f.Close() }()

	h := ctx.Writer.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Accel-Buffering", "no")
	mediaNoStore(h)
	if asAttachment {
		if filename == "" {
			filename = path.Base(dl.dest)
		}
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	}

	// With the total unknown (the provider sent no length, or did not answer
	// in time) no valid partial response can be built: send the whole file
	// from the start. HTTP lets a server ignore Range, and players skip to
	// their position. A 503 would be cleaner, but common players treat it as
	// fatal instead of retrying.
	start, end := int64(0), int64(-1)
	status := http.StatusOK
	if st.total > 0 {
		h.Set("Accept-Ranges", "bytes")
		end = st.total - 1
		if rs, ok := parseSingleRange(ctx.GetHeader("Range"), st.total); ok {
			start, end = rs[0], rs[1]
			status = http.StatusPartialContent
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, st.total))
		} else if ctx.GetHeader("Range") != "" && !strings.Contains(ctx.GetHeader("Range"), ",") {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", st.total))
			ctx.Status(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	}
	ctx.Status(status)
	if ctx.Request.Method == http.MethodHead {
		return
	}
	utils.InfoLog("Serving %s from in-progress cache (%s/%s, from byte %d)",
		label, utils.HumanBytes(st.written), utils.HumanBytes(st.total), start)

	buf := make([]byte, vodProgressiveChunk)
	stall := time.NewTimer(vodProgressiveStallTimeout)
	defer stall.Stop()
	off := start
	for end < 0 || off <= end {
		st = dl.state()
		if avail := dl.readableEnd(off); avail > off {
			n := avail - off
			if n > int64(len(buf)) {
				n = int64(len(buf))
			}
			if end >= 0 && n > end-off+1 {
				n = end - off + 1
			}
			nr, rerr := f.ReadAt(buf[:n], off)
			if nr > 0 {
				if _, werr := ctx.Writer.Write(buf[:nr]); werr != nil {
					return // client went away
				}
				ctx.Writer.Flush()
				off += int64(nr)
			}
			if rerr == nil || rerr == io.EOF {
				if nr > 0 {
					continue
				}
			} else {
				utils.WarnLog("Cache: read error serving %s at byte %d: %v", label, off, rerr)
				abortResponse(ctx)
				return
			}
			// Nothing readable at off although counted as written: the file
			// is being rewritten from the start. Wait for the next change.
		} else if st.done {
			if st.err != nil {
				utils.WarnLog("Cache: download of %s failed while serving it at byte %d: %v", label, off, st.err)
				abortResponse(ctx)
			}
			return // complete (unknown total) or failed
		} else if st.total > 0 {
			// Ahead of the download: fetch these bytes out of order if the
			// sequential download would take a while to bring them.
			dl.requestWindow(off)
		}

		select {
		case <-st.changed:
			if !stall.Stop() {
				select {
				case <-stall.C:
				default:
				}
			}
			stall.Reset(vodProgressiveStallTimeout)
		case <-stall.C:
			utils.WarnLog("Cache: download of %s made no progress for %s; ending the response at byte %d so the player reconnects",
				label, vodProgressiveStallTimeout, off)
			abortResponse(ctx)
			return
		case <-reqCtx.Done():
			return
		}
	}
}

// abortResponse ends a response that cannot be completed by dropping the
// connection, so the client sees an interrupted transfer and retries at its
// position. Merely returning would end a chunked body (unknown total) cleanly,
// passing a cut file off as complete.
func abortResponse(ctx *gin.Context) {
	ctx.Writer.Flush()
	if conn, _, err := ctx.Writer.Hijack(); err == nil {
		_ = conn.Close()
	}
}

// parseSingleRange parses a single "bytes=" range against a resource of size
// total and returns its inclusive bounds. It returns false for no Range, a
// multi-range request (served whole instead), or a range that cannot be
// satisfied.
func parseSingleRange(header string, total int64) ([2]int64, bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) || strings.Contains(header, ",") {
		return [2]int64{}, false
	}
	spec := strings.TrimSpace(header[len(prefix):])
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return [2]int64{}, false
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	var start, end int64
	switch {
	case first == "" && last == "":
		return [2]int64{}, false
	case first == "": // suffix: the last N bytes
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return [2]int64{}, false
		}
		if n > total {
			n = total
		}
		start, end = total-n, total-1
	default:
		s, err := strconv.ParseInt(first, 10, 64)
		if err != nil || s < 0 || s >= total {
			return [2]int64{}, false
		}
		start, end = s, total-1
		if last != "" {
			e, err := strconv.ParseInt(last, 10, 64)
			if err != nil || e < s {
				return [2]int64{}, false
			}
			if e < end {
				end = e
			}
		}
	}
	return [2]int64{start, end}, true
}
