/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"strconv"
	"strings"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// Series episode titles.
//
// Xtream has no endpoint that resolves an episode id on its own: the title
// lives in the get_series_info answer of the episode's series, and nothing in
// a playback request says which series that is. Crawling get_series_info for
// every series until the id turns up is unaffordable on a large catalogue
// (one round trip per series, tens of thousands of them). Instead, episode
// titles are harvested from get_series_info answers this server already
// receives (the /watch series search, and players browsing a series through
// player_api.php just before playing an episode), and from the series metadata
// stored with a cached episode.

// formatEpisodeTitle builds the display title of an episode, e.g.
// "Show — S01E02" or "Show — S01E02 — Pilot" when the episode has a name of
// its own (providers often just repeat the series name and number there).
func formatEpisodeTitle(series string, season, episode int, episodeTitle string) string {
	series = strings.TrimSpace(series)
	if series == "" {
		return strings.TrimSpace(episodeTitle)
	}
	base := fmt.Sprintf("%s — S%02dE%02d", series, season, episode)
	ep := strings.TrimSpace(episodeTitle)
	if ep == "" || episodeTitleIsGeneric(ep, series, season, episode) {
		return base
	}
	return base + " — " + ep
}

// episodeTitleIsGeneric reports whether a provider's episode title carries no
// information beyond the series name and episode number, such as
// "Show S01E02" or "Show - S01 E02".
func episodeTitleIsGeneric(ep, series string, season, episode int) bool {
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	e := norm(ep)
	e = strings.TrimPrefix(e, norm(series))
	// The series name in the episode title may differ from ours (a year
	// suffix, another spelling): anything ending in the SxxEyy marker is
	// still just "series + number".
	if strings.HasSuffix(e, fmt.Sprintf("s%02de%02d", season, episode)) {
		return true
	}
	for _, marker := range []string{
		fmt.Sprintf("s%02de%02d", season, episode),
		fmt.Sprintf("s%de%d", season, episode),
		fmt.Sprintf("e%02d", episode),
		fmt.Sprintf("episode%d", episode),
		strconv.Itoa(episode),
	} {
		e = strings.TrimPrefix(e, marker)
	}
	return e == ""
}

// harvestSeriesEpisodeTitles records the title of every episode in a
// get_series_info answer (already decoded to a map) in the VOD name index, so
// a later playback of any of them resolves without network I/O. seriesName
// may be empty: the answer's own info.name is used then. Returns the number of
// titles recorded.
func (c *Config) harvestSeriesEpisodeTitles(seriesName string, info map[string]interface{}) int {
	if info == nil {
		return 0
	}
	if strings.TrimSpace(seriesName) == "" {
		if im, ok := info["info"].(map[string]interface{}); ok {
			seriesName = fmt.Sprintf("%v", firstNonEmpty(im["name"], im["title"]))
		}
	}
	seriesName = strings.TrimSpace(seriesName)
	if seriesName == "" || seriesName == "<nil>" {
		return 0
	}
	bySeason, ok := info["episodes"].(map[string]interface{})
	if !ok {
		return 0
	}

	titles := make(map[string]string)
	for seasonKey, v := range bySeason {
		eps, ok := v.([]interface{})
		if !ok {
			continue
		}
		seasonFromKey, _ := strconv.Atoi(seasonKey)
		for _, e := range eps {
			em, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			id := normalizeStreamID(fmt.Sprintf("%v", firstNonEmpty(em["id"], em["stream_id"])))
			if id == "" || id == "<nil>" {
				continue
			}
			season := toInt(em["season"])
			if season == 0 {
				season = seasonFromKey
			}
			epTitle := ""
			if t, ok := em["title"].(string); ok {
				epTitle = t
			}
			titles[id] = formatEpisodeTitle(seriesName, season, toInt(em["episode_num"]), epTitle)
		}
	}
	if len(titles) == 0 {
		return 0
	}

	// Only new or changed titles are written, so browsing the same series
	// again costs nothing beyond the map comparison.
	changed := make(map[string]string)
	vodNameMu.Lock()
	for id, t := range titles {
		if vodNameIndex[id] != t {
			vodNameIndex[id] = t
			changed[id] = t
		}
	}
	vodNameMu.Unlock()
	if len(changed) > 0 {
		forgetTitleMisses(changed)
		if c.db != nil {
			go func() {
				if err := c.db.UpsertStreamNames(changed, nil, "vod"); err != nil {
					utils.WarnLog("stream_names: failed to persist %d series episode names: %v", len(changed), err)
				}
			}()
		}
		utils.DebugLog("Series titles: recorded %d episode title(s) for %q", len(changed), seriesName)
	}
	return len(titles)
}

// seriesTitleFromVODCache builds an episode title from the series metadata
// stored with a cached episode (written by /watch, which knows the series,
// season and episode it picked).
func (c *Config) seriesTitleFromVODCache(streamID string) string {
	if c.db == nil {
		return ""
	}
	e, err := c.db.GetVODCache(normalizeStreamID(streamID))
	if err != nil || e == nil {
		return ""
	}
	if strings.TrimSpace(e.SeriesTitle) != "" && (e.Season > 0 || e.Episode > 0) {
		return formatEpisodeTitle(e.SeriesTitle, e.Season, e.Episode, "")
	}
	return ""
}
