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
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// prepareVODHeaders returns a clean set of headers for strict VOD providers.
func prepareVODHeaders(ctx *gin.Context) http.Header {
	clean := http.Header{}
	// Accept
	if v := ctx.Request.Header.Get("Accept"); v != "" {
		clean.Set("Accept", v)
	} else {
		clean.Set("Accept", "*/*")
	}
	// Accept-Language
	if v := ctx.Request.Header.Get("Accept-Language"); v != "" {
		clean.Set("Accept-Language", v)
	} else {
		clean.Set("Accept-Language", utils.GetLanguageHeader())
	}
	// Range — only forward if the client actually sent it; do not inject bytes=0- because
	// that forces a 206 response which may lack a Content-Range total, leaving the player
	// unable to determine file size and triggering avformat errors.
	if v := ctx.Request.Header.Get("Range"); v != "" {
		clean.Set("Range", v)
	}
	// Connection
	clean.Set("Connection", "keep-alive")
	// UA and encoding
	clean.Set("User-Agent", utils.GetIPTVUserAgent())
	clean.Set("Accept-Encoding", "identity")
	return clean
}

// isVODPath reports whether the given URL path likely targets VOD content
// (movie or series) based on known path segments or file extensions.
func isVODPath(p string) bool {
	lp := strings.ToLower(p)
	// Live, timeshift, and HLS are never VOD even if they end in .ts
	if strings.Contains(lp, "/live/") || strings.Contains(lp, "/timeshift/") ||
		strings.Contains(lp, "/hls/") || strings.Contains(lp, "/hlsr/") || strings.Contains(lp, "/play/") {
		return false
	}
	if strings.Contains(lp, "/movie/") || strings.Contains(lp, "/series/") {
		return true
	}
	switch strings.ToLower(path.Ext(lp)) {
	case ".mp4", ".mkv", ".ts":
		return true
	default:
		return false
	}
}

// contentTypeForPath maps a file extension or known path to an appropriate
// Content-Type value for streaming responses.
func contentTypeForPath(p string) string {
	lp := strings.ToLower(p)
	ext := strings.ToLower(path.Ext(lp))
	if strings.Contains(lp, "/live/") || ext == ".ts" {
		return "video/mp2t"
	}
	switch ext {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}

// setNoBufferingHeaders configures common headers to minimize intermediary
// buffering and keep the connection alive during long-running streams.
func setNoBufferingHeaders(ctx *gin.Context, contentType string) {
	if contentType != "" {
		ctx.Header("Content-Type", contentType)
	}
	ctx.Header("Cache-Control", "no-store")
	ctx.Header("Pragma", "no-cache")
	ctx.Header("Connection", "keep-alive")
	ctx.Header("X-Accel-Buffering", "no")
}

type values []string

func (vs values) contains(s string) bool {
	for _, v := range vs {
		if v == s {
			return true
		}
	}
	return false
}

// mergeHttpHeader copies headers from src to dst without duplicating identical values.
func mergeHttpHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			if values(dst.Values(k)).contains(v) {
				continue
			}
			dst.Add(k, v)
		}
	}
}

// registerVODView creates a synthetic VOD session for /status and watch-history
// tracking. It resolves the best display title (canonical name lookup, then
// fallbackTitle, then streamID), calls RegisterVODView, and returns a cleanup
// func that schedules the grace-period teardown via UnregisterVODView. The
// caller should defer the returned func. No-op when username or streamID is
// empty.
func (c *Config) registerVODView(username, streamID, streamType, fallbackTitle string) func() {
	if c.sessionManager == nil || username == "" || streamID == "" {
		return func() {}
	}
	label := fallbackTitle
	if name, ok := c.resolveTitleAtStart(streamID, streamType); ok && strings.TrimSpace(name) != "" {
		label = name
	}
	if strings.TrimSpace(label) == "" {
		label = streamID
	}
	utils.InfoLog("VOD %s started: %s for user %s", streamType, label, username)
	c.sessionManager.RegisterVODView(username, streamID, streamType, label)
	return func() { c.sessionManager.UnregisterVODView(username, streamID) }
}

// serveLocalFileRange serves a local file with HTTP Range support for seamless seeking.
// Uses http.ServeContent, which handles Range parsing, conditional requests
// (If-Modified-Since / If-None-Match → 304), ETag generation, and HEAD for free.
// If asAttachment is true, a Content-Disposition header will be set using filename.
func serveLocalFileRange(ctx *gin.Context, filePath string, contentType string, filename string, asAttachment bool) {
	f, err := os.Open(filePath)
	if err != nil {
		ctx.Status(http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		ctx.Status(http.StatusNotFound)
		return
	}

	if contentType != "" {
		ctx.Header("Content-Type", contentType)
	}
	ctx.Header("X-Accel-Buffering", "no")
	if asAttachment {
		if filename == "" {
			filename = path.Base(filePath)
		}
		ctx.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	}

	http.ServeContent(ctx.Writer, ctx.Request, path.Base(filePath), fi.ModTime(), f)
}
