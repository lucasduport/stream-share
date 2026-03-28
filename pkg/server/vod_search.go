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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"regexp"

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

var vodM3UMu sync.Mutex

// lightweight in-memory cache for probed sizes to avoid re-hitting upstream on every search
var (
	vodSizeMu    sync.RWMutex
	vodSizeCache = make(map[string]int64) // key: streamID, value: size in bytes
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
		utils.DebugLog("VOD search: movie API results: %d (first: %s)", len(movies), func() string { if len(movies)>0 { return movies[0].Title }; return "" }())
		results = append(results, movies...)
	} else if err != nil {
		utils.WarnLog("VOD search: movie API search error: %v", err)
	}
	// Series via API
	if seriesResults, err := c.searchXtreamSeries(query); err == nil && len(seriesResults) > 0 {
		utils.DebugLog("VOD search: series API results: %d (first: %s)", len(seriesResults), func() string { if len(seriesResults)>0 { return seriesResults[0].Title }; return "" }())
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

	// Do NOT probe sizes here to keep the search ultra-fast and avoid client timeouts.
	// We'll enrich sizes lazily per page via /vod/enrich.
	maxProbe := 0
	// Build an index of movie streamID -> extension from the cached VOD M3U once
	extIndex := map[string]string{}
	if m3uPath, err := c.ensureVODM3UCache(); err == nil {
		if idx, err2 := parseVODM3UExtensions(m3uPath); err2 == nil {
			extIndex = idx
		}
	}

	// Search path no longer performs network calls for sizes; client setup kept for reference if re-enabled later.
	client := &http.Client{Timeout: 2500 * time.Millisecond}

	// Prefill sizes from cache where available

	// Prefill from cache when available
	for i := range results {
		if sz, ok := getCachedSize(results[i].StreamID); ok && sz > 0 {
			results[i].SizeBytes = sz
			results[i].Size = utils.HumanBytes(sz)
		}
	}
	// No probing here; leave sizes empty unless present in cache
	if maxProbe > 0 {
		type job struct{ idx int }
		jobs := make(chan job, maxProbe)
		var wg sync.WaitGroup
		mu := sync.Mutex{}
		// Tuneable worker pool size (default 12, but at most maxProbe)
		workers := 12
		if workers > maxProbe { workers = maxProbe }
		workerFn := func() {
			defer wg.Done()
			for {
				j, ok := <-jobs
				if !ok { return }
					i := j.idx
					streamID := results[i].StreamID
					if streamID == "" { continue }
					if results[i].SizeBytes > 0 { continue }
					// Build Xtream URL with best-effort extension
					typ := results[i].StreamType
					if typ == "" { typ = "movie" }
					basePath := "movie"
					if typ == "series" { basePath = "series" }
					finalID := streamID
					if ext := extIndex[streamID]; ext != "" {
						finalID = finalID + ext
					} else if path.Ext(finalID) == "" {
						if basePath == "series" { finalID += ".mkv" } else { finalID += ".mp4" }
					}
					vodURL := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
					// GET with Range
					req, _ := http.NewRequest("GET", vodURL, nil)
					req.Header.Set("Range", "bytes=0-0")
					req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
					req.Header.Set("Accept-Encoding", "identity")
					req.Header.Set("Accept-Language", utils.GetLanguageHeader())
					req.Header.Set("Accept", "*/*")
					if resp, err := client.Do(req); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if cr := resp.Header.Get("Content-Range"); cr != "" {
							if total := strings.TrimSpace(cr[strings.LastIndex(cr, "/")+1:]); total != "*" {
								if sz, perr := parseInt64(total); perr == nil && sz > 0 {
									mu.Lock()
									results[i].SizeBytes = sz
									results[i].Size = utils.HumanBytes(sz)
									mu.Unlock()
									setCachedSize(streamID, sz)
									continue
								}
							}
						}
						if cl := resp.Header.Get("Content-Length"); cl != "" {
							if sz, perr := parseInt64(cl); perr == nil && sz > 0 {
								mu.Lock()
								results[i].SizeBytes = sz
								results[i].Size = utils.HumanBytes(sz)
								mu.Unlock()
								setCachedSize(streamID, sz)
								continue
							}
						}
					}
			}
		}
		for w := 0; w < workers; w++ { wg.Add(1); go workerFn() }
		for i := 0; i < maxProbe; i++ { jobs <- job{idx: i} }
		close(jobs)
		wg.Wait()
	}

	// Sort results by title for stable ordering
	sort.SliceStable(results, func(i, j int) bool { return strings.ToLower(results[i].Title) < strings.ToLower(results[j].Title) })
	utils.DebugLog("VOD search returned %d results for query: %s", len(results), query)
	return results, nil
}

