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
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// Duplicate series episodes.
//
// Some providers list the same episode several times in a get_series_info
// answer, under different stream ids: same season and episode number, same
// title, same file. Players then show every copy, and picking two of them opens
// two downloads of identical content. dedupSeriesEpisodes keeps one entry per
// such group.
//
// Two entries are duplicates only when everything comparable says they are the
// same file: same season and episode number, same title (case and spacing
// aside), and the same technical fingerprint from the episode's info block
// (duration within seriesDedupDurationSlack, video codec, profile, pixel format
// and height, audio codec, container). An entry whose info lacks a duration or
// a video codec cannot be compared and is never merged. Entries of one episode
// with different fingerprints (e.g. an H.264 and an HEVC encode) are all kept:
// they are genuine alternatives, optionally told apart by a technical tag in
// their title.

// seriesDedupDurationSlack is how far two durations may differ (in seconds)
// and still be the same file: providers round durations inconsistently.
const seriesDedupDurationSlack = 2

// episodeFingerprint is the comparable technical description of an episode.
type episodeFingerprint struct {
	duration                  int
	video, profile, pixFmt    string
	height                    int
	audio, container          string
	known                     bool // duration and video codec are present
	videoLabel, audioLabel    string
	resolutionLabel, extLabel string
}

func (a episodeFingerprint) sameFile(b episodeFingerprint) bool {
	if !a.known || !b.known {
		return false
	}
	d := a.duration - b.duration
	if d < 0 {
		d = -d
	}
	return d <= seriesDedupDurationSlack &&
		a.video == b.video && a.profile == b.profile && a.pixFmt == b.pixFmt &&
		a.height == b.height && a.audio == b.audio && a.container == b.container
}

// fingerprintEpisode reads the technical fingerprint of a get_series_info
// episode entry.
func fingerprintEpisode(ep map[string]interface{}) episodeFingerprint {
	fp := episodeFingerprint{container: strings.ToLower(stringField(ep["container_extension"]))}
	info, _ := ep["info"].(map[string]interface{})
	if info == nil {
		return fp
	}
	fp.duration = numberField(info["duration_secs"])
	if v, ok := info["video"].(map[string]interface{}); ok {
		fp.video = strings.ToLower(stringField(v["codec_name"]))
		fp.profile = strings.ToLower(stringField(v["profile"]))
		fp.pixFmt = strings.ToLower(stringField(v["pix_fmt"]))
		fp.height = numberField(v["height"])
	}
	if a, ok := info["audio"].(map[string]interface{}); ok {
		fp.audio = strings.ToLower(stringField(a["codec_name"]))
	}
	fp.known = fp.duration > 0 && fp.video != ""
	fp.videoLabel = videoLabel(fp.video, fp.pixFmt)
	fp.audioLabel = audioLabel(fp.audio)
	if fp.height > 0 {
		fp.resolutionLabel = strconv.Itoa(fp.height) + "p"
	}
	fp.extLabel = strings.ToUpper(fp.container)
	return fp
}

// videoLabel names a video codec the way players and release names do.
func videoLabel(codec, pixFmt string) string {
	var name string
	switch codec {
	case "":
		return ""
	case "h264":
		name = "H.264"
	case "hevc", "h265":
		name = "HEVC"
	case "av1":
		name = "AV1"
	case "mpeg4":
		name = "MPEG-4"
	default:
		name = strings.ToUpper(codec)
	}
	if strings.Contains(pixFmt, "10") {
		name += " 10-bit"
	}
	return name
}

// audioLabel names an audio codec.
func audioLabel(codec string) string {
	switch codec {
	case "":
		return ""
	case "eac3":
		return "E-AC3"
	case "ac3":
		return "AC3"
	case "aac":
		return "AAC"
	case "dts":
		return "DTS"
	case "truehd":
		return "TrueHD"
	default:
		return strings.ToUpper(codec)
	}
}

// variantTags returns, for each fingerprint, a short tag made of the technical
// traits that differ between them ("HEVC 10-bit", "H.264 / E-AC3"...). A trait
// shared by every variant is left out. Empty tags when nothing tells them apart.
func variantTags(fps []episodeFingerprint) []string {
	traits := []func(episodeFingerprint) string{
		func(f episodeFingerprint) string { return f.videoLabel },
		func(f episodeFingerprint) string { return f.audioLabel },
		func(f episodeFingerprint) string { return f.resolutionLabel },
		func(f episodeFingerprint) string { return f.extLabel },
	}
	tags := make([][]string, len(fps))
	for _, trait := range traits {
		distinct := map[string]bool{}
		for _, f := range fps {
			distinct[trait(f)] = true
		}
		if len(distinct) < 2 {
			continue
		}
		for i, f := range fps {
			if v := trait(f); v != "" {
				tags[i] = append(tags[i], v)
			}
		}
	}
	out := make([]string, len(fps))
	for i, t := range tags {
		out[i] = strings.Join(t, " / ")
	}
	return out
}

