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

// handleLink links the calling Discord user to an LDAP username (self-service).
func (b *Bot) handleLink(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
	if len(args) != 1 {
		b.info(m.ChannelID, "🔗 Link Your Account", "Usage: `/link <ldap_username>`\n\nThis links your Discord account to your IPTV account.")
		return
	}
	ldapUser := strings.TrimSpace(args[0])
	if ldapUser == "" {
		b.info(m.ChannelID, "🔗 Link Your Account", "Usage: `/link <ldap_username>`\n\nThis links your Discord account to your IPTV account.")
		return
	}

	b.linkDiscord(s, m.ChannelID, m.Author.ID, m.Author.Username, ldapUser, true)
}

// linkDiscord is the shared core for self-service and admin linking: it POSTs the
// mapping to the API and reports the result. When self is true the response is
// phrased for the account owner; otherwise it names the linked Discord user
// (admin linking on someone's behalf).
func (b *Bot) linkDiscord(s *discordgo.Session, channelID, discordID, discordName, ldapUser string, self bool) {
	payload := map[string]interface{}{"discord_id": discordID, "discord_name": discordName, "ldap_user": ldapUser}
	ok, resp, err := b.makeAPIRequest("POST", "/discord/link", payload)
	if err != nil || !ok {
		b.fail(channelID, "❌ Link Failed", fmt.Sprintf("We couldn't link %s right now.\n\nError: `%v`", linkSubject(self), err))
		return
	}

	confirmed := ldapUser
	if data, ok := resp.(map[string]interface{}); ok {
		if u, exists := data["ldap_user"]; exists {
			confirmed = fmt.Sprintf("%v", u)
		}
	}
	if self {
		b.success(channelID, "✅ Linked Successfully", fmt.Sprintf("Your Discord account is now linked to `%s`.\n\nYou're all set to use other commands.", confirmed))
	} else {
		b.success(channelID, "✅ Linked Successfully (Admin)", fmt.Sprintf("Discord user **%s** (%s) is now linked to LDAP account `%s`.", discordName, discordID, confirmed))
	}
}

func linkSubject(self bool) string {
	if self {
		return "your account"
	}
	return "this user"
}

// resolveDiscordName looks up the username for a Discord user ID. It tries the
// guild member first (slash-command context), then a direct user fetch (DM
// context), falling back to the raw ID when neither is available.
func resolveDiscordName(s *discordgo.Session, guildID, discordID string) string {
	if guildID != "" {
		if member, err := s.GuildMember(guildID, discordID); err == nil && member != nil && member.User != nil {
			return member.User.Username
		}
	} else {
		if user, err := s.User(discordID); err == nil && user != nil {
			return user.Username
		}
	}
	return discordID
}
