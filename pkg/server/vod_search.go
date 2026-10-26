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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

// lightweight in-memory cache for probed sizes to avoid re-hitting upstream on every search
var (
	vodSizeMu    sync.RWMutex
	vodSizeCache = make(map[string]int64) // key: streamID, value: size in bytes
)

var (
	reSECombined = regexp.MustCompile(`\bs(\d{1,2})e(\d{1,2})\b`)
	reSeason     = regexp.MustCompile(`\bs(\d{1,2})\b`)
	reEpisode    = regexp.MustCompile(`\be(\d{1,2})\b`)
)

func getCachedSize(streamID string) (int64, bool) {
	vodSizeMu.RLock()
	defer vodSizeMu.RUnlock()
	sz, ok := vodSizeCache[streamID]
	return sz, ok
}

func setCachedSize(streamID string, size int64) {
	vodSizeMu.Lock()
	vodSizeCache[streamID] = size
	vodSizeMu.Unlock()
}

// searchXtreamVOD searches movies and series using the Xtream API only (no M3U mixing)
func (c *Config) searchXtreamVOD(query string) ([]types.VODResult, error) {
	utils.DebugLog("Searching VOD with query: %s", query)

	// Validate Xtream configuration
	if c.XtreamBaseURL == "" || c.XtreamUser.String() == "" || c.XtreamPassword.String() == "" {
		utils.ErrorLog("Xtream configuration is incomplete")
		return nil, fmt.Errorf("xtream configuration is incomplete")
	}

	results := make([]types.VODResult, 0, 50)
	// Movies via API
	if movies, err := c.searchXtreamMovies(query); err == nil && len(movies) > 0 {
		utils.DebugLog("VOD search: movie API results: %d (first: %s)", len(movies), func() string {
			if len(movies) > 0 {
				return movies[0].Title
			}
			return ""
		}())
		results = append(results, movies...)
	} else if err != nil {
		utils.WarnLog("VOD search: movie API search error: %v", err)
	}
	// Series via API
	if seriesResults, err := c.searchXtreamSeries(query); err == nil && len(seriesResults) > 0 {
		utils.DebugLog("VOD search: series API results: %d (first: %s)", len(seriesResults), func() string {
			if len(seriesResults) > 0 {
				return seriesResults[0].Title
			}
			return ""
		}())
		results = append(results, seriesResults...)
	} else if err != nil {
		utils.WarnLog("VOD search: series API search error: %v", err)
	}

	// Deduplicate by (StreamType, StreamID), keep the richer entry
	if len(results) > 1 {
		before := len(results)
		results = dedupeVODResults(results)
		if len(results) != before {
			utils.DebugLog("VOD search: deduplicated results: %d -> %d", before, len(results))
		}
	}

	// Prefill sizes from cache where available.
	// Network probing is intentionally disabled here to keep searches fast;
	// sizes are enriched lazily per page via /vod/enrich.
	for i := range results {
		if sz, ok := getCachedSize(results[i].StreamID); ok && sz > 0 {
			results[i].SizeBytes = sz
			results[i].Size = utils.HumanBytes(sz)
		}
	}

	// Sort results by title for stable ordering
	sort.SliceStable(results, func(i, j int) bool { return strings.ToLower(results[i].Title) < strings.ToLower(results[j].Title) })
	utils.DebugLog("VOD search returned %d results for query: %s", len(results), query)
	return results, nil
}

// searchXtreamMovies queries the Xtream API for VOD movies and filters by tokens.
// The full get_vod_streams catalog is cached (parsed) so repeated searches do
// not re-download and re-decode the multi-tens-of-MB JSON body per query.
func (c *Config) searchXtreamMovies(query string) ([]types.VODResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	tokens, _, _ := parseQueryTokens(q) // season/episode tokens ignored for movies
	utils.DebugLog("Movies search: using Xtream client (baseURL=%s, user=%s)", c.XtreamBaseURL, utils.MaskString(c.XtreamUser.String()))

	resp, err := c.getCachedVODCatalog()
	if err != nil {
		utils.WarnLog("Movies search: get_vod_streams failed: %v", err)
		return nil, err
	}
	arr, ok := resp.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected get_vod_streams format: %T", resp)
	}
	out := make([]types.VODResult, 0, 50)
	for _, it := range arr {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		name := fmt.Sprintf("%v", m["name"]) // movie title
		if name == "" {
			continue
		}
		if !allTokensIn(tokens, name) {
			continue
		}
		streamID := fmt.Sprintf("%v", m["stream_id"]) // numeric as string
		if strings.TrimSpace(streamID) == "" || streamID == "<nil>" {
			continue
		}
		year := fmt.Sprintf("%v", firstNonEmpty(m["releaseDate"], m["release_date"]))
		rating := fmt.Sprintf("%v", firstNonEmpty(m["rating"], m["vote_average"]))
		duration := fmt.Sprintf("%v", m["duration"]) // may be empty; providers sometimes return null -> "<nil>"
		if duration == "<nil>" {
			duration = ""
		}
		category := fmt.Sprintf("%v", m["category_name"]) // best-effort; some providers only give category_id

		out = append(out, types.VODResult{
			ID:         streamID,
			Title:      name,
			Category:   category,
			Duration:   duration,
			Year:       year,
			Rating:     rating,
			StreamID:   streamID,
			StreamType: "movie",
		})
	}
	utils.DebugLog("Movies search: returning %d results", len(out))
	return out, nil
}

