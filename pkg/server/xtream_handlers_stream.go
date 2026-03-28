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

// Prefer multiplexed streaming if enabled via env, otherwise fall back to legacy stream
// xtreamStream proxies streams; can switch to multiplexed mode via env flag.
func (c *Config) xtreamStream(ctx *gin.Context, oriURL *url.URL) {
    utils.DebugLog("-> Xtream streaming request: %s", ctx.Request.URL.Path)
    utils.DebugLog("-> Proxying to Xtream upstream: %s", oriURL.String())

    if c.sessionManager != nil && os.Getenv("FORCE_MULTIPLEXING") == "true" {
        utils.DebugLog("Using multiplexed streaming (FORCE_MULTIPLEXING=true)")
        c.multiplexedStream(ctx, oriURL)
        return
    }

    c.stream(ctx, oriURL)
}

func (c *Config) xtreamXMLTV(ctx *gin.Context) {
    client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    resp, err := client.GetXMLTV()
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    ctx.Data(http.StatusOK, "application/xml", resp)
}

func (c *Config) xtreamStreamHandler(ctx *gin.Context) {
    id := ctx.Param("id")
    rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamLive(ctx *gin.Context) {
    id := ctx.Param("id")
    rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamPlay(ctx *gin.Context) {
    token := ctx.Param("token")
    t := ctx.Param("type")
    rpURL, err := url.Parse(fmt.Sprintf("%s/play/%s/%s", c.XtreamBaseURL, token, t))
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamTimeshift(ctx *gin.Context) {
    duration := ctx.Param("duration")
    start := ctx.Param("start")
    id := ctx.Param("id")
    rpURL, err := url.Parse(fmt.Sprintf("%s/timeshift/%s/%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, duration, start, id))
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    c.stream(ctx, rpURL)
}

func (c *Config) xtreamStreamMovie(ctx *gin.Context) {
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
        username := ctx.GetString("username")
        if username == "" { username = ctx.Param("username") }
        if username != "" {
            movieTitle := idRaw
            if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
                movieTitle = name
            }
            c.sessionManager.RegisterVODView(username, idRaw, "movie", movieTitle)
            defer c.sessionManager.UnregisterVODView(username, idRaw)
        }
    }
    if c.db != nil {
        if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
            // If file exists and is ready, serve locally; if downloading, serve progressively from .part
            if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
                var ct string
                if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
                _ = c.db.TouchVODCache(idRaw)
                if strings.ToLower(entry.Status) == "ready" {
                    utils.InfoLog("Serving cached movie for %s from %s", idRaw, entry.FilePath)
                    serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
                    return
                }
                // Progressive serving from growing file
                utils.InfoLog("Serving progressively from cache (downloading) for %s from %s", idRaw, entry.FilePath)
                serveGrowingFileRange(ctx, entry.FilePath, ct, "", false, entry.TotalBytes)
                return
            }
        }
        // Not cached yet: auto-start 7-day caching in background and serve progressively
        // Determine extension from cached M3U if available, fallback to .mp4
        basePath := "movie"
        resolvedExt := c.findVODExtensionInCache(basePath, idRaw)
        finalID := idRaw
        if resolvedExt == "" { resolvedExt = ".mp4" }
        finalID += resolvedExt
        upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
        cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
        if cacheDir == "" { cacheDir = filepath.Join(os.TempDir(), "stream-share-cache") }
        _ = os.MkdirAll(cacheDir, 0o755)
        dest := filepath.Join(cacheDir, idRaw+resolvedExt)
        expires := time.Now().Add(7 * 24 * time.Hour)
        // Insert pending entry
        if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "movie", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil { utils.ErrorLog("Failed to record movie cache entry for %s: %v", idRaw, err) }
        if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, struct{}{}); !loaded {
            go func() {
                defer c.inProgressDownloads.Delete(idRaw)
                c.fetchToFile(upstream, dest, idRaw, expires)
            }()
        }
        // Serve progressively from growing file
        var ct string
        if ext := strings.ToLower(path.Ext(dest)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
        serveGrowingFileRange(ctx, dest, ct, "", false, 0)
        return
    }
    rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
    c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamSeries(ctx *gin.Context) {
    id := ctx.Param("id")
    idRaw := strings.TrimSuffix(id, path.Ext(id))
    // Reject IDs containing path separators or dot-dot to prevent path traversal.
    if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
        utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
        ctx.AbortWithStatus(http.StatusBadRequest)
        return
    }
    if c.sessionManager != nil {
        username := ctx.GetString("username")
        if username == "" { username = ctx.Param("username") }
        if username != "" {
            seriesTitle := idRaw
            if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
                seriesTitle = name
            }
            c.sessionManager.RegisterVODView(username, idRaw, "series", seriesTitle)
            defer c.sessionManager.UnregisterVODView(username, idRaw)
        }
    }
    if c.db != nil {
        if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
            if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
                var ct string
                if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
                _ = c.db.TouchVODCache(idRaw)
                if strings.ToLower(entry.Status) == "ready" {
                    utils.InfoLog("Serving cached episode for %s from %s", idRaw, entry.FilePath)
                    serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
                    return
                }
                utils.InfoLog("Serving progressively from cache (downloading) for %s from %s", idRaw, entry.FilePath)
                serveGrowingFileRange(ctx, entry.FilePath, ct, "", false, entry.TotalBytes)
                return
            }
        }
        // Not cached yet: auto-start 7-day caching in background
        basePath := "series"
        resolvedExt := c.findVODExtensionInCache(basePath, idRaw)
        finalID := idRaw
        if resolvedExt == "" { resolvedExt = ".mkv" }
        finalID += resolvedExt
        upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
        cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
        if cacheDir == "" { cacheDir = filepath.Join(os.TempDir(), "stream-share-cache") }
        _ = os.MkdirAll(cacheDir, 0o755)
        dest := filepath.Join(cacheDir, idRaw+resolvedExt)
        expires := time.Now().Add(7 * 24 * time.Hour)
        if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "series", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil { utils.ErrorLog("Failed to record series cache entry for %s: %v", idRaw, err) }
        if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, struct{}{}); !loaded {
            go func() {
                defer c.inProgressDownloads.Delete(idRaw)
                c.fetchToFile(upstream, dest, idRaw, expires)
            }()
        }
        var ct string
        if ext := strings.ToLower(path.Ext(dest)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
        serveGrowingFileRange(ctx, dest, ct, "", false, 0)
        return
    }
    rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    c.xtreamStream(ctx, rpURL)
}

