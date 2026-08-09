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
// selected item and returns immediately. It is fire-and-forget: the download
// link embed (sent separately by startVODDownloadFromSelection) is the
// user-facing response, and the user can check /library for cache status.
func (b *Bot) startVODCacheFromSelection(s *discordgo.Session, channelID, userID string, selected types.VODResult, days int) {
	// Resolve LDAP
	ok, resp, err := b.makeAPIRequest("GET", "/discord/"+userID+"/ldap", nil)
	if err != nil || !ok {
		utils.WarnLog("Discord: cache start failed to resolve LDAP for %s: %v", userID, err)
		return
	}
	data, _ := resp.(map[string]interface{})
	ldapUser := getString(data, "ldap_user")
	if ldapUser == "" {
		utils.WarnLog("Discord: cache start aborted — no LDAP user linked to %s", userID)
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
		utils.WarnLog("Discord: cache start failed for %s: %v", selected.StreamID, err)
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