// dedupeVODResults removes duplicates with the same (StreamType, StreamID), keeping the richer entry.
func dedupeVODResults(in []types.VODResult) []types.VODResult {
	type key struct{ typ, id string }
	best := make(map[key]types.VODResult, len(in))
	order := make([]key, 0, len(in))
	for _, r := range in {
		k := key{typ: strings.ToLower(r.StreamType), id: r.StreamID}
		if k.id == "" { // fallback key on Title to avoid total collapse when provider omits ids
			k = key{typ: strings.ToLower(r.StreamType), id: strings.ToLower(r.Title)}
		}
		if cur, ok := best[k]; ok {
			if isRicher(r, cur) {
				best[k] = r
			}
		} else {
			best[k] = r
			order = append(order, k)
		}
	}
	out := make([]types.VODResult, 0, len(best))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// isRicher decides whether a has more useful metadata than b.
func isRicher(a, b types.VODResult) bool {
	// Prefer entries with episode context
	if (a.SeriesTitle != "" && a.Episode > 0) != (b.SeriesTitle != "" && b.Episode > 0) {
		return a.SeriesTitle != "" && a.Episode > 0
	}
	// Prefer explicit EpisodeTitle
	if (a.EpisodeTitle != "") != (b.EpisodeTitle != "") {
		return a.EpisodeTitle != ""
	}
	// Prefer having rating
	if (a.Rating != "") != (b.Rating != "") {
		return a.Rating != ""
	}
	// Prefer having size info
	if (a.SizeBytes > 0) != (b.SizeBytes > 0) {
		return a.SizeBytes > 0
	}
	if (a.Size != "") != (b.Size != "") {
		return a.Size != ""
	}
	// Prefer having duration
	if (a.Duration != "") != (b.Duration != "") {
		return a.Duration != ""
	}
	// Prefer year
	if (a.Year != "") != (b.Year != "") {
		return a.Year != ""
	}
	// Prefer longer title (likely includes episode name)
	if len(a.Title) != len(b.Title) {
		return len(a.Title) > len(b.Title)
	}
	// Otherwise keep current
	return false
}

// errVODM3UNotReady is returned while the VOD M3U has never been downloaded
// and the first download is still running in the background.
var errVODM3UNotReady = errors.New("VOD M3U not downloaded yet")

// vodM3UFailBackoff is how long a failed VOD M3U download is remembered before
// a lookup may start another one; on slow providers each attempt is minutes of
// bandwidth, so failures must not turn into a retry loop.
const vodM3UFailBackoff = 5 * time.Minute

// vodM3UStallTimeout aborts a VOD M3U download that has received no bytes for
// this long. There is deliberately no overall deadline: a 400+ MB catalogue
// from a slow provider can legitimately take many minutes while still making
// progress.
const vodM3UStallTimeout = 60 * time.Second

// vodM3UFetch is one in-flight VOD M3U download; done closes when it ends.
type vodM3UFetch struct {
	done chan struct{}
	err  error
}

var (
	vodM3UMu       sync.Mutex
	vodM3UInFlight *vodM3UFetch // nil when no download is running
	vodM3ULastErr  error
	vodM3UFailedAt time.Time
)

// vodM3UClient has no overall timeout (see vodM3UStallTimeout). Providers can
// take minutes to start answering for a big catalogue, hence the long header
// timeout. Compression is left on so providers that gzip the playlist send
// roughly a tenth of the bytes.
var vodM3UClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 3 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	},
}

