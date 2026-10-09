/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License
 * as published by the Free Software Foundation, either version 3 of the
 * Free Software Foundation, either version 3 of the License, or
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
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	// retryTTL bounds how long a retry context stays valid after the failure.
	retryTTL = 30 * time.Minute
	// retryBackoff is the minimum delay between two retry attempts on the same
	// message, so a failing provider is not hammered.
	retryBackoff = 5 * time.Second
	// retryMaxAttempts caps the number of retries per failure message.
	retryMaxAttempts = 5
)

// handleRetry processes clicks on the "retry" button attached to failure embeds.
// It re-fires the original internal API request (search / download / cache start)
// with the stored parameters and edits the embed with the new outcome.
func (b *Bot) handleRetry(s *discordgo.Session, i *discordgo.InteractionCreate, msgID string) {
	b.retryLock.Lock()
	ctx, ok := b.pendingRetry[msgID]
	if !ok {
		b.retryLock.Unlock()
		b.ackRetryEphemeral(s, i, "This retry option has expired. Please run the command again.")
		return
	}
	if deny := b.retryGuard(ctx, i); deny != "" {
		b.retryLock.Unlock()
		b.ackRetryEphemeral(s, i, deny)
		return
	}
	ctx.InFlight = true
	ctx.Attempts++
	ctx.LastAttempt = time.Now()
	channelID := ctx.ChannelID
	kind := ctx.Kind
	b.retryLock.Unlock()

	// Acknowledge the click and disable the button while the retry runs.
	retryEmbeds := []*discordgo.MessageEmbed{{
		Title:       "🔁 Retrying…",
		Description: retryProgressDescription(kind),
		Color:       colorInfo,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}}
	retryComponents := retryButtonRow(true)
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     retryEmbeds,
			Components: retryComponents,
		},
	})

	go func() {
		defer func() {
			b.retryLock.Lock()
			if c, ok := b.pendingRetry[msgID]; ok && c == ctx {
				c.InFlight = false
			}
			b.retryLock.Unlock()
		}()
		b.executeRetry(s, msgID, channelID, ctx)
	}()
}

// ackRetryEphemeral sends a short ephemeral reply to a retry interaction.
func (b *Bot) ackRetryEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: content},
	})
}

// retryGuard returns a non-empty denial message when the retry click must be
// rejected (wrong user, already in flight, too many attempts, backoff window).
// The caller must hold b.retryLock.
func (b *Bot) retryGuard(ctx *retryContext, i *discordgo.InteractionCreate) string {
	if !b.isSameUser(ctx.UserID, i) {
		return "Only the user who triggered this request can retry it."
	}
	if ctx.InFlight {
		return "A retry is already in progress…"
	}
	if ctx.Attempts >= retryMaxAttempts {
		return "Too many retry attempts. Please run the command again."
	}
	if !ctx.LastAttempt.IsZero() && time.Since(ctx.LastAttempt) < retryBackoff {
		return "Please wait a moment before retrying again."
	}
	return ""
}

// retryProgressDescription returns the "Retrying…" embed body for a given kind.
func retryProgressDescription(kind retryKind) string {
	switch kind {
	case retrySearch:
		return "Re-running your search…"
	case retryDownload:
		return "Re-creating your download link…"
	case retryCache:
		return "Re-starting the cache request…"
	default:
		return "Retrying…"
	}
}

// executeRetry re-fires the stored request and edits the failure embed with the
// outcome. A retry always issues a NEW internal API request; the original
// interaction token is never replayed.
func (b *Bot) executeRetry(s *discordgo.Session, msgID, channelID string, ctx *retryContext) {
	msg := &discordgo.Message{ID: msgID, ChannelID: channelID}

	switch ctx.Kind {
	case retrySearch:
		b.runVODSearch(s, channelID, ctx.UserID, ctx.Query, ctx.Days, msg)

	case retryDownload:
		b.startVODDownloadFromSelection(s, channelID, ctx.UserID, ctx.Selected)
		// startVODDownloadFromSelection sends a NEW message on both success and
		// failure; replace the old failure embed with a short confirmation so the
		// channel does not keep a stale retry button around.
		b.finishRetryMessage(s, msg, colorInfo, "🔁 Retry Sent", "The download request was re-submitted. See the new message for the result.")

	case retryCache:
		b.retryCacheStart(s, msg, ctx)
	}
}

// retryCacheStart re-fires /cache/start for a stored selection. Because the
// original /cache/start is fire-and-forget and may already be running
// server-side after a bot-side timeout, it first checks /cache/by-stream to
// surface existing progress instead of blindly re-starting the download.
func (b *Bot) retryCacheStart(s *discordgo.Session, msg *discordgo.Message, ctx *retryContext) {
	channelID := ctx.ChannelID
	selected := ctx.Selected

	// Idempotency check: if the stream is already tracked server-side, report
	// the existing progress instead of duplicating the download.
	if found, status, percent, title, err := b.checkCacheProgress(selected.StreamID); err == nil && found {
		titleText := title
		if titleText == "" {
			titleText = selected.Title
		}
		var desc string
		switch status {
		case "ready":
			desc = fmt.Sprintf("✅ `%s` is already cached. No new download was started.", titleText)
		case "downloading":
			desc = fmt.Sprintf("⏳ `%s` is already downloading (%d%%). No new download was started.", titleText, percent)
		default:
			desc = fmt.Sprintf("ℹ️ `%s` is already tracked (status: %s). No new download was started.", titleText, status)
		}
		b.finishRetryMessage(s, msg, colorSuccess, "🔁 Cache Already Active", desc)
		return
	}

	// Not tracked yet: re-fire the cache start.
	b.startVODCacheFromSelection(s, channelID, ctx.UserID, selected, ctx.Days)
	b.finishRetryMessage(s, msg, colorInfo, "🔁 Cache Retry Sent", "The cache request was re-submitted. Check `/library` for progress.")
}

// finishRetryMessage replaces the "Retrying…" placeholder embed with a final
// state and removes the retry button so it cannot be clicked again.
func (b *Bot) finishRetryMessage(s *discordgo.Session, msg *discordgo.Message, color int, title, desc string) {
	if msg == nil {
		return
	}
	empty := []discordgo.MessageComponent{}
	if err := editEmbedWithComponents(s, msg.ChannelID, msg.ID, color, title, desc, empty); err != nil {
		utils.WarnLog("Discord: failed to finalize retry message: %v", err)
	}
	b.retryLock.Lock()
	delete(b.pendingRetry, msg.ID)
	b.retryLock.Unlock()
}

// clearRetry removes any retry context attached to msg (e.g. when the retried
// operation finally succeeds and the message is repurposed).
func (b *Bot) clearRetry(msg *discordgo.Message) {
	if msg == nil {
		return
	}
	b.retryLock.Lock()
	delete(b.pendingRetry, msg.ID)
	b.retryLock.Unlock()
}

// cleanupExpiredRetries removes retry contexts older than retryTTL.
func (b *Bot) cleanupExpiredRetries() {
	b.retryLock.Lock()
	defer b.retryLock.Unlock()
	cutoff := time.Now().Add(-retryTTL)
	for msgID, ctx := range b.pendingRetry {
		if ctx.Created.Before(cutoff) {
			delete(b.pendingRetry, msgID)
		}
	}
}
