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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// vodCacheClient is used exclusively by fetchToFile. No global timeout so large files
// can be downloaded fully; transport-level timeouts prevent infinite stalls.
//
// A download keeps one connection for as long as it can: the header, the
// container index at the end and the rest of the file are requested one after
// the other on the same keep-alive connection (see fetchToFile). New
// connections go through vodConnPacer.
var vodCacheClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           vodConnPacer.DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	},
}

// vodConnPacer spaces new provider connections by at least a second after the
// last one closed. Providers allowing a single connection per account track
// connections lazily: one just closed still counts for a moment, and the limit
// is enforced by cutting the oldest connection still open, so reconnecting at
// once can get the new connection cut a few seconds later. Reusing a
// keep-alive connection does not dial, so it is not delayed.
var vodConnPacer = &connPacer{spacing: time.Second}

// connPacer is a dialer that opens a connection only once spacing has passed
// since the last connection it dialed was closed.
type connPacer struct {
	mu        sync.Mutex
	spacing   time.Duration
	lastClose time.Time
	dialer    net.Dialer
}

// setSpacing changes the spacing and returns the previous one.
func (p *connPacer) setSpacing(d time.Duration) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.spacing
	p.spacing = d
	return prev
}

func (p *connPacer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	for {
		p.mu.Lock()
		wait := time.Until(p.lastClose.Add(p.spacing))
		p.mu.Unlock()
		if wait <= 0 {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
	c, err := p.dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &pacedConn{Conn: c, p: p}, nil
}

// pacedConn records when it is closed in its pacer.
type pacedConn struct {
	net.Conn
	p    *connPacer
	once sync.Once
}

func (c *pacedConn) Close() error {
	c.once.Do(func() {
		c.p.mu.Lock()
		c.p.lastClose = time.Now()
		c.p.mu.Unlock()
	})
	return c.Conn.Close()
}

// vodHeadSize is how much the first request of a download asks for. It is a
// bounded request so the connection can be reused for the container index at
// the end of the file (vodTailSize) before the rest of the file follows; the
// header is what a player reads first, so it is not delayed.
var vodHeadSize int64 = 1 << 20

// vodTailMin, vodTailMax and vodTailFraction size the end of the file fetched
// right after the header: demuxers read a container index stored there
// (matroska cues and tags, an MP4 moov written last) before playing. Cues are
// typically well under 1 MiB, but a long movie's moov is several MiB, so the
// tail is max(4 MiB, 0.5 % of the file), capped at 16 MiB. Its last
// vodTailFirst bytes come first: that is where the index nearly always is, so
// a player waiting for it is served after ~1 MiB rather than the whole tail.
//
// Files smaller than vodTailMinFile get no tail fetch: they download in
// moments, and on-demand windows cover a read near their end.
var (
	vodTailMin      int64 = 4 << 20
	vodTailMax      int64 = 16 << 20
	vodTailFraction int64 = 200 // 1/200 = 0.5 %
	vodTailFirst    int64 = 1 << 20
	vodTailMinFile  int64 = 32 << 20
)

// vodTailSize returns how much of the end of a file of size total is fetched
// ahead of the sequential download.
func vodTailSize(total int64) int64 {
	n := total / vodTailFraction
	if n < vodTailMin {
		n = vodTailMin
	}
	if n > vodTailMax {
		n = vodTailMax
	}
	if n > total {
		n = total
	}
	return n
}

// vodIndexAtEnd reports whether files with this extension commonly store
// their index at the end, so fetching the tail early saves players a wait.
func vodIndexAtEnd(dest string) bool {
	switch strings.ToLower(path.Ext(dest)) {
	case ".mkv", ".webm", ".mp4", ".m4v", ".mov":
		return true
	}
	return false
}

// vodCacheStallTimeout aborts a download attempt that receives no bytes for
// this long, so a connection the provider leaves open but silent is retried
// (resuming at the current offset) instead of hanging forever.
var vodCacheStallTimeout = 60 * time.Second

// vodCacheRetryBackoff is the wait before a download retry, multiplied by the
// attempt number (3 s, 6 s, ... 15 s).
var vodCacheRetryBackoff = 3 * time.Second

// fetchToFile downloads from upstream URL to a local file; marks DB entry ready/failed.
// On connection drops (unexpected EOF) it retries automatically using a Range header to
// resume from the current offset, up to maxCacheRetries times. Cancelling ctx aborts the
// download immediately, removes the partial file, and clears the DB entry.
//
// dl publishes the download's progress to requests served from the file while
// it is written (see serveVODDownload); it is finished on every return.
func (c *Config) fetchToFile(ctx context.Context, dl *vodDownload, upstream, dest, streamID, basePath string, expires time.Time) {
	result := fmt.Errorf("download did not complete")
	defer func() { dl.finish(result) }()

	utils.InfoLog("Caching start: %s -> %s", utils.MaskURL(upstream), dest)
	tmp := dest + ".part"

	// A new inode: readers still holding a previous attempt's file keep
	// reading their own copy instead of one truncated under them.
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		utils.ErrorLog("Cache: create file error: %v", err)
		c.cacheFail(streamID)
		return
	}
	defer func() { _ = f.Close() }()

	const maxCacheRetries = 5
	var downloaded, total int64
	lastUpdate := time.Now()
	completed := false

	// The provider connection is held through a slot (see provider.Arbiter):
	// when what someone watches needs it and this download has no viewer, the
	// download is paused (its connection closed), waits for a slot and
	// resumes where it stopped.
	label := "download of " + vodCachedLabel(streamID)
	var lease *provider.Lease
	slotMode := provider.Fresh
	parked := false
	release := func() {
		if lease != nil {
			lease.Release()
			lease = nil
			// An idle keep-alive connection is still a connection to the
			// provider: close it so whoever gets the slot next (another
			// download, a live stream) is the only one open.
			vodCacheClient.CloseIdleConnections()
		}
	}
	defer release()
	acquire := func() bool {
		if lease != nil && !lease.IsPreempted() {
			return true
		}
		release()
		l, aErr := c.providerSlots.Acquire(ctx, label, dl.foreground, slotMode)
		if aErr != nil {
			return false
		}
		if parked {
			utils.InfoLog("Provider slot: resuming %s at %s/%s", label, utils.HumanBytes(downloaded), utils.HumanBytes(total))
			parked = false
		}
		slotMode = provider.Resume
		lease = l
		return true
	}

	// attempt counts failures only: pausing for a window (see fetchWindows)
	// or for another connection (a preempted slot) is not one, and the
	// sequential download resumes right after it.
	attempt := 0
	retrying := false
	// tailFetched: the end of the file (see vodTailSize) has been fetched, or
	// tried, after the header. Players read the index there right after the
	// header, so it comes before the rest of the file, on the same connection.
	tailFetched := !vodIndexAtEnd(dest)
	// tailPieces fetches the end of the file in two bounded requests, last part
	// first, on the connection that carried the header.
	for attempt <= maxCacheRetries {
		if ctx.Err() != nil {
			break
		}
		if retrying {
			backoff := time.Duration(attempt) * vodCacheRetryBackoff
			utils.WarnLog("Cache: connection interrupted at %s/%s, retrying in %s (attempt %d/%d)",
				utils.HumanBytes(downloaded), utils.HumanBytes(total), backoff, attempt, maxCacheRetries)
			var preempt <-chan struct{}
			if lease != nil {
				preempt = lease.Preempted()
			}
			select {
			case <-time.After(backoff):
			case <-preempt: // give the slot back now rather than after the wait
			case <-ctx.Done():
			}
		}
		retrying = false
		if !acquire() {
			break
		}
		// Out-of-order reads waiting on this download come first; then the
		// sequential download resumes where it stopped.
		if total > 0 {
			c.fetchWindows(ctx, dl, f, upstream, total, lease)
			if lease.IsPreempted() {
				parked = true
				continue
			}
			if !tailFetched && downloaded > 0 {
				if total < vodTailMinFile {
					tailFetched = true
				} else {
					tailFetched = true
					if !c.fetchTail(ctx, dl, f, upstream, total, lease) {
						tailFetched = false // preempted: fetched once the download resumes
						parked = true
						continue
					}
					// Windows asked for meanwhile are fetched before the rest of
					// the file starts, so that request is not interrupted for them.
					continue
				}
			}
		}
		// Seek file to current offset so we append correctly on resume
		if _, seekErr := f.Seek(downloaded, io.SeekStart); seekErr != nil {
			utils.ErrorLog("Cache: seek error: %v", seekErr)
			c.cacheFail(streamID)
			return
		}
		dl.resumeSequential()

		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		req, reqErr := http.NewRequestWithContext(attemptCtx, "GET", upstream, nil)
		if reqErr != nil {
			cancelAttempt()
			utils.ErrorLog("Cache: failed to build request: %v", reqErr)
			c.cacheFail(streamID)
			return
		}
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		// The first request asks for the header only (bounded), so the same
		// connection can then carry the index at the end and the rest of the
		// file; later requests ask for everything left.
		bounded := downloaded == 0 && total == 0
		if bounded {
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", vodHeadSize-1))
		} else if downloaded > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", downloaded))
		}

		resp, doErr := vodCacheClient.Do(req)
		if doErr != nil {
			cancelAttempt()
			utils.WarnLog("Cache: upstream error (attempt %d): %v", attempt, doErr)
			attempt++
			retrying = true
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			bounded = false // the whole file: read it all
			// Provider returned 200 despite our Range request — must restart from beginning.
			if downloaded > 0 {
				utils.WarnLog("Cache: provider ignored Range header, restarting download for %s", streamID)
				downloaded = 0
				dl.restart()
				if tErr := f.Truncate(0); tErr != nil {
					cancelAttempt()
					_ = resp.Body.Close()
					utils.ErrorLog("Cache: truncate error: %v", tErr)
					c.cacheFail(streamID)
					return
				}
				if _, sErr := f.Seek(0, io.SeekStart); sErr != nil {
					cancelAttempt()
					_ = resp.Body.Close()
					utils.ErrorLog("Cache: seek error: %v", sErr)
					c.cacheFail(streamID)
					return
				}
			}
			if total == 0 {
				if cl := resp.Header.Get("Content-Length"); cl != "" {
					if v, pErr := strconv.ParseInt(cl, 10, 64); pErr == nil {
						total = v
					}
				}
			}
		case http.StatusPartialContent:
			// Resumed successfully — extract total from Content-Range.
			if total == 0 {
				if cr := resp.Header.Get("Content-Range"); cr != "" {
					if idx := strings.LastIndex(cr, "/"); idx >= 0 {
						if t := strings.TrimSpace(cr[idx+1:]); t != "*" {
							if v, pErr := strconv.ParseInt(t, 10, 64); pErr == nil {
								total = v
							}
						}
					}
				}
			}
			// A provider sending more than the header asked for: read on, as
			// for a request for everything left.
			if bounded && contentRangeEnd(resp.Header.Get("Content-Range")) != vodHeadSize-1 && (total == 0 || total > vodHeadSize) {
				bounded = false
			}
		case http.StatusRequestedRangeNotSatisfiable:
			// Nothing left past what we have: with the total unknown (the
			// header was the whole file), the download is complete.
			if downloaded > 0 && (total == 0 || downloaded >= total) {
				cancelAttempt()
				_ = resp.Body.Close()
				completed = true
			} else {
				cancelAttempt()
				_ = resp.Body.Close()
				utils.WarnLog("Cache: upstream status %d (attempt %d)", resp.StatusCode, attempt)
				attempt++
				retrying = true
				continue
			}
		default:
			cancelAttempt()
			_ = resp.Body.Close()
			utils.WarnLog("Cache: upstream status %d (attempt %d)", resp.StatusCode, attempt)
			attempt++
			retrying = true
			continue
		}
		if completed {
			cancelAttempt()
			break
		}
		dl.setTotal(total)

		// A reader asking for a window pauses this attempt (after a short
		// minimum run, so the sequential download always progresses): the
		// provider allows one connection, so this one is closed first.
		var preempted atomic.Bool
		watchDone := make(chan struct{})
		slot := lease.Preempted()
		go func() {
			minRun := time.NewTimer(vodWindowMainRun)
			defer minRun.Stop()
			select {
			case <-minRun.C:
			case <-slot: // the slot is wanted: no minimum run
				preempted.Store(true)
				cancelAttempt()
				return
			case <-watchDone:
				return
			}
			for {
				// The header request is short: windows wait for its end and
				// reuse its connection, instead of closing it.
				if !bounded && dl.windowWanted() {
					preempted.Store(true)
					cancelAttempt()
					return
				}
				select {
				case <-dl.wake:
				case <-slot:
					preempted.Store(true)
					cancelAttempt()
					return
				case <-watchDone:
					return
				}
			}
		}()

		// Written straight to the file (no buffering) and published right
		// after, so readers of the file see every counted byte.
		buf := make([]byte, 256*1024)
		var readErr error
		stall := time.AfterFunc(vodCacheStallTimeout, cancelAttempt)
		body := &stallReader{r: resp.Body, timer: stall, idle: vodCacheStallTimeout}
		for {
			nr, er := body.Read(buf)
			if nr > 0 {
				if _, ew := f.Write(buf[:nr]); ew != nil {
					stall.Stop()
					close(watchDone)
					cancelAttempt()
					_ = resp.Body.Close()
					utils.ErrorLog("Cache: write error: %v", ew)
					c.cacheFail(streamID)
					return
				}
				downloaded += int64(nr)
				dl.advance(int64(nr))
				if c.db != nil && time.Since(lastUpdate) > vodProgressInterval {
					// Narrow update: only the counters move, so there is no need
					// to rewrite the whole row on every tick.
					_ = c.db.UpdateVODProgress(streamID, downloaded, total)
					lastUpdate = time.Now()
				}
			}
			if er != nil {
				readErr = er
				break
			}
		}
		stall.Stop()
		close(watchDone)
		// Body first: closed after a complete read, its connection goes back
		// to the pool for the next request instead of being torn down.
		_ = resp.Body.Close()
		cancelAttempt()

		if (total > 0 && downloaded >= total) || (readErr == io.EOF && !bounded) {
			completed = true
			break
		}
		if readErr == io.EOF && bounded {
			continue // the header is in: the index and the rest follow
		}
		if preempted.Load() && ctx.Err() == nil {
			if lease.IsPreempted() {
				parked = true // the slot went to a watched stream; wait for another
			}
			continue // paused for a window or for another connection, not a failure
		}
		// io.ErrUnexpectedEOF or other transient errors: log and retry
		utils.WarnLog("Cache: read interrupted at %s/%s: %v", utils.HumanBytes(downloaded), utils.HumanBytes(total), readErr)
		attempt++
		retrying = true
	}

	if !completed {
		if ctx.Err() != nil {
			utils.InfoLog("Cache: download cancelled for %s; removing partial file", streamID)
			_ = os.Remove(tmp)
			if c.db != nil {
				_ = c.db.DeleteVODCacheEntry(streamID)
			}
			return
		}
		utils.ErrorLog("Cache: download failed after %d retries: %s", maxCacheRetries, utils.MaskURL(upstream))
		c.cacheFail(streamID)
		result = fmt.Errorf("download failed after %d retries", maxCacheRetries)
		return
	}

	n := downloaded
	if err := f.Sync(); err != nil {
		utils.WarnLog("Cache: fsync warning: %v", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		utils.ErrorLog("Cache: rename error: %v", err)
		c.cacheFail(streamID)
		return
	}
	result = nil
	utils.InfoLog("Caching done: %s (%s)", dest, utils.HumanBytes(n))
	if c.db != nil {
		var finalTitle string
		if t := c.findVODTitleInCache(basePath, streamID); strings.TrimSpace(t) != "" {
			finalTitle = strings.TrimSpace(t)
		}
		entry := &types.VODCacheEntry{StreamID: streamID, FilePath: dest, DownloadedBytes: n, TotalBytes: n, SizeBytes: n, Status: "ready", ExpiresAt: expires, LastAccess: time.Now()}
		if finalTitle != "" {
			entry.Title = finalTitle
		}
		_ = c.db.UpsertVODCache(entry)
	}
}

// contentRangeEnd returns the last byte of a "bytes a-b/total" Content-Range,
// or -1.
func contentRangeEnd(cr string) int64 {
	cr = strings.TrimPrefix(strings.TrimSpace(cr), "bytes ")
	span, _, _ := strings.Cut(cr, "/")
	_, b, ok := strings.Cut(span, "-")
	if !ok {
		return -1
	}
	v, err := strconv.ParseInt(b, 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// fetchTail fetches the end of the file (see vodTailSize) into f ahead of the
// sequential download, as windows, once the header is in: players read a
// container index there right after the header. The last vodTailFirst bytes
// come first, then the rest of the tail. It returns false when the slot was
// preempted meanwhile (the caller parks and tries again on resume); a failure
// is counted like a failed window and the download goes on.
func (c *Config) fetchTail(ctx context.Context, dl *vodDownload, f *os.File, upstream string, total int64, lease *provider.Lease) bool {
	tailStart := total - vodTailSize(total)
	firstStart := total - vodTailFirst
	if firstStart < tailStart {
		firstStart = tailStart
	}
	for _, w := range []byteSpan{{firstStart, total}, {tailStart, firstStart}} {
		if w.end <= w.start || dl.readableEnd(w.start) >= w.end {
			continue
		}
		if r := dl.readableEnd(w.start); r > w.start {
			w.start = r
		}
		if !c.fetchTailPiece(ctx, dl, f, upstream, total, lease, w) {
			return false
		}
	}
	return true
}

// fetchTailPiece fetches one piece of the tail; see fetchTail.
func (c *Config) fetchTailPiece(ctx context.Context, dl *vodDownload, f *os.File, upstream string, total int64, lease *provider.Lease, w byteSpan) bool {
	if dl.windowsDisabled() {
		return true
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-lease.Preempted():
			cancel()
		case <-stop:
		}
	}()
	err := fetchWindow(wctx, dl, f, upstream, w, total)
	if err != nil && lease.IsPreempted() {
		return false
	}
	if err != nil {
		utils.WarnLog("Cache: fetching the end of the file (bytes %d-%d) failed: %v", w.start, w.end-1, err)
		dl.windowFailed(errors.Is(err, errRangeIgnored))
		return true
	}
	utils.DebugLog("Cache: fetched the end of the file (bytes %d-%d) for %s", w.start, w.end-1, utils.MaskURL(upstream))
	return true
}

// errRangeIgnored reports a provider answering a ranged request with the whole
// file (200).
var errRangeIgnored = errors.New("provider ignored the Range header")

// fetchWindows fetches every window readers are waiting for (see
// vodDownload.requestWindow), one ranged request at a time, into f at their
// offsets. Readers are woken as the bytes land. A provider that ignores Range
// disables windows for this download; other failures are counted and retried
// on the readers' next request, up to vodWindowMaxFailures.
//
// A window being fetched when the slot is preempted is abandoned without
// counting as a failure; readers ask for it again.
func (c *Config) fetchWindows(ctx context.Context, dl *vodDownload, f *os.File, upstream string, total int64, lease *provider.Lease) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-lease.Preempted():
			cancel()
		case <-stop:
		}
	}()
	for wctx.Err() == nil {
		w, ok := dl.nextWindow()
		if !ok {
			return
		}
		err := fetchWindow(wctx, dl, f, upstream, w, total)
		if err != nil && lease.IsPreempted() {
			dl.requeueWindow(w) // fetched once the download has a slot again
			return
		}
		if err != nil {
			utils.WarnLog("Cache: out-of-order fetch of bytes %d-%d failed: %v", w.start, w.end-1, err)
			dl.windowFailed(errors.Is(err, errRangeIgnored))
			continue
		}
		utils.DebugLog("Cache: fetched bytes %d-%d ahead of the download for %s", w.start, w.end-1, utils.MaskURL(upstream))
	}
}