// normalizeEpisodeTitle compares titles ignoring case and spacing.
func normalizeEpisodeTitle(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// episodeCluster is one set of identical copies of an episode.
type episodeCluster struct {
	pos       int // index of its first copy in the season list (keeps ordering)
	survivor  map[string]interface{}
	survivorN int64 // numeric id of the survivor, for the lowest-id rule
	preferred bool  // survivor is a copy the caller prefers (e.g. already cached)
	checked   bool  // preferred has been computed for the current survivor
	fp        episodeFingerprint
	title     string
}

// dedupSeriesEpisodes returns info with duplicate episodes merged (see the
// package comment above) and the number of entries removed. info itself is
// never modified: a changed answer is a new top-level map with new episode
// lists, so callers that already handed info to someone else stay safe. When
// tagVariants is set, the kept alternatives of one episode get a technical tag
// appended to their title. Answers without duplicates are returned as-is.
//
// prefer, when non-nil, marks copies to keep over the others (e.g. one already
// in the VOD cache, so players are not sent to an uncached twin); among equally
// preferred copies the lowest numeric id wins.
func dedupSeriesEpisodes(info map[string]interface{}, tagVariants bool, prefer func(ep map[string]interface{}) bool) (map[string]interface{}, int) {
	bySeason, ok := info["episodes"].(map[string]interface{})
	if !ok || len(bySeason) == 0 {
		return info, 0
	}

	newSeasons := make(map[string]interface{}, len(bySeason))
	removedBySeason := map[string]int{}
	changed := false
	removed := 0
	for seasonKey, v := range bySeason {
		eps, ok := v.([]interface{})
		if !ok {
			newSeasons[seasonKey] = v
			continue
		}
		kept, n, retitled := dedupSeasonEpisodes(eps, tagVariants, prefer)
		if n > 0 || retitled {
			changed = true
		}
		removed += n
		removedBySeason[seasonKey] = n
		newSeasons[seasonKey] = kept
	}
	if !changed {
		return info, 0
	}

	out := make(map[string]interface{}, len(info))
	for k, v := range info {
		out[k] = v
	}
	out["episodes"] = newSeasons
	if seasons, ok := info["seasons"].([]interface{}); ok && removed > 0 {
		out["seasons"] = adjustSeasonCounts(seasons, removedBySeason)
	}
	return out, removed
}

// dedupSeasonEpisodes merges the duplicate copies of one season's episode list.
// It returns the kept entries in their original order (each cluster at the
// position of its first copy), how many were dropped, and whether any title
// was tagged.
func dedupSeasonEpisodes(eps []interface{}, tagVariants bool, prefer func(ep map[string]interface{}) bool) ([]interface{}, int, bool) {
	type slot struct {
		raw     interface{}     // entries that are never merged, as-is
		cluster *episodeCluster // or a cluster of copies
	}
	slots := make([]slot, 0, len(eps))
	groups := map[int][]*episodeCluster{} // episode_num → its clusters
	removed := 0

	for _, e := range eps {
		em, ok := e.(map[string]interface{})
		epNum := 0
		if ok {
			epNum = numberField(em["episode_num"])
		}
		if !ok || epNum <= 0 {
			slots = append(slots, slot{raw: e})
			continue
		}
		fp := fingerprintEpisode(em)
		title := normalizeEpisodeTitle(stringField(em["title"]))
		id := idNumber(em)
		var match *episodeCluster
		for _, cl := range groups[epNum] {
			if cl.title == title && cl.fp.sameFile(fp) {
				match = cl
				break
			}
		}
		if match == nil {
			cl := &episodeCluster{pos: len(slots), survivor: em, survivorN: id, fp: fp, title: title}
			// prefer is consulted lazily, only once a second copy shows up.
			groups[epNum] = append(groups[epNum], cl)
			slots = append(slots, slot{cluster: cl})
			continue
		}
		if !match.checked {
			match.preferred = prefer != nil && prefer(match.survivor)
			match.checked = true
		}
		removed++
		// Keep a preferred copy (e.g. already cached) first, then the lowest
		// numeric id: stable across provider refreshes, so a cached file or a
		// history row keyed by that id keeps matching.
		pref := prefer != nil && prefer(em)
		lower := id >= 0 && (match.survivorN < 0 || id < match.survivorN)
		if (pref && !match.preferred) || (pref == match.preferred && lower) {
			match.survivor, match.survivorN, match.fp, match.preferred = em, id, fp, pref
		}
	}

	retitled := false
	if tagVariants {
		for _, clusters := range groups {
			if len(clusters) < 2 {
				continue
			}
			fps := make([]episodeFingerprint, len(clusters))
			for i, cl := range clusters {
				fps[i] = cl.fp
			}
			for i, tag := range variantTags(fps) {
				if tag == "" {
					continue
				}
				cl := clusters[i]
				copied := make(map[string]interface{}, len(cl.survivor))
				for k, v := range cl.survivor {
					copied[k] = v
				}
				copied["title"] = fmt.Sprintf("%s [%s]", stringField(cl.survivor["title"]), tag)
				cl.survivor = copied
				retitled = true
			}
		}
	}

	if removed == 0 && !retitled {
		return eps, 0, false
	}
	kept := make([]interface{}, 0, len(slots))
	for _, s := range slots {
		if s.cluster != nil {
			kept = append(kept, s.cluster.survivor)
		} else {
			kept = append(kept, s.raw)
		}
	}
	return kept, removed, retitled
}

// adjustSeasonCounts lowers each season's episode_count by the number of
// duplicates removed from it, keeping the field's original JSON type. The
// season entries are copied, not modified.
func adjustSeasonCounts(seasons []interface{}, removedBySeason map[string]int) []interface{} {
	out := make([]interface{}, len(seasons))
	for i, s := range seasons {
		out[i] = s
		sm, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		n := removedBySeason[stringField(sm["season_number"])]
		count, has := sm["episode_count"]
		if n == 0 || !has {
			continue
		}
		newCount := numberField(count) - n
		if newCount < 0 {
			newCount = 0
		}
		copied := make(map[string]interface{}, len(sm))
		for k, v := range sm {
			copied[k] = v
		}
		switch count.(type) {
		case string:
			copied["episode_count"] = strconv.Itoa(newCount)
		case json.Number:
			copied["episode_count"] = json.Number(strconv.Itoa(newCount))
		default:
			copied["episode_count"] = newCount
		}
		out[i] = copied
	}
	return out
}

// seriesEpisodeDedup applies dedupSeriesEpisodes to a get_series_info answer
// when SERIES_EPISODE_DEDUP_ENABLED is set; otherwise it returns resp unchanged.
func (c *Config) seriesEpisodeDedup(resp interface{}) interface{} {
	if !c.SeriesEpisodeDedupEnabled {
		return resp
	}
	info, ok := resp.(map[string]interface{})
	if !ok {
		return resp
	}
	out, removed := dedupSeriesEpisodes(info, c.SeriesEpisodeDedupTagVariants, c.episodeCached)
	if removed > 0 {
		utils.DebugLog("Series dedup: dropped %d duplicate episode entr(ies)", removed)
	}
	return out
}

// episodeCached reports whether an episode's file is complete in the VOD cache
// (named <id><.ext>, as streamVODWithCache writes it; a download in progress
// is still a .part file). A stat, no database round trip.
func (c *Config) episodeCached(ep map[string]interface{}) bool {
	if !c.VODCacheEnabled {
		return false
	}
	id := strings.TrimSpace(stringField(firstNonEmpty(ep["id"], ep["stream_id"])))
	ext := strings.TrimSpace(stringField(ep["container_extension"]))
	if id == "" || ext == "" || !isValidStreamID(id) || strings.ContainsAny(ext, "/\\") {
		return false
	}
	fi, err := os.Stat(filepath.Join(utils.VODCacheDir(), id+"."+strings.TrimPrefix(ext, ".")))
	return err == nil && !fi.IsDir()
}

// idNumber is an episode's numeric id, or -1 when it has none.
func idNumber(ep map[string]interface{}) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(stringField(firstNonEmpty(ep["id"], ep["stream_id"]))), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// stringField renders a decoded JSON scalar as a string ("" for nil).
func stringField(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// numberField reads a decoded JSON number or numeric string as an int (0 when
// absent or not a number). Fractional values are rounded.
func numberField(v interface{}) int {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(math.Round(t))
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
		if f, err := t.Float64(); err == nil {
			return int(math.Round(f))
		}
		return 0
	case string:
		s := strings.TrimSpace(t)
		if i, err := strconv.Atoi(s); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(math.Round(f))
		}
	}
	return 0
}
