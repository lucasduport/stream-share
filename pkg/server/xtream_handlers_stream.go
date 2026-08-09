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
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
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
	pollTimer := time.NewTimer(200 * time.Millisecond)
	defer pollTimer.Stop()
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
			if !pollTimer.Stop() {
				select {
				case <-pollTimer.C:
				default:
				}
			}
			pollTimer.Reset(200 * time.Millisecond)
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
			case <-pollTimer.C:
			}
			continue
		}
		if rerr != nil {
			return
		}
	}
}

// streamVODWithCache serves a movie or series request, transparently caching
// it to local disk on first access. When a ready cache entry exists it serves
// the local file; otherwise it starts a background download and proxies to the
// upstream so the client gets immediate playback while caching proceeds in
// parallel. When no database is configured the request is proxied directly via
// the fallback handler.
//
// basePath is "movie" or "series"; defaultExt is the extension used when none
// can be resolved from the request or the M3U catalogue (".mp4" for movies,
// ".mkv" for series); fallback is the handler used when VOD caching is
// unavailable (c.xtreamStream for the Xtream-credentials path, c.stream for the
// proxy-credentials path).
func (c *Config) streamVODWithCache(ctx *gin.Context, basePath, defaultExt string, fallback func(ctx *gin.Context, oriURL *url.URL)) {
	id := ctx.Param("id")

	if c.VODCacheEnabled {
		idRaw := strings.TrimSuffix(id, path.Ext(id))
		if !isValidStreamID(idRaw) {
			utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
			ctx.AbortWithStatus(http.StatusBadRequest)
			return
		}

		if c.sessionManager != nil {
			username := c.resolveRequestUsername(ctx)
			if username != "" {
				label := idRaw
				if name, ok := c.resolveTitleAtStart(idRaw, basePath); ok && strings.TrimSpace(name) != "" {
					label = name
				}
				utils.InfoLog("VOD %s started: %s for user %s", basePath, label, username)
				c.sessionManager.RegisterVODView(username, idRaw, basePath, label)
				defer c.sessionManager.UnregisterVODView(username, idRaw)
			}
		}

		if c.db != nil {
			if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
				if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
					ct := contentTypeForPath(entry.FilePath)
					c.touchVODCache(idRaw)
					utils.InfoLog("Serving cached %s for %s from %s", basePath, c.vodLabel(idRaw), entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
			}

			// Not cached yet: auto-start 7-day caching in the background.
			upstream, dest, _ := c.resolveVODCacheURL(basePath, defaultExt, id)
			expires := time.Now().Add(7 * 24 * time.Hour)
			if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: basePath, FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
				utils.ErrorLog("Failed to record %s cache entry for %s: %v", basePath, idRaw, err)
			}
			c.startBackgroundDownload(upstream, dest, idRaw, basePath, expires)

			// Proxy to upstream directly: lets the IPTV server handle Content-Length,
			// Content-Range, and Range seeks natively. This avoids avformat errors
			// (MP4 moov at EOF) and seek loops. Background caching continues
			// independently; once complete, future requests serve from the local file.
			upstreamURL, upstreamErr := url.Parse(upstream)
			if upstreamErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
				return
			}
			c.stream(ctx, upstreamURL)
			return
		}
	}

	// Caching disabled or no database: proxy directly to upstream.
	rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	utils.DebugLog("VOD %s streaming request - proxying to upstream: %s", basePath, rpURL.String())
	fallback(ctx, rpURL)
}

func (c *Config) xtreamStreamMovie(ctx *gin.Context) {
	c.streamVODWithCache(ctx, "movie", ".mp4", c.xtreamStream)
}

func (c *Config) xtreamStreamSeries(ctx *gin.Context) {
	c.streamVODWithCache(ctx, "series", ".mkv", c.xtreamStream)
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
	c.streamVODWithCache(ctx, "movie", ".mp4", c.stream)
}

func (c *Config) xtreamProxyCredentialsSeriesStreamHandler(ctx *gin.Context) {
	c.streamVODWithCache(ctx, "series", ".mkv", c.stream)
}