// searchXtreamMovies queries the Xtream API for VOD movies and filters by tokens.
func (c *Config) searchXtreamMovies(query string) ([]types.VODResult, error) {
	q := strings.TrimSpace(query)
	if q == "" { return nil, nil }
	tokens, _, _ := parseQueryTokens(q) // season/episode tokens ignored for movies
	utils.DebugLog("Movies search: using Xtream client (baseURL=%s, user=%s)", c.XtreamBaseURL, utils.MaskString(c.XtreamUser.String()))
	cli, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, utils.GetIPTVUserAgent())
	if err != nil { return nil, err }
	resp, httpcode, contentType, err := cli.Action(c.ProxyConfig, "get_vod_streams", url.Values{})
	if err != nil {
		utils.WarnLog("Movies search: get_vod_streams failed (HTTP %d, CT=%s): %v", httpcode, contentType, err)
		return nil, err
	}
	arr, ok := resp.([]interface{})
	if !ok { return nil, fmt.Errorf("unexpected get_vod_streams format: %T", resp) }
	out := make([]types.VODResult, 0, 50)
	for _, it := range arr {
		m, ok := it.(map[string]interface{})
		if !ok { continue }
		name := fmt.Sprintf("%v", m["name"]) // movie title
		if name == "" { continue }
		if !allTokensIn(tokens, name) { continue }
		streamID := fmt.Sprintf("%v", m["stream_id"]) // numeric as string
		if strings.TrimSpace(streamID) == "" || streamID == "<nil>" { continue }
		year := fmt.Sprintf("%v", firstNonEmpty(m["releaseDate"], m["release_date"]))
		rating := fmt.Sprintf("%v", firstNonEmpty(m["rating"], m["vote_average"]))
	duration := fmt.Sprintf("%v", m["duration"]) // may be empty; providers sometimes return null -> "<nil>"
	if duration == "<nil>" { duration = "" }
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
	for _, k := range order { out = append(out, best[k]) }
	return out
}

// isRicher decides whether a has more useful metadata than b.
func isRicher(a, b types.VODResult) bool {
	// Prefer entries with episode context
	if (a.SeriesTitle != "" && a.Episode > 0) != (b.SeriesTitle != "" && b.Episode > 0) {
		return a.SeriesTitle != "" && a.Episode > 0
	}
	// Prefer explicit EpisodeTitle
	if (a.EpisodeTitle != "") != (b.EpisodeTitle != "") { return a.EpisodeTitle != "" }
	// Prefer having rating
	if (a.Rating != "") != (b.Rating != "") { return a.Rating != "" }
	// Prefer having size info
	if (a.SizeBytes > 0) != (b.SizeBytes > 0) { return a.SizeBytes > 0 }
	if (a.Size != "") != (b.Size != "") { return a.Size != "" }
	// Prefer having duration
	if (a.Duration != "") != (b.Duration != "") { return a.Duration != "" }
	// Prefer year
	if (a.Year != "") != (b.Year != "") { return a.Year != "" }
	// Prefer longer title (likely includes episode name)
	if len(a.Title) != len(b.Title) { return len(a.Title) > len(b.Title) }
	// Otherwise keep current
	return false
}

func (c *Config) ensureVODM3UCache() (string, error) {
	vodM3UMu.Lock()
	defer vodM3UMu.Unlock()

	// Cache directory preference: CACHE_FOLDER env or temp dir
	cacheDir := os.Getenv("CACHE_FOLDER")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), ".stream-share")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	cacheFile := filepath.Join(cacheDir, "vod_cache.m3u")

	// Check freshness vs. configured M3U cache expiration (hours)
	expHours := c.M3UCacheExpiration
	info, err := os.Stat(cacheFile)
	if err == nil {
		age := time.Since(info.ModTime())
		if age.Hours() < float64(expHours) {
			utils.DebugLog("Using cached VOD M3U: %s (age: %v)", cacheFile, age)
			return cacheFile, nil
		}
		// If expired but present, return stale file immediately and refresh in background to avoid blocking
		stalePath := cacheFile
		go func() {
			// best-effort refresh
			if err := c.refreshVODM3U(stalePath); err != nil {
				utils.WarnLog("Failed background VOD M3U refresh: %v", err)
			}
		}()
		utils.DebugLog("Using stale VOD M3U while refreshing in background: %s (age: %v)", cacheFile, age)
		return stalePath, nil
	}

	// No cache present: fetch synchronously
	if err := c.refreshVODM3U(cacheFile); err != nil { return "", err }
	return cacheFile, nil
}

