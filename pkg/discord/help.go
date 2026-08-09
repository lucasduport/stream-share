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
	"github.com/bwmarrin/discordgo"
)

// handleHelp sends an embed listing all available commands, grouped by
// user-facing and admin-only.
func (b *Bot) handleHelp(s *discordgo.Session, m *discordgo.MessageCreate) {
	userCmds := []discordgo.MessageEmbedField{
		{Name: "/watch <query> [days]", Value: "Search movies and shows. Pick from the dropdown to get a download link — the item is cached automatically for 1–14 days (default 7). Supports queries like `show s02e04`.", Inline: false},
		{Name: "/library", Value: "List all cached items with their expiry times and who requested them.", Inline: false},
		{Name: "/link <ldap_username>", Value: "Link your Discord account to your IPTV (LDAP) user. Run this once before using other commands.", Inline: false},
		{Name: "/help", Value: "Show this message.", Inline: false},
	}

	adminCmds := []discordgo.MessageEmbedField{
		{Name: "/status", Value: "Show active streams, viewers, and provider subscription state.", Inline: false},
		{Name: "/history [username] [alias] [period]", Value: "Watch history timeline for live and VOD. Omit `username` for a feed across all clients. `period`: 24h, 7d, 30d, 90d, all.", Inline: false},
		{Name: "/disconnect <username>", Value: "Forcibly disconnect a user from their stream.", Inline: false},
		{Name: "/timeout <username> <minutes>", Value: "Temporarily block a user for the given number of minutes.", Inline: false},
		{Name: "/linkadmin <discord_id> <ldap_username>", Value: "Link any Discord user to an LDAP account (admin only).", Inline: false},
	}

	fields := make([]*discordgo.MessageEmbedField, 0, len(userCmds)+len(adminCmds)+2)
	fields = append(fields, &discordgo.MessageEmbedField{Name: "─── User Commands ───", Value: "\u200b"})
	for i := range userCmds {
		fields = append(fields, &userCmds[i])
	}
	fields = append(fields, &discordgo.MessageEmbedField{Name: "─── Admin Commands ───", Value: "\u200b"})
	for i := range adminCmds {
		fields = append(fields, &adminCmds[i])
	}

	b.info(m.ChannelID, "📖 Command Help", "Here are all the commands you can use:", fields...)
}
