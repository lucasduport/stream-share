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
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// pickVODExtension tries a small set of common extensions and returns the first that appears valid for the upstream.
// It performs quick HEAD requests with a short timeout. Falls back to .mp4 if none are conclusive.
func (c *Config) pickVODExtension(ctx *gin.Context, basePath, streamID string) string {
	// Allow override via env
	order := []string{".mp4", ".ts", ".mkv", ""}
	if v := strings.TrimSpace(c.VODExtOrder); v != "" {
		// comma-separated, keep only known values to avoid surprises
		parts := strings.Split(v, ",")
		tmp := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == ".mp4" || p == ".mkv" || p == ".ts" || p == "" {
				tmp = append(tmp, p)
			}
		}
		if len(tmp) > 0 {
			order = tmp
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, ext := range order {
		probeURL := fmt.Sprintf("%s/%s/%s/%s/%s%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, streamID, ext)
		req, reqErr := http.NewRequestWithContext(context.Background(), "HEAD", probeURL, nil)
		if reqErr != nil {
			utils.DebugLog("VOD probe: failed to build HEAD request for %s: %v", utils.MaskURL(probeURL), reqErr)
			continue
		}
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Accept", "*/*")
		resp, err := client.Do(req)
		if err != nil {
			// Providers often RST HEAD; keep this low-noise
			utils.DebugLog("VOD probe skipped/noisy for %s: %v", utils.MaskURL(probeURL), err)
			continue
		}
		_ = resp.Body.Close()
		// Accept 2xx and 206
		if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusPartialContent {
			utils.DebugLog("VOD probe (HEAD) ok %d for %s", resp.StatusCode, utils.MaskURL(probeURL))
			return ext
		}
		// Some providers return non-standard 461 or block HEAD; try GET range fallback
		if resp.StatusCode == 461 || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
			utils.DebugLog("VOD probe (HEAD) status %d for %s, trying GET range fallback", resp.StatusCode, utils.MaskURL(probeURL))
			getReq, getReqErr := http.NewRequestWithContext(context.Background(), "GET", probeURL, nil)
			if getReqErr != nil {
				utils.DebugLog("VOD probe: failed to build GET request for %s: %v", utils.MaskURL(probeURL), getReqErr)
				continue
			}
			getReq.Header.Set("User-Agent", utils.GetIPTVUserAgent())
			getReq.Header.Set("Range", "bytes=0-0")
			if getResp, getErr := client.Do(getReq); getErr == nil {
				_, _ = io.Copy(io.Discard, getResp.Body)
				_ = getResp.Body.Close()
				if (getResp.StatusCode >= 200 && getResp.StatusCode < 300) || getResp.StatusCode == http.StatusPartialContent {
					utils.DebugLog("VOD probe (GET range) ok %d for %s", getResp.StatusCode, utils.MaskURL(probeURL))
					return ext
				}
				utils.DebugLog("VOD probe (GET range) status %d for %s", getResp.StatusCode, utils.MaskURL(probeURL))
			} else {
				utils.DebugLog("VOD probe (GET range) noisy for %s: %v", utils.MaskURL(probeURL), getErr)
			}
		} else {
			utils.DebugLog("VOD probe (HEAD) status %d for %s", resp.StatusCode, utils.MaskURL(probeURL))
		}
	}
	return ".mp4"
}

// extEntry is a memoised VOD extension lookup. A hit (non-empty ext) never
// expires; a miss expires so the catalogue is re-scanned after a refresh.
type extEntry struct {
	ext     string
	expires time.Time // zero for hits, which never expire
}

var vodExtCache sync.Map // basePath+"\x00"+streamID -> extEntry

// findVODExtensionInCache tries to locate the original extension for a given stream ID
// by scanning the cached VOD M3U or series entries. Returns empty string if unknown.
// vodExtCache memoises extension lookups, which are linear scans of the
// provider's full catalogue. This is reached per HTTP Range request whenever the
// client omits the extension and the item is not yet cached — so during a
// download, one playback would otherwise scan the catalogue hundreds of times.
// A miss is memoised too, since re-scanning to find nothing again is the
// expensive case; the entry expires so a refreshed catalogue is still picked up.
func (c *Config) findVODExtensionInCache(basePath, streamID string) string {
	key := basePath + "\x00" + streamID
	if v, ok := vodExtCache.Load(key); ok {
		e := v.(extEntry)
		if e.ext != "" || e.expires.IsZero() || time.Now().Before(e.expires) {
			return e.ext
		}
	}

	ext := c.scanVODExtension(basePath, streamID)

	entry := extEntry{ext: ext}
	if ext == "" {
		entry.expires = time.Now().Add(titleMissRetryAfter)
	}
	vodExtCache.Store(key, entry)
	return ext
}

// scanVODExtension does the actual catalogue scans.
func (c *Config) scanVODExtension(basePath, streamID string) string {
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

// findExtInM3U scans a given M3U file for an entry path containing basePath and having
// the last segment starting with streamID plus an extension.
func findExtInM3U(filePath, basePath, streamID string) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
			continue
		}
		// Quick path filter by basePath
		if !strings.Contains(line, "/"+basePath+"/") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			continue
		}
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
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	lastExtinf := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF") {
			// Capture the text after the comma as the display title
			if idx := strings.LastIndex(line, ","); idx != -1 && idx+1 < len(line) {
				lastExtinf = strings.TrimSpace(line[idx+1:])
			} else {
				lastExtinf = ""
			}
			continue
		}
		if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
			continue
		}
		if !strings.Contains(line, "/"+basePath+"/") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			continue
		}
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
		if t := findTitleInM3U(m3uPath, basePath, streamID); t != "" {
			return t
		}
	}
	c.ensureChannelIndex()
	if strings.TrimSpace(c.proxyfiedM3UPath) != "" {
		if t := findTitleInM3U(c.proxyfiedM3UPath, basePath, streamID); t != "" {
			return t
		}
	}
	return ""
}
