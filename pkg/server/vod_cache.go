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
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// vodCacheClient is used exclusively by fetchToFile. No global timeout so large files
// can be downloaded fully; transport-level timeouts prevent infinite stalls.
var vodCacheClient = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	},
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

	for attempt := 0; attempt <= maxCacheRetries; attempt++ {
		if ctx.Err() != nil {
			break
		}
		if attempt > 0 {
			backoff := time.Duration(attempt) * vodCacheRetryBackoff
			utils.WarnLog("Cache: connection interrupted at %s/%s, retrying in %s (attempt %d/%d)",
				utils.HumanBytes(downloaded), utils.HumanBytes(total), backoff, attempt, maxCacheRetries)
			time.Sleep(backoff)
			// Seek file to current offset so we append correctly on resume
			if _, seekErr := f.Seek(downloaded, io.SeekStart); seekErr != nil {
				utils.ErrorLog("Cache: seek error: %v", seekErr)
				c.cacheFail(streamID)
				return
			}
		}

		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		req, reqErr := http.NewRequestWithContext(attemptCtx, "GET", upstream, nil)
		if reqErr != nil {
			cancelAttempt()
			utils.ErrorLog("Cache: failed to build request: %v", reqErr)
			c.cacheFail(streamID)
			return
		}
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		if downloaded > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", downloaded))
		}

		resp, doErr := vodCacheClient.Do(req)
		if doErr != nil {
			cancelAttempt()
			utils.WarnLog("Cache: upstream error (attempt %d): %v", attempt, doErr)
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
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
		default:
			cancelAttempt()
			_ = resp.Body.Close()
			utils.WarnLog("Cache: upstream status %d (attempt %d)", resp.StatusCode, attempt)
			continue
		}
		dl.setTotal(total)

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
		cancelAttempt()
		_ = resp.Body.Close()

		if readErr == io.EOF || (total > 0 && downloaded >= total) {
			completed = true
			break
		}
		// io.ErrUnexpectedEOF or other transient errors: log and retry
		utils.WarnLog("Cache: read interrupted at %s/%s: %v", utils.HumanBytes(downloaded), utils.HumanBytes(total), readErr)
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
