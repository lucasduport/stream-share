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
	"os"
	"strings"
	"sync"
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

// maxResponseBytes caps how much of a provider response body is buffered in
// memory. Large-catalog endpoints (get_vod_streams at ~87 MB today, growing)
// need headroom; the cap exists only as a runaway-response guard, so it is set
// well above the largest known catalog and a hit is reported loudly rather
// than silently truncating the JSON (which would decode-fail downstream).
const maxResponseBytes = 512 * 1024 * 1024

// Client represents an Xtream API client
type Client struct {
	Username    string
	Password    string
	BaseURL     string
	UserAgent   string
	Client      *http.Client
	InsecureTLS bool
}

// bodyStallTimeout aborts a response whose body has received no bytes for this
// long. There is no overall deadline: large catalogues on a slow link
// legitimately take minutes while still making progress.
const bodyStallTimeout = 60 * time.Second

// retryBackoff is the pause before retry n (1-based) of a failed request, so
// a struggling provider is not hit with back-to-back retries.
func retryBackoff(n int) time.Duration {
	d := 250 * time.Millisecond << (n - 1)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

var (
	sharedClients   [2]*http.Client // [0] verified TLS, [1] XTREAM_INSECURE_TLS_ENABLED
	sharedClientsMu sync.Mutex
)

// sharedClient returns the process-wide API client, so every Xtream call reuses
// pooled keep-alive connections instead of paying a new TCP and TLS handshake
// to the provider (each Client used to carry its own transport).
//
// No global timeout: large-catalog endpoints (get_vod_streams ~87 MB and
// growing, get_series ~53 MB) can legitimately take minutes on a slow link.
// The transport's ResponseHeaderTimeout bounds the wait for headers and the
// body stall timeout (see Action) the rest. Compression is left to the
// transport, which asks for gzip and inflates it: catalogue JSON shrinks about
// tenfold on the wire, which dominates fetch time on a slow provider.
func sharedClient(insecureTLS bool) *http.Client {
	i := 0
	if insecureTLS {
		i = 1
	}
	sharedClientsMu.Lock()
	defer sharedClientsMu.Unlock()
	if sharedClients[i] == nil {
		tr := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
		}
		if insecureTLS {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- operator opt-in
		}
		sharedClients[i] = &http.Client{
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
	}
	return sharedClients[i]
}

// New creates a new Xtream client instance
func New(user, password, baseURL, userAgent string) (*Client, error) {
	_, err := url.Parse(baseURL)
	if err != nil {
		return nil, utils.PrintErrorAndReturn(fmt.Errorf("invalid base URL: %w", err))
	}
	insecureTLS := os.Getenv("XTREAM_INSECURE_TLS_ENABLED") == "true"
	return &Client{
		Username:    user,
		Password:    password,
		BaseURL:     baseURL,
		UserAgent:   utils.GetIPTVUserAgent(),
		Client:      sharedClient(insecureTLS),
		InsecureTLS: insecureTLS,
	}, nil
}

// Action executes Xtream API player_api actions using a raw HTTP call and returns parsed JSON or a fallback.
func (c *Client) Action(cfg *config.ProxyConfig, action string, q url.Values) (respBody interface{}, httpcode int, contentType string, err error) {
	return c.ActionContext(context.Background(), cfg, action, q)
}

// readBody reads one response body, aborting it (through cancel) when no
// bytes arrive for bodyStallTimeout.
func readBody(resp *http.Response, cancel context.CancelFunc) ([]byte, error) {
	stall := time.AfterFunc(bodyStallTimeout, cancel)
	defer stall.Stop()
	r := &stallReader{r: io.LimitReader(resp.Body, maxResponseBytes+1), timer: stall}
	var buf bytes.Buffer
	if resp.ContentLength > 0 && resp.ContentLength <= maxResponseBytes {
		buf.Grow(int(resp.ContentLength))
	}
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

// stallReader re-arms timer on every read that returns data, so the timer only
// fires when the body stalls for bodyStallTimeout.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(bodyStallTimeout)
	}
	return n, err
}

// ActionContext is Action bound to ctx: the call (retries included) stops when
// ctx ends, e.g. when the player that asked for it disconnects.
func (c *Client) ActionContext(ctx context.Context, cfg *config.ProxyConfig, action string, q url.Values) (respBody interface{}, httpcode int, contentType string, err error) {
	contentType = "application/json"
	utils.DebugLog("Processing Xtream action=%s", action)

	u, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/player_api.php")
	if err != nil {
		return nil, http.StatusInternalServerError, contentType, utils.PrintErrorAndReturn(err)
	}
	params := url.Values{}
	params.Set("username", c.Username)
	params.Set("password", c.Password)
	if strings.TrimSpace(action) != "" {
		params.Set("action", action)
	}
	for k, vs := range q {
		if k == "username" || k == "password" || k == "action" {
			continue
		}
		for _, v := range vs {
			if v != "" {
				params.Add(k, v)
			}
		}
	}
	u.RawQuery = params.Encode()
	utils.DebugLog("Xtream raw request: %s", u.String())

	client := c.Client

	var lastErr error
	var resp *http.Response
	var b []byte

	for i := 0; i < 5; i++ {
		if i > 0 {
			select {
			case <-time.After(retryBackoff(i)):
			case <-ctx.Done():
				lastErr = ctx.Err()
			}
			if ctx.Err() != nil {
				break
			}
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(attemptCtx, "GET", u.String(), nil)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		req.Header.Set("Accept", "application/json, text/plain, */*")
		resp, err = client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			// Read with a large cap; if the provider sends more, fail loudly
			// instead of silently truncating the JSON body.
			b, err = readBody(resp, cancel)
			_ = resp.Body.Close()
			cancel()
			if err != nil {
				if ctx.Err() == nil && attemptCtx.Err() != nil {
					err = fmt.Errorf("response body stalled for %v: %w", bodyStallTimeout, err)
				}
				lastErr = err
				b = nil
				continue
			}
			if len(b) > maxResponseBytes {
				utils.WarnLog("Xtream action=%s response exceeded %d MB cap; refusing truncated body", action, maxResponseBytes/(1024*1024))
				lastErr = fmt.Errorf("response exceeds %d MB cap", maxResponseBytes/(1024*1024))
				b = nil
				continue
			}
			break
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		cancel()
		lastErr = fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	if resp == nil || resp.StatusCode != http.StatusOK || len(b) == 0 {
		utils.DebugLog("Request failed, last error: %v", lastErr)
		return fallbackForAction(action), http.StatusBadGateway, contentType, lastErr
	}

	trim := bytes.TrimSpace(b)
	if len(trim) == 0 || bytes.Equal(trim, []byte("null")) || (len(trim) > 0 && trim[0] == '<') {
		return fallbackForAction(action), http.StatusOK, contentType, nil
	}
	if bytes.Equal(trim, []byte("{}")) {
		return map[string]interface{}{}, http.StatusOK, contentType, nil
	}
	if bytes.Equal(trim, []byte("[]")) {
		return []interface{}{}, http.StatusOK, contentType, nil
	}

	var result interface{}
	decoder := json.NewDecoder(bytes.NewReader(trim))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		utils.DebugLog("JSON decoding failed: %v", err)
		return fallbackForAction(action), http.StatusOK, contentType, err
	}
	return result, http.StatusOK, contentType, nil
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
