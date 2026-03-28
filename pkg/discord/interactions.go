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
    if i.Type != discordgo.InteractionMessageComponent { return }

    msgID := i.Message.ID
    customID := i.MessageComponentData().CustomID
    switch customID {
    case "vod_prev":
        // Hold write lock for the full read-modify-write on the context to avoid data races.
        b.selectLock.Lock()
        ctx, ok := b.pendingVODSelect[msgID]
        if !ok { b.selectLock.Unlock(); return }
        if !b.isSameUser(ctx.UserID, i) { b.selectLock.Unlock(); return }
        ctx.Page--
        if ctx.Page < 0 { ctx.Page = 0 }
        page := ctx.Page
        needsEnrich := ctx.EnrichedPages == nil || !ctx.EnrichedPages[page]
        b.selectLock.Unlock()

        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})

        if needsEnrich {
            b.selectLock.Lock()
            ctx2, ok2 := b.pendingVODSelect[msgID]
            if ok2 {
                payload := map[string]interface{}{"query": ctx2.Query, "results": ctx2.Results, "page": page, "per_page": ctx2.PerPage}
                b.selectLock.Unlock()
                if ok3, resp3, err3 := b.makeAPIRequest("POST", "/vod/enrich", payload); err3 == nil && ok3 {
                    if mp, _ := resp3.(map[string]interface{}); mp != nil {
                        b.selectLock.Lock()
                        if ctx3, ok4 := b.pendingVODSelect[msgID]; ok4 {
                            if arr, _ := mp["results"].([]interface{}); len(arr) == len(ctx3.Results) {
                                for idx := 0; idx < len(ctx3.Results) && idx < len(arr); idx++ {
                                    if rm, ok := arr[idx].(map[string]interface{}); ok {
                                        if v, ok := rm["Size"].(string); ok { ctx3.Results[idx].Size = v }
                                        if vb, ok := rm["SizeBytes"].(float64); ok { ctx3.Results[idx].SizeBytes = int64(vb) }
                                    }
                                }
                                if ctx3.EnrichedPages != nil { ctx3.EnrichedPages[page] = true }
                            }
                        }
                        b.selectLock.Unlock()
                    }
                }
            } else {
                b.selectLock.Unlock()
            }
        }

        b.selectLock.RLock()
        ctx4, ok4 := b.pendingVODSelect[msgID]
        b.selectLock.RUnlock()
        if ok4 {
            if err := b.updateVODInteractiveMessage(s, msgID, ctx4); err != nil {
                utils.WarnLog("Discord: failed to update VOD message (prev): %v", err)
            }
        }

    case "vod_next":
        b.selectLock.Lock()
        ctx, ok := b.pendingVODSelect[msgID]
        if !ok { b.selectLock.Unlock(); return }
        if !b.isSameUser(ctx.UserID, i) { b.selectLock.Unlock(); return }
        ctx.Page++
        page := ctx.Page
        needsEnrich := ctx.EnrichedPages == nil || !ctx.EnrichedPages[page]
        b.selectLock.Unlock()

        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})

        if needsEnrich {
            b.selectLock.Lock()
            ctx2, ok2 := b.pendingVODSelect[msgID]
            if ok2 {
                payload := map[string]interface{}{"query": ctx2.Query, "results": ctx2.Results, "page": page, "per_page": ctx2.PerPage}
                b.selectLock.Unlock()
                if ok3, resp3, err3 := b.makeAPIRequest("POST", "/vod/enrich", payload); err3 == nil && ok3 {
                    if mp, _ := resp3.(map[string]interface{}); mp != nil {
                        b.selectLock.Lock()
                        if ctx3, ok4 := b.pendingVODSelect[msgID]; ok4 {
                            if arr, _ := mp["results"].([]interface{}); len(arr) == len(ctx3.Results) {
                                for idx := 0; idx < len(ctx3.Results) && idx < len(arr); idx++ {
                                    if rm, ok := arr[idx].(map[string]interface{}); ok {
                                        if v, ok := rm["Size"].(string); ok { ctx3.Results[idx].Size = v }
                                        if vb, ok := rm["SizeBytes"].(float64); ok { ctx3.Results[idx].SizeBytes = int64(vb) }
                                    }
                                }
                                if ctx3.EnrichedPages != nil { ctx3.EnrichedPages[page] = true }
                            }
                        }
                        b.selectLock.Unlock()
                    }
                }
            } else {
                b.selectLock.Unlock()
            }
        }

        b.selectLock.RLock()
        ctx4, ok4 := b.pendingVODSelect[msgID]
        b.selectLock.RUnlock()
        if ok4 {
            if err := b.updateVODInteractiveMessage(s, msgID, ctx4); err != nil {
                utils.WarnLog("Discord: failed to update VOD message (next): %v", err)
            }
        }

    default:
        if customID != "vod_select" { return }

        b.selectLock.RLock()
        ctx, ok := b.pendingVODSelect[msgID]
        b.selectLock.RUnlock()
        if !ok { return }
        if !b.isSameUser(ctx.UserID, i) { return }

        data := i.MessageComponentData()
        if len(data.Values) == 0 { return }
        idx, err := strconv.Atoi(data.Values[0])

        b.selectLock.RLock()
        ctx2, ok2 := b.pendingVODSelect[msgID]
        b.selectLock.RUnlock()
        if !ok2 { return }
        if err != nil || idx < 0 || idx >= len(ctx2.Results) { return }

        selected := ctx2.Results[idx]
        if strings.HasPrefix(ctx2.Query, "cache:") {
            days := 1
            if p := strings.LastIndex(ctx2.Query, "for "); p != -1 {
                var n int
                _, _ = fmt.Sscanf(ctx2.Query[p:], "for %dd", &n)
                if n > 0 { days = n }
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
