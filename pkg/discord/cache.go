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
    "sort"
    "strconv"
    "strings"
    "time"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/types"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// handleCache implements: !cache <vod_name> <number_of_days>
func (b *Bot) handleCache(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
    if len(args) < 2 {
        b.info(m.ChannelID, "💾 Cache VOD",
            "Usage: `!cache <vod_name> <number_of_days>`\nExample: `!cache The Matrix 3` or `!cache Game of Thrones S08E03 5`\nNote: days must be < 15.")
        return
    }
    // Extract days (last arg) and query (preceding)
    daysStr := args[len(args)-1]
    days, err := strconv.Atoi(daysStr)
    if err != nil || days <= 0 || days >= 15 {
        b.warn(m.ChannelID, "⏳ Invalid Days", "Please provide a valid number of days between 1 and 14.")
        return
    }
    query := strings.TrimSpace(strings.Join(args[:len(args)-1], " "))
    if query == "" { b.warn(m.ChannelID, "💾 Cache VOD", "Provide a title to search."); return }

    // Loading embed
    loading, _ := s.ChannelMessageSendEmbed(m.ChannelID, &discordgo.MessageEmbed{Title:"🔎 Searching…", Description: fmt.Sprintf("Looking for `%s`", query), Color: colorInfo, Timestamp: time.Now().UTC().Format(time.RFC3339)})

    // Resolve user
    ok, resp, err := b.makeAPIRequest("GET", "/discord/"+m.Author.ID+"/ldap", nil)
    if err != nil || !ok {
        _ = editEmbed(s, loading, colorWarn, "🔗 Linking Required", "Link your account with `!link <ldap_username>`. ")
        return
    }
    data, _ := resp.(map[string]interface{})
    ldapUser := getString(data, "ldap_user")
    if ldapUser == "" { _ = editEmbed(s, loading, colorWarn, "🔗 Linking Required", "Link your account with `!link <ldap_username>`. "); return }

    // Search
    ok, resp, err = b.makeAPIRequest("POST", "/vod/search", map[string]string{"username": ldapUser, "query": query})
    if err != nil || !ok { _ = editEmbed(s, loading, colorError, "❌ Search Failed", "Could not complete search."); return }
    dmap, _ := resp.(map[string]interface{})
    arr, _ := dmap["results"].([]interface{})
    utils.DebugLog("Discord: Cache search API returned %d results for %q", len(arr), query)
    if len(arr) == 0 { _ = editEmbed(s, loading, colorInfo, "🔎 No Results", fmt.Sprintf("No results for `%s`.", query)); return }

    // Convert to typed results with inference and defaults
    results := toVODResults(arr)

    // Optional client-side filtering to improve matching like "... s02e04"
    tokens, fSeason, fEpisode := parseQueryFilters(query)
    if len(tokens) > 0 { results = filterVODResults(results, tokens, fSeason, fEpisode) }
    utils.DebugLog("Discord: Cache results after filter: %d", len(results))
    if len(results) == 0 {
        _ = editEmbed(s, loading, colorInfo, "🔎 No Results", fmt.Sprintf("No results matched `%s`. Try removing season/episode or using a shorter query.", query))
        return
    }

    // Stable sort identical to /vod
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

    // Single dropdown of 25 per page; enrich first page like /vod
    total := len(results)
    perPage := 25
    b.enrichFirstPage(query, results, perPage)
    withButtons := total > perPage
    ctx := &vodSelectContext{UserID: m.Author.ID, Channel: m.ChannelID, Query: fmt.Sprintf("cache:%s (for %dd)", query, days), Results: results, Page: 0, PerPage: perPage, Created: time.Now()}
    pages := (total+perPage-1)/perPage; if pages==0{pages=1}
    utils.DebugLog("Discord: Cache rendering %d results perPage=%d pages=%d", total, perPage, pages)
    start := 0; end := perPage; if end>total{end=total}
    one := 1
    components := make([]discordgo.MessageComponent, 0, 2)
    opts := buildOptionsForRange(results, start, end)
    placeholder := "Pick to cache…"; if pages>1 { placeholder = fmt.Sprintf("Pick to cache… (%d/%d)", 1, pages) }
    components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{ discordgo.SelectMenu{CustomID: "vod_select", Placeholder: placeholder, MinValues: &one, MaxValues: 1, Options: opts} }})
    if withButtons { components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{ discordgo.Button{Style: discordgo.SecondaryButton, Label: "Prev", CustomID: "vod_prev", Disabled: true}, discordgo.Button{Style: discordgo.SecondaryButton, Label: "Next", CustomID: "vod_next", Disabled: total<=perPage} }}) }
    embed := &discordgo.MessageEmbed{Title: "💾 Cache — Select Item", Description: fmt.Sprintf("%d result(s). Days: %d. Use the dropdown.", total, days), Color: colorInfo, Timestamp: time.Now().UTC().Format(time.RFC3339)}
    embeds := []*discordgo.MessageEmbed{embed}
    if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: loading.ID, Channel: m.ChannelID, Embeds: &embeds, Components: &components}); err != nil {
        msg, err2 := b.renderVODInteractiveMessage(s, ctx)
        if err2 != nil { utils.ErrorLog("Discord: cache render failed: %v", err2); _ = editEmbed(s, loading, colorWarn, "Too Many Results", fmt.Sprintf("Your search returned %d items, which is too many to display at once. Please refine your query.", total)); return }
        b.selectLock.Lock(); b.pendingVODSelect[msg.ID] = ctx; b.selectLock.Unlock()
    } else {
        b.selectLock.Lock(); b.pendingVODSelect[loading.ID] = ctx; b.selectLock.Unlock()
    }

    // Hook selection: reuse existing component handler 'vod_select'. We'll detect cache intent by ctx.Query prefix 'cache:'
}

