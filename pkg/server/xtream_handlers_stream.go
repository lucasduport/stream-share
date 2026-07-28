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
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

func (c *Config) xtreamApiGet(ctx *gin.Context) {
	const (
		apiGet = "apiget"
	)

	var (
		extension = ctx.Query("output")
		cacheName = apiGet + extension
	)

	xtreamM3uCacheLock.RLock()
	meta, ok := xtreamM3uCache[cacheName]
	d := time.Since(meta.Time)
	if !ok || d.Hours() >= float64(c.M3UCacheExpiration) {
		log.Printf("[stream-share] %v | %s | xtream cache API m3u file\n", time.Now().Format("2006/01/02 - 15:04:05"), ctx.ClientIP())
		xtreamM3uCacheLock.RUnlock()
		playlist, err := c.xtreamGenerateM3u(ctx, extension)
		if err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)) // nolint: errcheck
			return
		}
		if err := c.cacheXtreamM3u(playlist, cacheName); err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)) // nolint: errcheck
			return
		}
	} else {
		xtreamM3uCacheLock.RUnlock()
	}

	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	xtreamM3uCacheLock.RLock()
	path := xtreamM3uCache[cacheName].string
	xtreamM3uCacheLock.RUnlock()
	ctx.Header("Content-Type", "application/octet-stream")

	ctx.File(path)

}

// xtreamStream proxies a live stream through the session manager, which shares a
// single upstream connection across all viewers of the same channel and records
// the session so Discord/status endpoints see the active viewer. With a single
// viewer the multiplexed path applies full back-pressure and behaves like a
// direct proxy; additional viewers reuse the same upstream connection.
func (c *Config) xtreamStream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("-> Xtream streaming request: %s", ctx.Request.URL.Path)
	utils.DebugLog("-> Proxying to Xtream upstream: %s", oriURL.String())
	c.multiplexedStream(ctx, oriURL)
}

func (c *Config) xtreamXMLTV(ctx *gin.Context) {
	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	resp, err := client.GetXMLTV()
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	ctx.Data(http.StatusOK, "application/xml", resp)
}

