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
    "strings"
    "time"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// handleVOD implements the /vod command
// It searches across movies and series and lists everything in a single select with pagination.
func (b *Bot) handleVOD(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
    query := strings.TrimSpace(strings.Join(args, " "))
    if query == "" {
        b.info(m.ChannelID, "🎬 VOD Search", "Usage: `!vod <query>`\n\nSearches movies and shows. Use the dropdown to choose.")
        return
    }

    utils.DebugLog("Discord: VOD query received: %q", query)
    // Loading embed
    loading, _ := s.ChannelMessageSendEmbed(m.ChannelID, &discordgo.MessageEmbed{
        Title:       "🔎 Searching…",
        Description: fmt.Sprintf("Looking for `%s`", query),
        Color:       colorInfo,
        Timestamp:   time.Now().UTC().Format(time.RFC3339),
    })

    // Resolve LDAP
    ok, resp, err := b.makeAPIRequest("GET", "/discord/"+m.Author.ID+"/ldap", nil)
    if err != nil || !ok {
        _ = editEmbed(s, loading, colorWarn, "🔗 Linking Required", "Your Discord account isn't linked. Use `!link <ldap_username>`. ")
        return
    }
    dmap, _ := resp.(map[string]interface{})
    ldapUser := getString(dmap, "ldap_user")
    if ldapUser == "" { _ = editEmbed(s, loading, colorWarn, "🔗 Linking Required", "Link your account with `!link <ldap_username>`. "); return }

    // Search
    ok, resp, err = b.makeAPIRequest("POST", "/vod/search", map[string]string{"username": ldapUser, "query": query})
    if err != nil || !ok { _ = editEmbed(s, loading, colorError, "❌ Search Failed", "Couldn't complete search."); return }
    mp, _ := resp.(map[string]interface{})
    arr, _ := mp["results"].([]interface{})
    utils.DebugLog("Discord: API returned %d VOD results for %q", len(arr), query)
    if len(arr) == 0 { _ = editEmbed(s, loading, colorInfo, "🔎 No Results", fmt.Sprintf("No results for `%s`.", query)); return }
    results := toVODResults(arr)

    // Stable sort: series episodes grouped by show/season/episode, movies by title/year
    sortVODResults(results)

    // Optional client-side filtering to improve matching like "game of throne s02e04"
    tokens, fSeason, fEpisode := parseQueryFilters(query)
    if len(tokens) > 0 { utils.DebugLog("Discord: filter tokens=%v season=%d episode=%d", tokens, fSeason, fEpisode) }
    if len(tokens) > 0 {
        results = filterVODResults(results, tokens, fSeason, fEpisode)
    }
    utils.DebugLog("Discord: results after filter: %d", len(results))
    // Log first few for diagnostics
    for i := 0; i < len(results) && i < 4; i++ {
        r := results[i]
        utils.DebugLog("Discord: result[%d]: type=%s id=%s title=%s series=%s S%02dE%02d", i, r.StreamType, r.StreamID, r.Title, r.SeriesTitle, r.Season, r.Episode)
    }
    if len(results) == 0 {
        _ = editEmbed(s, loading, colorInfo, "🔎 No Results", fmt.Sprintf("No results matched `%s`. Try removing season/episode or using a shorter query.", query))
        return
    }

    // Build interactive selection context and render
    // Limit: single dropdown of 25 options per page with Prev/Next buttons when needed
    total := len(results)
    perPage := 25
    withButtons := total > perPage
    ctx := &vodSelectContext{UserID: m.Author.ID, Channel: m.ChannelID, Query: query, Results: results, Page: 0, PerPage: perPage, Created: time.Now(), EnrichedPages: map[int]bool{}}

    // Enrich only the first page sizes/metadata from server to keep fast responses
    b.enrichFirstPage(query, results, perPage)

    // Prepare current page options across multiple selects
    pages := (total + perPage - 1) / perPage
    if pages == 0 { pages = 1 }
    utils.DebugLog("Discord: rendering %d results perPage=%d pages=%d", total, perPage, pages)
    start := 0
    end := perPage
    if end > total { end = total }
    one := 1
    components := make([]discordgo.MessageComponent, 0, 2)
    // Single select of up to 25 options
    opts := buildOptionsForRange(results, start, end)
    placeholder := "Pick a title…"
    if pages > 1 { placeholder = fmt.Sprintf("Pick a title… (%d/%d)", 1, pages) }
    components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{ discordgo.SelectMenu{CustomID: "vod_select", Placeholder: placeholder, MinValues: &one, MaxValues: 1, Options: opts} }})
    if withButtons {
        components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{ discordgo.Button{Style: discordgo.SecondaryButton, Label: "Prev", CustomID: "vod_prev", Disabled: true}, discordgo.Button{Style: discordgo.SecondaryButton, Label: "Next", CustomID: "vod_next", Disabled: total <= perPage} }})
    }
    desc := fmt.Sprintf("Query: `%s` — %d result(s)%s\nUse the dropdown to choose.", query, total, func() string { if pages>1 { return fmt.Sprintf(" — Page 1/%d", pages) }; return "" }())
    embed := &discordgo.MessageEmbed{Title: "🎬 VOD Search Results", Description: desc, Color: colorInfo, Timestamp: time.Now().UTC().Format(time.RFC3339)}
    embeds := []*discordgo.MessageEmbed{embed}
    if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: loading.ID, Channel: m.ChannelID, Embeds: &embeds, Components: &components}); err != nil {
        // Fallback to send new without scaring the user; still paginate 25 by 25
        msg, err2 := b.renderVODInteractiveMessage(s, ctx)
        if err2 == nil {
            b.selectLock.Lock(); b.pendingVODSelect[msg.ID] = ctx; b.selectLock.Unlock()
        } else {
            // As a last resort, just log; don't show a misleading "too many results" message
            utils.WarnLog("Discord: failed to render VOD selection: edit=%v send=%v", err, err2)
            return
        }
    } else {
        b.selectLock.Lock(); b.pendingVODSelect[loading.ID] = ctx; b.selectLock.Unlock()
    }
    // Mark first page as enriched
    if ctx.EnrichedPages != nil { ctx.EnrichedPages[0] = true }
}

