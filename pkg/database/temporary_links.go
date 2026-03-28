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
 
package database

import (
    "fmt"
    "time"

    "github.com/lucasduport/stream-share/pkg/types"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// CreateTemporaryLink generates a new temporary download link
func (m *DBManager) CreateTemporaryLink(token, username, url, streamID, title string, expirationTime time.Time) error {
    utils.DebugLog("Database: Creating temporary link - token: %s, user: %s, expires: %v", token, username, expirationTime)
    if m == nil || m.db == nil {
        return fmt.Errorf("database not initialized")
    }
    _, err := m.db.Exec(`
        INSERT INTO temporary_links (token, username, url, expires_at, stream_id, title) 
        VALUES ($1, $2, $3, $4, $5, $6)
    `, token, username, url, expirationTime, streamID, title)
    if err != nil {
        utils.ErrorLog("Database error creating temporary link: %v", err)
        return err
    }
    return nil
}

// GetTemporaryLink retrieves a temporary link by token
func (m *DBManager) GetTemporaryLink(token string) (*types.TemporaryLink, error) {
    utils.DebugLog("Database: Getting temporary link for token %s", token)
    if m == nil || m.db == nil {
        return nil, fmt.Errorf("database not initialized")
    }

    link := &types.TemporaryLink{}
    err := m.db.QueryRow(`
        SELECT token, username, url, expires_at, stream_id, title
        FROM temporary_links 
        WHERE token = $1 AND expires_at > CURRENT_TIMESTAMP
    `, token).Scan(&link.Token, &link.Username, &link.URL, &link.ExpiresAt, &link.StreamID, &link.Title)

    if err != nil {
        return nil, err
    }
    return link, nil
}

// CleanupExpiredLinks removes expired temporary links
func (m *DBManager) CleanupExpiredLinks() (int64, error) {
    utils.DebugLog("Database: Cleaning up expired temporary links")
    if m == nil || m.db == nil {
        return 0, fmt.Errorf("database not initialized")
    }
    result, err := m.db.Exec(`DELETE FROM temporary_links WHERE expires_at < CURRENT_TIMESTAMP`)
    if err != nil {
        utils.ErrorLog("Database error cleaning up expired links: %v", err)
        return 0, err
    }
    rows, _ := result.RowsAffected()
    if rows > 0 {
        utils.InfoLog("Cleaned up %d expired temporary links", rows)
    }
    return rows, nil
}
