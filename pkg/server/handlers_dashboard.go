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
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// getInstanceInfo GET /api/internal/instance — identifies this deployment so an
// external dashboard aggregating several stream-share instances can tell them
// apart and label the data it pulls from each one's API.
func (c *Config) getInstanceInfo(ctx *gin.Context) {
	name := c.InstanceName
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"name":              name,
			"uptime_seconds":    int64(time.Since(c.startTime).Seconds()),
			"started_at":        c.startTime,
			"discord_enabled":   c.discordBot != nil,
			"vod_cache_enabled": c.VODCacheEnabled,
			"catchup_enabled":   c.CatchupEnabled,
			"db_connected":      c.db != nil,
		},
	})
}

// getDashboardStats GET /api/internal/stats?hours=N — aggregate figures for a
// dashboard overview: current live activity plus historical totals and
// leaderboards over the requested window (defaults to 24h; hours<=0 means
// all-time).
func (c *Config) getDashboardStats(ctx *gin.Context) {
	hours := 24
	if v, err := strconv.Atoi(ctx.Query("hours")); err == nil {
		hours = v
	}
	since := sinceFromHours(hours)

	data := map[string]interface{}{
		"hours": hours,
	}

	// Live activity, straight from in-memory session state.
	if c.sessionManager == nil {
		utils.ErrorLog("Session manager is nil in getDashboardStats")
	} else {
		streams := c.sessionManager.GetAllStreams()
		activeStreams := 0
		for _, s := range streams {
			if s.Active {
				activeStreams++
			}
		}
		allSessions := c.sessionManager.GetAllSessions()
		activeUserSet := make(map[string]struct{}, len(allSessions))
		for _, us := range allSessions {
			if us.StreamID != "" {
				activeUserSet[us.Username] = struct{}{}
			}
		}
		data["active_streams"] = activeStreams
		data["active_viewers"] = len(activeUserSet)
	}

	// Historical totals and leaderboards, from stream_history.
	if c.db == nil {
		utils.ErrorLog("Database is nil in getDashboardStats")
		ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: data})
		return
	}

	if watch, err := c.db.GetWatchStats(since); err == nil {
		data["sessions"] = watch.Sessions
		data["unique_viewers"] = watch.UniqueViewers
		data["watch_seconds"] = watch.WatchSeconds
	} else {
		utils.ErrorLog("Failed to get watch stats: %v", err)
	}

	const topN = 10
	if titles, err := c.db.GetTopTitles(since, topN); err == nil {
		type item struct {
			StreamID     string `json:"stream_id"`
			StreamType   string `json:"stream_type"`
			StreamTitle  string `json:"stream_title"`
			Views        int    `json:"views"`
			WatchSeconds int64  `json:"watch_seconds"`
		}
		out := make([]item, 0, len(titles))
		for _, t := range titles {
			out = append(out, item{
				StreamID:     t.StreamID,
				StreamType:   t.StreamType,
				StreamTitle:  c.historyLabel(t.StreamTitle, t.StreamID),
				Views:        t.Views,
				WatchSeconds: t.WatchSeconds,
			})
		}
		data["top_titles"] = out
	} else {
		utils.ErrorLog("Failed to get top titles: %v", err)
	}

	if users, err := c.db.GetTopUsers(since, topN); err == nil {
		type item struct {
			Username     string `json:"username"`
			Sessions     int    `json:"sessions"`
			WatchSeconds int64  `json:"watch_seconds"`
		}
		out := make([]item, 0, len(users))
		for _, u := range users {
			out = append(out, item{Username: u.Username, Sessions: u.Sessions, WatchSeconds: u.WatchSeconds})
		}
		data["top_users"] = out
	} else {
		utils.ErrorLog("Failed to get top users: %v", err)
	}

	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: data})
}