// Direct handlers using proxy credentials
func (c *Config) xtreamProxyCredentialsStreamHandler(ctx *gin.Context) {
    id := ctx.Param("id")
    utils.DebugLog("Direct stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
    rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { utils.ErrorLog("Failed to parse upstream URL: %v", err); ctx.AbortWithStatus(500); return }
    c.multiplexedStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsLiveStreamHandler(ctx *gin.Context) {
    id := ctx.Param("id")
    utils.DebugLog("Direct live stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
    rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { utils.ErrorLog("Failed to parse upstream URL: %v", err); ctx.AbortWithStatus(500); return }
    c.multiplexedStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsMovieStreamHandler(ctx *gin.Context) {
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
        username := ctx.GetString("username")
        if username == "" { username = ctx.Param("username") }
        if username != "" {
            movieTitle := idRaw
            if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
                movieTitle = name
            }
            c.sessionManager.RegisterVODView(username, idRaw, "movie", movieTitle)
            defer c.sessionManager.UnregisterVODView(username, idRaw)
        }
    }
    if c.db != nil {
        if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
            if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
                var ct string
                if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
                _ = c.db.TouchVODCache(idRaw)
                if strings.ToLower(entry.Status) == "ready" {
                    utils.InfoLog("Serving cached movie (proxy creds path) for %s from %s", idRaw, entry.FilePath)
                    serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
                    return
                }
                utils.InfoLog("Serving progressively from cache (downloading, proxy creds) for %s from %s", idRaw, entry.FilePath)
                serveGrowingFileRange(ctx, entry.FilePath, ct, "", false, entry.TotalBytes)
                return
            }
        }
        // Auto-start caching and serve progressively
        basePath := "movie"
        resolvedExt := c.findVODExtensionInCache(basePath, idRaw)
        finalID := idRaw
        if resolvedExt == "" { resolvedExt = ".mp4" }
        finalID += resolvedExt
        upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
        cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
        if cacheDir == "" { cacheDir = filepath.Join(os.TempDir(), "stream-share-cache") }
        _ = os.MkdirAll(cacheDir, 0o755)
        dest := filepath.Join(cacheDir, idRaw+resolvedExt)
        expires := time.Now().Add(7 * 24 * time.Hour)
        if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "movie", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil { utils.ErrorLog("Failed to record movie cache entry for %s: %v", idRaw, err) }
        if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, struct{}{}); !loaded {
            go func() {
                defer c.inProgressDownloads.Delete(idRaw)
                c.fetchToFile(upstream, dest, idRaw, expires)
            }()
        }
        var ct string
        if ext := strings.ToLower(path.Ext(dest)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
        serveGrowingFileRange(ctx, dest, ct, "", false, 0)
        return
    }
    rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { utils.ErrorLog("Failed to parse upstream URL: %v", err); ctx.AbortWithStatus(500); return }
    c.multiplexedStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsSeriesStreamHandler(ctx *gin.Context) {
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
        username := ctx.GetString("username")
        if username == "" { username = ctx.Param("username") }
        if username != "" {
            seriesTitle := idRaw
            if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
                seriesTitle = name
            }
            c.sessionManager.RegisterVODView(username, idRaw, "series", seriesTitle)
            defer c.sessionManager.UnregisterVODView(username, idRaw)
        }
    }
    if c.db != nil {
        if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
            if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
                var ct string
                if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
                _ = c.db.TouchVODCache(idRaw)
                if strings.ToLower(entry.Status) == "ready" {
                    utils.InfoLog("Serving cached episode (proxy creds path) for %s from %s", idRaw, entry.FilePath)
                    serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
                    return
                }
                utils.InfoLog("Serving progressively from cache (downloading, proxy creds) for %s from %s", idRaw, entry.FilePath)
                serveGrowingFileRange(ctx, entry.FilePath, ct, "", false, entry.TotalBytes)
                return
            }
        }
        basePath := "series"
        resolvedExt := c.findVODExtensionInCache(basePath, idRaw)
        finalID := idRaw
        if resolvedExt == "" { resolvedExt = ".mkv" }
        finalID += resolvedExt
        upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
        cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
        if cacheDir == "" { cacheDir = filepath.Join(os.TempDir(), "stream-share-cache") }
        _ = os.MkdirAll(cacheDir, 0o755)
        dest := filepath.Join(cacheDir, idRaw+resolvedExt)
        expires := time.Now().Add(7 * 24 * time.Hour)
        if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "series", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil { utils.ErrorLog("Failed to record series cache entry for %s: %v", idRaw, err) }
        if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, struct{}{}); !loaded {
            go func() {
                defer c.inProgressDownloads.Delete(idRaw)
                c.fetchToFile(upstream, dest, idRaw, expires)
            }()
        }
        var ct string
        if ext := strings.ToLower(path.Ext(dest)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
        serveGrowingFileRange(ctx, dest, ct, "", false, 0)
        return
    }
    rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
    if err != nil { utils.ErrorLog("Failed to parse upstream URL: %v", err); ctx.AbortWithStatus(500); return }
    c.multiplexedStream(ctx, rpURL)
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
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }

    req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", fmt.Sprintf("%s://%s/hls/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), ctx.Param("chunk")), nil)
    if reqErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr)); return }

    mergeHttpHeader(req.Header, ctx.Request.Header)

    resp, doErr := http.DefaultClient.Do(req)
    if doErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(doErr)); return }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusFound {
        loc, locErr := resp.Location()
        if locErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(locErr)); return }
        id := ctx.Param("id")
        if strings.Contains(loc.String(), id) {
            hlsChannelsRedirectURLLock.Lock(); hlsChannelsRedirectURL[id] = *loc; hlsChannelsRedirectURLLock.Unlock()
            hlsReq, hlsReqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", loc.String(), nil)
            if hlsReqErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsReqErr)); return }
            mergeHttpHeader(hlsReq.Header, ctx.Request.Header)
            hlsResp, hlsDoErr := http.DefaultClient.Do(hlsReq)
            if hlsDoErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsDoErr)); return }
            defer hlsResp.Body.Close()

            b, readErr := io.ReadAll(hlsResp.Body)
            if readErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr)); return }
            body := string(b)
            body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
            utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
            mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
            ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
            return
        }
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("Unable to HLS stream")))
        return
    }

    utils.DebugLog("HLS stream response status: %d", resp.StatusCode)
    ctx.Status(resp.StatusCode)
}

