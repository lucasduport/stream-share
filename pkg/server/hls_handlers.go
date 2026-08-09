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
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// HLS helpers and handlers
var hlsChannelsRedirectURL map[string]url.URL = map[string]url.URL{}
var hlsChannelsRedirectURLLock = sync.RWMutex{}

// proxyHlsChunk fetches an HLS chunk from upstream. When the upstream responds
// with 302, it follows the redirect, caches the resolved URL keyed by stream id,
// fetches the canonical body, and rewrites upstream credentials to proxy
// credentials so client-side URLs point back at this proxy. client controls
// redirect policy for both the initial and follow-up requests.
func (c *Config) proxyHlsChunk(ctx *gin.Context, client *http.Client, upstream string) {
	req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", upstream, nil)
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

	upstream := fmt.Sprintf("%s://%s/hls/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), ctx.Param("chunk"))
	c.proxyHlsChunk(ctx, http.DefaultClient, upstream)
}

func (c *Config) hlsXtreamStream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("HLS stream request with URL: %s", oriURL.String())
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	c.proxyHlsChunk(ctx, client, oriURL.String())
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
