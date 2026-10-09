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

// vodEntry is what a single VOD M3U entry contributes to a catalogue lookup:
// its file extension and the #EXTINF display title attached to it.
type vodEntry struct {
	ext   string
	title string
}

// entryEntry is a memoised VOD catalogue lookup. A hit never expires; a miss
// expires so the catalogue is re-scanned after a refresh.
type entryEntry struct {
	entry   vodEntry
	expires time.Time // zero for hits, which never expire
}

var vodEntryCache sync.Map // basePath+"\x00"+streamID -> entryEntry

// findVODEntryInCache returns the catalogue entry (extension + title) for a
// stream ID, scanning the cached VOD M3U or the proxified main M3U. Results
// are memoised: the lookups are linear scans of the provider's full catalogue
// and are reached per HTTP Range request whenever the client omits the
// extension and the item is not yet cached — during a download, one playback
// would otherwise scan the catalogue hundreds of times. A miss is memoised
// too, since re-scanning to find nothing again is the expensive case; the
// entry expires so a refreshed catalogue is still picked up.
func (c *Config) findVODEntryInCache(basePath, streamID string) vodEntry {
	key := basePath + "\x00" + streamID
	if v, ok := vodEntryCache.Load(key); ok {
		e := v.(entryEntry)
		if (e.entry != vodEntry{}) || e.expires.IsZero() || time.Now().Before(e.expires) {
			return e.entry
		}
	}

	entry := c.scanVODEntry(basePath, streamID)

	e := entryEntry{entry: entry}
	if entry == (vodEntry{}) {
		e.expires = time.Now().Add(titleMissRetryAfter)
	}
	vodEntryCache.Store(key, e)
	return entry
}

// findVODExtensionInCache tries to locate the original extension for a given
// stream ID by scanning the cached VOD M3U or the proxified main M3U.
// Returns empty string if unknown.
func (c *Config) findVODExtensionInCache(basePath, streamID string) string {
	return c.findVODEntryInCache(basePath, streamID).ext
}

// findVODTitleInCache tries to locate the display title for a given stream ID
// from cached M3U(s).
func (c *Config) findVODTitleInCache(basePath, streamID string) string {
	return c.findVODEntryInCache(basePath, streamID).title
}

// scanVODEntry does the actual catalogue scans, one pass per file.
func (c *Config) scanVODEntry(basePath, streamID string) vodEntry {
	// First scan the cached VOD M3U for both movies and series
	if m3uPath, err := c.ensureVODM3UCache(); err == nil {
		if entry := findEntryInM3U(m3uPath, basePath, streamID); entry != (vodEntry{}) {
			return entry
		}
	}
	// Fallback: proxified main M3U if available
	c.ensureChannelIndex()
	if strings.TrimSpace(c.proxyfiedM3UPath) != "" {
		if entry := findEntryInM3U(c.proxyfiedM3UPath, basePath, streamID); entry != (vodEntry{}) {
			return entry
		}
	}
	return vodEntry{}
}

// findEntryInM3U scans a given M3U file in a single pass for the entry whose
// URL path contains basePath and whose last segment starts with streamID plus
// an extension. It returns the extension and the #EXTINF title preceding it.
func findEntryInM3U(filePath, basePath, streamID string) vodEntry {
	f, err := os.Open(filePath)
	if err != nil {
		return vodEntry{}
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
			return vodEntry{ext: path.Ext(last), title: lastExtinf}
		}
		// not a match; reset extinf to avoid using wrong title for unrelated URLs
		lastExtinf = ""
	}
	return vodEntry{}
}
