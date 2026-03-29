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
    "net/http"
    "path"
    "strings"
    "os"
    "strconv"

    "github.com/gin-gonic/gin"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// isVODPath reports whether the given URL path likely targets VOD content
// (movie or series) based on known path segments or file extensions.
// prepareVODHeaders returns a clean set of headers for strict VOD providers.
func prepareVODHeaders(ctx *gin.Context) http.Header {
    clean := http.Header{}
    // Accept
    if v := ctx.Request.Header.Get("Accept"); v != "" { clean.Set("Accept", v) } else { clean.Set("Accept", "*/*") }
    // Accept-Language
    if v := ctx.Request.Header.Get("Accept-Language"); v != "" { clean.Set("Accept-Language", v) } else { clean.Set("Accept-Language", utils.GetLanguageHeader()) }
    // Range
    if v := ctx.Request.Header.Get("Range"); v != "" { clean.Set("Range", v) } else { clean.Set("Range", "bytes=0-") }
    // Connection
    clean.Set("Connection", "keep-alive")
    // UA and encoding
    clean.Set("User-Agent", utils.GetIPTVUserAgent())
    clean.Set("Accept-Encoding", "identity")
    return clean
}

func isVODPath(p string) bool {
    lp := strings.ToLower(p)
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

// serveLocalFileRange serves a local file with HTTP Range support for seamless seeking.
// If asAttachment is true, a Content-Disposition header will be set using filename.
func serveLocalFileRange(ctx *gin.Context, filePath string, contentType string, filename string, asAttachment bool) {
    // Stat file
    fi, err := os.Stat(filePath)
    if err != nil || fi.IsDir() {
        ctx.Status(http.StatusNotFound)
        return
    }

    // Open file
    f, err := os.Open(filePath)
    if err != nil {
        ctx.Status(http.StatusInternalServerError)
        return
    }
    defer f.Close()

    size := fi.Size()
    modTime := fi.ModTime()

    // Common headers
    if contentType == "" { contentType = "application/octet-stream" }
    ctx.Header("Content-Type", contentType)
    ctx.Header("Accept-Ranges", "bytes")
    ctx.Header("Last-Modified", modTime.UTC().Format(http.TimeFormat))
    ctx.Header("X-Accel-Buffering", "no")
    if asAttachment {
        if filename == "" { filename = path.Base(filePath) }
        ctx.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
    }

    // Handle HEAD quickly (respect Range semantics by sending appropriate headers without body)
    if ctx.Request.Method == http.MethodHead {
        rangeHdr := ctx.GetHeader("Range")
        if rangeHdr == "" {
            ctx.Header("Content-Length", strconv.FormatInt(size, 10))
            ctx.Status(http.StatusOK)
            return
        }
        // Parse single range only
        start, end, ok := parseRange(rangeHdr, size)
        if !ok {
            ctx.Header("Content-Range", fmt.Sprintf("bytes */%d", size))
            ctx.Status(http.StatusRequestedRangeNotSatisfiable)
            return
        }
        length := end - start + 1
        ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
        ctx.Header("Content-Length", strconv.FormatInt(length, 10))
        ctx.Status(http.StatusPartialContent)
        return
    }

    // GET with optional Range
    rangeHdr := ctx.GetHeader("Range")
    if rangeHdr == "" {
        // Full content
        ctx.Header("Content-Length", strconv.FormatInt(size, 10))
        ctx.Status(http.StatusOK)
        // Stream efficiently
        if _, err := io.Copy(ctx.Writer, f); err != nil {
            utils.DebugLog("serveLocalFileRange: full copy error (client likely disconnected): %v", err)
        }
        return
    }

    // Parse and serve partial content
    start, end, ok := parseRange(rangeHdr, size)
    if !ok {
        ctx.Header("Content-Range", fmt.Sprintf("bytes */%d", size))
        ctx.Status(http.StatusRequestedRangeNotSatisfiable)
        return
    }
    length := end - start + 1
    if _, err := f.Seek(start, io.SeekStart); err != nil {
        ctx.Status(http.StatusInternalServerError)
        return
    }
    ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
    ctx.Header("Content-Length", strconv.FormatInt(length, 10))
    ctx.Status(http.StatusPartialContent)

    // Copy desired range
    if _, err := io.CopyN(ctx.Writer, f, length); err != nil {
        // client canceled or short read; end
        return
    }
}

// parseRange parses a single HTTP Range header value "bytes=start-end" with support
// for open-ended and suffix ranges. Returns start, end, ok.
func parseRange(h string, size int64) (int64, int64, bool) {
    h = strings.TrimSpace(h)
    if h == "" || !strings.HasPrefix(strings.ToLower(h), "bytes=") {
        return 0, 0, false
    }
    spec := strings.TrimSpace(h[len("bytes="):])
    // Only first range supported
    if idx := strings.Index(spec, ","); idx != -1 {
        spec = spec[:idx]
    }
    parts := strings.Split(spec, "-")
    if len(parts) != 2 {
        return 0, 0, false
    }
    // Suffix range: bytes=-N
    if parts[0] == "" {
        // last N bytes
        n, err := strconv.ParseInt(parts[1], 10, 64)
        if err != nil || n <= 0 {
            return 0, 0, false
        }
        if n > size { n = size }
        return size - n, size - 1, true
    }
    // Normal or open-ended
    start, err := strconv.ParseInt(parts[0], 10, 64)
    if err != nil || start < 0 || start >= size {
        return 0, 0, false
    }
    if parts[1] == "" {
        return start, size - 1, true
    }
    end, err := strconv.ParseInt(parts[1], 10, 64)
    if err != nil || end < start { return 0, 0, false }
    if end >= size { end = size - 1 }
    return start, end, true
}
