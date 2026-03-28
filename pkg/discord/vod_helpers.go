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

package discord

import (
    "fmt"
    "regexp"
    "sort"
    "strconv"
    "strings"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/types"
)

// buildLabelForVOD generates a compact label for a VOD item
func buildLabelForVOD(r types.VODResult) string {
    if r.StreamType == "series" && r.SeriesTitle != "" && r.Episode > 0 {
        base := fmt.Sprintf("%s S%02dE%02d", r.SeriesTitle, r.Season, r.Episode)
        if r.EpisodeTitle != "" { base += " — " + r.EpisodeTitle }
        if r.Year != "" { base += fmt.Sprintf(" (%s)", r.Year) }
        return base
    }
    label := r.Title
    if r.Year != "" { label = fmt.Sprintf("%s (%s)", label, r.Year) }
    return label
}

// buildDescriptionForVOD builds the select option description (must be <=100 runes)
func buildDescriptionForVOD(r types.VODResult) string {
    parts := []string{}
    if r.StreamType != "" { parts = append(parts, strings.Title(r.StreamType)) }
    if r.Category != "" { parts = append(parts, r.Category) }
    if r.Size != "" { parts = append(parts, r.Size) }
    if r.Rating != "" { parts = append(parts, "⭐ "+r.Rating) }
    return strings.Join(parts, "  •  ")
}

// inferSeriesFromTitleUnified extracts series name/season/episode/ep title from a title string
func inferSeriesFromTitleUnified(title string) (bool, string, int, int, string) {
    t := strings.TrimSpace(title)
    if t == "" { return false, "", 0, 0, "" }
    re1 := regexp.MustCompile(`(?i)^(.*?)\s*(?:\([^)]*\)\s*)?(?:FHD|HD|UHD|4K|1080p|720p|MULTI)?\s*S(\d{1,2})\s*[EEx×](\d{1,2})(?:\s*[-–—:]\s*(.*))?$`)
    if m := re1.FindStringSubmatch(t); m != nil {
        name := cleanSeriesName(m[1])
        sn := atoiSafe(m[2])
        ep := atoiSafe(m[3])
        epTitle := strings.TrimSpace(m[4])
        return true, name, sn, ep, epTitle
    }
    re2 := regexp.MustCompile(`(?i)^(.*?)\s*S(\d{1,2})\s*[EEx×](\d{1,2})(?:\s*[-–—:]\s*(.*))?`)
    if m := re2.FindStringSubmatch(t); m != nil {
        name := cleanSeriesName(m[1])
        sn := atoiSafe(m[2])
        ep := atoiSafe(m[3])
        epTitle := strings.TrimSpace(m[4])
        return true, name, sn, ep, epTitle
    }
    return false, "", 0, 0, ""
}

// toVODResults converts API result array to typed slice with inference and defaults
func toVODResults(arr []interface{}) []types.VODResult {
    results := make([]types.VODResult, 0, len(arr))
    for _, it := range arr {
        rm, ok := it.(map[string]interface{})
        if !ok { continue }
        vr := types.VODResult{
            ID:          getString(rm, "ID"),
            Title:       getString(rm, "Title"),
            Category:    getString(rm, "Category"),
            Duration:    getString(rm, "Duration"),
            Year:        getString(rm, "Year"),
            Rating:      getString(rm, "Rating"),
            StreamID:    getString(rm, "StreamID"),
            Size:        getString(rm, "Size"),
            StreamType:  strings.ToLower(getString(rm, "StreamType")),
            SeriesTitle: getString(rm, "SeriesTitle"),
        }
        if v, ok := rm["Season"].(float64); ok { vr.Season = int(v) }
        if v, ok := rm["Episode"].(float64); ok { vr.Episode = int(v) }
        // Inference for series-like titles
        if vr.StreamType != "movie" {
            if inferred, name, sn, ep, epT := inferSeriesFromTitleUnified(vr.Title); inferred {
                if vr.SeriesTitle == "" { vr.SeriesTitle = name }
                if vr.Season == 0 { vr.Season = sn }
                if vr.Episode == 0 { vr.Episode = ep }
                if vr.EpisodeTitle == "" { vr.EpisodeTitle = epT }
                if vr.StreamType == "" { vr.StreamType = "series" }
            }
        }
        if vr.StreamType == "" { vr.StreamType = "movie" }
        results = append(results, vr)
    }
    return results
}

// sortVODResults applies a stable sort identical to /vod view
func sortVODResults(results []types.VODResult) {
    sort.SliceStable(results, func(i, j int) bool {
        a, b := results[i], results[j]
        if a.StreamType != b.StreamType {
            return a.StreamType < b.StreamType // series before movies
        }
        if a.StreamType == "series" && b.StreamType == "series" {
            if a.SeriesTitle != b.SeriesTitle { return strings.ToLower(a.SeriesTitle) < strings.ToLower(b.SeriesTitle) }
            if a.Season != b.Season { return a.Season < b.Season }
            if a.Episode != b.Episode { return a.Episode < b.Episode }
            return strings.ToLower(a.Title) < strings.ToLower(b.Title)
        }
        if a.Title != b.Title { return strings.ToLower(a.Title) < strings.ToLower(b.Title) }
        return a.Year < b.Year
    })
}

// enrichFirstPage requests size enrichment for page 0 and mutates the results slice
func (b *Bot) enrichFirstPage(query string, results []types.VODResult, perPage int) {
    if len(results) == 0 { return }
    payload := map[string]interface{}{"query": query, "results": results, "page": 0, "per_page": perPage}
    if ok2, resp2, err2 := b.makeAPIRequest("POST", "/vod/enrich", payload); err2 == nil && ok2 {
        if mp2, _ := resp2.(map[string]interface{}); mp2 != nil {
            if arr2, _ := mp2["results"].([]interface{}); len(arr2) == len(results) {
                for i := 0; i < len(results) && i < len(arr2); i++ {
                    if rm, ok := arr2[i].(map[string]interface{}); ok {
                        if v, ok := rm["Size"].(string); ok { results[i].Size = v }
                        if vb, ok := rm["SizeBytes"].(float64); ok { results[i].SizeBytes = int64(vb) }
                    }
                }
            }
        }
    }
}

// buildOptionsForRange constructs up to 25 select options for the given window
func buildOptionsForRange(results []types.VODResult, start, end int) []discordgo.SelectMenuOption {
    if start < 0 { start = 0 }
    if end > len(results) { end = len(results) }
    if start > end { start = end }
    opts := make([]discordgo.SelectMenuOption, 0, end-start)
    for i := start; i < end; i++ {
        r := results[i]
        label := buildLabelForVOD(r)
        if r.Size != "" { label = fmt.Sprintf("%s — %s", label, r.Size) }
        if len([]rune(label)) > 100 { label = string([]rune(label)[:97]) + "..." }
        desc := buildDescriptionForVOD(r)
        if len([]rune(desc)) > 100 { desc = trimTo(desc, 100) }
        opts = append(opts, discordgo.SelectMenuOption{Label: label, Value: strconv.Itoa(i), Description: desc})
    }
    return opts
}

// cleanSeriesName tries to remove common noise from series names (already used by vod.go)
func cleanSeriesName(s string) string {
    // reuse simple trimming used elsewhere
    return strings.TrimSpace(s)
}

func atoiSafe(s string) int {
    n, _ := strconv.Atoi(s)
    return n
}
