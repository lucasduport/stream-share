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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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

// getPHPFailBackoff is how long a failed get.php refresh is remembered before
// another may start while a previous copy is still being served: on slow
// providers each attempt is minutes of bandwidth.
const getPHPFailBackoff = 5 * time.Minute

// getPHPFetch is one in-flight get.php refresh; done closes when it ends.
type getPHPFetch struct {
	done chan struct{}
	err  error
}

var (
	getPHPMu       sync.Mutex
	getPHPInFlight = map[string]*getPHPFetch{} // cache key -> running refresh
	getPHPFailedAt = map[string]time.Time{}    // cache key -> last failed refresh
)

// getPHPCachePath is where the rewritten get.php answer for cacheKey lives. It
// is stable across restarts (unlike a random temp file), so a restart serves
// the last playlist instead of making the first player wait for a full
// download.
func getPHPCachePath(cacheKey string) string {
	sum := sha256.Sum256([]byte(cacheKey))
	return filepath.Join(utils.CacheRoot(), "m3u", "get-"+hex.EncodeToString(sum[:8])+".m3u")
}

// xtreamM3uCached returns the cached get.php file for cacheKey and whether it
// is younger than the configured expiration. A file left by a previous run is
// adopted on first use. path is empty when nothing is cached.
func (c *Config) xtreamM3uCached(cacheKey string) (path string, fresh bool) {
	xtreamM3uCacheLock.RLock()
	meta, ok := xtreamM3uCache[cacheKey]
	xtreamM3uCacheLock.RUnlock()
	if !ok {
		p := getPHPCachePath(cacheKey)
		info, err := os.Stat(p)
		if err != nil {
			return "", false
		}
		meta = cacheMeta{p, info.ModTime()}
		xtreamM3uCacheLock.Lock()
		if cur, exists := xtreamM3uCache[cacheKey]; exists {
			meta = cur
		} else {
			xtreamM3uCache[cacheKey] = meta
		}
		xtreamM3uCacheLock.Unlock()
	}
	return meta.string, time.Since(meta.Time).Hours() < float64(c.M3UCacheExpiration)
}

// startGetPHPRefresh starts a background get.php download for cacheKey unless
// one is already running, in which case that one is returned, so concurrent
// players share a single provider download. With hasCopy set (a previous
// playlist is being served meanwhile) it returns nil while a recent failure is
// in backoff.
func (c *Config) startGetPHPRefresh(cacheKey, rawURL string, hasCopy bool) *getPHPFetch {
	getPHPMu.Lock()
	defer getPHPMu.Unlock()
	if f := getPHPInFlight[cacheKey]; f != nil {
		return f
	}
	if hasCopy && time.Since(getPHPFailedAt[cacheKey]) < getPHPFailBackoff {
		return nil
	}
	f := &getPHPFetch{done: make(chan struct{})}
	getPHPInFlight[cacheKey] = f
	go func() {
		dest := getPHPCachePath(cacheKey)
		tracks, err := c.fetchAndRewriteXtreamM3U(rawURL, dest)
		if err == nil && tracks == 0 {
			err = fmt.Errorf("empty playlist returned by Xtream backend")
		}
		if err == nil {
			xtreamM3uCacheLock.Lock()
			xtreamM3uCache[cacheKey] = cacheMeta{dest, time.Now()}
			xtreamM3uCacheLock.Unlock()
			utils.DebugLog("Cached stream-rewritten Xtream M3U at %s (%d tracks)", dest, tracks)
			c.shareGetPHPAsVODM3U(rawURL, dest)
		} else {
			utils.WarnLog("get.php refresh failed: %v", err)
		}
		getPHPMu.Lock()
		delete(getPHPInFlight, cacheKey)
		if err != nil {
			getPHPFailedAt[cacheKey] = time.Now()
		} else {
			delete(getPHPFailedAt, cacheKey)
		}
		getPHPMu.Unlock()
		f.err = err
		close(f.done)
	}()
	return f
}

// xtreamGet proxies get.php, caching the M3U on disk and guarding empty results.
// The provider M3U is stream-rewritten to disk with line-level credential
// replacement (constant memory), instead of being fully parsed into RAM via
// m3u.Parse. On a 424 MB / 1.45M-track catalog this keeps peak RSS well under
// 500 MB and removes the multi-minute parse from the first-fetch path.
//
// A cached playlist is always served at once, even past its expiration: the
// refresh runs in the background (one per playlist variant) and replaces the
// file atomically when complete. Only a player with nothing cached at all
// waits for the download, and it stops waiting when it disconnects.
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
	cacheKey := m3uURL.String()

	path, fresh := c.xtreamM3uCached(cacheKey)
	if !fresh {
		f := c.startGetPHPRefresh(cacheKey, rawURL, path != "")
		if path != "" {
			if f != nil {
				utils.DebugLog("get.php: serving the cached playlist while it refreshes")
			}
		} else {
			utils.InfoLog("xtream cache m3u file refresh requested by %s", ctx.ClientIP())
			select {
			case <-f.done:
			case <-ctx.Request.Context().Done():
				ctx.Abort() // client went away; the download carries on for the next one
				return
			}
			if f.err != nil {
				_ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(f.err))
				return
			}
			path, _ = c.xtreamM3uCached(cacheKey)
		}
	}

	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	serveFileCompressed(ctx, path, "application/octet-stream")
}

