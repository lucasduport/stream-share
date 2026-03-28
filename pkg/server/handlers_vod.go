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
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"io"
	"strconv"
	"sort"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// Optional timeout-aware session manager interface (non-breaking)
type timeoutAware interface {
	// Returns (true, until) when user is timed out; (false, zeroTime) otherwise.
	IsUserTimedOut(username string) (bool, time.Time)
}

// searchVOD searches for VOD content matching the query
func (c *Config) searchVOD(ctx *gin.Context) {
	utils.DebugLog("API: VOD search request received")

	var req struct {
		Username string `json:"username"`
		Query    string `json:"query"`
	}

	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorLog("API: Invalid VOD search request: %v", err)
		ctx.JSON(http.StatusBadRequest, types.APIResponse{
			Success: false,
			Error:   "Invalid request: " + err.Error(),
		})
		return
	}

	utils.DebugLog("API: Searching VOD for user %s, query: %s", req.Username, req.Query)

	// Enforce timeout if supported by session manager
	if c.sessionManager != nil {
		if sm, ok := interface{}(c.sessionManager).(timeoutAware); ok {
			if timedOut, until := sm.IsUserTimedOut(req.Username); timedOut {
				utils.WarnLog("API: VOD search blocked for timed-out user %s (until %s)", req.Username, until.Format(time.RFC3339))
				ctx.JSON(http.StatusForbidden, types.APIResponse{
					Success: false,
					Error:   fmt.Sprintf("User '%s' is currently timed out until %s", req.Username, until.Format(time.RFC3339)),
				})
				return
			}
		}
	}

	results, err := c.searchXtreamVOD(req.Query)
	if err != nil {
		utils.ErrorLog("API: VOD search failed: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Failed to search VOD: " + err.Error(),
		})
		return
	}

	utils.DebugLog("API: Found %d VOD results for query: %s", len(results), req.Query)

	token := uuid.New().String()
	vodRequest := &types.VODRequest{
		Username:  req.Username,
		Query:     req.Query,
		Results:   results,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Minute),
		Token:     token,
	}

	// TODO: Store the VOD request in the database

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"request_token": token,
			"results":       results,
			"expires_at":    vodRequest.ExpiresAt,
		},
	})
}

