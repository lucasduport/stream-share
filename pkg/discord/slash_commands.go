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

// command definitions
func (b *Bot) commandSpecs() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "watch",
			Description: "Search movies and shows; pick to get a link and auto-cache",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "query", Description: "Title to search (supports S01E02)", Required: true},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "days", Description: "Days to keep cached (1–14; default 7)", Required: false, MinValue: floatPtr(1), MaxValue: 14},
			},
		},
		{
			Name:        "library",
			Description: "List cached items and when they expire",
		},
		{
			Name:        "help",
			Description: "Show all available commands and usage",
		},
		{
			Name:        "link",
			Description: "Link your Discord account to your IPTV (LDAP) user",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Your LDAP username", Required: true},
			},
		},
		{
			Name:                     "linkadmin",
			Description:              "Link any Discord user to an LDAP account (admin only)",
			DefaultMemberPermissions: int64Ptr(discordgo.PermissionManageGuild),
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "discord_id", Description: "Discord user ID to link", Required: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "ldap_username", Description: "LDAP username to link to", Required: true},
			},
		},
		{
			Name:                     "status",
			Description:              "Show active streams and users",
			DefaultMemberPermissions: int64Ptr(discordgo.PermissionManageGuild),
		},
		{
			Name:                     "history",
			Description:              "Watch history timeline (live + VOD)",
			DefaultMemberPermissions: int64Ptr(discordgo.PermissionManageGuild),
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Client to drill into by username/IP (omit for a global timeline of all clients)", Required: false},
				{Type: discordgo.ApplicationCommandOptionString, Name: "alias", Description: "Client to drill into by its assigned alias instead of username/IP", Required: false},
				{Type: discordgo.ApplicationCommandOptionString, Name: "period", Description: "Time window", Required: false, Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "Last 24 hours", Value: "24h"},
					{Name: "Last 7 days", Value: "7d"},
					{Name: "Last 30 days", Value: "30d"},
					{Name: "Last 90 days", Value: "90d"},
					{Name: "All time", Value: "all"},
				}},
			},
		},
		{
			Name:                     "disconnect",
			Description:              "Forcibly disconnect a user",
			DefaultMemberPermissions: int64Ptr(discordgo.PermissionManageGuild),
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Username to disconnect", Required: true},
			},
		},
		{
			Name:                     "timeout",
			Description:              "Temporarily block a user for N minutes",
			DefaultMemberPermissions: int64Ptr(discordgo.PermissionManageGuild),
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Username to timeout", Required: true},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "minutes", Description: "Timeout duration in minutes (>0)", Required: true, MinValue: floatPtr(1)},
			},
		},
	}
}

func floatPtr(v float64) *float64 { return &v }
func int64Ptr(v int64) *int64     { return &v }

// registerSlashCommands registers commands globally or in a dev guild.
func (b *Bot) registerSlashCommands() error {
	if b.session == nil {
		return fmt.Errorf("session not initialized")
	}
	if b.session.State == nil || b.session.State.User == nil {
		return fmt.Errorf("session user not ready")
	}
	appID := b.session.State.User.ID
	guildID := b.devGuildID
	// If no explicit dev guild, and the bot is in exactly one guild, auto-scope to that for fast iteration.
	if guildID == "" && b.session.State != nil && len(b.session.State.Guilds) == 1 {
		guildID = b.session.State.Guilds[0].ID
		b.devGuildID = guildID
		utils.InfoLog("Slash commands: auto-using guild %s for development registration", guildID)
	}
	specs := b.commandSpecs()
	// Use BulkOverwrite to avoid duplicates and keep commands in sync
	var (
		cmds []*discordgo.ApplicationCommand
		err  error
	)
	if guildID != "" {
		cmds, err = b.session.ApplicationCommandBulkOverwrite(appID, guildID, specs)
	} else {
		cmds, err = b.session.ApplicationCommandBulkOverwrite(appID, "", specs)
	}
	if err != nil {
		return fmt.Errorf("bulk overwrite: %w", err)
	}
	b.registeredCommands = cmds
	scope := "global"
	if guildID != "" {
		scope = "guild:" + guildID
	}
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	utils.InfoLog("Slash commands registered (%s): %v", scope, names)
	return nil
}

// unregisterSlashCommands removes commands from dev guild (fast). Global deletions are slow, so skip if global.
func (b *Bot) unregisterSlashCommands() error {
	if b.session == nil || len(b.registeredCommands) == 0 {
		return nil
	}
	if b.devGuildID == "" {
		return nil
	}
	if b.session.State == nil || b.session.State.User == nil {
		return nil
	}
	appID := b.session.State.User.ID
	for _, cmd := range b.registeredCommands {
		_ = b.session.ApplicationCommandDelete(appID, b.devGuildID, cmd.ID)
	}
	b.registeredCommands = nil
	return nil
}

