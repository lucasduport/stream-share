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
    "time"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// Common embed colors
const (
    colorInfo    = 0x5BC0DE // teal-ish
    colorSuccess = 0x28A745 // green
    colorWarn    = 0xFFC107 // amber
    colorError   = 0xDC3545 // red
)

// sendEmbed is a small helper to send a styled embed.
func (b *Bot) sendEmbed(channelID string, color int, title, description string, fields ...*discordgo.MessageEmbedField) error {
    embed := &discordgo.MessageEmbed{
        Title:       title,
        Description: description,
        Color:       color,
        Timestamp:   time.Now().UTC().Format(time.RFC3339),
    }
    if len(fields) > 0 {
        embed.Fields = make([]*discordgo.MessageEmbedField, 0, len(fields))
        for _, f := range fields {
            if f != nil {
                embed.Fields = append(embed.Fields, f)
            }
        }
    }
    _, err := b.session.ChannelMessageSendEmbed(channelID, embed)
    return err
}

// Convenience wrappers with fixed color themes.
func (b *Bot) info(channelID, title, desc string, fields ...*discordgo.MessageEmbedField) {
    if err := b.sendEmbed(channelID, colorInfo, title, desc, fields...); err != nil {
        utils.ErrorLog("Discord: failed to send info embed: %v", err)
    }
}
func (b *Bot) success(channelID, title, desc string, fields ...*discordgo.MessageEmbedField) {
    if err := b.sendEmbed(channelID, colorSuccess, title, desc, fields...); err != nil {
        utils.ErrorLog("Discord: failed to send success embed: %v", err)
    }
}
func (b *Bot) warn(channelID, title, desc string, fields ...*discordgo.MessageEmbedField) {
    if err := b.sendEmbed(channelID, colorWarn, title, desc, fields...); err != nil {
        utils.ErrorLog("Discord: failed to send warning embed: %v", err)
    }
}
func (b *Bot) fail(channelID, title, desc string, fields ...*discordgo.MessageEmbedField) {
    if err := b.sendEmbed(channelID, colorError, title, desc, fields...); err != nil {
        utils.ErrorLog("Discord: failed to send error embed: %v", err)
    }
}

// editEmbed transforms a previously sent embed message into another embed in-place.
func editEmbed(s *discordgo.Session, msg *discordgo.Message, color int, title, desc string) error {
    if msg == nil { return nil }
    embed := &discordgo.MessageEmbed{Title: title, Description: desc, Color: color, Timestamp: time.Now().UTC().Format(time.RFC3339)}
    embeds := []*discordgo.MessageEmbed{embed}
    _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: msg.ID, Channel: msg.ChannelID, Embeds: &embeds})
    return err
}