// In handleInteractionCreate -> case "vod_select" continues to start a download. For caching, detect context.Query prefix and call cache API instead
func (b *Bot) startVODCacheFromSelection(s *discordgo.Session, channelID, userID string, selected types.VODResult, days int) {
    // Resolve LDAP
    ok, resp, err := b.makeAPIRequest("GET", "/discord/"+userID+"/ldap", nil)
    if err != nil || !ok { b.fail(channelID, "❌ Cache Failed", "Couldn't resolve your account."); return }
    data, _ := resp.(map[string]interface{})
    ldapUser := getString(data, "ldap_user")
    if ldapUser == "" { b.warn(channelID, "🔗 Linking Required", "Link your account with `!link <ldap_username>`. "); return }

    payload := map[string]interface{}{
        "username": ldapUser,
        "stream_id": selected.StreamID,
        "type": selected.StreamType,
        "title": selected.Title,
        "series_title": selected.SeriesTitle,
        "season": selected.Season,
        "episode": selected.Episode,
        "days": days,
    }
    ok, resp, err = b.makeAPIRequest("POST", "/cache/start", payload)
    if err != nil || !ok { b.fail(channelID, "❌ Cache Failed", fmt.Sprintf("Couldn't start caching: %v", err)); return }
    d, _ := resp.(map[string]interface{})
    sid := getString(d, "stream_id")
    exp := getString(d, "expires_at")
    title := selected.Title
    if selected.SeriesTitle != "" && selected.Episode > 0 { title = fmt.Sprintf("%s — S%02dE%02d %s", selected.SeriesTitle, selected.Season, selected.Episode, selected.EpisodeTitle) }
    // Initial embed with progress bar
    embed := &discordgo.MessageEmbed{Title: "💾 Caching", Description: fmt.Sprintf("%s\nExpires: %s\n\n%s", title, exp, renderBar(0, 0)), Color: colorInfo, Timestamp: time.Now().UTC().Format(time.RFC3339)}
    msg, _ := b.session.ChannelMessageSendEmbed(channelID, embed)
    if sid == "" { return }
    // Poll progress for up to 12 hours or until ready/failed
    deadline := time.Now().Add(12*time.Hour)
    for time.Now().Before(deadline) {
        time.Sleep(2*time.Second)
        ok, resp, err := b.makeAPIRequest("GET", "/cache/progress/"+sid, nil)
        if err != nil || !ok { continue }
        dm, _ := resp.(map[string]interface{})
        status := strings.ToLower(getString(dm, "status"))
        downloaded := getInt64(dm, "downloaded_bytes")
        total := getInt64(dm, "total_bytes")
        percent := int(getInt64(dm, "percent"))
        bar := renderBar(downloaded, total)
        if status == "ready" || percent >= 100 {
            emb := &discordgo.MessageEmbed{Title: "✅ Cache Ready", Description: fmt.Sprintf("%s\nExpires: %s\n\n%s", title, exp, renderBar(total, total)), Color: colorSuccess, Timestamp: time.Now().UTC().Format(time.RFC3339)}
            _, _ = b.session.ChannelMessageEditEmbed(channelID, msg.ID, emb)
            break
        }
        if status == "failed" {
            emb := &discordgo.MessageEmbed{Title: "❌ Cache Failed", Description: fmt.Sprintf("%s\nPlease retry later.", title), Color: colorError, Timestamp: time.Now().UTC().Format(time.RFC3339)}
            _, _ = b.session.ChannelMessageEditEmbed(channelID, msg.ID, emb)
            break
        }
        emb := &discordgo.MessageEmbed{Title: "💾 Caching", Description: fmt.Sprintf("%s\nExpires: %s\n\n%s (%d%%)", title, exp, bar, percent), Color: colorInfo, Timestamp: time.Now().UTC().Format(time.RFC3339)}
        _, _ = b.session.ChannelMessageEditEmbed(channelID, msg.ID, emb)
    }
}