// cleanupExistingCommands deletes commands in the scope we plan to use before re-registering.
func (b *Bot) cleanupExistingCommands() error {
	if b.session == nil || b.session.State == nil || b.session.State.User == nil {
		return nil
	}
	appID := b.session.State.User.ID
	guildID := b.devGuildID
	// If no dev guild set but only one guild present, use it (consistent with registration decision)
	if guildID == "" && b.session.State != nil && len(b.session.State.Guilds) == 1 {
		guildID = b.session.State.Guilds[0].ID
	}
	// Clean only in the chosen scope to avoid accidentally wiping global commands when dev is intended
	cmds, err := b.session.ApplicationCommands(appID, guildID)
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if err := b.session.ApplicationCommandDelete(appID, guildID, c.ID); err != nil {
			utils.WarnLog("Failed to delete command %s in scope %s: %v", c.Name, func() string {
				if guildID != "" {
					return guildID
				}
				return "global"
			}(), err)
		}
	}
	return nil
}

// handleApplicationCommand routes slash commands to existing logic.
func (b *Bot) handleApplicationCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	name := i.ApplicationCommandData().Name

	switch name {
	case "help":
		ackEphemeral(s, i, "Loading commands…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleHelp(s, mc)

	case "link":
		username := optString(i, "username")
		ackEphemeral(s, i, "Linking…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleLink(s, mc, []string{username})

	case "linkadmin":
		if !b.requireAdmin(s, i) {
			return
		}
		discordID := optString(i, "discord_id")
		ldapUser := optString(i, "ldap_username")
		ackEphemeral(s, i, "Linking…")
		mc := toMessageCreateFromInteraction(i, "")
		b.linkDiscordAdmin(s, mc, []string{discordID, ldapUser})

	case "watch":
		query := optString(i, "query")
		days := int(optInt(i, "days"))
		if days <= 0 {
			days = 7
		}
		ackEphemeral(s, i, "Searching…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleVOD(s, mc, strings.Fields(query), days)

	case "library":
		ackEphemeral(s, i, "Fetching library…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleCachedList(s, mc)

	case "status":
		if !b.requireAdmin(s, i) {
			return
		}
		ackEphemeral(s, i, "Getting status…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleStatus(s, mc, nil)

	case "history":
		if !b.requireAdmin(s, i) {
			return
		}
		username := optString(i, "username")
		alias := optString(i, "alias")
		hours := periodToHours(optString(i, "period"))
		ackEphemeral(s, i, "Fetching history…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleHistory(s, mc, username, alias, hours)

	case "disconnect":
		if !b.requireAdmin(s, i) {
			return
		}
		username := optString(i, "username")
		ackEphemeral(s, i, "Disconnecting…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleDisconnect(s, mc, []string{username})

	case "timeout":
		if !b.requireAdmin(s, i) {
			return
		}
		username := optString(i, "username")
		minutes := int(optInt(i, "minutes"))
		ackEphemeral(s, i, "Applying timeout…")
		mc := toMessageCreateFromInteraction(i, "")
		b.handleTimeout(s, mc, []string{username, fmt.Sprintf("%d", minutes)})
	}
}

// Helpers to extract options
func optString(i *discordgo.InteractionCreate, name string) string {
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == name && o.StringValue() != "" {
			return o.StringValue()
		}
	}
	return ""
}
func optInt(i *discordgo.InteractionCreate, name string) int64 {
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == name {
			return o.IntValue()
		}
	}
	return 0
}

// requireAdmin checks the caller's admin role and, on failure, replies with an
// ephemeral permission-denied message. Returns true when access is granted, so
// callers can write `if !b.requireAdmin(s, i) { return }`.
func (b *Bot) requireAdmin(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if b.isAdmin(i.Member) {
		return true
	}
	ackEphemeral(s, i, "You don't have permission to use this command.")
	return false
}

// ackEphemeral sends an ephemeral "working on it" reply to clear Discord's spinner.
func ackEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:   discordgo.MessageFlagsEphemeral,
			Content: content,
		},
	})
}

// toMessageCreateFromInteraction builds a minimal MessageCreate to reuse legacy handlers
func toMessageCreateFromInteraction(i *discordgo.InteractionCreate, content string) *discordgo.MessageCreate {
	mc := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "", Content: content, Timestamp: time.Now(), ChannelID: channelIDFromInteraction(i)}}
	switch {
	case i.Member != nil && i.Member.User != nil:
		mc.Author = i.Member.User
		mc.GuildID = i.GuildID
	case i.User != nil:
		mc.Author = i.User
	default:
		// Fallback: empty user prevents nil dereference; handlers will fail gracefully
		// (e.g. LDAP lookup returns no user → "Link your account" reply)
		mc.Author = &discordgo.User{}
	}
	return mc
}

func channelIDFromInteraction(i *discordgo.InteractionCreate) string {
	if i.ChannelID != "" {
		return i.ChannelID
	}
	if i.Interaction != nil && i.ChannelID != "" {
		return i.ChannelID
	}
	return ""
}