func vodM3UCachePath() string {
	return filepath.Join(utils.VODCacheDir(), "vod_cache.m3u")
}

// ensureVODM3UCache returns the path of the cached VOD M3U without ever
// waiting on the provider: a stale file is served as-is while a single
// background download refreshes it, and a missing file yields
// errVODM3UNotReady while the first download runs. Request paths call this,
// so they degrade to defaults instead of blocking on a multi-minute download.
// Background work that needs the catalogue should use waitVODM3UCache.
func (c *Config) ensureVODM3UCache() (string, error) {
	cacheFile := vodM3UCachePath()
	info, err := os.Stat(cacheFile)
	if err != nil {
		c.startVODM3URefresh()
		return "", errVODM3UNotReady
	}
	if age := time.Since(info.ModTime()); age.Hours() >= float64(c.M3UCacheExpiration) {
		if c.startVODM3URefresh() != nil {
			utils.DebugLog("Using stale VOD M3U while refreshing in background: %s (age: %v)", cacheFile, age)
		}
	}
	return cacheFile, nil
}

// vodM3UCached reports whether a VOD M3U has been downloaded at least once.
func vodM3UCached() bool {
	_, err := os.Stat(vodM3UCachePath())
	return err == nil
}

// waitVODM3UCache is ensureVODM3UCache for background work: when no cached
// file exists yet it waits for the in-flight download instead of failing.
func (c *Config) waitVODM3UCache() (string, error) {
	if p, err := c.ensureVODM3UCache(); err == nil {
		return p, nil
	}
	f := c.startVODM3URefresh()
	if f == nil {
		vodM3UMu.Lock()
		defer vodM3UMu.Unlock()
		return "", fmt.Errorf("%w (last attempt: %v)", errVODM3UNotReady, vodM3ULastErr)
	}
	<-f.done
	if f.err != nil {
		return "", f.err
	}
	return vodM3UCachePath(), nil
}

// startVODM3URefresh starts a background VOD M3U download unless one is
// already running, in which case that one is returned, so concurrent callers
// share a single provider download. Returns nil while a recent failure is in
// backoff.
func (c *Config) startVODM3URefresh() *vodM3UFetch {
	if c.XtreamBaseURL == "" {
		return nil // plain M3U deployment: there is no VOD catalogue to fetch
	}
	vodM3UMu.Lock()
	defer vodM3UMu.Unlock()
	if vodM3UInFlight != nil {
		return vodM3UInFlight
	}
	if vodM3ULastErr != nil && time.Since(vodM3UFailedAt) < vodM3UFailBackoff {
		return nil
	}
	f := &vodM3UFetch{done: make(chan struct{})}
	vodM3UInFlight = f
	go func() {
		f.err = c.refreshVODM3U(vodM3UCachePath())
		vodM3UMu.Lock()
		vodM3UInFlight = nil
		vodM3ULastErr = f.err
		if f.err != nil {
			vodM3UFailedAt = time.Now()
			utils.WarnLog("VOD M3U download failed (next attempt in %v): %v", vodM3UFailBackoff, f.err)
		}
		vodM3UMu.Unlock()
		close(f.done)
	}()
	return f
}

// refreshVODM3U downloads the VOD M3U into cacheFile. The body goes to a
// sibling .part file renamed into place only once complete, so an aborted
// download never replaces a good cache with a truncated one.
func (c *Config) refreshVODM3U(cacheFile string) error {
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		return err
	}
	getURL := fmt.Sprintf("%s/get.php?username=%s&password=%s&type=m3u_plus&output=m3u8",
		c.XtreamBaseURL, c.XtreamUser.String(), c.XtreamPassword.String())
	utils.InfoLog("Refreshing VOD M3U from Xtream: %s", utils.MaskURL(getURL))
	start := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", getURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	resp, err := vodM3UClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned %d for M3U request", resp.StatusCode)
	}

	tmp := cacheFile + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	stall := time.AfterFunc(vodM3UStallTimeout, cancel)
	defer stall.Stop()
	n, err := io.Copy(f, &stallReader{r: resp.Body, timer: stall, idle: vodM3UStallTimeout})
	if err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err == nil && n == 0 {
		err = errors.New("empty VOD M3U returned by Xtream backend")
	}
	if err == nil {
		err = os.Rename(tmp, cacheFile)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("VOD M3U download aborted after %s (%v): %w", utils.HumanBytes(n), time.Since(start).Round(time.Second), err)
	}
	utils.InfoLog("Stored VOD M3U to %s (%s in %v)", cacheFile, utils.HumanBytes(n), time.Since(start).Round(time.Second))
	return nil
}

