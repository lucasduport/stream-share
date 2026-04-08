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

package xtream

import (
    "bytes"
    "context"
    "crypto/tls"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"

    "github.com/lucasduport/stream-share/pkg/config"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// API endpoint constants
const (
    getLiveCategories   = "get_live_categories"
    getLiveStreams      = "get_live_streams"
    getVodCategories    = "get_vod_categories"
    getVodStreams       = "get_vod_streams"
    getVodInfo          = "get_vod_info"
    getSeriesCategories = "get_series_categories"
    getSeries           = "get_series"
    getSerieInfo        = "get_series_info"
    getShortEPG         = "get_short_epg"
    getSimpleDataTable  = "get_simple_data_table"
)

// Client represents an Xtream API client
type Client struct {
    Username  string
    Password  string
    BaseURL   string
    UserAgent string
    Client    *http.Client
}

// New creates a new Xtream client instance
func New(user, password, baseURL, userAgent string) (*Client, error) {
    _, err := url.Parse(baseURL)
    if err != nil {
        return nil, utils.PrintErrorAndReturn(fmt.Errorf("invalid base URL: %w", err))
    }
    httpClient := &http.Client{
        Timeout: 10 * time.Second,
        CheckRedirect: func(req *http.Request, via []*http.Request) error {
            if len(via) >= 10 { return http.ErrUseLastResponse }
            return nil
        },
    }
    return &Client{
        Username:  user,
        Password:  password,
        BaseURL:   baseURL,
        UserAgent: utils.GetIPTVUserAgent(),
        Client:    httpClient,
    }, nil
}

// Action executes Xtream API player_api actions using a raw HTTP call and returns parsed JSON or a fallback.
func (c *Client) Action(cfg *config.ProxyConfig, action string, q url.Values) (respBody interface{}, httpcode int, contentType string, err error) {
    contentType = "application/json"
    utils.DebugLog("Processing Xtream action=%s", action)

    u, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/player_api.php")
    if err != nil {
        return nil, http.StatusInternalServerError, contentType, utils.PrintErrorAndReturn(err)
    }
    params := url.Values{}
    params.Set("username", c.Username)
    params.Set("password", c.Password)
    if strings.TrimSpace(action) != "" { params.Set("action", action) }
    for k, vs := range q {
        if k == "username" || k == "password" || k == "action" { continue }
        for _, v := range vs { if v != "" { params.Add(k, v) } }
    }
    u.RawQuery = params.Encode()
    utils.DebugLog("Xtream raw request: %s", u.String())

    client := &http.Client{ Timeout: 10 * time.Second, Transport: &http.Transport{ TLSClientConfig: &tls.Config{InsecureSkipVerify: true} } }

    var lastErr error
    var resp *http.Response
    var b []byte

    for i := 0; i < 5; i++ {
        req, err := http.NewRequest("GET", u.String(), nil)
        if err != nil { lastErr = err; continue }
        req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
        req.Header.Set("Accept", "application/json, text/plain, */*")
        resp, err = client.Do(req)
        if err != nil { lastErr = err; continue }
        defer resp.Body.Close()
        if resp.StatusCode == http.StatusOK {
            b, err = io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
            if err != nil { lastErr = err; continue }
            break
        } else {
            lastErr = fmt.Errorf("HTTP status %d", resp.StatusCode)
        }
    }

    if resp == nil || resp.StatusCode != http.StatusOK || len(b) == 0 {
        utils.DebugLog("Request failed, last error: %v", lastErr)
        return fallbackForAction(action), http.StatusBadGateway, contentType, lastErr
    }

    trim := bytes.TrimSpace(b)
    if len(trim) == 0 || bytes.Equal(trim, []byte("null")) || (len(trim) > 0 && trim[0] == '<') {
        return fallbackForAction(action), http.StatusOK, contentType, nil
    }
    if bytes.Equal(trim, []byte("{}")) { return map[string]interface{}{}, http.StatusOK, contentType, nil }
    if bytes.Equal(trim, []byte("[]")) { return []interface{}{}, http.StatusOK, contentType, nil }

    var result interface{}
    decoder := json.NewDecoder(bytes.NewReader(trim))
    decoder.UseNumber()
    if err := decoder.Decode(&result); err != nil {
        utils.DebugLog("JSON decoding failed: %v", err)
        return fallbackForAction(action), http.StatusOK, contentType, err
    }
    return result, http.StatusOK, contentType, nil
}

// GetXMLTV retrieves the EPG data in XMLTV format
func (c *Client) GetXMLTV() ([]byte, error) {
    u, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/xmltv.php")
    if err != nil { return nil, utils.PrintErrorAndReturn(err) }
    params := url.Values{}
    params.Set("username", c.Username)
    params.Set("password", c.Password)
    u.RawQuery = params.Encode()
    utils.DebugLog("XMLTV request: %s", u.String())

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
    if err != nil { return nil, utils.PrintErrorAndReturn(err) }
    req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
    req.Header.Set("Accept", "application/xml, text/xml")
    resp, err := c.Client.Do(req)
    if err != nil { return nil, utils.PrintErrorAndReturn(err) }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK { return nil, utils.PrintErrorAndReturn(fmt.Errorf("unexpected status code: %d", resp.StatusCode)) }
    limitedReader := &io.LimitedReader{R: resp.Body, N: 50 * 1024 * 1024}
    xmlData, err := io.ReadAll(limitedReader)
    if err != nil { return nil, utils.PrintErrorAndReturn(fmt.Errorf("failed to read XMLTV data: %w", err)) }
    return xmlData, nil
}

func createEmergencyCategoryData() []map[string]interface{} {
    utils.DebugLog("Creating emergency fallback category data")
    return []map[string]interface{}{{"category_id": "1", "category_name": "Default Category", "parent_id": "0"}}
}

// fallbackForAction returns a sensible empty structure per action
func fallbackForAction(action string) interface{} {
    switch action {
    case getLiveCategories, getVodCategories, getSeriesCategories:
        return createEmergencyCategoryData()
    case getLiveStreams, getVodStreams, getSeries:
        return []interface{}{}
    case getVodInfo, getSerieInfo, getShortEPG, getSimpleDataTable:
        return map[string]interface{}{}
    default:
        return map[string]interface{}{}
    }
}