// xtreamPlayerAPI proxies player_api actions with a local login path to avoid brittle unmarshaling differences.
func (c *Config) xtreamPlayerAPI(ctx *gin.Context, q url.Values) {
	var action string
	if len(q["action"]) > 0 {
		action = q["action"][0]
	}

	if strings.TrimSpace(action) == "" {
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
				"url":             fmt.Sprintf("%s://%s", c.Scheme(), c.HostConfig.Hostname),
				"port":            strconv.Itoa(c.AdvertisedHTTPPort()),
				"https_port":      strconv.Itoa(c.AdvertisedHTTPPort()),
				"server_protocol": c.Scheme(),
				"rtmp_port":       strconv.Itoa(c.AdvertisedHTTPPort()),
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

	if key, ok := c.playerAPICatalogKey(action, q); ok {
		c.servePlayerAPICatalog(ctx, action, key, q)
		return
	}

	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}

	// Bound to the player's request: a player that gives up stops the call
	// (and its retries) instead of leaving it running against the provider.
	resp, httpcode, contentType, err := client.ActionContext(ctx.Request.Context(), c.ProxyConfig, action, q)
	if err != nil {
		if ctx.Request.Context().Err() != nil {
			ctx.Abort()
			return
		}
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
	if action == "get_series_info" {
		// Players browse a series right before playing an episode: keep its
		// episode titles so the playback shows up named in /status and history.
		if info, ok := resp.(map[string]interface{}); ok {
			c.harvestSeriesEpisodeTitles("", info)
		}
		// Merge duplicate copies after harvesting, which wants every id.
		processedResp = c.seriesEpisodeDedup(processedResp)
	}

	if config.CacheFolder != "" && utils.IsDebugLogEnabled() {
		readableJSON, _ := json.Marshal(processedResp)
		filename := fmt.Sprintf("%s_%s.json", action, time.Now().Format("20060102_150405"))
		utils.WriteResponseToFile(filename, readableJSON, contentType)
	}

	ctx.JSON(http.StatusOK, processedResp)
}

// playerAPICatalogPrefixes maps the catalogue list actions served from the
// shared catalog cache to their cache-key prefix. The stream lists share their
// keys with the /watch search and the channel-name warm-up, so players and
// searches reuse a single provider download.
var playerAPICatalogPrefixes = map[string]string{
	"get_live_streams":      "live_streams",
	"get_vod_streams":       "vod_streams",
	"get_series":            "series",
	"get_live_categories":   "live_categories",
	"get_vod_categories":    "vod_categories",
	"get_series_categories": "series_categories",
}

// catalogKey is the catalog-cache key of action for this provider account,
// with an optional category_id narrowing the list.
func (c *Config) catalogKey(action, categoryID string) string {
	key := playerAPICatalogPrefixes[action] + ":" + c.XtreamBaseURL + ":" + c.XtreamUser.String()
	if categoryID != "" {
		key += ":category_id=" + categoryID
	}
	return key
}

// playerAPICatalogKey reports whether a player_api request can be served from
// the catalog cache, and under which key. Only the catalogue list actions
// qualify, with no parameter other than category_id: anything else is passed
// through uncached rather than letting arbitrary query strings grow the cache.
func (c *Config) playerAPICatalogKey(action string, q url.Values) (string, bool) {
	if _, ok := playerAPICatalogPrefixes[action]; !ok {
		return "", false
	}
	categoryID := ""
	for k, vs := range q {
		switch k {
		case "username", "password", "action":
		case "category_id":
			if len(vs) > 1 {
				return "", false
			}
			categoryID = strings.TrimSpace(vs[0])
		default:
			return "", false
		}
	}
	return c.catalogKey(action, categoryID), true
}

// catalogFetcher returns a fetch function for the catalog cache that runs
// action against the provider. A transport failure, or a body the provider
// sent but that is empty or unusable, comes back as a *catalogResultError so
// it is never cached and a previous good answer can be served instead.
func (c *Config) catalogFetcher(action string, q url.Values) func() (interface{}, error) {
	return func() (interface{}, error) {
		client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, "")
		if err != nil {
			return nil, err
		}
		resp, httpcode, contentType, err := client.Action(c.ProxyConfig, action, q)
		if err != nil {
			return nil, &catalogResultError{data: resp, code: httpcode,
				err: fmt.Errorf("%s failed (HTTP %d, CT=%s): %w", action, httpcode, contentType, err)}
		}
		if q.Get("category_id") == "" && !usableCatalog(resp) {
			// A whole-catalogue list never legitimately comes back empty;
			// that, or the client's emergency category stub, means the
			// provider sent nothing usable.
			return nil, &catalogResultError{data: resp, code: httpcode}
		}
		return resp, nil
	}
}