func (c *Config) hlsXtreamStream(ctx *gin.Context, oriURL *url.URL) {
    utils.DebugLog("HLS stream request with URL: %s", oriURL.String())
    client := &http.Client{ CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse } }
    req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", oriURL.String(), nil)
    if reqErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr)); return }
    mergeHttpHeader(req.Header, ctx.Request.Header)
    resp, doErr := client.Do(req)
    if doErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(doErr)); return }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusFound {
        loc, locErr := resp.Location()
        if locErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(locErr)); return }
        id := ctx.Param("id")
        if strings.Contains(loc.String(), id) {
            hlsChannelsRedirectURLLock.Lock(); hlsChannelsRedirectURL[id] = *loc; hlsChannelsRedirectURLLock.Unlock()
            hlsReq, hlsReqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", loc.String(), nil)
            if hlsReqErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsReqErr)); return }
            mergeHttpHeader(hlsReq.Header, ctx.Request.Header)
            hlsResp, hlsDoErr := client.Do(hlsReq)
            if hlsDoErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsDoErr)); return }
            defer hlsResp.Body.Close()

            b, readErr := io.ReadAll(hlsResp.Body)
            if readErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr)); return }
            body := string(b)
            body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
            utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
            mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
            ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
            return
        }
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("Unable to HLS stream")))
        return
    }

    utils.DebugLog("HLS stream response status: %d", resp.StatusCode)
    ctx.Status(resp.StatusCode)
}

