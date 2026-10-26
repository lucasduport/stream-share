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
// support) and the body follows the download: a read ahead of what has been
// written waits for it. A player whose position is far ahead of the download
// (a seek, or an MP4 index stored at the end of the file) waits on the body,
// and if it times out it reconnects and resumes from its position, attached
// to the same download.

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

// vodDownload is the state of one cache download, shared by every request
// served from it. The download writes straight to the .part file (no
// buffering), so every byte counted in written is readable from it.
type vodDownload struct {
	part, dest string

	mu      sync.Mutex
	written int64 // bytes written to the .part file
	total   int64 // full size, 0 while unknown
	headers bool  // the provider has answered (total is known if it sent one)
	done    bool
	err     error
	changed chan struct{} // closed and replaced on every state change
}

func newVODDownload(dest string) *vodDownload {
	return &vodDownload{part: dest + ".part", dest: dest, changed: make(chan struct{})}
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

// advance records n more bytes written to the .part file.
func (d *vodDownload) advance(n int64) {
	d.mu.Lock()
	d.written += n
	d.notifyLocked()
	d.mu.Unlock()
}

// restart records that the .part file was truncated to rewrite it from the
// start (the provider ignored a resume Range).
func (d *vodDownload) restart() {
	d.mu.Lock()
	d.written = 0
	d.notifyLocked()
	d.mu.Unlock()
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
		if off < st.written {
			n := st.written - off
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
