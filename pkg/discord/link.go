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

// handleLink links a Discord user to LDAP username.
func (b *Bot) handleLink(s *discordgo.Session, m *discordgo.MessageCreate, args []string) {
    if len(args) != 1 {
        b.info(m.ChannelID, "🔗 Link Your Account", "Usage: `!link <ldap_username>`\n\nThis links your Discord account to your IPTV account.")
        return
    }
    ldapUser := strings.TrimSpace(args[0])
    if ldapUser == "" {
        b.info(m.ChannelID, "🔗 Link Your Account", "Usage: `!link <ldap_username>`\n\nThis links your Discord account to your IPTV account.")
        return
    }

    payload := map[string]interface{}{"discord_id": m.Author.ID, "discord_name": m.Author.Username, "ldap_user": ldapUser}
    ok, resp, err := b.makeAPIRequest("POST", "/discord/link", payload)
    if err != nil || !ok { b.fail(m.ChannelID, "❌ Link Failed", fmt.Sprintf("We couldn't link your account right now.\n\nError: `%v`", err)); return }

    confirmed := ldapUser
    if data, ok := resp.(map[string]interface{}); ok {
        if u, exists := data["ldap_user"]; exists { confirmed = fmt.Sprintf("%v", u) }
    }
    b.success(m.ChannelID, "✅ Linked Successfully", fmt.Sprintf("Your Discord account is now linked to `%s`.\n\nYou're all set to use other commands.", confirmed))
}
