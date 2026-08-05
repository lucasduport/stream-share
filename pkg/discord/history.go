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
	"net/url"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// periodToHours maps a period choice to hours. "all" or "" → 0 (no lower bound).
func periodToHours(period string) int {
	switch period {
	case "24h":
		return 24
	case "7d":
		return 24 * 7
	case "30d":
		return 24 * 30
	case "90d":
		return 24 * 90
	default:
		return 0 // all time
	}
}

// handleHistory shows watch history. With no username/alias: a global timeline
// feed of recent watch events across all clients. With a username (a raw
// LDAP username or IP) or an alias (resolved to its IP via the ip-aliases
// API): that client's recent watch timeline. `hours`=0 means all-time.
func (b *Bot) handleHistory(s *discordgo.Session, m *discordgo.MessageCreate, username, alias string, hours int) {
	if username != "" && alias != "" {
		b.fail(m.ChannelID, "❌ History Failed", "Provide only one of `username` or `alias`, not both.")
		return
	}
	if alias != "" {
		ok, data, err := b.makeAPIRequest("GET", "/ip-aliases/resolve/"+url.PathEscape(alias), nil)
		mp, _ := data.(map[string]interface{})
		ip, _ := mp["ip_address"].(string)
		if err != nil || !ok || ip == "" {
			b.fail(m.ChannelID, "❌ History Failed", fmt.Sprintf("No alias named %q found.", alias))
			return
		}
		username = ip
	}

	endpoint := "/history"
	title := "📜 Watch History — Recent Activity"
	if username != "" {
		endpoint = "/history/" + url.PathEscape(username)
		title = "📜 Watch History — " + username
	}
	if hours > 0 {
		endpoint += fmt.Sprintf("?hours=%d", hours)
	}
	ok, data, err := b.makeSlowAPIRequest("GET", endpoint, nil)
	if err != nil || !ok {
		b.fail(m.ChannelID, "❌ History Failed", fmt.Sprintf("Failed to get history: %v", err))
		return
	}
	mp, _ := data.(map[string]interface{})
	// /history/:username resolves an IP alias into display_name (LDAP usernames
	// pass through unchanged) — prefer it for the title so an aliased viewer
	// doesn't show up by raw IP here while everywhere else shows the alias.
	if username != "" {
		if dn, ok := mp["display_name"].(string); ok && strings.TrimSpace(dn) != "" {
			title = "📜 Watch History — " + dn
		}
	}
	text := ""
	if s, ok := mp["text"].(string); ok {
		text = strings.TrimSpace(s)
	}
	if text == "" {
		text = "No watch history in this period."
	}
	// Discord embed description limit is 4096 chars; truncate defensively.
	if len(text) > 3900 {
		text = text[:3900] + "\n… (truncated)"
	}
	b.info(m.ChannelID, title, text)
}