func (c *Config) xtreamStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamLive(ctx *gin.Context) {
	id := ctx.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamPlay(ctx *gin.Context) {
	token := ctx.Param("token")
	t := ctx.Param("type")
	rpURL, err := url.Parse(fmt.Sprintf("%s/play/%s/%s", c.XtreamBaseURL, token, t))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamTimeshift(ctx *gin.Context) {
	duration := ctx.Param("duration")
	start := ctx.Param("start")
	idRaw := ctx.Param("id")

	// Proxy upstream if catchup is disabled or the channel has native upstream support.
	if c.catchupManager == nil || !c.catchupManager.IsEnabled() || c.catchupManager.HasUpstreamCatchup(idRaw) {
		rpURL, err := url.Parse(fmt.Sprintf("%s/timeshift/%s/%s/%s/%s/%s",
			c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, duration, start, idRaw))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		c.stream(ctx, rpURL)
		return
	}

	startTime, err := parseTimeshiftStart(start)
	if err != nil {
		utils.WarnLog("Catchup: unparseable start param %q: %v", start, err)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	utils.DebugLog("Catchup: timeshift start raw=%q parsed=%s (unix=%d)", start, startTime.Format(time.RFC3339), startTime.Unix())

	// A client is actively reading the catchup buffer (e.g. resuming after a
	// pause) — cancel any pending pause-grace stop so recording continues
	// uninterrupted and there's no gap once the buffer catches up to live.
	if c.sessionManager != nil {
		c.sessionManager.NotifyCatchupActivity(idRaw)
	}

	buf := c.catchupManager.GetBuffer(idRaw)
	if buf == nil {
		utils.DebugLog("Catchup: no buffer for %s (never watched or already cleaned up)", c.streamLabel(idRaw))
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	utils.DebugLog("Catchup: buffer for %s started at %s (unix=%d), buffered=%d bytes", c.streamLabel(idRaw), buf.StartTime().Format(time.RFC3339), buf.StartTime().Unix(), buf.BytesBuffered())

	// Set response headers before the first write so they reach the client.
	ctx.Header("Content-Type", "video/mp2t")
	ctx.Header("Cache-Control", "no-cache")

	offset := buf.OffsetForTime(startTime)
	utils.DebugLog("Catchup: serving %s from local buffer at offset %d / %d bytes (start raw=%q parsed=%s)", c.streamLabel(idRaw), offset, buf.BytesBuffered(), start, startTime.Format(time.RFC3339))
	c.serveFromCatchupBuffer(ctx, buf, startTime)

	// If the client disconnected or the handler aborted, we're done.
	select {
	case <-ctx.Request.Context().Done():
		return
	default:
	}
	if ctx.IsAborted() {
		return
	}

	// Buffer exhausted — transition seamlessly to the live upstream so TiviMate
	// catches up and continues watching without reconnecting on its own.
	liveURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s",
		c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, idRaw))
	if err != nil {
		return
	}
	utils.DebugLog("Catchup: buffer exhausted for %s, transitioning to live upstream", c.streamLabel(idRaw))
	c.stream(ctx, liveURL)
}

// parseTimeshiftStart parses a timeshift start parameter.
// TiviMate sends "YYYY-MM-DD:HH-MM" in local time; time.Local is used, which Go
// initialises from the TZ environment variable at startup. Set TZ in the container
// (e.g. TZ=Europe/Amsterdam) so that local timestamps are interpreted correctly.
// Unix timestamp integers are also accepted and are always timezone-independent.
func parseTimeshiftStart(start string) (time.Time, error) {
	if n, err := strconv.ParseInt(start, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	return time.ParseInLocation("2006-01-02:15-04", start, time.Local)
}

// alignToTSPacket seeks f to startOffset then scans forward (up to 3 packet-widths) for
// two consecutive MPEG-TS sync bytes (0x47) exactly 188 bytes apart, confirming a packet
// boundary. f is left positioned at the aligned offset. If no alignment is found, f is
// repositioned at startOffset.
func alignToTSPacket(f *os.File, startOffset int64) {
	const syncByte = byte(0x47)
	const pktSize = 188

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return
	}
	search := make([]byte, pktSize*3)
	n, _ := f.Read(search)
	search = search[:n]
	for i := 0; i+pktSize < len(search); i++ {
		if search[i] == syncByte && search[i+pktSize] == syncByte {
			_, _ = f.Seek(startOffset+int64(i), io.SeekStart)
			return
		}
	}
	// No boundary found; restore original position.
	_, _ = f.Seek(startOffset, io.SeekStart)
}

// serveFromCatchupBuffer serves a timeshift request from the local disk buffer.
// It resolves the correct file for startTime (which may be the previous file after a
// rotation), serves that file to completion, then seamlessly continues from the current
// file when necessary — so rewinds that span a rotation boundary play through without
// interruption.
func (c *Config) serveFromCatchupBuffer(ctx *gin.Context, buf *catchup.DiskBuffer, startTime time.Time) {
	filePath, startOffset := buf.FileForTime(startTime)
	currentPath := buf.FilePath()

	if filePath != currentPath {
		// Serving from the previous (fully-written) file. A closed drainDone signals
		// "no more data to wait for" — the prev file is complete.
		closedCh := make(chan struct{})
		close(closedCh)
		c.streamFileSegment(ctx, filePath, startOffset, closedCh)
		if ctx.Request.Context().Err() != nil || ctx.IsAborted() {
			return
		}
		// Seamlessly continue from the beginning of the current file.
		filePath = buf.FilePath() // re-read in case another rotation happened
		startOffset = 0
	}

	c.streamFileSegment(ctx, filePath, startOffset, buf.DrainDone())
}

// streamFileSegment serves bytes from filePath starting at startOffset, flushing to the
// client as they arrive. It blocks until drainDone closes (all data written), polling
// every 200 ms when EOF is hit before that point.
func (c *Config) streamFileSegment(ctx *gin.Context, filePath string, startOffset int64, drainDone <-chan struct{}) {
	f, err := os.Open(filePath)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open buffer file %s: %v", filePath, err)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	alignToTSPacket(f, startOffset)

	readBuf := make([]byte, 64*1024)
	clientGone := ctx.Request.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		default:
		}
		n, rerr := f.Read(readBuf)
		if n > 0 {
			if _, werr := ctx.Writer.Write(readBuf[:n]); werr != nil {
				return
			}
			if flusher, ok := ctx.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			select {
			case <-drainDone:
				// All writes are on disk. Flush any bytes written between our last
				// read and drain completion, then exit.
				drainDone = nil // nil channel blocks forever — prevent re-entry
				for {
					n2, err2 := f.Read(readBuf)
					if n2 > 0 {
						if _, werr := ctx.Writer.Write(readBuf[:n2]); werr != nil {
							return
						}
						if flusher, ok := ctx.Writer.(http.Flusher); ok {
							flusher.Flush()
						}
					}
					if err2 != nil {
						return
					}
				}
			case <-clientGone:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if rerr != nil {
			return
		}
	}
}

func (c *Config) xtreamStreamMovieWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	// Normalize DB key: cached entries are stored by bare stream_id without extension
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if c.sessionManager != nil {
		username := c.resolveRequestUsername(ctx)
		if username != "" {
			movieTitle := idRaw
			if name, ok := c.resolveTitleAtStart(idRaw, "movie"); ok && strings.TrimSpace(name) != "" {
				movieTitle = name
			}
			utils.InfoLog("VOD movie started: %s for user %s", movieTitle, username)
			c.sessionManager.RegisterVODView(username, idRaw, "movie", movieTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			// If file exists and is ready, serve locally; if DB status is stale, serve as complete.
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				c.touchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached movie for %s from %s", c.vodLabel(idRaw), entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", c.vodLabel(idRaw))
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		// Not cached yet: auto-start 7-day caching in background
		// Use the extension from the request if present; only fall back to the
		// M3U cache lookup and then a hardcoded default when the client omitted it.
		basePath := "movie"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mp4"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := utils.VODCacheDir()
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		// Insert pending entry
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "movie", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record movie cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel() // another goroutine owns this download; discard our cancel
		} else {
			go func() {
				// The background download owns downloadCtx/downloadCancel for its
				// entire run, independent of any single proxied viewer request's
				// lifetime — it must keep going until it actually finishes (or
				// fails), the same way an explicit /cache request does.
				defer c.inProgressDownloads.Delete(idRaw)
				defer downloadCancel()
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly: lets the IPTV server handle Content-Length, Content-Range,
		// and Range seeks natively. This avoids avformat errors (MP4 moov at EOF) and seek loops.
		// Background caching continues independently of this request; once complete, future
		// requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamMovie(ctx *gin.Context) {
	if c.VODCacheEnabled {
		c.xtreamStreamMovieWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
		c.xtreamStream(ctx, rpURL)
	}
}

func (c *Config) xtreamStreamSeriesWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if c.sessionManager != nil {
		username := c.resolveRequestUsername(ctx)
		if username != "" {
			seriesTitle := idRaw
			if name, ok := c.resolveTitleAtStart(idRaw, "series"); ok && strings.TrimSpace(name) != "" {
				seriesTitle = name
			}
			utils.InfoLog("VOD series started: %s for user %s", seriesTitle, username)
			c.sessionManager.RegisterVODView(username, idRaw, "series", seriesTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				c.touchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached episode for %s from %s", c.vodLabel(idRaw), entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", c.vodLabel(idRaw))
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		// Not cached yet: auto-start 7-day caching in background
		basePath := "series"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mkv"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := utils.VODCacheDir()
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "series", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record series cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel()
		} else {
			go func() {
				// The background download owns downloadCtx/downloadCancel for its
				// entire run, independent of any single proxied viewer request's
				// lifetime — it must keep going until it actually finishes (or
				// fails), the same way an explicit /cache request does.
				defer c.inProgressDownloads.Delete(idRaw)
				defer downloadCancel()
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly for proper headers and native Range-seek support.
		// Background caching continues independently of this request; once complete,
		// future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamSeries(ctx *gin.Context) {
	if c.VODCacheEnabled {
		c.xtreamStreamSeriesWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		c.xtreamStream(ctx, rpURL)
	}
}

// Direct handlers using proxy credentials
func (c *Config) xtreamProxyCredentialsStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	utils.DebugLog("Direct stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsLiveStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	utils.DebugLog("Direct live stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsMovieStreamHandler(ctx *gin.Context) {
	if c.VODCacheEnabled {
		c.xtreamProxyCredentialsMovieStreamHandlerWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			utils.ErrorLog("Failed to parse upstream URL: %v", err)
			ctx.AbortWithStatus(500)
			return
		}
		utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
		c.stream(ctx, rpURL)
	}
}

func (c *Config) xtreamProxyCredentialsMovieStreamHandlerWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	utils.DebugLog("Direct movie stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	if c.sessionManager != nil {
		username := c.resolveRequestUsername(ctx)
		if username != "" {
			movieTitle := idRaw
			if name, ok := c.resolveTitleAtStart(idRaw, "movie"); ok && strings.TrimSpace(name) != "" {
				movieTitle = name
			}
			utils.InfoLog("VOD movie started: %s for user %s", movieTitle, username)
			c.sessionManager.RegisterVODView(username, idRaw, "movie", movieTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				c.touchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached movie (proxy creds path) for %s from %s", c.vodLabel(idRaw), entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", c.vodLabel(idRaw))
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		// Auto-start caching
		basePath := "movie"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mp4"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := utils.VODCacheDir()
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "movie", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record movie cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel()
		} else {
			go func() {
				// The background download owns downloadCtx/downloadCancel for its
				// entire run, independent of any single proxied viewer request's
				// lifetime — it must keep going until it actually finishes (or
				// fails), the same way an explicit /cache request does.
				defer c.inProgressDownloads.Delete(idRaw)
				defer downloadCancel()
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly for proper headers and native Range-seek support.
		// Background caching continues independently of this request; once complete,
		// future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.stream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsSeriesStreamHandler(ctx *gin.Context) {
	if c.VODCacheEnabled {
		c.xtreamProxyCredentialsSeriesStreamHandlerWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			utils.ErrorLog("Failed to parse upstream URL: %v", err)
			ctx.AbortWithStatus(500)
			return
		}
		utils.DebugLog("Series streaming request - using Xtream credentials for upstream: %s", rpURL.String())
		c.stream(ctx, rpURL)
	}
}

func (c *Config) xtreamProxyCredentialsSeriesStreamHandlerWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	utils.DebugLog("Direct series stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	if c.sessionManager != nil {
		username := c.resolveRequestUsername(ctx)
		if username != "" {
			seriesTitle := idRaw
			if name, ok := c.resolveTitleAtStart(idRaw, "series"); ok && strings.TrimSpace(name) != "" {
				seriesTitle = name
			}
			utils.InfoLog("VOD series started: %s for user %s", seriesTitle, username)
			c.sessionManager.RegisterVODView(username, idRaw, "series", seriesTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				c.touchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached episode (proxy creds path) for %s from %s", c.vodLabel(idRaw), entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", c.vodLabel(idRaw))
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		basePath := "series"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mkv"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := utils.VODCacheDir()
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "series", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record series cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel()
		} else {
			go func() {
				// The background download owns downloadCtx/downloadCancel for its
				// entire run, independent of any single proxied viewer request's
				// lifetime — it must keep going until it actually finishes (or
				// fails), the same way an explicit /cache request does.
				defer c.inProgressDownloads.Delete(idRaw)
				defer downloadCancel()
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly for proper headers and native Range-seek support.
		// Background caching continues independently of this request; once complete,
		// future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.stream(ctx, rpURL)
}

// HLS helpers and handlers
var hlsChannelsRedirectURL map[string]url.URL = map[string]url.URL{}
var hlsChannelsRedirectURLLock = sync.RWMutex{}

func (c *Config) xtreamHlsStream(ctx *gin.Context) {
	chunk := ctx.Param("chunk")
	s := strings.Split(chunk, "_")
	if len(s) != 2 {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("HSL malformed chunk")))
		return
	}
	channel := s[0]

	redirURL, err := getHlsRedirectURL(channel)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}

	req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", fmt.Sprintf("%s://%s/hls/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), ctx.Param("chunk")), nil)
	if reqErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr))
		return
	}

	mergeHttpHeader(req.Header, ctx.Request.Header)

	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(doErr))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusFound {
		loc, locErr := resp.Location()
		if locErr != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(locErr))
			return
		}
		id := ctx.Param("id")
		if strings.Contains(loc.String(), id) {
			hlsChannelsRedirectURLLock.Lock()
			hlsChannelsRedirectURL[id] = *loc
			hlsChannelsRedirectURLLock.Unlock()
			hlsReq, hlsReqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", loc.String(), nil)
			if hlsReqErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsReqErr))
				return
			}
			mergeHttpHeader(hlsReq.Header, ctx.Request.Header)
			hlsResp, hlsDoErr := http.DefaultClient.Do(hlsReq)
			if hlsDoErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsDoErr))
				return
			}
			defer func() { _ = hlsResp.Body.Close() }()

			b, readErr := io.ReadAll(hlsResp.Body)
			if readErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr))
				return
			}
			body := string(b)
			body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
			utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
			return
		}
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("unable to HLS stream")))
		return
	}

	utils.DebugLog("HLS stream response status: %d", resp.StatusCode)
	ctx.Status(resp.StatusCode)
}

