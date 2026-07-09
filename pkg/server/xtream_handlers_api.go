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
    utils.DebugLog("Xtream backend request: user=%s, baseURL=%s", c.XtreamUser.String(), c.XtreamBaseURL)

    upstreamQ := url.Values{}
    upstreamQ.Set("username", c.XtreamUser.String())
    upstreamQ.Set("password", c.XtreamPassword.String())
    for k, vals := range ctx.Request.URL.Query() {
        if k == "username" || k == "password" {
            continue
        }
        for _, v := range vals {
            upstreamQ.Add(k, v)
        }
    }
    rawURL := c.XtreamBaseURL + "/get.php?" + upstreamQ.Encode()

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
            _ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(fmt.Errorf("empty playlist returned by Xtream backend")))
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
        if c.HTTPS {
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
        if config.CacheFolder != "" && utils.IsDebugLogEnabled() {
            readableJSON, _ := json.Marshal(loginResp)
            filename := fmt.Sprintf("login_%s.json", time.Now().Format("20060102_150405"))
            utils.WriteResponseToFile(filename, readableJSON, "application/json")
        }
        ctx.JSON(http.StatusOK, loginResp)
        return
    }

    client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
    if err != nil {
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }

    resp, httpcode, contentType, err := client.Action(c.ProxyConfig, action, q)
    if err != nil {
        _ = ctx.AbortWithError(httpcode, utils.PrintErrorAndReturn(err))
        return
    }

    if contentType == "application/json" {
        if s, ok := resp.(string); ok && strings.TrimSpace(s) == "" {
            _ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(fmt.Errorf("empty JSON response from Xtream backend for action: %s", action)))
            return
        }
        if b, ok := resp.([]byte); ok && len(bytes.TrimSpace(b)) == 0 {
            _ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(fmt.Errorf("empty JSON response from Xtream backend for action: %s", action)))
            return
        }
    }

    utils.InfoLog("Action\t%s requested by %s", action, ctx.ClientIP())
    processedResp := xtreamapi.ProcessResponse(resp)
    if action == "get_live_streams" {
        c.harvestChannelNames(processedResp)
        if c.catchupManager != nil && c.catchupManager.IsEnabled() {
            processedResp = c.injectCatchupFlags(processedResp)
        }
    }

    if config.CacheFolder != "" && utils.IsDebugLogEnabled() {
        readableJSON, _ := json.Marshal(processedResp)
        filename := fmt.Sprintf("%s_%s.json", action, time.Now().Format("20060102_150405"))
        utils.WriteResponseToFile(filename, readableJSON, contentType)
    }

    ctx.JSON(http.StatusOK, processedResp)
}

func (c *Config) xtreamPlayerAPIGET(ctx *gin.Context) { c.xtreamPlayerAPI(ctx, ctx.Request.URL.Query()) }

// harvestChannelNames extracts stream_id → name pairs and EPG channel IDs from a get_live_streams
// response and refreshes the API channel name index used by /status and logs.
// It returns the number of channel names indexed.
func (c *Config) harvestChannelNames(resp interface{}) int {
    streams, ok := resp.([]interface{})
    if !ok {
        return 0
    }
    names := make(map[string]string, len(streams))
    epgIDs := make(map[string]string, len(streams))
    for _, item := range streams {
        m, ok := item.(map[string]interface{})
        if !ok {
            continue
        }
        id := normalizeStreamID(fmt.Sprintf("%v", m["stream_id"]))
        name := strings.TrimSpace(fmt.Sprintf("%v", m["name"]))
        if id != "" && name != "" {
            names[id] = name
        }
        if epgID, _ := m["epg_channel_id"].(string); strings.TrimSpace(epgID) != "" {
            epgIDs[id] = strings.TrimSpace(epgID)
        }
    }
    c.updateAPIChannelIndex(names, epgIDs)
    return len(names)
}

// warmChannelNameIndex fetches get_live_streams once so channel names can be
// resolved even before a player requests the list through the proxy — e.g. right
// after a container restart, when a player (TiviMate in Xtream API mode) serves
// the channel list from its own cache and never re-fetches it.
func (c *Config) warmChannelNameIndex() {
    client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, "")
    if err != nil {
        utils.WarnLog("Channel name warm-up: failed to create Xtream client: %v", err)
        return
    }
    resp, _, _, err := client.Action(c.ProxyConfig, "get_live_streams", nil)
    if err != nil {
        utils.WarnLog("Channel name warm-up: get_live_streams failed: %v", err)
        return
    }
    if n := c.harvestChannelNames(xtreamapi.ProcessResponse(resp)); n > 0 {
        utils.InfoLog("Channel name warm-up: indexed %d channel names from get_live_streams", n)
    } else {
        utils.WarnLog("Channel name warm-up: get_live_streams returned no usable channel names")
    }
}

func (c *Config) injectCatchupFlags(resp interface{}) interface{} {
    streams, ok := resp.([]interface{})
    if !ok {
        return resp
    }
    upstreamCatchup := make(map[string]bool, len(streams))
    for _, item := range streams {
        m, ok := item.(map[string]interface{})
        if !ok {
            continue
        }
        streamID := fmt.Sprintf("%v", m["stream_id"])
        if v, exists := m["tv_archive"]; exists {
            switch vv := v.(type) {
            case json.Number:
                n, _ := vv.Int64()
                upstreamCatchup[streamID] = n == 1
            case float64:
                upstreamCatchup[streamID] = vv == 1
            }
        }
        m["tv_archive"] = 1
        m["tv_archive_duration"] = c.catchupManager.AdvertisedHours()
    }
    c.catchupManager.SetUpstreamCatchup(upstreamCatchup)
    return streams
}

func (c *Config) xtreamPlayerAPIPOST(ctx *gin.Context) {
    contents, err := io.ReadAll(ctx.Request.Body)
    if err != nil {
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }
    q, err := url.ParseQuery(string(contents))
    if err != nil {
        _ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
        return
    }
    c.xtreamPlayerAPI(ctx, q)
}
