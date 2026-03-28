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

    "github.com/bwmarrin/discordgo"
)

// handleStatus displays consolidated proxy status.
func (b *Bot) handleStatus(s *discordgo.Session, m *discordgo.MessageCreate, _ []string) {
    ok, data, err := b.makeAPIRequest("GET", "/status", nil)
    if err != nil || !ok { b.fail(m.ChannelID, "❌ Status Failed", fmt.Sprintf("Failed to get status: %v", err)); return }
    mp, _ := data.(map[string]interface{})
    streams := 0
    if v, ok := mp["streams_count"].(float64); ok { streams = int(v) }
    users := 0
    if v, ok := mp["users_count_active"].(float64); ok { users = int(v) }
    text := ""
    if sstr, ok := mp["text"].(string); ok { text = strings.TrimSpace(sstr) }
    desc := fmt.Sprintf("Active Streams: **%d**\nActive Users: **%d**", streams, users)
    if text != "" { desc += "\n\n" + text } else if streams == 0 { desc += "\n\nNo active streams." }
    b.info(m.ChannelID, "📊 IPTV Proxy Status", desc)
}
