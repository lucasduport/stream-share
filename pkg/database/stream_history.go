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
    "context"
    "database/sql"
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

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    var discordID string
    if err := m.db.QueryRowContext(ctx, `SELECT discord_id FROM discord_ldap_mapping WHERE ldap_username = $1`, username).Scan(&discordID); err != nil && err != sql.ErrNoRows {
        utils.WarnLog("Failed to look up discord_id for stream history: %v", err)
    }

    var id int64
    err := m.db.QueryRowContext(ctx, `
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
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _, err := m.db.ExecContext(ctx, `UPDATE stream_history SET end_time = CURRENT_TIMESTAMP WHERE id = $1`, historyID)
    if err != nil {
        utils.ErrorLog("Database error closing stream history: %v", err)
        return err
    }
    return nil
}

// HistoryEntry is one watch-history row.
type HistoryEntry struct {
    Username    string
    StreamID    string
    StreamType  string
    StreamTitle string
    StartTime   time.Time
    EndTime     sql.NullTime
    DurationSec int64 // COALESCE(end_time, now) - start_time, in seconds
}

// GetUserHistory returns a user's history rows newer than `since`, newest first, capped at limit.
// If since is the zero Time, no lower bound is applied.
func (m *DBManager) GetUserHistory(username string, since time.Time, limit int) ([]HistoryEntry, error) {
    utils.DebugLog("Database: Getting history for user %s (since %v, limit %d)", username, since, limit)
    if m == nil || m.db == nil {
        return nil, fmt.Errorf("database not initialized")
    }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    sinceParam := sql.NullTime{Time: since, Valid: !since.IsZero()}

    rows, err := m.db.QueryContext(ctx, `
        SELECT stream_id, stream_type, COALESCE(stream_title, ''), start_time, end_time,
               EXTRACT(EPOCH FROM (COALESCE(end_time, NOW()) - start_time))::bigint
        FROM stream_history
        WHERE username = $1 AND ($2::timestamp IS NULL OR start_time >= $2)
        ORDER BY start_time DESC
        LIMIT $3
    `, username, sinceParam, limit)
    if err != nil {
        utils.ErrorLog("Database error getting user history: %v", err)
        return nil, err
    }
    defer func() { _ = rows.Close() }()

    entries := make([]HistoryEntry, 0)
    for rows.Next() {
        e := HistoryEntry{Username: username}
        if err := rows.Scan(&e.StreamID, &e.StreamType, &e.StreamTitle, &e.StartTime, &e.EndTime, &e.DurationSec); err != nil {
            utils.WarnLog("Database error scanning history row: %v", err)
            continue
        }
        entries = append(entries, e)
    }
    return entries, rows.Err()
}

// GetRecentHistory returns the most recent watch events across ALL users, newest
// first, capped at limit. If since is the zero Time, no lower bound is applied.
func (m *DBManager) GetRecentHistory(since time.Time, limit int) ([]HistoryEntry, error) {
    utils.DebugLog("Database: Getting recent history feed (since %v, limit %d)", since, limit)
    if m == nil || m.db == nil {
        return nil, fmt.Errorf("database not initialized")
    }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    sinceParam := sql.NullTime{Time: since, Valid: !since.IsZero()}

    rows, err := m.db.QueryContext(ctx, `
        SELECT username, stream_id, stream_type, COALESCE(stream_title, ''), start_time, end_time,
               EXTRACT(EPOCH FROM (COALESCE(end_time, NOW()) - start_time))::bigint
        FROM stream_history
        WHERE ($1::timestamp IS NULL OR start_time >= $1)
        ORDER BY start_time DESC
        LIMIT $2
    `, sinceParam, limit)
    if err != nil {
        utils.ErrorLog("Database error getting recent history: %v", err)
        return nil, err
    }
    defer func() { _ = rows.Close() }()

    entries := make([]HistoryEntry, 0)
    for rows.Next() {
        var e HistoryEntry
        if err := rows.Scan(&e.Username, &e.StreamID, &e.StreamType, &e.StreamTitle, &e.StartTime, &e.EndTime, &e.DurationSec); err != nil {
            utils.WarnLog("Database error scanning history row: %v", err)
            continue
        }
        entries = append(entries, e)
    }
    return entries, rows.Err()
}

// GetStreamHistoryStats gets statistics about stream usage
func (m *DBManager) GetStreamHistoryStats() (map[string]interface{}, error) {
    utils.DebugLog("Database: Getting stream history statistics")
    if m == nil || m.db == nil {
        return nil, fmt.Errorf("database not initialized")
    }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    stats := make(map[string]interface{})
    var totalStreams int
    if err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM stream_history").Scan(&totalStreams); err != nil {
        utils.ErrorLog("Database error counting streams: %v", err)
        return nil, err
    }
    stats["total_streams"] = totalStreams

    var activeUsers int
    if err := m.db.QueryRowContext(ctx, `
        SELECT COUNT(DISTINCT username) FROM stream_history WHERE start_time > $1
    `, time.Now().Add(-24*time.Hour)).Scan(&activeUsers); err != nil {
        utils.ErrorLog("Database error counting active users: %v", err)
        return nil, err
    }
    stats["active_users_24h"] = activeUsers

    return stats, nil
}