// refreshVODM3U downloads the VOD M3U into cacheFile path.
func (c *Config) refreshVODM3U(cacheFile string) error {
	getURL := fmt.Sprintf("%s/get.php?username=%s&password=%s&type=m3u_plus&output=m3u8",
		c.XtreamBaseURL, c.XtreamUser.String(), c.XtreamPassword.String())
	utils.InfoLog("Refreshing VOD M3U from Xtream: %s", utils.MaskURL(getURL))
	req, err := http.NewRequest("GET", getURL, nil)
	if err != nil { return err }
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	// Short timeout for refresh to avoid tying resources
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { return fmt.Errorf("backend returned %d for M3U request", resp.StatusCode) }
	f, err := os.Create(cacheFile)
	if err != nil { return err }
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil { return err }
	utils.InfoLog("Stored VOD M3U to %s", cacheFile)
	return nil
}

// parseVODM3UExtensions scans the cached VOD M3U once and builds a map of streamID -> extension.
func parseVODM3UExtensions(m3uPath string) (map[string]string, error) {
	f, err := os.Open(m3uPath)
	if err != nil { return nil, err }
	defer f.Close()
	sc := bufio.NewScanner(f)
	idx := make(map[string]string, 4096)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if !(strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")) { continue }
		u, err := url.Parse(line)
		if err != nil { continue }
		// Movie/series entries embed streamID possibly with extension as last path segment
		last := path.Base(u.Path)
		if last == "" { continue }
		id := last
		ext := path.Ext(last)
		if ext != "" { id = strings.TrimSuffix(last, ext) }
		if id == "" { continue }
		if ext != "" {
			if _, exists := idx[id]; !exists {
				idx[id] = ext
			}
		}
	}
	if err := sc.Err(); err != nil { return idx, err }
	return idx, nil
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
	if s == "" { return nil, 0, 0 }
	// Find sXXeYY combined
	if m := regexp.MustCompile(`\bs(\d{1,2})e(\d{1,2})\b`).FindStringSubmatch(s); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil { season = v }
		if v, err := strconv.Atoi(m[2]); err == nil { episode = v }
		s = strings.ReplaceAll(s, m[0], " ")
	}
	// Separate sXX and eYY
	if season == 0 {
		if m := regexp.MustCompile(`\bs(\d{1,2})\b`).FindStringSubmatch(s); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil { season = v }
			s = strings.ReplaceAll(s, m[0], " ")
		}
	}
	if episode == 0 {
		if m := regexp.MustCompile(`\be(\d{1,2})\b`).FindStringSubmatch(s); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil { episode = v }
			s = strings.ReplaceAll(s, m[0], " ")
		}
	}
	tokens = strings.Fields(s)
	return tokens, season, episode
}

// allTokensIn checks that all tokens are contained in the haystack string (case-insensitive).
func allTokensIn(tokens []string, hay string) bool {
	if len(tokens) == 0 { return true }
	h := strings.ToLower(hay)
	for _, t := range tokens {
		if !strings.Contains(h, t) { return false }
	}
	return true
}


// searchSeriesInM3UFile scans the cached M3U and returns episodic entries as series results.
func searchSeriesInM3UFile(m3uPath string, query string) ([]types.VODResult, error) {
	f, err := os.Open(m3uPath)
	if err != nil { return nil, err }
	defer f.Close()

	q := strings.TrimSpace(query)
	qTokens, qSeason, qEpisode := parseQueryTokens(q)
	sc := bufio.NewScanner(f)
	lastEXTINF := ""
	results := make([]types.VODResult, 0, 50)
	// Regexes to extract S/E and split title
	reSE := regexp.MustCompile(`(?i)\bS(\d{1,2})\s*[EExx×](\d{1,2})\b`)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" { continue }
		if strings.HasPrefix(line, "#EXTINF:") { lastEXTINF = line; continue }
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			u, err := url.Parse(line); if err != nil { continue }
			if !strings.Contains(u.Path, "/series/") { continue }
			title := ""; category := ""
			if lastEXTINF != "" {
				if idx := strings.LastIndex(lastEXTINF, ","); idx != -1 && idx+1 < len(lastEXTINF) {
					title = strings.TrimSpace(lastEXTINF[idx+1:])
				}
				attrs := lastEXTINF
				if i := strings.Index(attrs, " "); i != -1 { attrs = attrs[i+1:] }
				const key = `group-title="`
				if pos := strings.Index(attrs, key); pos != -1 { start := pos + len(key); if end := strings.Index(attrs[start:], `"`); end != -1 { category = attrs[start:start+end] } }
			}
			if title == "" { title = path.Base(u.Path) }
			// Token-based match: require all non-season tokens; enforce numeric season/episode if specified
			if len(qTokens) > 0 && !allTokensIn(qTokens, title) { lastEXTINF = ""; continue }
			// Extract S/E
			season, episode := 0, 0
			if m := reSE.FindStringSubmatch(title); m != nil {
				season, _ = strconv.Atoi(m[1])
				episode, _ = strconv.Atoi(m[2])
			}
			if qSeason > 0 && season > 0 && season != qSeason { lastEXTINF = ""; continue }
			if qEpisode > 0 && episode > 0 && episode != qEpisode { lastEXTINF = ""; continue }
			seriesTitle := title
			if i := reSE.FindStringIndex(seriesTitle); i != nil { seriesTitle = strings.TrimSpace(strings.Trim(seriesTitle[:i[0]], "-—–:|• ")) }
			// StreamID is the last path segment
			streamID := path.Base(u.Path)
			results = append(results, types.VODResult{
				ID:           streamID,
				Title:        title,
				Category:     category,
				Duration:     "",
				Year:         "",
				Rating:       "",
				StreamID:     streamID,
				StreamType:   "series",
				SeriesTitle:  seriesTitle,
				Season:       season,
				Episode:      episode,
				EpisodeTitle: "",
			})
			lastEXTINF = ""
		}
	}
	if err := sc.Err(); err != nil { return nil, err }
	return results, nil
}