// enrichVODPage enriches only the current page of VOD results with metadata that may be slow to compute (e.g., size).
// It takes the full result list with minimal fields and returns the same list with the specified page enriched.
func (c *Config) enrichVODPage(ctx *gin.Context) {
	var req struct {
		Query   string           `json:"query"`
		Results []types.VODResult `json:"results"`
		Page    int              `json:"page"`
		PerPage int              `json:"per_page"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "Invalid request: " + err.Error()})
		return
	}
	if req.PerPage <= 0 { req.PerPage = 25 }
	total := len(req.Results)
	if total == 0 {
		ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: map[string]interface{}{"results": req.Results}})
		return
	}
	pages := (total + req.PerPage - 1) / req.PerPage
	if pages == 0 { pages = 1 }
	if req.Page < 0 { req.Page = 0 }
	if req.Page >= pages { req.Page = pages - 1 }
	start := req.Page * req.PerPage
	end := start + req.PerPage
	if end > total { end = total }

	// Build an index of movie streamID -> extension from the cached VOD M3U once
	extIndex := map[string]string{}
	if m3uPath, err := c.ensureVODM3UCache(); err == nil {
		if idx, err2 := parseVODM3UExtensions(m3uPath); err2 == nil { extIndex = idx }
	}
	// Shared HTTP client with per-request timeout
	client := &http.Client{Timeout: 2500 * time.Millisecond, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 { return http.ErrUseLastResponse }
		if len(via) > 0 { prev := via[len(via)-1]; for k, vv := range prev.Header { arr := make([]string, len(vv)); copy(arr, vv); req.Header[k] = arr } }
		return nil
	}}

	// Prefill from cache where available
	for i := start; i < end; i++ {
		if sz, ok := getCachedSize(req.Results[i].StreamID); ok && sz > 0 {
			req.Results[i].SizeBytes = sz
			req.Results[i].Size = utils.HumanBytes(sz)
		}
	}
	// Probe only current page
	type job struct{ idx int }
	count := end - start
	if count > 0 {
		jobs := make(chan job, count)
		var wg sync.WaitGroup
		mu := sync.Mutex{}
		workers := 8
		if workers > count { workers = count }
		workerFn := func() {
			defer wg.Done()
			for j := range jobs {
				i := j.idx
				streamID := req.Results[i].StreamID
				if streamID == "" { continue }
				if req.Results[i].SizeBytes > 0 { continue }
				// Build Xtream URL with best-effort extension
				typ := req.Results[i].StreamType; if typ == "" { typ = "movie" }
				basePath := "movie"; if typ == "series" { basePath = "series" }
				finalID := streamID
				if ext := extIndex[streamID]; ext != "" { finalID += ext } else if path.Ext(finalID) == "" { if basePath == "series" { finalID += ".mkv" } else { finalID += ".mp4" } }
				vodURL := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
				// Range GET
				reqHTTP, _ := http.NewRequest("GET", vodURL, nil)
				reqHTTP.Header.Set("Range", "bytes=0-0")
				reqHTTP.Header.Set("User-Agent", utils.GetIPTVUserAgent())
				reqHTTP.Header.Set("Accept-Encoding", "identity")
				reqHTTP.Header.Set("Accept-Language", utils.GetLanguageHeader())
				reqHTTP.Header.Set("Accept", "*/*")
				if resp, err := client.Do(reqHTTP); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if cr := resp.Header.Get("Content-Range"); cr != "" {
						if total := strings.TrimSpace(cr[strings.LastIndex(cr, "/")+1:]); total != "*" {
							if sz, perr := parseInt64(total); perr == nil && sz > 0 {
								mu.Lock(); req.Results[i].SizeBytes = sz; req.Results[i].Size = utils.HumanBytes(sz); mu.Unlock(); setCachedSize(streamID, sz); continue
							}
						}
					}
					if cl := resp.Header.Get("Content-Length"); cl != "" {
						if sz, perr := parseInt64(cl); perr == nil && sz > 0 {
							mu.Lock(); req.Results[i].SizeBytes = sz; req.Results[i].Size = utils.HumanBytes(sz); mu.Unlock(); setCachedSize(streamID, sz); continue
						}
					}
				}
			}
		}
		for w := 0; w < workers; w++ { wg.Add(1); go workerFn() }
		for i := start; i < end; i++ { jobs <- job{idx: i} }
		close(jobs)
		wg.Wait()
	}

	// Keep ordering stable for the client
	sort.SliceStable(req.Results, func(i, j int) bool { return strings.ToLower(req.Results[i].Title) < strings.ToLower(req.Results[j].Title) })
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: map[string]interface{}{"results": req.Results}})
}

// createVODDownload creates a temporary download link for VOD content
func (c *Config) createVODDownload(ctx *gin.Context) {
	utils.DebugLog("API: VOD download request received")

	var req struct {
		Username string `json:"username"`
		StreamID string `json:"stream_id"`
		Title    string `json:"title"`
		Type     string `json:"type"` // movie or series
	}

	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorLog("API: Invalid VOD download request: %v", err)
		ctx.JSON(http.StatusBadRequest, types.APIResponse{
			Success: false,
			Error:   "Invalid request: " + err.Error(),
		})
		return
	}

	utils.DebugLog("API: Creating download for user %s, stream %s, title %s", req.Username, req.StreamID, req.Title)

	// Enforce timeout if supported by session manager
	if c.sessionManager != nil {
		if sm, ok := interface{}(c.sessionManager).(timeoutAware); ok {
			if timedOut, until := sm.IsUserTimedOut(req.Username); timedOut {
				utils.WarnLog("API: VOD download blocked for timed-out user %s (until %s)", req.Username, until.Format(time.RFC3339))
				ctx.JSON(http.StatusForbidden, types.APIResponse{
					Success: false,
					Error:   fmt.Sprintf("User '%s' is currently timed out until %s", req.Username, until.Format(time.RFC3339)),
				})
				return
			}
		}
	}

	if c.sessionManager == nil {
		utils.ErrorLog("Session manager is nil in createVODDownload")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Session manager not initialized",
		})
		return
	}

	// Check if the user is currently streaming something
	userSession := c.sessionManager.GetUserSession(req.Username)
	if userSession != nil && userSession.StreamID != "" && userSession.StreamType == "live" {
		utils.WarnLog("User %s tried to download while streaming %s", req.Username, userSession.StreamID)
		ctx.JSON(http.StatusConflict, types.APIResponse{
			Success: false,
			Error:   "User is currently watching a live stream. Please stop streaming first.",
		})
		return
	}

	// Generate a download URL for the VOD content, preserving the original extension from M3U
	basePath := "movie"
	if strings.ToLower(req.Type) == "series" {
		basePath = "series"
	}
	finalID := req.StreamID
	if path.Ext(finalID) == "" {
		// Try to resolve extension from cached M3U (movie/series), then fall back
		if ext := c.findVODExtensionInCache(basePath, finalID); ext != "" {
			utils.DebugLog("VOD extension resolved from cache: %s%s", finalID, ext)
			finalID = finalID + ext
		} else if basePath == "series" { 
			// Some providers predominantly use .mkv for series
			utils.DebugLog("VOD extension not found in cache for series id=%s; defaulting to .mkv", finalID)
			finalID = finalID + ".mkv"
		}
	}
	vodURL := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
	utils.DebugLog("API: VOD URL created: %s", utils.MaskURL(vodURL))

	// Generate a temporary download token
	token, err := c.sessionManager.GenerateTemporaryLink(req.Username, req.StreamID, req.Title, vodURL)
	if err != nil {
		utils.ErrorLog("API: Failed to generate temporary link: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Failed to generate download link: " + err.Error(),
		})
		return
	}

	// Create a proxied download URL with REVERSE_PROXY behavior
	protocol := "http"
	if c.ProxyConfig.HTTPS { protocol = "https" }
	hostPart := fmt.Sprintf("%s:%d", c.HostConfig.Hostname, c.HostConfig.Port)
	rev := strings.ToLower(strings.TrimSpace(os.Getenv("REVERSE_PROXY")))
	if rev == "1" || rev == "true" || rev == "yes" {
		// Reverse proxy front: prefer hostname without explicit port.
		// If DISCORD_API_URL is set, mirror its scheme and host (which may include port if user specified it).
		if api := strings.TrimSpace(os.Getenv("DISCORD_API_URL")); api != "" {
			if u, err := url.Parse(api); err == nil {
				if u.Scheme != "" { protocol = u.Scheme }
				if u.Host != "" { hostPart = u.Host } else { hostPart = c.HostConfig.Hostname }
			} else {
				hostPart = c.HostConfig.Hostname
			}
		} else {
			// No explicit API URL; keep default hostname, no port
			hostPart = c.HostConfig.Hostname
		}
	}
	downloadURL := fmt.Sprintf("%s://%s/download/%s", protocol, hostPart, token)

	utils.InfoLog("Created VOD download link for user %s, title: %s, token: %s", req.Username, req.Title, token)

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"download_url": downloadURL,
			"token":        token,
			"expires_at":   time.Now().Add(24 * time.Hour),
		},
	})
}

// pickVODExtension tries a small set of common extensions and returns the first that appears valid for the upstream.
// It performs quick HEAD requests with a short timeout. Falls back to .mp4 if none are conclusive.
func (c *Config) pickVODExtension(ctx *gin.Context, basePath, streamID string) string {
	// Allow override via env
	order := []string{".mp4", ".ts", ".mkv", ""}
	if v := strings.TrimSpace(utils.GetEnvOrDefault("VOD_EXT_ORDER", "")); v != "" {
		// comma-separated, keep only known values to avoid surprises
		parts := strings.Split(v, ",")
		tmp := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == ".mp4" || p == ".mkv" || p == ".ts" || p == "" { tmp = append(tmp, p) }
		}
		if len(tmp) > 0 { order = tmp }
	}
	client := &http.Client{ Timeout: 3 * time.Second }
	for _, ext := range order {
		url := fmt.Sprintf("%s/%s/%s/%s/%s%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, streamID, ext)
		req, _ := http.NewRequestWithContext(context.Background(), "HEAD", url, nil)
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Accept", "*/*")
		resp, err := client.Do(req)
		if err != nil { 
			// Providers often RST HEAD; keep this low-noise
			utils.DebugLog("VOD probe skipped/noisy for %s: %v", utils.MaskURL(url), err)
			continue 
		}
		resp.Body.Close()
		// Accept 2xx and 206
		if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusPartialContent {
			utils.DebugLog("VOD probe (HEAD) ok %d for %s", resp.StatusCode, utils.MaskURL(url))
			return ext
		}
		// Some providers return non-standard 461 or block HEAD; try GET range fallback
	if resp.StatusCode == 461 || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
			utils.DebugLog("VOD probe (HEAD) status %d for %s, trying GET range fallback", resp.StatusCode, utils.MaskURL(url))
			getReq, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
			getReq.Header.Set("User-Agent", utils.GetIPTVUserAgent())
			getReq.Header.Set("Range", "bytes=0-0")
			if getResp, getErr := client.Do(getReq); getErr == nil {
				io.Copy(io.Discard, getResp.Body)
				getResp.Body.Close()
				if (getResp.StatusCode >= 200 && getResp.StatusCode < 300) || getResp.StatusCode == http.StatusPartialContent {
					utils.DebugLog("VOD probe (GET range) ok %d for %s", getResp.StatusCode, utils.MaskURL(url))
					return ext
				}
				utils.DebugLog("VOD probe (GET range) status %d for %s", getResp.StatusCode, utils.MaskURL(url))
			} else {
		utils.DebugLog("VOD probe (GET range) noisy for %s: %v", utils.MaskURL(url), getErr)
			}
		} else {
			utils.DebugLog("VOD probe (HEAD) status %d for %s", resp.StatusCode, utils.MaskURL(url))
		}
	}
	return ".mp4"
}

// getVODRequestStatus gets the status of a VOD download request
func (c *Config) getVODRequestStatus(ctx *gin.Context) {
	requestID := ctx.Param("requestid")
	utils.DebugLog("API: Getting VOD request status for ID: %s", requestID)

	// TODO: Implement actual status checking from database
	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"status":   "completed",
			"progress": 100,
		},
	})
}

// findVODExtensionInCache tries to locate the original extension for a given stream ID
// by scanning the cached VOD M3U or series entries. Returns empty string if unknown.
func (c *Config) findVODExtensionInCache(basePath, streamID string) string {
	// First scan the cached VOD M3U for both movies and series
	if m3uPath, err := c.ensureVODM3UCache(); err == nil {
		if ext := findExtInM3U(m3uPath, basePath, streamID); ext != "" {
			return ext
		}
	}
	// Fallback: proxified main M3U if available
	c.ensureChannelIndex()
	if strings.TrimSpace(c.proxyfiedM3UPath) != "" {
		if ext := findExtInM3U(c.proxyfiedM3UPath, basePath, streamID); ext != "" {
			return ext
		}
	}
	return ""
}

// startCache starts caching a given VOD or series episode to local disk for a limited number of days (max 14)
func (c *Config) startCache(ctx *gin.Context) {
	var req struct {
		Username    string `json:"username"`
		StreamID    string `json:"stream_id"`
		Type        string `json:"type"` // movie or series
		Title       string `json:"title"`
		SeriesTitle string `json:"series_title"`
		Season      int    `json:"season"`
		Episode     int    `json:"episode"`
		Days        int    `json:"days"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "Invalid request: " + err.Error()})
		return
	}
	if req.Days <= 0 || req.Days >= 15 {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "days must be between 1 and 14"})
		return
	}
	if req.StreamID == "" { ctx.JSON(http.StatusBadRequest, types.APIResponse{Success:false, Error:"stream_id is required"}); return }
	t := strings.ToLower(strings.TrimSpace(req.Type))
	if t != "movie" && t != "series" { t = "movie" }

	// If already cached and valid, return it
	if c.db != nil {
		if entry, err := c.db.GetVODCache(req.StreamID); err == nil && entry != nil && entry.Status == "ready" {
			_ = c.db.TouchVODCache(req.StreamID)
			ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: map[string]interface{}{
				"cached": true,
				"stream_id": entry.StreamID,
				"status": entry.Status,
				"expires_at": entry.ExpiresAt,
			}})
			return
		}
	}

	// Determine target folder
	baseDir := os.Getenv("CACHE_FOLDER")
	if strings.TrimSpace(baseDir) == "" { baseDir = filepath.Join(os.TempDir(), "stream-share-cache") }
	_ = os.MkdirAll(baseDir, 0o755)

	// Resolve extension to build proper upstream URL
	basePath := "movie"
	if t == "series" { basePath = "series" }
	finalID := req.StreamID
	if path.Ext(finalID) == "" {
		// 1) Try to resolve from cached M3U first (movie/series)
		if ext := c.findVODExtensionInCache(basePath, finalID); ext != "" {
			utils.DebugLog("Cache: using M3U extension %s for %s", ext, finalID)
			finalID += ext
		} else {
			// 2) Optional: allow network probing only if explicitly enabled
			probeEnv := strings.ToLower(strings.TrimSpace(os.Getenv("VOD_EXT_PROBE")))
			if probeEnv == "1" || probeEnv == "true" || probeEnv == "yes" {
				if ext := c.pickVODExtension(nil, basePath, finalID); ext != "" {
					utils.DebugLog("Cache: probed extension %s for %s due to VOD_EXT_PROBE", ext, finalID)
					finalID += ext
				}
			}
			// 3) Still unknown? Use sane defaults without probing
			if path.Ext(finalID) == "" {
				def := ".mp4"; if basePath == "series" { def = ".mkv" }
				utils.DebugLog("Cache: defaulting extension %s for %s", def, finalID)
				finalID += def
			}
		}
	}
	upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)

	// Build local filename as <id>.<ext> for consistency
	ext := path.Ext(finalID)
	if ext == "" { ext = ".mp4" }
	// ensure we use the bare stream id without any accidental extension
	idOnly := strings.TrimSuffix(req.StreamID, path.Ext(req.StreamID))
	filename := filepath.Join(baseDir, idOnly+ext)

	// Build a safe, user-friendly title to persist (prefer M3U title)
	var safeTitle string
	if tt := c.findVODTitleInCache(basePath, req.StreamID); strings.TrimSpace(tt) != "" {
		safeTitle = strings.TrimSpace(tt)
	}
	// Fallbacks when M3U title not found
	if safeTitle == "" && t == "series" && strings.TrimSpace(req.SeriesTitle) != "" && (req.Season > 0 || req.Episode > 0) {
		safeTitle = fmt.Sprintf("%s — S%02dE%02d", req.SeriesTitle, req.Season, req.Episode)
	}
	if safeTitle == "" {
		safeTitle = strings.TrimSpace(req.Title)
	}
	if safeTitle == "" { safeTitle = "Unknown title" }

	// Persist a pending entry
	expires := time.Now().Add(time.Duration(req.Days) * 24 * time.Hour)
	if c.db != nil {
		_ = c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: req.StreamID, Type: t, Title: safeTitle, SeriesTitle: req.SeriesTitle, Season: req.Season, Episode: req.Episode, FilePath: filename, RequestedBy: req.Username, Status: "downloading", CreatedAt: time.Now(), ExpiresAt: expires})
	}

	// Spawn background download
	go c.fetchToFile(upstream, filename, req.StreamID, expires)

	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: map[string]interface{}{
		"cached": false,
		"stream_id": req.StreamID,
		"status": "downloading",
		"expires_at": expires,
	}})
}

