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
	if msg == nil {
		return nil
	}
	embed := &discordgo.MessageEmbed{Title: title, Description: desc, Color: color, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	embeds := []*discordgo.MessageEmbed{embed}
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: msg.ID, Channel: msg.ChannelID, Embeds: &embeds})
	return err
}

// editEmbedWithComponents is like editEmbed but also replaces the message
// components (e.g. to attach or disable a Retry button).
func editEmbedWithComponents(s *discordgo.Session, channelID, msgID string, color int, title, desc string, components []discordgo.MessageComponent) error {
	embed := &discordgo.MessageEmbed{Title: title, Description: desc, Color: color, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	embeds := []*discordgo.MessageEmbed{embed}
	edit := &discordgo.MessageEdit{ID: msgID, Channel: channelID, Embeds: &embeds}
	if components != nil {
		edit.Components = &components
	}
	_, err := s.ChannelMessageEditComplex(edit)
	return err
}

// retryButtonRow builds the single-button action row used on failure embeds.
// disabled is set while a retry is in flight or when the context is missing.
func retryButtonRow(disabled bool) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Style: discordgo.PrimaryButton, Label: "🔁 Retry", CustomID: "retry", Disabled: disabled},
		}},
	}
}

// failWithRetry sends a failure embed with a Retry button and registers a retry
// context so the button can re-fire the original request. It returns the sent
// message so callers can re-key the context if the send path differs.
func (b *Bot) failWithRetry(channelID string, ctx *retryContext, title, desc string) {
	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: desc,
		Color:       colorError,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	msg, err := b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: retryButtonRow(false),
	})
	if err != nil {
		utils.ErrorLog("Discord: failed to send retryable error embed: %v", err)
		b.fail(channelID, title, desc)
		return
	}
	ctx.Created = time.Now()
	b.retryLock.Lock()
	b.pendingRetry[msg.ID] = ctx
	b.retryLock.Unlock()
}

// editFailWithRetry transforms an existing message (e.g. a loading embed) into a
// failure embed with a Retry button, keyed by that message's ID.
func (b *Bot) editFailWithRetry(s *discordgo.Session, msg *discordgo.Message, ctx *retryContext, title, desc string) {
	if msg == nil {
		b.failWithRetry(ctx.ChannelID, ctx, title, desc)
		return
	}
	ctx.Created = time.Now()
	if err := editEmbedWithComponents(s, msg.ChannelID, msg.ID, colorError, title, desc, retryButtonRow(false)); err != nil {
		utils.ErrorLog("Discord: failed to edit failure embed with retry: %v", err)
		return
	}
	b.retryLock.Lock()
	b.pendingRetry[msg.ID] = ctx
	b.retryLock.Unlock()
}
