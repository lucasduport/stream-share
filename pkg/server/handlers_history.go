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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// sinceFromHours converts an hours window into a lower-bound time.
// hours <= 0 yields the zero time (no lower bound / all-time).
func sinceFromHours(hours int) time.Time {
	if hours > 0 {
		return time.Now().Add(-time.Duration(hours) * time.Hour)
	}
	return time.Time{}
}

// pagingFromQuery reads `limit`/`offset` query params, applying defaultLimit
// when limit is unset or invalid and capping it at maxLimit.
func pagingFromQuery(ctx *gin.Context, defaultLimit, maxLimit int) (limit, offset int) {
	limit = defaultLimit
	if v, err := strconv.Atoi(ctx.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if v, err := strconv.Atoi(ctx.Query("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// getHistoryFeed GET /api/internal/history?hours=N&limit=N&offset=N — a chronological
// timeline of watch events across ALL clients (newest first), each annotated with who
// watched, the channel/VOD name, type and duration. Defaults to the 40 most recent
// events; pass limit/offset to page through more (dashboards typically want this).
func (c *Config) getHistoryFeed(ctx *gin.Context) {
	if c.db == nil {
		utils.ErrorLog("Database is nil in getHistoryFeed")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Database not initialized",
		})
		return
	}

	hours, _ := strconv.Atoi(ctx.Query("hours"))
	since := sinceFromHours(hours)
	limit, offset := pagingFromQuery(ctx, 40, 500)

	entries, err := c.db.GetRecentHistory(since, limit, offset)
	if err != nil {
		utils.ErrorLog("Failed to get history feed: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to get history feed: %v", err),
		})
		return
	}

	total, err := c.db.CountHistory("", since)
	if err != nil {
		utils.WarnLog("Failed to count history feed: %v", err)
	}

	aliases := c.loadIPAliasMap()
	type item struct {
		Username    string    `json:"username"`
		DisplayName string    `json:"display_name"`
		StreamID    string    `json:"stream_id"`
		StreamType  string    `json:"stream_type"`
		StreamTitle string    `json:"stream_title"`
		StartTime   time.Time `json:"start_time"`
		DurationSec int64     `json:"duration_sec"`
	}
	feedItems := make([]item, 0, len(entries))
	for _, e := range entries {
		feedItems = append(feedItems, item{
			Username:    e.Username,
			DisplayName: displayNameFor(e.Username, aliases),
			StreamID:    e.StreamID,
			StreamType:  e.StreamType,
			StreamTitle: c.historyLabel(e.StreamTitle, e.StreamID),
			StartTime:   e.StartTime,
			DurationSec: e.DurationSec,
		})
	}

	var b strings.Builder
	if len(feedItems) == 0 {
		b.WriteString("No watch history in this period.")
	} else {
		for _, it := range feedItems {
			dur := utils.HumanDuration(time.Duration(it.DurationSec) * time.Second)
			fmt.Fprintf(&b, "- %s · %s · %s [%s] (%s)\n",
				it.StartTime.Format("01-02 15:04"), it.DisplayName, it.StreamTitle, it.StreamType, dur,
			)
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"feed":   feedItems,
			"text":   b.String(),
			"hours":  hours,
			"count":  len(feedItems),
			"total":  total,
			"limit":  limit,
			"offset": offset,
		},
	})
}

// historyLabel resolves the best display name for a history row: the stored title
// when present, otherwise the cached channel/VOD name index (no network I/O),
// falling back to the raw stream id.
func (c *Config) historyLabel(storedTitle, streamID string) string {
	if t := strings.TrimSpace(storedTitle); t != "" {
		return t
	}
	if name, ok := c.resolveStreamName(streamID); ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return streamID
}

// getUserHistory GET /api/internal/history/:username?hours=N — one user's rows.
func (c *Config) getUserHistory(ctx *gin.Context) {
	if c.db == nil {
		utils.ErrorLog("Database is nil in getUserHistory")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Database not initialized",
		})
		return
	}

	username := ctx.Param("username")
	if username == "" {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{
			Success: false,
			Error:   "username is required",
		})
		return
	}

	hours, _ := strconv.Atoi(ctx.Query("hours"))
	since := sinceFromHours(hours)
	limit, offset := pagingFromQuery(ctx, 50, 500)

	entries, err := c.db.GetUserHistory(username, since, limit, offset)
	if err != nil {
		utils.ErrorLog("Failed to get user history: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to get user history: %v", err),
		})
		return
	}

	total, err := c.db.CountHistory(username, since)
	if err != nil {
		utils.WarnLog("Failed to count user history: %v", err)
	}

	type item struct {
		StreamID    string    `json:"stream_id"`
		StreamType  string    `json:"stream_type"`
		StreamTitle string    `json:"stream_title"`
		StartTime   time.Time `json:"start_time"`
		DurationSec int64     `json:"duration_sec"`
	}
	entryItems := make([]item, 0, len(entries))
	for _, e := range entries {
		entryItems = append(entryItems, item{
			StreamID:    e.StreamID,
			StreamType:  e.StreamType,
			StreamTitle: c.historyLabel(e.StreamTitle, e.StreamID),
			StartTime:   e.StartTime,
			DurationSec: e.DurationSec,
		})
	}

	var b strings.Builder
	if len(entryItems) == 0 {
		fmt.Fprintf(&b, "No watch history for %s in this period.", username)
	} else {
		for _, it := range entryItems {
			dur := utils.HumanDuration(time.Duration(it.DurationSec) * time.Second)
			fmt.Fprintf(&b, "- %s · %s [%s] (%s)\n",
				it.StartTime.Format("01-02 15:04"), it.StreamTitle, it.StreamType, dur,
			)
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"username":     username,
			"display_name": displayNameFor(username, c.loadIPAliasMap()),
			"entries":      entryItems,
			"text":         b.String(),
			"hours":        hours,
			"count":        len(entryItems),
			"total":        total,
			"limit":        limit,
			"offset":       offset,
		},
	})
}