// getCacheByStream returns cache info for a stream id
func (c *Config) getCacheByStream(ctx *gin.Context) {
	id := ctx.Param("streamid")
	if id == "" || c.db == nil {
		ctx.JSON(http.StatusNotFound, types.APIResponse{Success:false, Error:"not found"})
		return
	}
	if e, err := c.db.GetVODCache(id); err == nil {
		// Do not expose internal file paths
		resp := map[string]interface{}{
			"stream_id": e.StreamID,
			"status": e.Status,
			"downloaded_bytes": e.DownloadedBytes,
			"total_bytes": e.TotalBytes,
			"size_bytes": e.SizeBytes,
			"expires_at": e.ExpiresAt,
			"type": e.Type,
			"title": e.Title,
			"series_title": e.SeriesTitle,
			"season": e.Season,
			"episode": e.Episode,
		}
		ctx.JSON(http.StatusOK, types.APIResponse{Success:true, Data: resp})
	} else {
		ctx.JSON(http.StatusNotFound, types.APIResponse{Success:false, Error: err.Error()})
	}
}

// getCacheProgress returns minimal progress info for a given stream id
func (c *Config) getCacheProgress(ctx *gin.Context) {
	id := ctx.Param("streamid")
	if id == "" || c.db == nil { ctx.JSON(http.StatusNotFound, types.APIResponse{Success:false, Error:"not found"}); return }
	e, err := c.db.GetVODCache(id)
	if err != nil { ctx.JSON(http.StatusNotFound, types.APIResponse{Success:false, Error: err.Error()}); return }
	// Compute percentage
	var percent int
	if e.TotalBytes > 0 {
		percent = int((e.DownloadedBytes * 100) / e.TotalBytes)
		if percent > 100 { percent = 100 }
	} else if strings.ToLower(e.Status) == "ready" && e.SizeBytes > 0 {
		percent = 100
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success:true, Data: map[string]interface{}{
		"stream_id": e.StreamID,
		"status": e.Status,
		"downloaded_bytes": e.DownloadedBytes,
		"total_bytes": e.TotalBytes,
		"percent": percent,
		"expires_at": e.ExpiresAt,
		"title": e.Title,
		"series_title": e.SeriesTitle,
		"season": e.Season,
		"episode": e.Episode,
		"requested_by": e.RequestedBy,
	}})
}

