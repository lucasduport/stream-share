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
    "strconv"
    "strings"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/types"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// trimTo shortens s to at most max runes, appending … if trimmed (requires max >= 3 to include ellipsis)
func trimTo(s string, max int) string {
    if max <= 0 { return "" }
    r := []rune(s)
    if len(r) <= max { return s }
    if max <= 3 { return string(r[:max]) }
    return string(r[:max-3]) + "..."
}

// getString safely extracts string from a map[string]interface{}
func getString(m map[string]interface{}, key string) string {
    if val, ok := m[key].(string); ok {
        return val
    }
    return ""
}

// isSameUser verifies the interaction comes from the expected user.
func (b *Bot) isSameUser(expected string, i *discordgo.InteractionCreate) bool {
    if i.Member != nil && i.Member.User != nil {
        return i.Member.User.ID == expected
    }
    if i.User != nil {
        return i.User.ID == expected
    }
    return false
}



func getInt64(m map[string]interface{}, k string) int64 {
    if v, ok := m[k]; ok {
        switch t := v.(type) {
        case float64:
            return int64(t)
        case int64:
            return t
        case int:
            return int64(t)
        case string:
            if n, err := strconv.ParseInt(t, 10, 64); err == nil { return n }
        }
    }
    return 0
}

// renderBar returns a textual progress bar and bytes summary
func renderBar(done, total int64) string {
    // 20 char bar
    const width = 20
    var pct int
    if total > 0 { pct = int((done*100)/total) } else { pct = 0 }
    if pct > 100 { pct = 100 }
    filled := (pct * width) / 100
    if filled > width { filled = width }
    bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
    var size string
    if total > 0 {
        size = fmt.Sprintf("%s/%s", utils.HumanBytes(done), utils.HumanBytes(total))
    } else if done > 0 {
        size = utils.HumanBytes(done)
    } else {
        size = "starting…"
    }
    return fmt.Sprintf("`[%s]` %d%% — %s", bar, pct, size)
}

// parseQueryFilters splits the query on spaces and extracts optional SxxEyy tokens.
// Returns lowercase tokens (space-split) and season/episode if present (0 if not).
func parseQueryFilters(q string) (tokens []string, season, episode int) {
    s := strings.TrimSpace(strings.ToLower(q))
    if s == "" { return nil, 0, 0 }
    // Extract season/episode like s02e04, s2e4
    if m := regexp.MustCompile(`(?i)\bs(\d{1,2})e(\d{1,2})\b`).FindStringSubmatch(s); m != nil {
        season = atoiSafe(m[1])
        episode = atoiSafe(m[2])
    } else {
        // Support separate tokens like s02 e04
        if m := regexp.MustCompile(`(?i)\bs(\d{1,2})\b`).FindStringSubmatch(s); m != nil { season = atoiSafe(m[1]) }
        if m := regexp.MustCompile(`(?i)\be(\d{1,2})\b`).FindStringSubmatch(s); m != nil { episode = atoiSafe(m[1]) }
    }
    tokens = strings.Fields(s)
    return tokens, season, episode
}

// filterVODResults applies AND contains matching for all tokens and optional season/episode match.
// If season/episode are provided (>0), they must match when the item has these fields.
func filterVODResults(results []types.VODResult, tokens []string, season, episode int) []types.VODResult {
    if len(tokens) == 0 { return results }
    out := make([]types.VODResult, 0, len(results))
    for _, r := range results {
        // Build lowercase haystack
        parts := []string{}
        if r.SeriesTitle != "" { parts = append(parts, r.SeriesTitle) }
        if r.Title != "" { parts = append(parts, r.Title) }
        if r.EpisodeTitle != "" { parts = append(parts, r.EpisodeTitle) }
        if r.Category != "" { parts = append(parts, r.Category) }
        if r.Year != "" { parts = append(parts, r.Year) }
        // Also include canonical sxxexx string if season/episode exist
        if r.Season > 0 && r.Episode > 0 { parts = append(parts, fmt.Sprintf("s%02de%02d", r.Season, r.Episode)) }
        hay := strings.ToLower(strings.Join(parts, " "))

        matchedAll := true
        for _, t := range tokens {
            // Skip season/episode tokens from textual contains, they'll be validated numerically below
            if regexp.MustCompile(`^s\d{1,2}e\d{1,2}$`).MatchString(t) || regexp.MustCompile(`^s\d{1,2}$`).MatchString(t) || regexp.MustCompile(`^e\d{1,2}$`).MatchString(t) {
                continue
            }
            if !strings.Contains(hay, t) { matchedAll = false; break }
        }
        if !matchedAll { continue }

        // If a numeric season/episode was requested, enforce when available on item
        if season > 0 {
            if r.Season > 0 && r.Season != season { continue }
        }
        if episode > 0 {
            if r.Episode > 0 && r.Episode != episode { continue }
        }

        out = append(out, r)
    }
    if len(out) == 0 { return results }
    return out
}