// fetchWindow fetches w with a ranged request and writes it at its offset.
func fetchWindow(ctx context.Context, dl *vodDownload, f *os.File, upstream string, w byteSpan, total int64) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", upstream, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", w.start, w.end-1))
	resp, err := vodCacheClient.Do(req)
	if err != nil {
		return err
	}
	// Deferred after cancel, so it runs first: a body read to its end and
	// closed hands its connection back for the next request.
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		return errRangeIgnored
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	// The answer must start where asked; anything else cannot be placed.
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", w.start)) {
		return fmt.Errorf("unexpected Content-Range %q", cr)
	}

	stall := time.AfterFunc(vodCacheStallTimeout, cancel)
	defer stall.Stop()
	body := &stallReader{r: resp.Body, timer: stall, idle: vodCacheStallTimeout}
	buf := make([]byte, 256*1024)
	off := w.start
	for off < w.end {
		n := int64(len(buf))
		if rest := w.end - off; rest < n {
			n = rest
		}
		nr, er := body.Read(buf[:n])
		if nr > 0 {
			if _, ew := f.WriteAt(buf[:nr], off); ew != nil {
				return ew
			}
			dl.addWindow(w.start, off+int64(nr))
			off += int64(nr)
		}
		if er != nil {
			if er == io.EOF && off >= w.end {
				break
			}
			if er == io.EOF && off >= total {
				break
			}
			return er
		}
	}
	// Reach the end of the body (nothing is left of a conforming answer), so
	// the connection is reusable.
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	return nil
}