func (c *Config) hlsXtreamStream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("HLS stream request with URL: %s", oriURL.String())
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", oriURL.String(), nil)
	if reqErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr))
		return
	}
	mergeHttpHeader(req.Header, ctx.Request.Header)
	resp, doErr := client.Do(req)
	if doErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(doErr))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusFound {
		loc, locErr := resp.Location()
		if locErr != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(locErr))
			return
		}
		id := ctx.Param("id")
		if strings.Contains(loc.String(), id) {
			hlsChannelsRedirectURLLock.Lock()
			hlsChannelsRedirectURL[id] = *loc
			hlsChannelsRedirectURLLock.Unlock()
			hlsReq, hlsReqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", loc.String(), nil)
			if hlsReqErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsReqErr))
				return
			}
			mergeHttpHeader(hlsReq.Header, ctx.Request.Header)
			hlsResp, hlsDoErr := client.Do(hlsReq)
			if hlsDoErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsDoErr))
				return
			}
			defer func() { _ = hlsResp.Body.Close() }()

			b, readErr := io.ReadAll(hlsResp.Body)
			if readErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr))
				return
			}
			body := string(b)
			body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
			utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
			return
		}
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("unable to HLS stream")))
		return
	}

	utils.DebugLog("HLS stream response status: %d", resp.StatusCode)
	ctx.Status(resp.StatusCode)
}

func (c *Config) xtreamHlsrStream(ctx *gin.Context) {
	channel := ctx.Param("channel")
	redirURL, err := getHlsRedirectURL(channel)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	nextURL, parseErr := url.Parse(fmt.Sprintf("%s://%s/hlsr/%s/%s/%s/%s/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), c.XtreamUser, c.XtreamPassword, ctx.Param("channel"), ctx.Param("hash"), ctx.Param("chunk")))
	if parseErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(parseErr))
		return
	}
	c.hlsXtreamStream(ctx, nextURL)
}

// Restore helper used by HLS handlers
func getHlsRedirectURL(channel string) (*url.URL, error) {
	hlsChannelsRedirectURLLock.RLock()
	defer hlsChannelsRedirectURLLock.RUnlock()
	u, ok := hlsChannelsRedirectURL[channel+".m3u8"]
	if !ok {
		return nil, utils.PrintErrorAndReturn(errors.New("HSL redirect url not found"))
	}
	return &u, nil
}
