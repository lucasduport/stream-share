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
    "time"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// command definitions
func (b *Bot) commandSpecs() []*discordgo.ApplicationCommand {
    return []*discordgo.ApplicationCommand{
        {
            Name:        "vod",
            Description: "Search movies and shows; pick from a dropdown",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "query", Description: "Title to search (supports S01E02)", Required: true},
            },
        },
        {
            Name:        "link",
            Description: "Link your Discord account to your IPTV (LDAP) user",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Your LDAP username", Required: true},
            },
        },
        {
            Name:        "cache",
            Description: "Cache a movie/episode on the server (max 14 days)",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "title", Description: "Movie or series title (supports S01E02)", Required: true},
                {Type: discordgo.ApplicationCommandOptionInteger, Name: "days", Description: "Days to keep cached (1–14)", Required: true, MinValue: floatPtr(1), MaxValue: 14},
            },
        },
        {
            Name:        "cached",
            Description: "List cached items and when they expire",
        },
        {
            Name:        "status",
            Description: "Show active streams and users",
        },
        {
            Name:        "disconnect",
            Description: "Forcibly disconnect a user",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Username to disconnect", Required: true},
            },
        },
        {
            Name:        "timeout",
            Description: "Temporarily block a user for N minutes",
            Options: []*discordgo.ApplicationCommandOption{
                {Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Username to timeout", Required: true},
                {Type: discordgo.ApplicationCommandOptionInteger, Name: "minutes", Description: "Timeout duration in minutes (>0)", Required: true, MinValue: floatPtr(1)},
            },
        },
    }
}

func floatPtr(v float64) *float64 { return &v }

// registerSlashCommands registers commands globally or in a dev guild.
func (b *Bot) registerSlashCommands() error {
    if b.session == nil { return fmt.Errorf("session not initialized") }
    if b.session.State == nil || b.session.State.User == nil { return fmt.Errorf("session user not ready") }
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
        err error
    )
    if guildID != "" {
        cmds, err = b.session.ApplicationCommandBulkOverwrite(appID, guildID, specs)
    } else {
        cmds, err = b.session.ApplicationCommandBulkOverwrite(appID, "", specs)
    }
    if err != nil { return fmt.Errorf("bulk overwrite: %w", err) }
    b.registeredCommands = cmds
    scope := "global"; if guildID != "" { scope = "guild:" + guildID }
    names := make([]string, 0, len(cmds))
    for _, c := range cmds { names = append(names, c.Name) }
    utils.InfoLog("Slash commands registered (%s): %v", scope, names)
    return nil
}

// unregisterSlashCommands removes commands from dev guild (fast). Global deletions are slow, so skip if global.
func (b *Bot) unregisterSlashCommands() error {
    if b.session == nil || len(b.registeredCommands) == 0 { return nil }
    if b.devGuildID == "" { return nil }
    if b.session.State == nil || b.session.State.User == nil { return nil }
    appID := b.session.State.User.ID
    for _, cmd := range b.registeredCommands {
        _ = b.session.ApplicationCommandDelete(appID, b.devGuildID, cmd.ID)
    }
    b.registeredCommands = nil
    return nil
}

// cleanupExistingCommands deletes commands in the scope we plan to use before re-registering.
func (b *Bot) cleanupExistingCommands() error {
    if b.session == nil || b.session.State == nil || b.session.State.User == nil { return nil }
    appID := b.session.State.User.ID
    guildID := b.devGuildID
    // If no dev guild set but only one guild present, use it (consistent with registration decision)
    if guildID == "" && b.session.State != nil && len(b.session.State.Guilds) == 1 {
        guildID = b.session.State.Guilds[0].ID
    }
    // Clean only in the chosen scope to avoid accidentally wiping global commands when dev is intended
    cmds, err := b.session.ApplicationCommands(appID, guildID)
    if err != nil { return err }
    for _, c := range cmds {
        if err := b.session.ApplicationCommandDelete(appID, guildID, c.ID); err != nil {
            utils.WarnLog("Failed to delete command %s in scope %s: %v", c.Name, func() string { if guildID != "" { return guildID } ; return "global" }(), err)
        }
    }
    return nil
}