// vodTouchInterval throttles last_access updates for a cached VOD item.
//
// The column is only read by cleanupStaleVODFiles, which compares it against a
// multi-hour staleness window on a daily sweep, so minute granularity loses
// nothing. Unthrottled it was a row rewrite per HTTP Range request, and one
// playback issues hundreds — each an UPDATE, a WAL record and a dead tuple for
// autovacuum to collect.
const vodTouchInterval = time.Minute

// vodProgressInterval is how often an in-flight download reports its byte
// counters. The only consumer is the Discord progress readout, which nobody
// watches at one-second resolution.
const vodProgressInterval = 5 * time.Second

var vodLastTouch sync.Map // streamID -> time.Time

// touchVODCache marks a cached item as recently used, at most once per
// vodTouchInterval per stream.
func (c *Config) touchVODCache(streamID string) {
	if c.db == nil {
		return
	}
	now := time.Now()
	if prev, ok := vodLastTouch.Load(streamID); ok {
		if now.Sub(prev.(time.Time)) < vodTouchInterval {
			return
		}
	}
	vodLastTouch.Store(streamID, now)
	if err := c.db.TouchVODCache(streamID); err != nil {
		utils.DebugLog("vod cache: failed to touch last_access for %s: %v", streamID, err)
	}
}

func (c *Config) cacheFail(streamID string) {
	if c.db != nil {
		_ = c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: streamID, Status: "failed", LastAccess: time.Now(), ExpiresAt: time.Now().Add(2 * time.Hour)})
	}
}