// usableCatalog reports whether a decoded provider list holds real entries:
// not empty and not the emergency fallback the Xtream client substitutes for
// an unusable category answer.
func usableCatalog(resp interface{}) bool {
	switch v := resp.(type) {
	case []interface{}:
		return len(v) > 0
	case []map[string]interface{}:
		return false // xtream client's emergency category data
	case nil:
		return false
	default:
		return true
	}
}

// servePlayerAPICatalog answers a catalogue list action from the catalog
// cache: fresh when possible, otherwise the provider's last good answer while a
// background refresh runs. The JSON sent to players is encoded once per cached
// catalogue, not per request.
func (c *Config) servePlayerAPICatalog(ctx *gin.Context, action, key string, q url.Values) {
	utils.InfoLog("Action\t%s requested by %s", action, ctx.ClientIP())
	e, stale, err := fetchCatalogEntry(key, false, c.catalogFetcher(action, q))
	if err != nil {
		var re *catalogResultError
		if !errors.As(err, &re) {
			_ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(err))
			return
		}
		if re.err != nil {
			code := re.code
			if code < 400 {
				code = http.StatusBadGateway
			}
			_ = ctx.AbortWithError(code, utils.PrintErrorAndReturn(re.err))
			return
		}
		// Nothing cached and the provider sent nothing usable: pass on what
		// it sent (empty list, emergency categories), as before caching.
		ctx.JSON(http.StatusOK, xtreamapi.ProcessResponse(re.data))
		return
	}
	if stale {
		utils.DebugLog("Action\t%s served from cached catalogue fetched %s ago", action, time.Since(e.fetchedAt).Round(time.Second))
	}

	body, err := e.renderedJSON(func(data interface{}) ([]byte, error) {
		processed := xtreamapi.ProcessResponse(data)
		if action == "get_live_streams" {
			// Names come from the full list: /status and logs must still name
			// a channel the catalogue filter hides from players.
			c.harvestChannelNames(processed)
		}
		if c.catalogFilter != nil {
			processed = c.filterPlayerAPICatalog(action, processed)
		}
		if action == "get_live_streams" && c.catchupManager != nil && c.catchupManager.IsEnabled() {
			processed = c.injectCatchupFlags(processed)
		}
		return json.Marshal(processed)
	})
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}

	if config.CacheFolder != "" && utils.IsDebugLogEnabled() {
		filename := fmt.Sprintf("%s_%s.json", action, time.Now().Format("20060102_150405"))
		utils.WriteResponseToFile(filename, body, "application/json")
	}
	ctx.Header("Vary", "Accept-Encoding")
	if acceptsGzip(ctx.Request) {
		if gz, gerr := catalogGzip(key, e, body); gerr == nil {
			ctx.Header("Content-Encoding", "gzip")
			ctx.Data(http.StatusOK, "application/json; charset=utf-8", gz)
			return
		}
	}
	ctx.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (c *Config) xtreamPlayerAPIGET(ctx *gin.Context) {
	c.xtreamPlayerAPI(ctx, ctx.Request.URL.Query())
}

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
// The parsed catalog is cached, so the periodic refresher and any concurrent
// player fetches share a single provider round-trip.
func (c *Config) warmChannelNameIndex() {
	resp, err := c.getCachedLiveStreamsCatalog()
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

// getCachedLiveStreamsCatalog returns the parsed get_live_streams catalog,
// fetching and caching it on first use. Shared by the startup warm-up, the
// periodic refresher, and the player_api handler so the full live catalog is
// fetched at most once per TTL window.
func (c *Config) getCachedLiveStreamsCatalog() (interface{}, error) {
	return fetchCatalogOnce(c.catalogKey("get_live_streams", ""), c.catalogFetcher("get_live_streams", nil))
}

func (c *Config) injectCatchupFlags(resp interface{}) interface{} {
	streams, ok := resp.([]interface{})
	if !ok {
		return resp
	}
	// The list comes from the shared catalog cache, which other goroutines
	// read concurrently: flag shallow copies, never the cached entries.
	out := make([]interface{}, len(streams))
	upstreamCatchup := make(map[string]bool, len(streams))
	for i, item := range streams {
		out[i] = item
		orig, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		m := make(map[string]interface{}, len(orig)+1)
		for k, v := range orig {
			m[k] = v
		}
		out[i] = m
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
	return out
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