func (c *Config) xtreamHlsrStream(ctx *gin.Context) {
    channel := ctx.Param("channel")
    redirURL, err := getHlsRedirectURL(channel)
    if err != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)); return }
    nextURL, parseErr := url.Parse(fmt.Sprintf("%s://%s/hlsr/%s/%s/%s/%s/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), c.XtreamUser, c.XtreamPassword, ctx.Param("channel"), ctx.Param("hash"), ctx.Param("chunk")))
    if parseErr != nil { _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(parseErr)); return }
    c.hlsXtreamStream(ctx, nextURL)
}

// Restore helper used by HLS handlers
func getHlsRedirectURL(channel string) (*url.URL, error) {
    hlsChannelsRedirectURLLock.RLock(); defer hlsChannelsRedirectURLLock.RUnlock()
    u, ok := hlsChannelsRedirectURL[channel+".m3u8"]
    if !ok { return nil, utils.PrintErrorAndReturn(errors.New("HSL redirect url not found")) }
    return &u, nil
}


// serveGrowingFileRange serves a locally growing file (.part) with HTTP Range support.
// It waits for the file to appear and grow as needed to fulfill the requested range.
// If a completed file exists, it behaves like serveLocalFileRange.
// totalSize may be 0 if unknown; when known, it will be used in Content-Range.
func serveGrowingFileRange(ctx *gin.Context, filePath string, contentType string, filename string, asAttachment bool, totalSize int64) {
    // Resolve actual path (prefer .part if exists)
    partPath := filePath + ".part"
    pathToOpen := filePath
    if st, err := os.Stat(partPath); err == nil && !st.IsDir() {
        pathToOpen = partPath
    } else if st, err := os.Stat(filePath); err == nil && !st.IsDir() {
        pathToOpen = filePath
        if totalSize == 0 { totalSize = st.Size() }
    } else {
        // Wait briefly for writer to create the .part file
        deadline := time.Now().Add(3 * time.Second)
        for time.Now().Before(deadline) {
            if st, err := os.Stat(partPath); err == nil && !st.IsDir() { pathToOpen = partPath; break }
            time.Sleep(100 * time.Millisecond)
        }
    }

    // Open file
    f, err := os.Open(pathToOpen)
    if err != nil {
        // As last resort, 404
        ctx.Status(http.StatusNotFound)
        return
    }
    defer f.Close()

    // Determine dynamic size getter
    getSize := func() int64 {
        if st, err := f.Stat(); err == nil { return st.Size() }
        return 0
    }

    // Common headers
    if contentType == "" { contentType = contentTypeForPath(pathToOpen) }
    ctx.Header("Content-Type", contentType)
    ctx.Header("Accept-Ranges", "bytes")
    ctx.Header("X-Accel-Buffering", "no")
    if asAttachment {
        if filename == "" { filename = path.Base(filePath) }
        ctx.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
    }

    // HEAD handling: honor Range if provided using current size
    if ctx.Request.Method == http.MethodHead {
        sizeNow := getSize()
        rng := ctx.GetHeader("Range")
        if rng == "" {
            if totalSize > 0 { ctx.Header("Content-Length", strconv.FormatInt(totalSize, 10)) } else { ctx.Header("Content-Length", strconv.FormatInt(sizeNow, 10)) }
            ctx.Status(http.StatusOK)
            return
        }
        if start, end, ok := parseRange(rng, max64(totalSize, sizeNow)); ok {
            // Clamp end to available if total unknown
            if totalSize == 0 {
                sizeNow = getSize()
                if end >= sizeNow { end = sizeNow - 1 }
            }
            length := end - start + 1
            tot := totalSize
            if tot == 0 { tot = getSize() }
            ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, tot))
            ctx.Header("Content-Length", strconv.FormatInt(length, 10))
            ctx.Status(http.StatusPartialContent)
            return
        }
        ctx.Header("Content-Range", fmt.Sprintf("bytes */%d", max64(totalSize, sizeNow)))
        ctx.Status(http.StatusRequestedRangeNotSatisfiable)
        return
    }

    // GET with optional Range
    rng := ctx.GetHeader("Range")
    if rng == "" {
        // Progressive full-stream: do not set Content-Length to allow chunked transfer
        ctx.Status(http.StatusOK)
        // Start from offset 0 and stream as file grows
        var offset int64 = 0
        buf := make([]byte, 256*1024)
        for {
            // Ensure reader is at current offset
            if cur, _ := f.Seek(0, io.SeekCurrent); cur != offset {
                if _, err := f.Seek(offset, io.SeekStart); err != nil { return }
            }
            n, _ := f.Read(buf)
            if n > 0 {
                if _, werr := ctx.Writer.Write(buf[:n]); werr != nil { return }
                offset += int64(n)
                // Flush when possible
                if fl, ok := ctx.Writer.(http.Flusher); ok { fl.Flush() }
                continue
            }
            // EOF: check if file is still growing
            // If .part still exists, wait for more data
            if _, err2 := os.Stat(partPath); err2 == nil {
                // Wait a bit for more data
                select {
                case <-ctx.Request.Context().Done():
                    return
                case <-time.After(200 * time.Millisecond):
                    continue
                }
            }
            // No .part: finished file
            return
        }
    }

    // Range request
    // Determine available length now (use totalSize if known for parsing upper-bound)
    sizeNow := getSize()
    parseBase := max64(totalSize, sizeNow)
    // Use a relaxed large upper bound at parse-time to extract start/end even when file is empty
    relaxedBase := parseBase
    if relaxedBase == 0 { relaxedBase = 1 << 60 }
    start, end, ok := parseRange(rng, relaxedBase)
    if !ok {
        ctx.Header("Content-Range", fmt.Sprintf("bytes */%d", parseBase))
        ctx.Status(http.StatusRequestedRangeNotSatisfiable)
        return
    }

    // Detect open-ended range (bytes=N-): player is seeking and wants everything from N onwards.
    // Do NOT wait for end < sizeNow in this case — that would block until the full download finishes.
    rawSpec := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(rng)), "bytes="))
    if idx := strings.Index(rawSpec, ","); idx >= 0 { rawSpec = rawSpec[:idx] }
    openEnded := strings.HasSuffix(strings.TrimSpace(rawSpec), "-")

    // Wait until start is available; for bounded ranges also wait for end.
    for {
        sizeNow = getSize()
        if start < sizeNow && (openEnded || end < sizeNow) { break }
        if _, err := os.Stat(partPath); err != nil {
            if sizeNow == 0 || start >= sizeNow {
                ctx.Header("Content-Range", fmt.Sprintf("bytes */%d", sizeNow))
                ctx.Status(http.StatusRequestedRangeNotSatisfiable)
                return
            }
            if end >= sizeNow { end = sizeNow - 1 }
            openEnded = false // download done; treat as bounded
            break
        }
        select {
        case <-ctx.Request.Context().Done():
            return
        case <-time.After(150 * time.Millisecond):
        }
    }

    if _, err := f.Seek(start, io.SeekStart); err != nil { ctx.Status(http.StatusInternalServerError); return }

    if openEnded {
        // Open-ended seek: stream progressively from start, waiting for file growth.
        // Use known totalSize or '*' to avoid telling the player the file ends at current download offset.
        curEnd := getSize() - 1
        if curEnd < start { curEnd = start }
        totStr := "*"
        if totalSize > 0 { totStr = strconv.FormatInt(totalSize, 10) }
        ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%s", start, curEnd, totStr))
        ctx.Status(http.StatusPartialContent)
        var offset int64 = start
        buf := make([]byte, 256*1024)
        for {
            if cur, _ := f.Seek(0, io.SeekCurrent); cur != offset {
                if _, err := f.Seek(offset, io.SeekStart); err != nil { return }
            }
            n, _ := f.Read(buf)
            if n > 0 {
                if _, werr := ctx.Writer.Write(buf[:n]); werr != nil { return }
                offset += int64(n)
                if fl, ok := ctx.Writer.(http.Flusher); ok { fl.Flush() }
                continue
            }
            if _, err2 := os.Stat(partPath); err2 == nil {
                select {
                case <-ctx.Request.Context().Done():
                    return
                case <-time.After(200 * time.Millisecond):
                    continue
                }
            }
            return
        }
    }

    // Bounded range: serve exactly the requested bytes.
    // Use '*' for total when still downloading to avoid reporting partial size as the file's true size.
    length := end - start + 1
    totStr := "*"
    if totalSize > 0 {
        totStr = strconv.FormatInt(totalSize, 10)
    } else if _, statErr := os.Stat(partPath); statErr != nil {
        totStr = strconv.FormatInt(getSize(), 10) // download complete; use actual size
    }
    ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%s", start, end, totStr))
    ctx.Header("Content-Length", strconv.FormatInt(length, 10))
    ctx.Status(http.StatusPartialContent)

    var remaining = length
    buf := make([]byte, 256*1024)
    for remaining > 0 {
        toRead := int64(len(buf))
        if remaining < toRead { toRead = remaining }
        n, err := f.Read(buf[:toRead])
        if n > 0 {
            if _, werr := ctx.Writer.Write(buf[:n]); werr != nil { return }
            remaining -= int64(n)
            if fl, ok := ctx.Writer.(http.Flusher); ok { fl.Flush() }
            continue
        }
        if err == io.EOF || err == io.ErrUnexpectedEOF {
            if _, statErr := os.Stat(partPath); statErr == nil {
                select {
                case <-ctx.Request.Context().Done():
                    return
                case <-time.After(150 * time.Millisecond):
                    continue
                }
            }
            return
        }
        if err != nil {
            return
        }
    }
}