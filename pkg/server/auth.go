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
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-ldap/ldap/v3"
	"github.com/google/uuid"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

var internalAPIKey string

func init() {
	// Generate a random API key at startup or use from environment
	envKey := os.Getenv("INTERNAL_API_KEY")
	if envKey != "" {
		internalAPIKey = envKey
		utils.InfoLog("Using API key from environment")
	} else {
		internalAPIKey = uuid.New().String()
		utils.InfoLog("Generated new internal API key: %s", internalAPIKey)
	}
}

func GetAPIKey() string {
	return internalAPIKey
}

// apiKeyAuth middleware validates the internal API key
func (c *Config) apiKeyAuth() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		key := ctx.GetHeader("X-API-Key")
		utils.DebugLog("API Key auth check - received key: %s...", utils.MaskString(key))

		if key != internalAPIKey {
			utils.DebugLog("API authentication failed - invalid key: %s", utils.MaskString(key))
			ctx.AbortWithStatusJSON(401, types.APIResponse{
				Success: false,
				Error:   "Invalid API key",
			})
			return
		}
		utils.DebugLog("API authentication successful for endpoint: %s", ctx.Request.URL.Path)
		ctx.Next()
	}
}

// authRequest represents credentials supplied via form/query params
// for endpoints using GET/POST with standard query binding.
type authRequest struct {
    Username string `form:"username" binding:"required"`
    Password string `form:"password" binding:"required"`
}

// authenticate validates form/query credentials using LDAP (if enabled) or
// local credentials. Used for GET/POST endpoints.
func (c *Config) authenticate(ctx *gin.Context) {
    utils.DebugLog("-> Incoming URL: %s", ctx.Request.URL)
    var authReq authRequest
    if err := ctx.Bind(&authReq); err != nil {
        utils.DebugLog("Bind error: %v", err)
        _ = ctx.AbortWithError(http.StatusBadRequest, err)
        return
    }

    // Only use LDAP authentication to validate client access
    if c.ProxyConfig.LDAPEnabled {
        utils.DebugLog("LDAP authentication enabled for user: %s", authReq.Username)
        ok := ldapAuthenticate(
            c.ProxyConfig.LDAPServer,
            c.ProxyConfig.LDAPBaseDN,
            c.ProxyConfig.LDAPBindDN,
            c.ProxyConfig.LDAPBindPassword,
            c.ProxyConfig.LDAPUserAttribute,
            c.ProxyConfig.LDAPGroupAttribute,
            c.ProxyConfig.LDAPRequiredGroup,
            authReq.Username,
            authReq.Password,
        )
        if !ok {
            utils.DebugLog("LDAP authentication failed for user: %s", authReq.Username)
            ctx.AbortWithStatus(http.StatusUnauthorized)
            return
        }
        utils.DebugLog("LDAP authentication succeeded for user: %s", authReq.Username)
        return
    }

    // If LDAP is not enabled, fallback to local credentials
    utils.DebugLog("Local authentication for user: %s", authReq.Username)
    if c.ProxyConfig.User.String() != authReq.Username || c.ProxyConfig.Password.String() != authReq.Password {
        utils.DebugLog("Local authentication failed for user: %s", authReq.Username)
        ctx.AbortWithStatus(http.StatusUnauthorized)
    }
}

