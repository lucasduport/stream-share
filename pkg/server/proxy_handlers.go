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
    "net"
    "net/http"
    "net/url"
    "path"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// getM3U sends the proxified M3U file generated during bootstrap.
func (c *Config) getM3U(ctx *gin.Context) {
    ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
    ctx.Header("Content-Type", "application/octet-stream")
    ctx.File(c.proxyfiedM3UPath)
}

// reverseProxy forwards a track request to the upstream using Xtream creds.
func (c *Config) reverseProxy(ctx *gin.Context) {
    rpURL, err := url.Parse(c.track.URI)
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
        return
    }

    // Always use Xtream creds for upstream query
    q := rpURL.Query()
    q.Set("username", c.XtreamUser.String())
    q.Set("password", c.XtreamPassword.String())
    rpURL.RawQuery = q.Encode()

    utils.DebugLog("-> Upstream username: %s, password: %s", c.XtreamUser.String(), c.XtreamPassword.String())
    utils.DebugLog("-> Final upstream URL: %s", rpURL.String())

    c.stream(ctx, rpURL)
}

// m3u8ReverseProxy forwards HLS index/chunk requests to upstream using Xtream creds.
func (c *Config) m3u8ReverseProxy(ctx *gin.Context) {
    id := ctx.Param("id")
    rpURL, err := url.Parse(strings.ReplaceAll(c.track.URI, path.Base(c.track.URI), id))
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
        return
    }

    q := rpURL.Query()
    q.Set("username", c.XtreamUser.String())
    q.Set("password", c.XtreamPassword.String())
    rpURL.RawQuery = q.Encode()

    utils.DebugLog("-> Upstream username: %s, password: %s", c.XtreamUser.String(), c.XtreamPassword.String())
    utils.DebugLog("-> Final upstream URL: %s", rpURL.String())

    c.stream(ctx, rpURL)
}

// stream proxies the content from upstream to the client, preserving status
// and most headers, while normalizing VOD header sets for stricter providers.
func (c *Config) stream(ctx *gin.Context, oriURL *url.URL) {
    utils.DebugLog("-> Streaming request URL: %s", ctx.Request.URL)
    utils.DebugLog("-> Proxying to upstream URL: %s", oriURL.String())

    // Configure HTTP transport suitable for long-lived streaming
    transport := &http.Transport{
        Proxy: http.ProxyFromEnvironment,
        DialContext: (&net.Dialer{
            Timeout:   30 * time.Second,
            KeepAlive: 30 * time.Second,
        }).DialContext,
        ForceAttemptHTTP2:     false,
        MaxIdleConns:          100,
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:   10 * time.Second,
        ExpectContinueTimeout: 1 * time.Second,
    }

    // No global Timeout; let the stream run as long as the client stays connected
    client := &http.Client{Transport: transport}

    // Prepare the upstream request (bound to client context so it cancels if client disconnects)
    req, err := http.NewRequestWithContext(ctx.Request.Context(), "GET", oriURL.String(), nil)
    if err != nil {
        utils.ErrorLog("Failed to create request: %v", err)
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }

    // For VOD endpoints, some providers are extremely strict: use a whitelist header set
    p := oriURL.Path
    isVOD := isVODPath(p)

    if isVOD {
        req.Header = prepareVODHeaders(ctx)
    } else {
        // Non-VOD: copy and normalize minimally
        mergeHttpHeader(req.Header, ctx.Request.Header)
        req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
        req.Header.Del("Accept-Encoding")
        req.Header.Set("Accept-Encoding", "identity")
        if req.Header.Get("Accept") == "" { req.Header.Set("Accept", "*/*") }
        if req.Header.Get("Connection") == "" { req.Header.Set("Connection", "keep-alive") }
    }

    // Execute the upstream request
    resp, err := client.Do(req)
    if err != nil {
        utils.DebugLog("-> Upstream request error: %v", err)
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }
    defer resp.Body.Close()

    utils.DebugLog("-> Upstream response status: %d", resp.StatusCode)
    if resp.StatusCode == 461 {
        utils.DebugLog("Upstream returned 461 (often blocks HEAD/Range or unexpected headers). UA=%q, AE=%q", req.Header.Get("User-Agent"), req.Header.Get("Accept-Encoding"))
    }

    // Copy response headers and status code
    mergeHttpHeader(ctx.Writer.Header(), resp.Header)
    ctx.Status(resp.StatusCode)

    // Stream the response body to the client with flushes
    w := ctx.Writer
    buf := make([]byte, 64*1024)

    for {
        // Respect client cancellation
        select {
        case <-ctx.Request.Context().Done():
            utils.DebugLog("Client cancelled stream for URL: %s", ctx.Request.URL)
            return
        default:
        }

        n, rerr := resp.Body.Read(buf)
        if n > 0 {
            if _, werr := w.Write(buf[:n]); werr != nil {
                utils.DebugLog("Client write error: %v", werr)
                return
            }
            if f, ok := w.(http.Flusher); ok { f.Flush() }
        }
        if rerr != nil {
            if rerr != io.EOF {
                utils.DebugLog("Upstream read error: %v", rerr)
            }
            return
        }
    }
}
