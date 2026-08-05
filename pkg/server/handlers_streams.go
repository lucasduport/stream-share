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
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// ViewerInfo identifies one active viewer: a stable raw ID (the LDAP username,
// or — when LDAP is disabled — the client's IP address, which is the de-facto
// per-viewer identity; see resolveRequestUsername in server.go) plus a
// DisplayName resolved through any configured IP alias (see ip_aliases.go).
type ViewerInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// activeStreamItem is the API view of one active stream: display-resolved
// title/EPG id, current viewers, and (for live streams, when enabled) best-
// effort technical audio/video info from stream_probe.go.
type activeStreamItem struct {
	StreamID     string          `json:"stream_id"`
	StreamType   string          `json:"stream_type"`
	StreamTitle  string          `json:"stream_title"`
	EPGChannelID string          `json:"epg_channel_id,omitempty"`
	ViewerCount  int             `json:"viewer_count"`
	Viewers      []ViewerInfo    `json:"viewers"`
	StartedAt    time.Time       `json:"started_at"`
	Duration     string          `json:"duration"`
	Tech         *StreamTechInfo `json:"tech,omitempty"`
}

// buildActiveStreamItem resolves a stream session into its API view. Title
// resolution mirrors resolveTitleAtStart's fallback: the stored title is used
// when present and meaningful, otherwise a live channel-index/VOD lookup is
// attempted, falling back to the raw stream ID as a last resort so the field
// is never blank. aliases is the current IP->alias map (see loadIPAliasMap),
// loaded once by the caller and passed down so listing many streams doesn't
// mean one alias query per stream.
//
// This also attaches any cached technical info and kicks off a background
// refresh if the cache is stale/missing — never blocks the caller. Live
// streams are probed by sampling their shared upstream connection; VOD/series
// are probed straight off the local file once fully cached (see
// vodCacheFilePath).
func (c *Config) buildActiveStreamItem(s *types.StreamSession, aliases map[string]string) activeStreamItem {
	viewers := s.GetViewers()
	viewerInfos := make([]ViewerInfo, 0, len(viewers))
	for u := range viewers {
		viewerInfos = append(viewerInfos, ViewerInfo{ID: u, DisplayName: displayNameFor(u, aliases)})
	}

	title := strings.TrimSpace(s.StreamTitle)
	if title == "" || title == s.StreamID {
		if name, ok := c.resolveTitleAtStart(s.StreamID, s.StreamType); ok && strings.TrimSpace(name) != "" {
			title = name
		}
	}
	if title == "" {
		title = s.StreamID
	}

	epgID, _ := lookupEPGChannelID(normalizeStreamID(s.StreamID))
	dur := time.Since(s.StartTime).Truncate(time.Second)

	item := activeStreamItem{
		StreamID:     s.StreamID,
		StreamType:   s.StreamType,
		StreamTitle:  title,
		EPGChannelID: epgID,
		ViewerCount:  len(viewerInfos),
		Viewers:      viewerInfos,
		StartedAt:    s.StartTime,
		Duration:     dur.String(),
	}

	switch s.StreamType {
	case "live":
		if info, ok := getCachedTechInfo(s.StreamID); ok {
			item.Tech = info
		}
		c.warmLiveTechInfo(s.StreamID)
	case "movie", "series":
		if filePath, ok := c.vodCacheFilePath(s.StreamID); ok {
			if info, ok := getCachedTechInfo(s.StreamID); ok {
				item.Tech = info
			}
			c.warmVODTechInfo(s.StreamID, filePath)
		}
	}

	return item
}

// vodCacheFilePath returns the local file path for a stream's cache entry if
// it is fully downloaded and ready to probe. Cheap no-op (single map lookup
// via the DB layer, no ffprobe work) when the feature is disabled or the item
// isn't cached yet/at all.
func (c *Config) vodCacheFilePath(streamID string) (string, bool) {
	if !c.StreamTechProbeEnabled || c.db == nil {
		return "", false
	}
	entry, err := c.db.GetVODCache(streamID)
	if err != nil || entry == nil || strings.ToLower(entry.Status) != "ready" {
		return "", false
	}
	return entry.FilePath, true
}

// getAllStreams returns information about all active streams
func (c *Config) getAllStreams(ctx *gin.Context) {
	utils.DebugLog("API: Getting all active streams")

	if c.sessionManager == nil {
		utils.ErrorLog("Session manager is nil in getAllStreams")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Session manager not initialized",
		})
		return
	}

	streams := c.sessionManager.GetAllStreams()
	utils.DebugLog("API: Found %d active streams", len(streams))

	aliases := c.loadIPAliasMap()
	items := make([]activeStreamItem, 0, len(streams))
	for _, s := range streams {
		items = append(items, c.buildActiveStreamItem(s, aliases))
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    items,
	})
}

// getStreamInfo returns information about a specific stream
func (c *Config) getStreamInfo(ctx *gin.Context) {
	streamID := ctx.Param("streamid")
	utils.DebugLog("API: Getting stream info for: %s", streamID)

	if c.sessionManager == nil {
		utils.ErrorLog("Session manager is nil in getStreamInfo")
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Session manager not initialized",
		})
		return
	}

	stream, exists := c.sessionManager.GetStreamInfo(streamID)
	if !exists || !stream.Active {
		utils.DebugLog("API: Stream not found or inactive: %s", streamID)
		ctx.JSON(http.StatusNotFound, types.APIResponse{
			Success: false,
			Error:   "Stream not found or inactive",
		})
		return
	}

	utils.DebugLog("API: Found active %s with %d viewers", c.streamLabel(streamID), len(stream.GetViewers()))

	item := c.buildActiveStreamItem(stream, c.loadIPAliasMap())
	// This is a single-item lookup, so it's worth the extra latency of a
	// synchronous probe when there's nothing usable cached yet.
	if item.Tech == nil {
		switch stream.StreamType {
		case "live":
			if info := c.forceProbeLiveTechInfo(stream.StreamID); info != nil {
				item.Tech = info
			}
		case "movie", "series":
			if filePath, ok := c.vodCacheFilePath(stream.StreamID); ok {
				if info := c.forceProbeVODTechInfo(stream.StreamID, filePath); info != nil {
					item.Tech = info
				}
			}
		}
	}

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    item,
	})
}
