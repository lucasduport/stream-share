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
	"errors"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	connectRetryMin = time.Second
	connectRetryMax = 10 * time.Minute
)

// isPermanentGatewayError reports gateway close codes that retrying cannot fix
// (bad token, invalid shard/API version/intents, disallowed intents).
func isPermanentGatewayError(err error) bool {
	return websocket.IsCloseError(err, 4004, 4010, 4011, 4012, 4013, 4014)
}

// connectWithRetry calls open until it succeeds, hits a permanent error or stop is closed.
// It returns true only when the connection is open.
func connectWithRetry(open func() error, stop <-chan struct{}, minWait time.Duration) bool {
	wait := minWait
	for {
		select {
		case <-stop:
			return false
		default:
		}
		err := open()
		if err == nil || errors.Is(err, discordgo.ErrWSAlreadyOpen) {
			return true
		}
		if isPermanentGatewayError(err) {
			utils.ErrorLog("Discord connection refused permanently, not retrying: %v", err)
			return false
		}
		utils.WarnLog("Discord connection failed: %v; retrying in %s", err, wait)
		select {
		case <-stop:
			return false
		case <-time.After(wait):
		}
		wait = min(wait*2, connectRetryMax)
	}
}
