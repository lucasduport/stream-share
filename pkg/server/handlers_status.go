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

package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// statusSummary returns a compact summary of who is watching what
func (c *Config) statusSummary(ctx *gin.Context) {
	if c.sessionManager == nil {
		utils.ErrorLog("Session manager is nil in statusSummary")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Session manager not initialized",
		})
		return
	}

	streams := c.sessionManager.GetAllStreams()
	type item struct {
		StreamID    string    `json:"stream_id"`
		StreamType  string    `json:"stream_type"`
		StreamTitle string    `json:"stream_title"`
		ViewerCount int       `json:"viewer_count"`
		Viewers     []string  `json:"viewers"`
		StartedAt   time.Time `json:"started_at"`
		Duration    string    `json:"duration"`
	}
	summary := make([]item, 0, len(streams))

	for _, s := range streams {
		if !s.Active {
			continue
		}
		viewers := s.GetViewers()
		names := make([]string, 0, len(viewers))
		for u := range viewers {
			names = append(names, u) // LDAP username
		}
		dur := time.Since(s.StartTime).Truncate(time.Second)

		// Prefer channel name from M3U mapping if StreamTitle is empty or equals the ID
		title := strings.TrimSpace(s.StreamTitle)
		if title == "" || title == s.StreamID {
			if name, ok := c.getChannelNameByID(s.StreamID); ok && strings.TrimSpace(name) != "" {
				title = name
			}
		}

		summary = append(summary, item{
			StreamID:    s.StreamID,
			StreamType:  s.StreamType,
			StreamTitle: title,
			ViewerCount: len(names),
			Viewers:     names,
			StartedAt:   s.StartTime,
			Duration:    dur.String(),
		})
	}

	// Derive user and stream counts
	allSessions := c.sessionManager.GetAllSessions()
	activeUserSet := make(map[string]struct{}, len(allSessions))
	for _, us := range allSessions {
	if us.StreamID != "" {
			activeUserSet[us.Username] = struct{}{}
		}
	}
	activeUsers := make([]string, 0, len(activeUserSet))
	for u := range activeUserSet {
		activeUsers = append(activeUsers, u)
	}

	var b strings.Builder
	if len(summary) == 0 {
		b.WriteString("No active streams.")
	} else {
		b.WriteString("Active streams:\n")
		for _, it := range summary {
			title := it.StreamTitle
			if strings.TrimSpace(title) == "" {
				title = it.StreamID
			}
			b.WriteString(fmt.Sprintf(
				"- %s [%s] — %d viewer(s): %s (since %s)\n",
				title, it.StreamType, it.ViewerCount, strings.Join(it.Viewers, ", "), it.Duration,
			))
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"summary":            summary,
			"text":               b.String(),
			"streams_count":      len(summary),
			"users_count_total":  len(allSessions),
			"users_count_active": len(activeUserSet),
			"active_users":       activeUsers,
		},
	})
}
