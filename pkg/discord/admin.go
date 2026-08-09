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
	"strings"
	"fmt"
	"net/url"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) isAdmin(member *discordgo.Member) bool {
	if b.adminRoleID == "" {
		return true // no admin role configured — allow anyone (backwards compat)
	}
	if member == nil {
		return false
	}
	for _, r := range member.Roles {
		if r == b.adminRoleID {
			return true
		}
	}
	return false
}

// handleDisconnect forcibly disconnects a user (admin only).
func (b *Bot) handleDisconnect(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
	if len(args) != 1 {
		b.info(m.ChannelID, "🔌 Disconnect User", "Usage: `!disconnect <username>`")
		return
	}
	username := args[0]
	ok, _, err := b.makeAPIRequest("POST", "/users/disconnect/"+url.PathEscape(username), nil)
	if err != nil || !ok {
		b.fail(m.ChannelID, "❌ Disconnect Failed", fmt.Sprintf("We couldn't disconnect this user.\n\nError: `%v`", err))
		return
	}
	b.success(m.ChannelID, "✅ User Disconnected", fmt.Sprintf("User **%s** has been disconnected.", username))
}

// handleTimeout temporarily blocks a user (admin only).
func (b *Bot) handleTimeout(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
	if len(args) != 2 {
		b.info(m.ChannelID, "⏳ Timeout User", "Usage: `!timeout <username> <minutes>`")
		return
	}
	username := args[0]
	minutes := 0
	_, _ = fmt.Sscanf(args[1], "%d", &minutes)
	if minutes <= 0 {
		b.warn(m.ChannelID, "⏳ Invalid Timeout", "Timeout minutes must be a positive number.")
		return
	}
	ok, _, err := b.makeAPIRequest("POST", "/users/timeout/"+url.PathEscape(username), map[string]int{"minutes": minutes})
	if err != nil || !ok {
		b.fail(m.ChannelID, "❌ Timeout Failed", fmt.Sprintf("We couldn't set a timeout for this user.\n\nError: `%v`", err))
		return
	}
	b.success(m.ChannelID, "✅ Timeout Applied", fmt.Sprintf("User **%s** has been timed out for **%d** minutes.", username, minutes))
}

// handleLinkAdmin allows an admin to link any Discord user to any LDAP account
func (b *Bot) handleLinkAdmin(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
	if len(args) != 2 {
		b.info(m.ChannelID, "🔗 Link User (Admin)", "Usage: `!linkadmin <discord_user_id> <ldap_username>`\n\nThis links any Discord user to an LDAP account (admin only).")
		return
	}
	
	discordID := strings.TrimSpace(args[0])
	ldapUser := strings.TrimSpace(args[1])
	
	if discordID == "" || ldapUser == "" {
		b.fail(m.ChannelID, "❌ Link Failed", "Both Discord ID and LDAP username are required.")
		return
	}

	// Get Discord user info to get the username
	var discordName string
	if m.GuildID != "" {
		// Try to get user from guild
		member, err := s.GuildMember(m.GuildID, discordID)
		if err == nil && member != nil && member.User != nil {
			discordName = member.User.Username
		} else {
			// Fallback to just using the ID as name
			discordName = discordID
		}
	} else {
		// Direct message context
		user, err := s.User(discordID)
		if err == nil && user != nil {
			discordName = user.Username
		} else {
			discordName = discordID
		}
	}

	payload := map[string]interface{}{"discord_id": discordID, "discord_name": discordName, "ldap_user": ldapUser}
	ok, resp, err := b.makeAPIRequest("POST", "/discord/link/admin", payload)
	if err != nil || !ok {
		b.fail(m.ChannelID, "❌ Link Failed", fmt.Sprintf("We couldn't link this user right now.\n\nError: `%v`", err))
		return
	}

	confirmed := ldapUser
	if data, ok := resp.(map[string]interface{}); ok {
		if u, exists := data["ldap_user"]; exists {
			confirmed = fmt.Sprintf("%v", u)
		}
	}
	b.success(m.ChannelID, "✅ Linked Successfully (Admin)", fmt.Sprintf("Discord user **%s** (%s) is now linked to LDAP account `%s`.", discordName, discordID, confirmed))
}