// handleCachedList shows current cached items with time until expiry
func (b *Bot) handleCachedList(s *discordgo.Session, m *discordgo.MessageCreate) {
	ok, resp, err := b.makeAPIRequest("GET", "/cache/list", nil)
	if err != nil || !ok {
		b.fail(m.ChannelID, "❌ Cache List Failed", "Couldn't fetch cached items.")
		return
	}
	arr, _ := resp.([]interface{})
	if len(arr) == 0 {
		b.info(m.ChannelID, "💾 Cached Items", "No active cached items.")
		return
	}
	const per = 10
	pages := (len(arr)+per-1)/per
	for p := 0; p < pages; p++ {
		start := p*per
		end := start+per
		if end > len(arr) { end = len(arr) }
		lines := make([]string, 0, end-start)
		for _, it := range arr[start:end] {
			mapp, _ := it.(map[string]interface{})
			typ := getString(mapp, "type")
			title := strings.TrimSpace(getString(mapp, "title"))
			if typ == "series" {
				st := getString(mapp, "series_title")
				if strings.TrimSpace(st) != "" { title = st }
				if title == "" { title = "Series" }
				season := int(getInt64(mapp, "season"))
				episode := int(getInt64(mapp, "episode"))
				if season > 0 || episode > 0 {
					title = fmt.Sprintf("%s S%02dE%02d", title, season, episode)
				}
			} else {
				if title == "" { title = "Unknown title" }
			}
			by := strings.TrimSpace(getString(mapp, "requested_by"))
			leftSecs := int(getInt64(mapp, "time_left_seconds"))
			// Humanize left: prioritize days, else hours
			left := "expired"
			if leftSecs > 0 {
				days := leftSecs / 86400
				if days >= 1 {
					if days == 1 { left = "1 day" } else { left = fmt.Sprintf("%d days", days) }
				} else {
					hours := (leftSecs + 3599) / 3600 // round up
					if hours <= 1 { left = "1 hour" } else { left = fmt.Sprintf("%d hours", hours) }
				}
			}
			// Build line: Title [— by user] — expires in X
			line := fmt.Sprintf("• %s", title)
			if by != "" { line += fmt.Sprintf(" — by %s", by) }
			line += fmt.Sprintf(" — expires in %s", left)
			lines = append(lines, line)
		}
		desc := strings.Join(lines, "\n")
		if pages > 1 { desc += fmt.Sprintf("\n\nPage %d/%d", p+1, pages) }
		b.info(m.ChannelID, "💾 Cached Items", desc)
	}
}