// stallReader re-arms timer on every read that returns data, so the timer
// only fires (cancelling the request) when the body stalls for idle.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.idle)
	}
	return n, err
}

// parseInt64 converts string to int64, ignoring commas/spaces
func parseInt64(s string) (int64, error) {
	s = strings.TrimSpace(s)
	// Remove thousands separators if any
	s = strings.ReplaceAll(s, ",", "")
	var n int64
	var err error
	// fast path
	n, err = strconv.ParseInt(s, 10, 64)
	return n, err
}

// parseQueryTokens splits the query into tokens and extracts optional s/e (e.g., s02e04, s2e4, or separate s02 e04).
// Returns lowercase tokens and season/episode numbers (0 if missing). Season/Episode tokens are removed from tokens.
func parseQueryTokens(q string) (tokens []string, season, episode int) {
	s := strings.TrimSpace(strings.ToLower(q))
	if s == "" {
		return nil, 0, 0
	}
	// Find sXXeYY combined
	if m := reSECombined.FindStringSubmatch(s); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil {
			season = v
		}
		if v, err := strconv.Atoi(m[2]); err == nil {
			episode = v
		}
		s = strings.ReplaceAll(s, m[0], " ")
	}
	// Separate sXX and eYY
	if season == 0 {
		if m := reSeason.FindStringSubmatch(s); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				season = v
			}
			s = strings.ReplaceAll(s, m[0], " ")
		}
	}
	if episode == 0 {
		if m := reEpisode.FindStringSubmatch(s); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				episode = v
			}
			s = strings.ReplaceAll(s, m[0], " ")
		}
	}
	tokens = strings.Fields(s)
	return tokens, season, episode
}

// allTokensIn checks that all tokens are contained in the haystack string (case-insensitive).
func allTokensIn(tokens []string, hay string) bool {
	if len(tokens) == 0 {
		return true
	}
	h := strings.ToLower(hay)
	for _, t := range tokens {
		if !strings.Contains(h, t) {
			return false
		}
	}
	return true
}

