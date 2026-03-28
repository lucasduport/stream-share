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

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

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

	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    streams,
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

	utils.DebugLog("API: Found active stream %s with %d viewers", streamID, len(stream.GetViewers()))
	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    stream,
	})
}
