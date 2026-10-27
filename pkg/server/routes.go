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
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// routes registers the primary API surface under the configurable custom endpoint.
// It decides between Xtream-backed endpoints and proxified M3U based on config.
func (c *Config) routes(r *gin.RouterGroup) {
	r = r.Group(c.CustomEndpoint)

	// Xtream service endpoints
	if c.XtreamBaseURL != "" {
		c.xtreamRoutes(r)
		if c.xtreamServesM3U() {
			r.GET("/"+c.M3UFileName, c.authenticate, c.xtreamGetAuto)
			// XXX Private need: for external Android app
			r.POST("/"+c.M3UFileName, c.authenticate, c.xtreamGetAuto)

			return
		}
	}

	c.m3uRoutes(r)
}

// xtreamServesM3U reports whether M3U_URL is the Xtream account's own get.php,
// in which case the playlist is answered by the Xtream get.php proxy (streamed
// and cached on disk) and the M3U_URL playlist is never served as such — so it
// must not be downloaded and parsed into memory at boot either.
func (c *Config) xtreamServesM3U() bool {
	if c.XtreamBaseURL == "" || c.RemoteURL == nil {
		return false
	}
	q := c.RemoteURL.Query()
	return strings.Contains(c.XtreamBaseURL, c.RemoteURL.Host) &&
		c.XtreamUser.String() == q.Get("username") &&
		c.XtreamPassword.String() == q.Get("password")
}

// xtreamRoutes registers endpoints backed by an Xtream provider.
func (c *Config) xtreamRoutes(r *gin.RouterGroup) {
	getphp := gin.HandlerFunc(c.xtreamGet)
	if c.XtreamGenerateApiGet {
		getphp = c.xtreamApiGet
	}
	r.GET("/get.php", c.authenticate, getphp)
	r.POST("/get.php", c.authenticate, getphp)
	r.GET("/apiget", c.authenticate, c.xtreamApiGet)
	r.GET("/player_api.php", c.authenticate, c.xtreamPlayerAPIGET)
	r.POST("/player_api.php", c.appAuthenticate, c.xtreamPlayerAPIPOST)
	r.GET("/xmltv.php", c.authenticate, c.xtreamXMLTV)
	r.GET(fmt.Sprintf("/%s/%s/:id", c.XtreamUser.String(), c.XtreamPassword.String()), c.xtreamStreamHandler)
	r.GET(fmt.Sprintf("/live/%s/%s/:id", c.XtreamUser.String(), c.XtreamPassword.String()), c.xtreamStreamLive)
	r.GET(fmt.Sprintf("/timeshift/%s/%s/:duration/:start/:id", c.XtreamUser.String(), c.XtreamPassword.String()), c.xtreamStreamTimeshift)
	r.GET(fmt.Sprintf("/movie/%s/%s/:id", c.XtreamUser.String(), c.XtreamPassword.String()), c.xtreamStreamMovie)
	r.GET(fmt.Sprintf("/series/%s/%s/:id", c.XtreamUser.String(), c.XtreamPassword.String()), c.xtreamStreamSeries)
	r.GET(fmt.Sprintf("/hlsr/:token/%s/%s/:channel/:hash/:chunk", c.XtreamUser.String(), c.XtreamPassword.String()), c.xtreamHlsrStream)
	r.GET("/hls/:token/:chunk", c.xtreamHlsStream)
	r.GET("/play/:token/:type", c.xtreamStreamPlay)
}

// m3uRoutes exposes a proxified, credential-rewritten M3U and its track endpoints.
func (c *Config) m3uRoutes(r *gin.RouterGroup) {
	r.GET("/"+c.M3UFileName, c.authenticate, c.getM3U)
	// XXX Private need: for external Android app
	r.POST("/"+c.M3UFileName, c.authenticate, c.getM3U)

	if len(c.playlist.Tracks) == 0 {
		return
	}
	// One route for every track, dispatched by index. Registering a route per
	// track cost an allocation and a radix-tree node each, which on a
	// million-track playlist meant minutes of boot and hundreds of MB.
	r.GET(fmt.Sprintf("/%s/%s/%s/:track/*file", c.endpointAntiColision, c.XtreamUser.String(), c.XtreamPassword.String()), c.trackProxy)
}

// trackProxy serves /<anti-collision>/<user>/<pass>/<index>/<file> for the
// proxified M3U: the index selects the track, and the file must be the
// track's own file name (or, for an HLS track, any single segment name).
func (c *Config) trackProxy(ctx *gin.Context) {
	i, err := strconv.Atoi(ctx.Param("track"))
	if err != nil || i < 0 || i >= len(c.playlist.Tracks) {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	track := &c.playlist.Tracks[i]
	file := strings.TrimPrefix(ctx.Param("file"), "/")
	trackConfig := &Config{ProxyConfig: c.ProxyConfig, track: track}
	if strings.HasSuffix(track.URI, ".m3u8") {
		if file == "" || strings.Contains(file, "/") {
			ctx.AbortWithStatus(http.StatusNotFound)
			return
		}
		ctx.Params = append(ctx.Params, gin.Param{Key: "id", Value: file})
		trackConfig.m3u8ReverseProxy(ctx)
		return
	}
	if file != path.Base(track.URI) {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	trackConfig.reverseProxy(ctx)
}