// appAuthenticate validates credentials for application/x-www-form-urlencoded
// bodies (player_api POST). It replays the body to allow downstream reading.
func (c *Config) appAuthenticate(ctx *gin.Context) {
    utils.DebugLog("-> Incoming URL: %s", ctx.Request.URL)

    contents, err := io.ReadAll(ctx.Request.Body)
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
        return
    }

    q, err := url.ParseQuery(string(contents))
    if err != nil {
        ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
        return
    }
    if len(q["username"]) == 0 || len(q["password"]) == 0 {
        ctx.AbortWithError(http.StatusBadRequest, fmt.Errorf("bad body url query parameters")) // nolint: errcheck
        return
    }
    log.Printf("[stream-share] %v | %s |App Auth\n", time.Now().Format("2006/01/02 - 15:04:05"), ctx.ClientIP())

    // Use LDAP authentication if enabled
    if c.ProxyConfig.LDAPEnabled {
        utils.DebugLog("LDAP app authentication for user: %s", q["username"][0])
        ok := ldapAuthenticate(
            c.ProxyConfig.LDAPServer,
            c.ProxyConfig.LDAPBaseDN,
            c.ProxyConfig.LDAPBindDN,
            c.ProxyConfig.LDAPBindPassword,
            c.ProxyConfig.LDAPUserAttribute,
            c.ProxyConfig.LDAPGroupAttribute,
            c.ProxyConfig.LDAPRequiredGroup,
            q["username"][0],
            q["password"][0],
        )
        if !ok {
            utils.DebugLog("LDAP app authentication failed for user: %s", q["username"][0])
            ctx.AbortWithStatus(http.StatusUnauthorized)
            return
        }
        utils.DebugLog("LDAP app authentication succeeded for user: %s", q["username"][0])
    } else if c.ProxyConfig.User.String() != q["username"][0] || c.ProxyConfig.Password.String() != q["password"][0] {
        utils.DebugLog("Local app authentication failed for user: %s", q["username"][0])
        ctx.AbortWithStatus(http.StatusUnauthorized)
        return
    }

    ctx.Request.Body = io.NopCloser(bytes.NewReader(contents))
}

// ldapAuthenticate binds with an optional service account, finds the user DN,
// optionally validates group membership, then attempts a user bind.
func ldapAuthenticate(server, baseDN, bindDN, bindPassword, userAttr, groupAttr, requiredGroup, username, password string) bool {
    utils.DebugLog("LDAP DialURL: %s", server)
    l, err := ldap.DialURL(server)
    if err != nil {
        utils.DebugLog("LDAP DialURL error: %v", err)
        return false
    }
    defer l.Close()

    // Bind with service account
    if bindDN != "" && bindPassword != "" {
        utils.DebugLog("LDAP service bind attempt: DN=%s", bindDN)
        if err := l.Bind(bindDN, bindPassword); err != nil {
            utils.DebugLog("LDAP service bind error: %v", err)
            return false
        }
        utils.DebugLog("LDAP service bind succeeded")
    }

    // Search for user DN
    filter := fmt.Sprintf("(%s=%s)", userAttr, ldap.EscapeFilter(username))
    utils.DebugLog("LDAP search: baseDN=%s, filter=%s", baseDN, filter)
    searchRequest := ldap.NewSearchRequest(
        baseDN,
        ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
        filter,
        []string{"dn", groupAttr}, // Include group attribute
        nil,
    )
    sr, err := l.Search(searchRequest)
    if err != nil {
        utils.DebugLog("LDAP search error: %v", err)
        return false
    }
    if len(sr.Entries) == 0 {
        utils.DebugLog("LDAP search: no entries found for user: %s", username)
        return false
    }
    userDN := sr.Entries[0].DN
    utils.DebugLog("LDAP user DN found: %s", userDN)

    // Check group membership if requiredGroup is specified
    if requiredGroup != "" && groupAttr != "" {
        hasGroup := false
        for _, entry := range sr.Entries {
            for _, groupValue := range entry.GetAttributeValues(groupAttr) {
                utils.DebugLog("LDAP user group: %s", groupValue)
                if strings.Contains(strings.ToLower(groupValue), strings.ToLower(requiredGroup)) {
                    hasGroup = true
                    break
                }
            }
        }
        if !hasGroup {
            utils.DebugLog("LDAP user %s is not a member of required group: %s", username, requiredGroup)
            return false
        }
        utils.DebugLog("LDAP user %s is a member of required group: %s", username, requiredGroup)
    }

    // Try to bind as user
    utils.DebugLog("LDAP user bind attempt: DN=%s", userDN)
    if err := l.Bind(userDN, password); err != nil {
        utils.DebugLog("LDAP user bind error: %v", err)
        return false
    }
    utils.DebugLog("LDAP user bind succeeded for user: %s", username)
    return true
}
