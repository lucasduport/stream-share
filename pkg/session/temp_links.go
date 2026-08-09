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

package session

import (
	"fmt"
	"time"

	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// GenerateTemporaryLink creates a temporary download link
func (sm *SessionManager) GenerateTemporaryLink(username, streamID, title, rawURL string) (string, error) {
	expiresAt := time.Now().Add(sm.tempLinkTimeout)
	tempLink := &types.TemporaryLink{
		Username:  username,
		URL:       rawURL,
		ExpiresAt: expiresAt,
		StreamID:  streamID,
		Title:     title,
	}

	var token string
	for attempt := 0; attempt < 5; attempt++ {
		t, err := utils.GenerateShortToken(8)
		if err != nil {
			return "", fmt.Errorf("failed to generate token: %v", err)
		}
		// Fast path: skip tokens already in the in-memory map.
		sm.tempLinkLock.RLock()
		_, exists := sm.tempLinks[t]
		sm.tempLinkLock.RUnlock()
		if exists {
			continue
		}
		tempLink.Token = t

		// The DB primary key is the real collision guard — it survives process
		// restarts when the in-memory map is empty. Insert first and retry on a
		// unique violation rather than swallowing the error.
		if sm.db != nil {
			if err := sm.db.CreateTemporaryLink(t, username, rawURL, streamID, title, expiresAt); err != nil {
				if database.IsUniqueViolation(err) {
					continue
				}
				return "", fmt.Errorf("failed to persist temporary link: %v", err)
			}
		}

		sm.tempLinkLock.Lock()
		sm.tempLinks[t] = tempLink
		sm.tempLinkLock.Unlock()
		token = t
		break
	}
	if token == "" {
		return "", fmt.Errorf("failed to generate a unique token after retries")
	}

	utils.InfoLog("Generated temporary link for user %s, expires at %v", username, expiresAt)
	return token, nil
}

// GetTemporaryLink retrieves a temporary link by token
func (sm *SessionManager) GetTemporaryLink(token string) (*types.TemporaryLink, error) {
	// First check in memory
	sm.tempLinkLock.RLock()
	tempLink, exists := sm.tempLinks[token]
	sm.tempLinkLock.RUnlock()

	if exists && time.Now().Before(tempLink.ExpiresAt) {
		return tempLink, nil
	}

	// If not in memory or expired, try the database
	if sm.db != nil {
		return sm.db.GetTemporaryLink(token)
	}
	return nil, fmt.Errorf("temporary link not found or expired")
}