// searchXtreamSeries searches series and flattens episodes matching the query.
// The get_series catalog is cached (parsed) so repeated searches do not
// re-download the full catalog; get_series_info calls for matching series are
// issued with bounded parallelism instead of sequentially.
func (c *Config) searchXtreamSeries(query string) ([]types.VODResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	qTokens, qSeason, qEpisode := parseQueryTokens(q)
	utils.DebugLog("Series search: using resilient Xtream client (baseURL=%s, user=%s)", c.XtreamBaseURL, utils.MaskString(c.XtreamUser.String()))
	cli, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, utils.GetIPTVUserAgent())
	if err != nil {
		utils.WarnLog("Series search: failed to create resilient client: %v", err)
		return nil, err
	}

	resp, err := c.getCachedSeriesCatalog()
	if err != nil {
		utils.WarnLog("Series search: get_series failed: %v", err)
		return nil, err
	}

	arr, ok := resp.([]interface{})
	if !ok {
		utils.WarnLog("Series search: unexpected get_series format: %T", resp)
		return nil, fmt.Errorf("unexpected get_series format: %T", resp)
	}

	// First pass: collect matching series (name + id + metadata) without
	// fetching series_info yet.
	type seriesMatch struct {
		name  string
		id    string
		genre string
		year  string
	}
	matches := make([]seriesMatch, 0, 16)
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		seriesName := fmt.Sprintf("%v", m["name"])
		if seriesName == "" {
			continue
		}
		if !allTokensIn(qTokens, seriesName) {
			continue
		}
		seriesID := fmt.Sprintf("%v", m["series_id"])
		if seriesID == "" || seriesID == "<nil>" {
			continue
		}
		matches = append(matches, seriesMatch{
			name:  seriesName,
			id:    seriesID,
			genre: fmt.Sprintf("%v", m["genre"]),
			year:  fmt.Sprintf("%v", firstNonEmpty(m["releaseDate"], m["release_date"])),
		})
	}

	// Second pass: fetch get_series_info for all matches with bounded
	// parallelism (default 8 concurrent), instead of sequentially per series.
	const maxConcurrent = 8
	type infoResult struct {
		match seriesMatch
		eps   map[string]interface{} // episodes by season
		ok    bool
	}
	results := make([]infoResult, len(matches))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, mt := range matches {
		wg.Add(1)
		go func(idx int, match seriesMatch) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			utils.DebugLog("Series search: fetching series info for '%s' (series_id=%s)", match.name, match.id)
			infoResp, httpcode, contentType, err := cli.Action(c.ProxyConfig, "get_series_info", url.Values{"series_id": {match.id}})
			if err != nil {
				utils.WarnLog("Series search: get_series_info failed for id=%s: %v (HTTP %d, CT=%s)", match.id, err, httpcode, contentType)
				return
			}
			im, ok := infoResp.(map[string]interface{})
			if !ok {
				utils.WarnLog("Series search: unexpected series_info format for id=%s: %T", match.id, infoResp)
				return
			}
			c.harvestSeriesEpisodeTitles(match.name, im)
			if deduped, ok := c.seriesEpisodeDedup(im).(map[string]interface{}); ok {
				im = deduped
			}
			epsBySeason, ok := im["episodes"].(map[string]interface{})
			if !ok {
				return
			}
			results[idx] = infoResult{match: match, eps: epsBySeason, ok: true}
		}(i, mt)
	}
	wg.Wait()

	// Third pass: flatten episodes into VOD results (single-threaded, preserves order).
	out := make([]types.VODResult, 0, 50)
	for _, r := range results {
		if !r.ok {
			continue
		}
		seriesName := r.match.name
		genre := r.match.genre
		year := r.match.year
		totalEps := 0
		for seasonStr, epsV := range r.eps {
			seasonNum, _ := strconv.Atoi(seasonStr)
			eps, ok := epsV.([]interface{})
			if !ok {
				continue
			}
			for _, e := range eps {
				em, ok := e.(map[string]interface{})
				if !ok {
					continue
				}
				title := fmt.Sprintf("%v", em["title"])
				if len(qTokens) > 0 && !allTokensIn(qTokens, title) && !allTokensIn(qTokens, seriesName) {
					continue
				}
				streamID := fmt.Sprintf("%v", firstNonEmpty(em["id"], em["stream_id"]))
				if streamID == "" || streamID == "<nil>" {
					continue
				}
				epNum := toInt(em["episode_num"])
				if qSeason > 0 && seasonNum != qSeason {
					continue
				}
				if qEpisode > 0 && epNum != qEpisode {
					continue
				}
				var duration, rating string
				if infoSub, ok := em["info"].(map[string]interface{}); ok {
					duration = fmt.Sprintf("%v", infoSub["duration"])
					rating = fmt.Sprintf("%v", firstNonEmpty(infoSub["rating"], infoSub["vote_average"]))
				}

				out = append(out, types.VODResult{
					ID:           streamID,
					Title:        fmt.Sprintf("%s S%02dE%02d — %s", seriesName, seasonNum, epNum, title),
					Category:     genre,
					Duration:     duration,
					Year:         year,
					Rating:       rating,
					StreamID:     streamID,
					StreamType:   "series",
					SeriesTitle:  seriesName,
					Season:       seasonNum,
					Episode:      epNum,
					EpisodeTitle: title,
				})
				totalEps++
			}
		}
		utils.DebugLog("Series search: '%s' yielded %d episode entries after filtering", seriesName, totalEps)
	}
	utils.DebugLog("Series search: returning %d results", len(out))
	return out, nil
}

// getCachedVODCatalog returns the parsed get_vod_streams catalog, fetching and
// caching it on first use. Concurrent callers share a single fetch.
func (c *Config) getCachedVODCatalog() (interface{}, error) {
	return fetchCatalogOnce(c.catalogKey("get_vod_streams", ""), c.catalogFetcher("get_vod_streams", url.Values{}))
}

// getCachedSeriesCatalog returns the parsed get_series catalog, fetching and
// caching it on first use. Concurrent callers share a single fetch.
func (c *Config) getCachedSeriesCatalog() (interface{}, error) {
	return fetchCatalogOnce(c.catalogKey("get_series", ""), c.catalogFetcher("get_series", url.Values{}))
}

// firstNonEmpty returns the first non-empty/non-nil value among candidates
func firstNonEmpty(values ...interface{}) interface{} {
	for _, v := range values {
		if v == nil {
			continue
		}
		s := fmt.Sprintf("%v", v)
		if s != "" && s != "<nil>" {
			return v
		}
	}
	return ""
}

// toInt best-effort conversion from interface{} number/string to int
func toInt(v interface{}) int {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		s := fmt.Sprintf("%v", v)
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0
		}
		return n
	}
}