// listCache returns active cache entries without exposing file paths
func (c *Config) listCache(ctx *gin.Context) {
	if c.db == nil { ctx.JSON(http.StatusOK, types.APIResponse{Success:true, Data: []interface{}{}}); return }
	list, err := c.db.ListVODCache(0)
	if err != nil { ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success:false, Error: err.Error()}); return }
	out := make([]map[string]interface{}, 0, len(list))
	now := time.Now()
	for _, e := range list {
		left := e.ExpiresAt.Sub(now)
		if left < 0 { left = 0 }
		item := map[string]interface{}{
			"stream_id": e.StreamID,
			"type": e.Type,
			"title": e.Title,
			"series_title": e.SeriesTitle,
			"season": e.Season,
			"episode": e.Episode,
			"status": e.Status,
			"requested_by": e.RequestedBy,
			"downloaded_bytes": e.DownloadedBytes,
			"total_bytes": e.TotalBytes,
			"size_bytes": e.SizeBytes,
			"expires_at": e.ExpiresAt,
			"time_left_seconds": int(left.Seconds()),
		}
		out = append(out, item)
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success:true, Data: out})
}

// fetchToFile downloads from upstream URL to a local file; marks DB entry ready/failed
func (c *Config) fetchToFile(upstream, dest, streamID string, expires time.Time) {
	utils.InfoLog("Caching start: %s -> %s", utils.MaskURL(upstream), dest)
	tmp := dest + ".part"
	// Create file
	f, err := os.Create(tmp)
	if err != nil { utils.ErrorLog("Cache: create file error: %v", err); c.cacheFail(streamID); return }
	defer f.Close()
	// Request with UA and support for resume in future
	req, _ := http.NewRequestWithContext(context.Background(), "GET", upstream, nil)
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil { utils.ErrorLog("Cache: upstream error: %v", err); c.cacheFail(streamID); return }
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		utils.ErrorLog("Cache: upstream status %d", resp.StatusCode)
		c.cacheFail(streamID); return
	}
	// Progress: known total?
	var total int64
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil { total = v }
	}
	var downloaded int64
	buf := make([]byte, 256*1024)
	lastUpdate := time.Now()
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			if _, ew := f.Write(buf[:nr]); ew != nil { utils.ErrorLog("Cache: write error: %v", ew); c.cacheFail(streamID); return }
			downloaded += int64(nr)
			// Periodically persist progress (throttle)
			if c.db != nil && time.Since(lastUpdate) > 1*time.Second {
				_ = c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: streamID, FilePath: dest, DownloadedBytes: downloaded, TotalBytes: total, Status: "downloading", ExpiresAt: expires, LastAccess: time.Now()})
				lastUpdate = time.Now()
			}
		}
		if er != nil {
			if er == io.EOF { break }
			utils.ErrorLog("Cache: read error: %v", er); c.cacheFail(streamID); return
		}
	}
	n := downloaded
	if err := f.Sync(); err != nil { utils.WarnLog("Cache: fsync warning: %v", err) }
	if err := os.Rename(tmp, dest); err != nil { utils.ErrorLog("Cache: rename error: %v", err); c.cacheFail(streamID); return }
	utils.InfoLog("Caching done: %s (%s)", dest, utils.HumanBytes(n))
	if c.db != nil {
		// Try to resolve and store the M3U title on completion (best-effort)
		basePath := "movie"
		if strings.Contains(upstream, "/series/") { basePath = "series" }
		var finalTitle string
		if t := c.findVODTitleInCache(basePath, streamID); strings.TrimSpace(t) != "" {
			finalTitle = strings.TrimSpace(t)
		}
		entry := &types.VODCacheEntry{StreamID: streamID, FilePath: dest, DownloadedBytes: n, TotalBytes: n, SizeBytes: n, Status: "ready", ExpiresAt: expires, LastAccess: time.Now()}
		if finalTitle != "" { entry.Title = finalTitle }
		_ = c.db.UpsertVODCache(entry)
	}
}