// searchXtreamSeries searches series and flattens episodes matching the query
func (c *Config) searchXtreamSeries(query string) ([]types.VODResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	qTokens, qSeason, qEpisode := parseQueryTokens(q)
	// Use resilient client to avoid FlexInt unmarshaling issues
	utils.DebugLog("Series search: using resilient Xtream client (baseURL=%s, user=%s)", c.XtreamBaseURL, utils.MaskString(c.XtreamUser.String()))
	cli, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, utils.GetIPTVUserAgent())
	if err != nil {
		utils.WarnLog("Series search: failed to create resilient client: %v", err)
		return nil, err
	}

	resp, httpcode, contentType, err := cli.Action(c.ProxyConfig, "get_series", url.Values{})
	if err != nil {
		utils.WarnLog("Series search: get_series failed (HTTP %d, CT=%s): %v", httpcode, contentType, err)
		return nil, err
	}

	arr, ok := resp.([]interface{})
	if !ok {
		utils.WarnLog("Series search: unexpected get_series format: %T", resp)
		return nil, fmt.Errorf("unexpected get_series format: %T", resp)
	}

	out := make([]types.VODResult, 0, 50)
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		seriesName := fmt.Sprintf("%v", m["name"])
		if seriesName == "" {
			continue
		}
		// Only require non-season tokens to be in the series name
		if !allTokensIn(qTokens, seriesName) { continue }
		seriesID := fmt.Sprintf("%v", m["series_id"])
		if seriesID == "" || seriesID == "<nil>" {
			continue
		}
		genre := fmt.Sprintf("%v", m["genre"]) // may be empty
		year := fmt.Sprintf("%v", firstNonEmpty(m["releaseDate"], m["release_date"]))

		utils.DebugLog("Series search: candidate '%s' (id=%s, genre=%s, year=%s)", seriesName, seriesID, genre, year)
		utils.DebugLog("Series search: fetching series info for '%s' (series_id=%s)", seriesName, seriesID)
		infoResp, httpcode, contentType, err := cli.Action(c.ProxyConfig, "get_series_info", url.Values{"series_id": {seriesID}})
		if err != nil {
			utils.WarnLog("Series search: get_series_info failed for id=%s: %v (HTTP %d, CT=%s)", seriesID, err, httpcode, contentType)
			continue
		}
		im, ok := infoResp.(map[string]interface{})
		if !ok {
			utils.WarnLog("Series search: unexpected series_info format for id=%s: %T", seriesID, infoResp)
			continue
		}
		epsBySeason, ok := im["episodes"].(map[string]interface{})
		if !ok {
			// Some providers use episodes as array with season inside
			continue
		}
	totalEps := 0
	for seasonStr, epsV := range epsBySeason {
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
				// Apply token AND match on either episode title or series name
				if len(qTokens) > 0 && !(allTokensIn(qTokens, title) || allTokensIn(qTokens, seriesName)) { continue }
				streamID := fmt.Sprintf("%v", firstNonEmpty(em["id"], em["stream_id"]))
				if streamID == "" || streamID == "<nil>" {
					continue
				}
				epNum := toInt(em["episode_num"]) // best-effort
				// Enforce numeric season/episode if specified
				if qSeason > 0 && seasonNum != qSeason { continue }
				if qEpisode > 0 && epNum != qEpisode { continue }
				// info subobject for duration/rating
				var duration, rating string
				if infoSub, ok := em["info"].(map[string]interface{}); ok {
					duration = fmt.Sprintf("%v", infoSub["duration"]) // may be ""
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
