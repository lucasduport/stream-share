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
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
    "github.com/lucasduport/stream-share/pkg/config"
    "github.com/lucasduport/stream-share/pkg/utils"
    xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
    xproc "github.com/lucasduport/stream-share/pkg/xtream"
)

// xtreamGetAuto forwards get.php with non-credential query params preserved.
func (c *Config) xtreamGetAuto(ctx *gin.Context) {
    newQuery := ctx.Request.URL.Query()
    q := c.RemoteURL.Query()
    for k, v := range q {
        if k == "username" || k == "password" {
            continue
        }
        newQuery.Add(k, strings.Join(v, ","))
    }
    ctx.Request.URL.RawQuery = newQuery.Encode()
    c.xtreamGet(ctx)
}

// xtreamGet proxies get.php, caching the M3U on disk and guarding empty results.
func (c *Config) xtreamGet(ctx *gin.Context) {
    utils.DebugLog("Xtream backend request using Xtream credentials: user=%s, password=%s, baseURL=%s", c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL)
    rawURL := fmt.Sprintf("%s/get.php?username=%s&password=%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword)

    q := ctx.Request.URL.Query()
    for k, v := range q {
        if k == "username" || k == "password" {
            continue
        }
        rawURL = fmt.Sprintf("%s&%s=%s", rawURL, k, strings.Join(v, ","))
    }

    m3uURL, err := url.Parse(rawURL)
    if err != nil {
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }

    xtreamM3uCacheLock.RLock()
    meta, ok := xtreamM3uCache[m3uURL.String()]
    d := time.Since(meta.Time)
    if !ok || d.Hours() >= float64(c.M3UCacheExpiration) {
        utils.InfoLog("xtream cache m3u file refresh requested by %s", ctx.ClientIP())
        xtreamM3uCacheLock.RUnlock()
        playlist, err := m3u.Parse(m3uURL.String())
        if err != nil {
            _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
            return
        }
        if len(playlist.Tracks) == 0 {
            _ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(fmt.Errorf("Xtream backend returned empty playlist")))
            return
        }
        if err := c.cacheXtreamM3u(&playlist, m3uURL.String()); err != nil {
            _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
            return
        }
    } else {
        xtreamM3uCacheLock.RUnlock()
    }

    ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
    xtreamM3uCacheLock.RLock()
    path := xtreamM3uCache[m3uURL.String()].string
    xtreamM3uCacheLock.RUnlock()
    ctx.Header("Content-Type", "application/octet-stream")
    ctx.File(path)
}

// xtreamPlayerAPI proxies player_api actions with a local login path to avoid brittle unmarshaling differences.
func (c *Config) xtreamPlayerAPI(ctx *gin.Context, q url.Values) {
    var action string
    if len(q["action"]) > 0 {
        action = q["action"][0]
    }

    if strings.TrimSpace(action) == "" {
        protocol := "http"
        if c.ProxyConfig.HTTPS {
            protocol = "https"
        }
        now := time.Now()
        nowUnix := strconv.FormatInt(now.Unix(), 10)
        expDate := strconv.FormatInt(now.Add(365*24*time.Hour).Unix(), 10)

        loginResp := map[string]interface{}{
            "user_info": map[string]interface{}{
                "username":               c.User.String(),
                "password":               c.Password.String(),
                "message":                "",
                "auth":                   "1",
                "status":                 "Active",
                "exp_date":               expDate,
                "is_trial":               "0",
                "active_cons":            "0",
                "created_at":             nowUnix,
                "max_connections":        "1",
                "allowed_output_formats": []string{"m3u8", "ts"},
            },
            "server_info": map[string]interface{}{
                "url":             fmt.Sprintf("%s://%s", protocol, c.HostConfig.Hostname),
                "port":            strconv.Itoa(c.AdvertisedPort),
                "https_port":      strconv.Itoa(c.AdvertisedPort),
                "server_protocol": protocol,
                "rtmp_port":       strconv.Itoa(c.AdvertisedPort),
                "timezone":        "UTC",
                "timestamp_now":   nowUnix,
                "time_now":        now.UTC().Format("2006-01-02 15:04:05"),
            },
        }

        utils.InfoLog("Action\tlogin (local) requested by %s", ctx.ClientIP())
        if config.CacheFolder != "" {
            readableJSON, _ := json.Marshal(loginResp)
            filename := fmt.Sprintf("login_%s.json", time.Now().Format("20060102_150405"))
            utils.WriteResponseToFile(filename, readableJSON, "application/json")
        }
        ctx.JSON(http.StatusOK, loginResp)
        return
    }

    client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }

    resp, httpcode, contentType, err := client.Action(c.ProxyConfig, action, q)
    if err != nil {
        ctx.AbortWithError(httpcode, utils.PrintErrorAndReturn(err))
        return
    }

    if contentType == "application/json" {
        if s, ok := resp.(string); ok && strings.TrimSpace(s) == "" {
            ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(fmt.Errorf("Xtream backend returned empty JSON response for action: %s", action)))
            return
        }
        if b, ok := resp.([]byte); ok && len(bytes.TrimSpace(b)) == 0 {
            ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(fmt.Errorf("Xtream backend returned empty JSON response for action: %s", action)))
            return
        }
    }

    utils.InfoLog("Action\t%s requested by %s", action, ctx.ClientIP())
    processedResp := xproc.ProcessResponse(resp)

    if config.CacheFolder != "" {
        readableJSON, _ := json.Marshal(processedResp)
        filename := fmt.Sprintf("%s_%s.json", action, time.Now().Format("20060102_150405"))
        utils.WriteResponseToFile(filename, readableJSON, contentType)
    }

    ctx.JSON(http.StatusOK, processedResp)
}

func (c *Config) xtreamPlayerAPIGET(ctx *gin.Context) { c.xtreamPlayerAPI(ctx, ctx.Request.URL.Query()) }

func (c *Config) xtreamPlayerAPIPOST(ctx *gin.Context) {
    contents, err := io.ReadAll(ctx.Request.Body)
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }
    q, err := url.ParseQuery(string(contents))
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }
    c.xtreamPlayerAPI(ctx, q)
}
