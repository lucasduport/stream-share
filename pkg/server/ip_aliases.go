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
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const maxIPAliasLength = 64

// loadIPAliasMap returns every configured IP alias, or an empty map if the
// feature can't be used right now (no DB, or a query error) — callers
// degrade gracefully to showing raw IPs rather than failing.
func (c *Config) loadIPAliasMap() map[string]string {
	if c.db == nil {
		return map[string]string{}
	}
	m, err := c.db.GetIPAliasMap()
	if err != nil {
		utils.WarnLog("Failed to load IP aliases: %v", err)
		return map[string]string{}
	}
	return m
}

// displayNameFor resolves the best display label for a viewer identity.
// When LDAP is disabled, resolveRequestUsername falls back to the raw client
// IP as the de-facto per-viewer identity (see server.go), so identifier is
// checked against the configured aliases only when it parses as an IP
// address — an LDAP username needs no resolution and is returned unchanged.
func displayNameFor(identifier string, aliases map[string]string) string {
	if net.ParseIP(identifier) == nil {
		return identifier
	}
	if alias, ok := aliases[identifier]; ok && alias != "" {
		return alias
	}
	return identifier
}

// listIPAliases GET /api/internal/ip-aliases — every configured IP -> alias mapping.
func (c *Config) listIPAliases(ctx *gin.Context) {
	if c.db == nil {
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Database not initialized"})
		return
	}
	aliases, err := c.db.ListIPAliases()
	if err != nil {
		utils.ErrorLog("Failed to list IP aliases: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Failed to list IP aliases: " + err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: aliases})
}

// upsertIPAlias POST /api/internal/ip-aliases — body {"ip_address": "...", "alias": "..."}.
// Creates the alias if the IP has none yet, or replaces its existing one — each
// IP only ever has one alias.
func (c *Config) upsertIPAlias(ctx *gin.Context) {
	var req struct {
		IPAddress string `json:"ip_address"`
		Alias     string `json:"alias"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "Invalid request: " + err.Error()})
		return
	}

	ip := strings.TrimSpace(req.IPAddress)
	if net.ParseIP(ip) == nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "ip_address is not a valid IP address"})
		return
	}
	alias := strings.TrimSpace(req.Alias)
	if alias == "" {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "alias is required"})
		return
	}
	if len(alias) > maxIPAliasLength {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: fmt.Sprintf("alias must be %d characters or fewer", maxIPAliasLength)})
		return
	}

	if c.db == nil {
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Database not initialized"})
		return
	}
	if err := c.db.UpsertIPAlias(ip, alias); err != nil {
		if errors.Is(err, database.ErrIPAliasInUse) {
			ctx.JSON(http.StatusConflict, types.APIResponse{Success: false, Error: err.Error()})
			return
		}
		utils.ErrorLog("Failed to save IP alias for %s: %v", ip, err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Failed to save alias: " + err.Error()})
		return
	}

	utils.InfoLog("IP alias set: %s -> %s", ip, alias)
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: database.IPAlias{IPAddress: ip, Alias: alias}})
}

// resolveIPAlias GET /api/internal/ip-aliases/resolve/:alias — looks up the IP
// address an alias was assigned to (case-insensitive). Used by the Discord bot
// to let admins look up a viewer by the friendly name they gave it instead of
// the raw IP.
func (c *Config) resolveIPAlias(ctx *gin.Context) {
	alias := strings.TrimSpace(ctx.Param("alias"))
	if alias == "" {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "alias is required"})
		return
	}
	if c.db == nil {
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Database not initialized"})
		return
	}
	ip, found, err := c.db.GetIPByAlias(alias)
	if err != nil {
		utils.ErrorLog("Failed to resolve IP alias %q: %v", alias, err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Failed to resolve alias: " + err.Error()})
		return
	}
	if !found {
		ctx.JSON(http.StatusNotFound, types.APIResponse{Success: false, Error: fmt.Sprintf("no IP address found for alias %q", alias)})
		return
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: database.IPAlias{IPAddress: ip, Alias: alias}})
}

// deleteIPAlias POST /api/internal/ip-aliases/delete/:ip — removes the alias for
// an IP address, if one exists.
func (c *Config) deleteIPAlias(ctx *gin.Context) {
	ip := strings.TrimSpace(ctx.Param("ip"))
	if net.ParseIP(ip) == nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "ip is not a valid IP address"})
		return
	}
	if c.db == nil {
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Database not initialized"})
		return
	}
	if err := c.db.DeleteIPAlias(ip); err != nil {
		utils.ErrorLog("Failed to delete IP alias for %s: %v", ip, err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{Success: false, Error: "Failed to delete alias: " + err.Error()})
		return
	}
	utils.InfoLog("IP alias removed: %s", ip)
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Message: fmt.Sprintf("Alias removed for %s", ip)})
}
