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
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// handleInteractionCreate processes all component interactions (dropdowns, buttons).
func (b *Bot) handleInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}

	msgID := i.Message.ID
	customID := i.MessageComponentData().CustomID
	switch customID {
	case "vod_prev":
		b.handleVODPageChange(s, i, msgID, -1, "prev")

	case "vod_next":
		b.handleVODPageChange(s, i, msgID, +1, "next")

	default:
		if customID != "vod_select" {
			return
		}

		b.selectLock.RLock()
		ctx, ok := b.pendingVODSelect[msgID]
		b.selectLock.RUnlock()
		if !ok {
			return
		}
		if !b.isSameUser(ctx.UserID, i) {
			return
		}

		data := i.MessageComponentData()
		if len(data.Values) == 0 {
			return
		}
		idx, err := strconv.Atoi(data.Values[0])

		b.selectLock.RLock()
		ctx2, ok2 := b.pendingVODSelect[msgID]
		b.selectLock.RUnlock()
		if !ok2 {
			return
		}
		if err != nil || idx < 0 || idx >= len(ctx2.Results) {
			return
		}

		selected := ctx2.Results[idx]
		if strings.HasPrefix(ctx2.Query, "cache:") {
			days := 1
			if p := strings.LastIndex(ctx2.Query, "for "); p != -1 {
				var n int
				_, _ = fmt.Sscanf(ctx2.Query[p:], "for %dd", &n)
				if n > 0 {
					days = n
				}
			}
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: fmt.Sprintf("Caching: %s (days=%d)", selected.Title, days)},
			})
			go b.startVODCacheFromSelection(s, ctx2.Channel, ctx2.UserID, selected, days)
		} else {
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: fmt.Sprintf("Starting download for: %s", selected.Title)},
			})
			go b.startVODDownloadFromSelection(s, ctx2.Channel, ctx2.UserID, selected)
		}
	}
}

// handleVODPageChange advances the paginated VOD selection for msgID by delta
// (negative to go back, positive to go forward), enriches the newly shown page
// if needed, and re-renders the message. label is used only for log messages.
func (b *Bot) handleVODPageChange(s *discordgo.Session, i *discordgo.InteractionCreate, msgID string, delta int, label string) {
	// Hold the write lock for the full read-modify-write on the context to avoid data races.
	b.selectLock.Lock()
	ctx, ok := b.pendingVODSelect[msgID]
	if !ok || !b.isSameUser(ctx.UserID, i) {
		b.selectLock.Unlock()
		return
	}
	ctx.Page += delta
	if ctx.Page < 0 {
		ctx.Page = 0
	}
	page := ctx.Page
	needsEnrich := ctx.EnrichedPages == nil || !ctx.EnrichedPages[page]
	b.selectLock.Unlock()

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})

	if needsEnrich {
		b.enrichVODPage(msgID, page)
	}

	b.selectLock.RLock()
	ctx, ok = b.pendingVODSelect[msgID]
	b.selectLock.RUnlock()
	if ok {
		if err := b.updateVODInteractiveMessage(s, msgID, ctx); err != nil {
			utils.WarnLog("Discord: failed to update VOD message (%s): %v", label, err)
		}
	}
}

// enrichVODPage requests size metadata for the given page of the selection
// stored under msgID and merges it into the cached results. It is a no-op if the
// context has been evicted or the enrich API call fails. The caller must not hold
// selectLock.
func (b *Bot) enrichVODPage(msgID string, page int) {
	b.selectLock.Lock()
	ctx, ok := b.pendingVODSelect[msgID]
	if !ok {
		b.selectLock.Unlock()
		return
	}
	payload := map[string]interface{}{"query": ctx.Query, "results": ctx.Results, "page": page, "per_page": ctx.PerPage}
	b.selectLock.Unlock()

	ok, resp, err := b.makeAPIRequest("POST", "/vod/enrich", payload)
	if err != nil || !ok {
		return
	}
	mp, _ := resp.(map[string]interface{})
	if mp == nil {
		return
	}
	arr, _ := mp["results"].([]interface{})

	b.selectLock.Lock()
	defer b.selectLock.Unlock()
	ctx, ok = b.pendingVODSelect[msgID]
	if !ok || len(arr) != len(ctx.Results) {
		return
	}
	for idx := 0; idx < len(ctx.Results) && idx < len(arr); idx++ {
		rm, ok := arr[idx].(map[string]interface{})
		if !ok {
			continue
		}
		if v, ok := rm["Size"].(string); ok {
			ctx.Results[idx].Size = v
		}
		if vb, ok := rm["SizeBytes"].(float64); ok {
			ctx.Results[idx].SizeBytes = int64(vb)
		}
	}
	if ctx.EnrichedPages != nil {
		ctx.EnrichedPages[page] = true
	}
}