// handleApplicationCommand routes slash commands to existing logic.
func (b *Bot) handleApplicationCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    if i.Type != discordgo.InteractionApplicationCommand { return }
    name := i.ApplicationCommandData().Name

    switch name {
    case "link":
        username := optString(i, "username")
        // Immediate ephemeral ack to avoid spinner
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Linking…"}})
    // Reuse existing handler via a minimal MessageCreate without legacy prefix
    mc := toMessageCreateFromInteraction(i, "")
    b.handleLink(s, mc, []string{username})

    case "vod":
        query := optString(i, "query")
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Searching…"}})
    mc := toMessageCreateFromInteraction(i, "")
    b.handleVOD(s, mc, strings.Fields(query))

    case "cache":
        title := optString(i, "title")
        days := int(optInt(i, "days"))
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Preparing cache…"}})
    mc := toMessageCreateFromInteraction(i, "")
    b.handleCache(s, mc, append(strings.Fields(title), strconv.Itoa(days)))

    case "cached":
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Fetching cached list…"}})
    mc := toMessageCreateFromInteraction(i, "")
        b.handleCachedList(s, mc)

    case "status":
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Getting status…"}})
    mc := toMessageCreateFromInteraction(i, "")
        b.handleStatus(s, mc, nil)

    case "disconnect":
        username := optString(i, "username")
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Disconnecting…"}})
    mc := toMessageCreateFromInteraction(i, "")
        b.handleDisconnect(s, mc, []string{username})

    case "timeout":
        username := optString(i, "username")
        minutes := int(optInt(i, "minutes"))
        _ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: "Applying timeout…"}})
    mc := toMessageCreateFromInteraction(i, "")
        b.handleTimeout(s, mc, []string{username, fmt.Sprintf("%d", minutes)})
    }
}

// Helpers to extract options
func optString(i *discordgo.InteractionCreate, name string) string {
    for _, o := range i.ApplicationCommandData().Options {
        if o.Name == name && o.StringValue() != "" { return o.StringValue() }
    }
    return ""
}
func optInt(i *discordgo.InteractionCreate, name string) int64 {
    for _, o := range i.ApplicationCommandData().Options {
        if o.Name == name { return o.IntValue() }
    }
    return 0
}

// toMessageCreateFromInteraction builds a minimal MessageCreate to reuse legacy handlers
func toMessageCreateFromInteraction(i *discordgo.InteractionCreate, content string) *discordgo.MessageCreate {
    mc := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "", Content: content, Timestamp: time.Now(), ChannelID: channelIDFromInteraction(i)}}
    if i.Member != nil && i.Member.User != nil {
        mc.Author = i.Member.User
        mc.GuildID = i.GuildID
    } else if i.User != nil {
        mc.Author = i.User
        mc.GuildID = ""
    } else {
        // Fallback: empty user prevents nil dereference; handlers will fail gracefully
        // (e.g. LDAP lookup returns no user → "Link your account" reply)
        mc.Author = &discordgo.User{}
    }
    return mc
}

func channelIDFromInteraction(i *discordgo.InteractionCreate) string {
    if i.ChannelID != "" { return i.ChannelID }
    if i.Interaction != nil && i.Interaction.ChannelID != "" { return i.Interaction.ChannelID }
    return ""
}

// closeInteractionThinking edits the original interaction response to clear the "thinking…" state
func closeInteractionThinking(s *discordgo.Session, i *discordgo.InteractionCreate) {
    // Best-effort: edit to a minimal ephemeral note; do not delete to avoid UI ‘interaction failed’ glitches
    note := ""
    _, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &note})
}
