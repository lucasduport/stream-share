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

// GetUserHistory returns a user's history rows newer than `since`, newest first, capped at
// limit and starting after the first `offset` rows. If since is the zero Time, no lower
// bound is applied.
func (m *DBManager) GetUserHistory(username string, since time.Time, limit, offset int) ([]HistoryEntry, error) {
	utils.DebugLog("Database: Getting history for user %s (since %v, limit %d, offset %d)", username, since, limit, offset)
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
        LIMIT $3 OFFSET $4
    `, username, sinceParam, limit, offset)
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
// first, capped at limit and starting after the first `offset` rows. If since is
// the zero Time, no lower bound is applied.
func (m *DBManager) GetRecentHistory(since time.Time, limit, offset int) ([]HistoryEntry, error) {
	utils.DebugLog("Database: Getting recent history feed (since %v, limit %d, offset %d)", since, limit, offset)
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
        LIMIT $2 OFFSET $3
    `, sinceParam, limit, offset)
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

// CountHistory returns the total number of history rows starting at or after
// `since` (optionally filtered to one user), for computing pagination totals.
// If since is the zero Time, no lower bound is applied. Pass an empty username
// to count across all users.
func (m *DBManager) CountHistory(username string, since time.Time) (int, error) {
	if m == nil || m.db == nil {
		return 0, fmt.Errorf("database not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sinceParam := sql.NullTime{Time: since, Valid: !since.IsZero()}

	var count int
	var err error
	if username == "" {
		err = m.db.QueryRowContext(ctx, `
            SELECT COUNT(*) FROM stream_history WHERE ($1::timestamp IS NULL OR start_time >= $1)
        `, sinceParam).Scan(&count)
	} else {
		err = m.db.QueryRowContext(ctx, `
            SELECT COUNT(*) FROM stream_history WHERE username = $1 AND ($2::timestamp IS NULL OR start_time >= $2)
        `, username, sinceParam).Scan(&count)
	}
	if err != nil {
		utils.ErrorLog("Database error counting history: %v", err)
		return 0, err
	}
	return count, nil
}

// WatchStats summarizes viewing activity over a time window.
type WatchStats struct {
	Sessions      int
	UniqueViewers int
	WatchSeconds  int64
}

// GetWatchStats aggregates session count, unique viewers and total watch time
// for rows starting at or after `since`. If since is the zero Time, no lower
// bound is applied (all-time).
func (m *DBManager) GetWatchStats(since time.Time) (WatchStats, error) {
	utils.DebugLog("Database: Getting watch stats (since %v)", since)
	var stats WatchStats
	if m == nil || m.db == nil {
		return stats, fmt.Errorf("database not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sinceParam := sql.NullTime{Time: since, Valid: !since.IsZero()}

	err := m.db.QueryRowContext(ctx, `
        SELECT COUNT(*), COUNT(DISTINCT username),
               COALESCE(SUM(EXTRACT(EPOCH FROM (COALESCE(end_time, NOW()) - start_time))), 0)::bigint
        FROM stream_history
        WHERE ($1::timestamp IS NULL OR start_time >= $1)
    `, sinceParam).Scan(&stats.Sessions, &stats.UniqueViewers, &stats.WatchSeconds)
	if err != nil {
		utils.ErrorLog("Database error getting watch stats: %v", err)
		return stats, err
	}
	return stats, nil
}

// TitleStat is an aggregate row for one piece of content over a time window.
type TitleStat struct {
	StreamID     string
	StreamType   string
	StreamTitle  string
	Views        int
	WatchSeconds int64
}

// GetTopTitles returns the most-watched titles (by total watch time) for rows
// starting at or after `since`, capped at limit. If since is the zero Time,
// no lower bound is applied.
func (m *DBManager) GetTopTitles(since time.Time, limit int) ([]TitleStat, error) {
	utils.DebugLog("Database: Getting top titles (since %v, limit %d)", since, limit)
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sinceParam := sql.NullTime{Time: since, Valid: !since.IsZero()}

	rows, err := m.db.QueryContext(ctx, `
        SELECT stream_id, stream_type, MAX(NULLIF(stream_title, '')) AS title,
               COUNT(*), COALESCE(SUM(EXTRACT(EPOCH FROM (COALESCE(end_time, NOW()) - start_time))), 0)::bigint AS watch_seconds
        FROM stream_history
        WHERE ($1::timestamp IS NULL OR start_time >= $1)
        GROUP BY stream_id, stream_type
        ORDER BY watch_seconds DESC
        LIMIT $2
    `, sinceParam, limit)
	if err != nil {
		utils.ErrorLog("Database error getting top titles: %v", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]TitleStat, 0)
	for rows.Next() {
		var t TitleStat
		var title sql.NullString
		if err := rows.Scan(&t.StreamID, &t.StreamType, &title, &t.Views, &t.WatchSeconds); err != nil {
			utils.WarnLog("Database error scanning top title row: %v", err)
			continue
		}
		if title.Valid {
			t.StreamTitle = title.String
		} else {
			t.StreamTitle = t.StreamID
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UserStat is an aggregate row for one viewer over a time window.
type UserStat struct {
	Username     string
	Sessions     int
	WatchSeconds int64
}

// GetTopUsers returns the users with the most total watch time for rows
// starting at or after `since`, capped at limit. If since is the zero Time,
// no lower bound is applied.
func (m *DBManager) GetTopUsers(since time.Time, limit int) ([]UserStat, error) {
	utils.DebugLog("Database: Getting top users (since %v, limit %d)", since, limit)
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sinceParam := sql.NullTime{Time: since, Valid: !since.IsZero()}

	rows, err := m.db.QueryContext(ctx, `
        SELECT username, COUNT(*), COALESCE(SUM(EXTRACT(EPOCH FROM (COALESCE(end_time, NOW()) - start_time))), 0)::bigint AS watch_seconds
        FROM stream_history
        WHERE ($1::timestamp IS NULL OR start_time >= $1)
        GROUP BY username
        ORDER BY watch_seconds DESC
        LIMIT $2
    `, sinceParam, limit)
	if err != nil {
		utils.ErrorLog("Database error getting top users: %v", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]UserStat, 0)
	for rows.Next() {
		var u UserStat
		if err := rows.Scan(&u.Username, &u.Sessions, &u.WatchSeconds); err != nil {
			utils.WarnLog("Database error scanning top user row: %v", err)
			continue
		}
		out = append(out, u)
	}
	return out, rows.Err()
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
