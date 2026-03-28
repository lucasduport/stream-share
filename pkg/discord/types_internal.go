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
    "net/http"
    "sync"
    "time"

    "github.com/bwmarrin/discordgo"
    "github.com/lucasduport/stream-share/pkg/types"
)

// Bot represents the Discord bot and its stateful maps for interactive flows.
type Bot struct {
    session         *discordgo.Session
    token           string
    adminRoleID     string
    apiURL          string
    apiKey          string
    client          *http.Client

    cleanupInterval time.Duration
    stopChan        chan struct{} // closed by Stop() to terminate background goroutines

    // Component-based selection contexts
    pendingVODSelect map[string]*vodSelectContext // messageID -> selection context
    selectLock       sync.RWMutex

    // Slash commands
    devGuildID        string
    registeredCommands []*discordgo.ApplicationCommand
}


// Context for component-based VOD selection (dropdown + buttons)
type vodSelectContext struct {
    UserID  string
    Channel string
    Query   string
    Results []types.VODResult
    Page    int
    PerPage int
    Created time.Time
    // Tracks which pages have been enriched (full name, rating, size) to avoid redundant refreshes
    EnrichedPages map[int]bool
}
