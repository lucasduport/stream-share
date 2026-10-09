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
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// startVODCacheFromSelection fires off a background cache request for the
// selected item. On failure it surfaces a retryable embed so the user can
// re-fire the request; retrying is idempotent because the server short-circuits
// streams that are already cached or downloading.
func (b *Bot) startVODCacheFromSelection(s *discordgo.Session, channelID, userID string, selected types.VODResult, days int) {
	if days <= 0 {
		days = 7
	}
	retryCtx := &retryContext{Kind: retryCache, UserID: userID, ChannelID: channelID, Selected: selected, Days: days}
	// Resolve LDAP
	ok, resp, err := b.makeAPIRequest("GET", "/discord/"+userID+"/ldap", nil)
	if err != nil || !ok {
		b.failWithRetry(channelID, retryCtx, "❌ Cache Start Failed", "Failed to retrieve your user information. Please try again later.")
		return
	}
	data, _ := resp.(map[string]interface{})
	ldapUser := getString(data, "ldap_user")
	if ldapUser == "" {
		b.warn(channelID, "🔗 Linking Required", "Your Discord account is not linked to an IPTV user.\n\nPlease link it first:\n`/link <ldap_username>`")
		return
	}

	payload := map[string]interface{}{
		"username":     ldapUser,
		"stream_id":    selected.StreamID,
		"type":         selected.StreamType,
		"title":        selected.Title,
		"series_title": selected.SeriesTitle,
		"season":       selected.Season,
		"episode":      selected.Episode,
		"days":         days,
	}
	ok, resp, err = b.makeAPIRequest("POST", "/cache/start", payload)
	if err != nil || !ok {
		b.failWithRetry(channelID, retryCtx, "❌ Cache Start Failed", apiErrorMessage("Failed to start caching", err, resp))
		return
	}
	d, _ := resp.(map[string]interface{})
	status := getString(d, "status")
	if status == "ready" {
		utils.DebugLog("Discord: cache hit — %s already cached", selected.StreamID)
	} else {
		utils.DebugLog("Discord: cache started for %s (status=%s, days=%d)", selected.StreamID, status, days)
	}
}

// checkCacheProgress queries /cache/by-stream/:streamid and reports the current
// cache state for a stream. It returns (found, status, percent, title, error).
// found is false when the stream is not present in the cache table at all.
func (b *Bot) checkCacheProgress(streamID string) (found bool, status string, percent int, title string, err error) {
	ok, resp, err := b.makeAPIRequest("GET", "/cache/by-stream/"+streamID, nil)
	if err != nil || !ok {
		return false, "", 0, "", err
	}
	d, _ := resp.(map[string]interface{})
	if d == nil {
		return false, "", 0, "", nil
	}
	status = getString(d, "status")
	title = getString(d, "title")
	if status == "" {
		return false, "", 0, "", nil
	}
	// Prefer the dedicated progress endpoint shape when available; the
	// by-stream payload carries downloaded/total bytes we can compute from.
	downloaded := getInt64(d, "downloaded_bytes")
	total := getInt64(d, "total_bytes")
	if total > 0 {
		percent = int((downloaded * 100) / total)
		if percent > 100 {
			percent = 100
		}
	} else if status == "ready" {
		percent = 100
	}
	return true, status, percent, title, nil
}
