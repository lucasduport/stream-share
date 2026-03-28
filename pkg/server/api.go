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
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// setupInternalAPI configures internal API routes for bot communication
func (c *Config) setupInternalAPI(r *gin.Engine) {
	utils.InfoLog("Setting up internal API endpoints")

	// Check if database is initialized
	if c.db == nil {
		utils.ErrorLog("CRITICAL: Database not initialized when setting up API")
	}

	// Check if session manager is initialized
	if c.sessionManager == nil {
		utils.ErrorLog("CRITICAL: Session manager not initialized when setting up API")
	}

	api := r.Group("/api/internal")
	api.Use(c.apiKeyAuth())

	// Add recovery middleware to prevent panics from taking down the server
	api.Use(gin.Recovery())
	api.Use(func(ctx *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				utils.ErrorLog("API PANIC RECOVERED: %v\nStack trace: %s", err, debug.Stack())
				ctx.AbortWithStatusJSON(http.StatusInternalServerError, types.APIResponse{
					Success: false,
					Error:   fmt.Sprintf("Internal server error: %v", err),
				})
			}
		}()
		ctx.Next()
	})

	// User management endpoints
	api.GET("/users", c.getAllUsers)
	api.GET("/users/:username", c.getUserInfo)
	api.POST("/users/disconnect/:username", c.disconnectUser)
	api.POST("/users/timeout/:username", c.timeoutUser)

	// Stream management endpoints
	api.GET("/streams", c.getAllStreams)
	api.GET("/streams/:streamid", c.getStreamInfo)

	// Discord integration endpoints
	api.POST("/discord/link", c.linkDiscordUser)
	api.GET("/discord/:discordid/ldap", c.getLDAPFromDiscord)

	// VOD search and download endpoints
	api.POST("/vod/search", c.searchVOD)
	api.POST("/vod/enrich", c.enrichVODPage)
	api.POST("/vod/download", c.createVODDownload)
	api.GET("/vod/status/:requestid", c.getVODRequestStatus)

	// Caching endpoints (used by Discord)
	api.POST("/cache/start", c.startCache)
	api.GET("/cache/by-stream/:streamid", c.getCacheByStream)
	api.GET("/cache/progress/:streamid", c.getCacheProgress)
	api.GET("/cache/list", c.listCache)

	// Status summary for Discord and dashboards
	api.GET("/status", c.statusSummary)

	// Debug endpoint to verify API is working
	api.GET("/ping", func(ctx *gin.Context) {
		utils.DebugLog("API ping received")
		ctx.JSON(http.StatusOK, types.APIResponse{
			Success: true,
			Message: "API is running",
			Data: map[string]interface{}{
				"time":          time.Now().String(),
				"db_connected":  c.db != nil,
				"session_mgr":   c.sessionManager != nil,
				"discord_ready": c.discordBot != nil,
			},
		})
	})

	utils.InfoLog("Internal API routes configured successfully")
}