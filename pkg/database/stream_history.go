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

    "github.com/lucasduport/stream-share/pkg/utils"
)

// AddStreamHistory records a new stream session
func (m *DBManager) AddStreamHistory(username, streamID, streamType, streamTitle, ipAddress, userAgent string) (int64, error) {
    utils.DebugLog("Database: Recording stream history - user: %s, stream: %s, type: %s", username, streamID, streamType)
    if m == nil || m.db == nil {
        return 0, fmt.Errorf("database not initialized")
    }

    var discordID string
    _ = m.db.QueryRow(`SELECT discord_id FROM discord_ldap_mapping WHERE ldap_username = $1`, username).Scan(&discordID)

    var id int64
    err := m.db.QueryRow(`
        INSERT INTO stream_history 
          (username, discord_id, stream_id, stream_type, stream_title, ip_address, user_agent) 
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        RETURNING id
    `, username, discordID, streamID, streamType, streamTitle, ipAddress, userAgent).Scan(&id)
    if err != nil {
        utils.ErrorLog("Database error adding stream history: %v", err)
        return 0, err
    }
    return id, nil
}

// CloseStreamHistory marks a stream session as ended
func (m *DBManager) CloseStreamHistory(historyID int64) error {
    utils.DebugLog("Database: Closing stream history record %d", historyID)
    if m == nil || m.db == nil {
        return fmt.Errorf("database not initialized")
    }
    _, err := m.db.Exec(`UPDATE stream_history SET end_time = CURRENT_TIMESTAMP WHERE id = $1`, historyID)
    if err != nil {
        utils.ErrorLog("Database error closing stream history: %v", err)
        return err
    }
    return nil
}

// GetStreamHistoryStats gets statistics about stream usage
func (m *DBManager) GetStreamHistoryStats() (map[string]interface{}, error) {
    utils.DebugLog("Database: Getting stream history statistics")
    if m == nil || m.db == nil {
        return nil, fmt.Errorf("database not initialized")
    }

    stats := make(map[string]interface{})
    var totalStreams int
    if err := m.db.QueryRow("SELECT COUNT(*) FROM stream_history").Scan(&totalStreams); err != nil {
        utils.ErrorLog("Database error counting streams: %v", err)
        return nil, err
    }
    stats["total_streams"] = totalStreams

    var activeUsers int
    if err := m.db.QueryRow(`
        SELECT COUNT(DISTINCT username) FROM stream_history WHERE start_time > $1
    `, time.Now().Add(-24*time.Hour)).Scan(&activeUsers); err != nil {
        utils.ErrorLog("Database error counting active users: %v", err)
        return nil, err
    }
    stats["active_users_24h"] = activeUsers

    return stats, nil
}