func (c *Config) cacheFail(streamID string) {
	if c.db != nil {
		_ = c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: streamID, Status: "failed", LastAccess: time.Now(), ExpiresAt: time.Now().Add(2*time.Hour)})
	}
}

// sanitizeFilename makes a safe filename
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" { return "vod" }
	repl := []string{"/","-", "\\","-",":","-","*","x","?","","\"","","<","","<","",">","","|","-"}
	for i := 0; i < len(repl); i += 2 { s = strings.ReplaceAll(s, repl[i], repl[i+1]) }
	// collapse spaces
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 { s = s[:120] }
	return s
}
// findExtInM3U scans a given M3U file for an entry path containing basePath and having
// the last segment starting with streamID plus an extension.
func findExtInM3U(filePath, basePath, streamID string) string {
	f, err := os.Open(filePath)
	if err != nil { return "" }
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if !(strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) { continue }
		// Quick path filter by basePath
		if !strings.Contains(line, "/"+basePath+"/") { continue }
		u, err := url.Parse(line)
		if err != nil { continue }
		last := path.Base(u.Path)
		if strings.HasPrefix(last, streamID+".") {
			return path.Ext(last)
		}
	}
	return ""
}

// findTitleInM3U scans for the #EXTINF title associated to a given streamID URL
func findTitleInM3U(filePath, basePath, streamID string) string {
	f, err := os.Open(filePath)
	if err != nil { return "" }
	defer f.Close()
	sc := bufio.NewScanner(f)
	lastExtinf := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" { continue }
		if strings.HasPrefix(line, "#EXTINF") {
			// Capture the text after the comma as the display title
			if idx := strings.LastIndex(line, ","); idx != -1 && idx+1 < len(line) {
				lastExtinf = strings.TrimSpace(line[idx+1:])
			} else {
				lastExtinf = ""
			}
			continue
		}
		if !(strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) { continue }
		if !strings.Contains(line, "/"+basePath+"/") { continue }
		u, err := url.Parse(line)
		if err != nil { continue }
		last := path.Base(u.Path)
		if strings.HasPrefix(last, streamID+".") {
			return lastExtinf
		}
		// not a match; reset extinf to avoid using wrong title for unrelated URLs
		lastExtinf = ""
	}
	return ""
}

// findVODTitleInCache tries to locate the display title for a given stream ID from cached M3U(s)
func (c *Config) findVODTitleInCache(basePath, streamID string) string {
	if m3uPath, err := c.ensureVODM3UCache(); err == nil {
		if t := findTitleInM3U(m3uPath, basePath, streamID); t != "" { return t }
	}
	c.ensureChannelIndex()
	if strings.TrimSpace(c.proxyfiedM3UPath) != "" {
		if t := findTitleInM3U(c.proxyfiedM3UPath, basePath, streamID); t != "" { return t }
	}
	return ""
}