// isValidStreamID rejects stream IDs that could escape the cache directory via
// path separators or dot-dot traversal.
func isValidStreamID(idRaw string) bool {
	return !strings.Contains(idRaw, "/") && !strings.Contains(idRaw, "..")
}

// resolveVODCacheURL builds the upstream URL and local cache path for a VOD
// stream ID, resolving the file extension from the request, the M3U catalogue
// cache, or a default. Returns the upstream URL, the local destination path, and
// the bare stream ID (without extension).
func (c *Config) resolveVODCacheURL(basePath, defaultExt, id string) (upstream, dest, idRaw string) {
	idRaw = strings.TrimSuffix(id, path.Ext(id))
	resolvedExt := path.Ext(id)
	if resolvedExt == "" {
		resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
	}
	if resolvedExt == "" {
		resolvedExt = defaultExt
	}
	finalID := idRaw + resolvedExt
	upstream = fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
	cacheDir := utils.VODCacheDir()
	_ = os.MkdirAll(cacheDir, 0o755)
	dest = filepath.Join(cacheDir, idRaw+resolvedExt)
	return upstream, dest, idRaw
}

// startBackgroundDownload returns the cache download of a VOD stream, starting
// it unless one is already running: there is at most one per stream ID, so its
// requests share a single provider connection. onStart, if set, runs before a
// new download begins (not when joining one). The download outlives the
// request that started it — it runs until completion or failure, the same way
// an explicit /cache request does.
func (c *Config) startBackgroundDownload(upstream, dest, streamID, basePath string, expires time.Time, onStart func()) *vodDownload {
	for {
		dl := newVODDownload(dest)
		v, loaded := c.inProgressDownloads.LoadOrStore(streamID, dl)
		if loaded {
			existing := v.(*vodDownload)
			if existing.failed() {
				// Ended without the file: start over rather than serve it.
				c.inProgressDownloads.CompareAndDelete(streamID, existing)
				continue
			}
			return existing
		}
		if onStart != nil {
			onStart()
		}
		go func() {
			defer c.inProgressDownloads.CompareAndDelete(streamID, dl)
			c.fetchToFile(context.Background(), dl, upstream, dest, streamID, basePath, expires)
		}()
		return dl
	}
}
