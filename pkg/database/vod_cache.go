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
    "database/sql"
    "fmt"

    "github.com/lucasduport/stream-share/pkg/types"
    "github.com/lucasduport/stream-share/pkg/utils"
)

// UpsertVODCache stores or updates a cache entry
func (m *DBManager) UpsertVODCache(e *types.VODCacheEntry) error {
    if m == nil || m.db == nil { return fmt.Errorf("database not initialized") }
    _, err := m.db.Exec(`
        INSERT INTO vod_cache (stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,COALESCE($13, CURRENT_TIMESTAMP),$14,COALESCE($15, CURRENT_TIMESTAMP))
        ON CONFLICT(stream_id) DO UPDATE SET
          type = COALESCE(NULLIF(EXCLUDED.type, ''), vod_cache.type),
          title = COALESCE(NULLIF(EXCLUDED.title, ''), vod_cache.title),
          series_title = COALESCE(NULLIF(EXCLUDED.series_title, ''), vod_cache.series_title),
          season = CASE WHEN EXCLUDED.season IS NOT NULL AND EXCLUDED.season <> 0 THEN EXCLUDED.season ELSE vod_cache.season END,
          episode = CASE WHEN EXCLUDED.episode IS NOT NULL AND EXCLUDED.episode <> 0 THEN EXCLUDED.episode ELSE vod_cache.episode END,
          file_path = COALESCE(NULLIF(EXCLUDED.file_path, ''), vod_cache.file_path),
          requested_by = COALESCE(NULLIF(EXCLUDED.requested_by, ''), vod_cache.requested_by),
          downloaded_bytes = EXCLUDED.downloaded_bytes,
          total_bytes = EXCLUDED.total_bytes,
          size_bytes = CASE WHEN EXCLUDED.size_bytes IS NOT NULL AND EXCLUDED.size_bytes <> 0 THEN EXCLUDED.size_bytes ELSE vod_cache.size_bytes END,
          status = COALESCE(NULLIF(EXCLUDED.status, ''), vod_cache.status),
          expires_at = EXCLUDED.expires_at,
          last_access = COALESCE(EXCLUDED.last_access, CURRENT_TIMESTAMP)
    `, e.StreamID, e.Type, e.Title, e.SeriesTitle, e.Season, e.Episode, e.FilePath, e.RequestedBy, e.DownloadedBytes, e.TotalBytes, e.SizeBytes, e.Status, e.CreatedAt, e.ExpiresAt, e.LastAccess)
    if err != nil { utils.ErrorLog("DB UpsertVODCache error: %v", err) }
    return err
}

// GetVODCache returns a cache entry for a stream id if exists and not expired
func (m *DBManager) GetVODCache(streamID string) (*types.VODCacheEntry, error) {
    if m == nil || m.db == nil { return nil, fmt.Errorf("database not initialized") }
    row := m.db.QueryRow(`SELECT stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access
        FROM vod_cache WHERE stream_id=$1 AND expires_at > CURRENT_TIMESTAMP`, streamID)
    var e types.VODCacheEntry
    if err := row.Scan(&e.StreamID, &e.Type, &e.Title, &e.SeriesTitle, &e.Season, &e.Episode, &e.FilePath, &e.RequestedBy, &e.DownloadedBytes, &e.TotalBytes, &e.SizeBytes, &e.Status, &e.CreatedAt, &e.ExpiresAt, &e.LastAccess); err != nil {
        return nil, err
    }
    return &e, nil
}

// TouchVODCache updates last_access
func (m *DBManager) TouchVODCache(streamID string) error {
    if m == nil || m.db == nil { return fmt.Errorf("database not initialized") }
    _, err := m.db.Exec(`UPDATE vod_cache SET last_access=CURRENT_TIMESTAMP WHERE stream_id=$1`, streamID)
    return err
}

// CleanupExpiredCache deletes expired rows
func (m *DBManager) CleanupExpiredCache() (int64, error) {
    if m == nil || m.db == nil { return 0, fmt.Errorf("database not initialized") }
    res, err := m.db.Exec(`DELETE FROM vod_cache WHERE expires_at < CURRENT_TIMESTAMP`)
    if err != nil { return 0, err }
    n, _ := res.RowsAffected()
    if n > 0 { utils.InfoLog("Cleaned up %d expired vod_cache entries", n) }
    return n, nil
}

// ListVODCache returns non-expired cache entries ordered by soonest expiry first. If limit<=0, returns all.
func (m *DBManager) ListVODCache(limit int) ([]types.VODCacheEntry, error) {
    if m == nil || m.db == nil { return nil, fmt.Errorf("database not initialized") }
    var rows *sql.Rows
    var err error
    if limit > 0 {
        rows, err = m.db.Query(`SELECT stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access
            FROM vod_cache WHERE expires_at > CURRENT_TIMESTAMP ORDER BY expires_at ASC LIMIT $1`, limit)
    } else {
        rows, err = m.db.Query(`SELECT stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access
            FROM vod_cache WHERE expires_at > CURRENT_TIMESTAMP ORDER BY expires_at ASC`)
    }
    if err != nil { return nil, err }
    defer rows.Close()
    list := make([]types.VODCacheEntry, 0)
    for rows.Next() {
        var e types.VODCacheEntry
        if err := rows.Scan(&e.StreamID, &e.Type, &e.Title, &e.SeriesTitle, &e.Season, &e.Episode, &e.FilePath, &e.RequestedBy, &e.DownloadedBytes, &e.TotalBytes, &e.SizeBytes, &e.Status, &e.CreatedAt, &e.ExpiresAt, &e.LastAccess); err != nil {
            return nil, err
        }
        list = append(list, e)
    }
    return list, nil
}
