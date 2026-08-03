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

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// UpsertVODCache stores or updates a cache entry
func (m *DBManager) UpsertVODCache(e *types.VODCacheEntry) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `
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
          -- Guarded like the columns around them: a proxied Range request for an
          -- item that is still downloading re-runs this upsert with zeroed
          -- counters, and an unconditional assignment reset the progress the
          -- downloader had just reported. Use DeleteVODCacheEntry to start over.
          downloaded_bytes = CASE WHEN EXCLUDED.downloaded_bytes IS NOT NULL AND EXCLUDED.downloaded_bytes <> 0 THEN EXCLUDED.downloaded_bytes ELSE vod_cache.downloaded_bytes END,
          total_bytes = CASE WHEN EXCLUDED.total_bytes IS NOT NULL AND EXCLUDED.total_bytes <> 0 THEN EXCLUDED.total_bytes ELSE vod_cache.total_bytes END,
          size_bytes = CASE WHEN EXCLUDED.size_bytes IS NOT NULL AND EXCLUDED.size_bytes <> 0 THEN EXCLUDED.size_bytes ELSE vod_cache.size_bytes END,
          status = COALESCE(NULLIF(EXCLUDED.status, ''), vod_cache.status),
          expires_at = EXCLUDED.expires_at,
          last_access = COALESCE(EXCLUDED.last_access, CURRENT_TIMESTAMP)
    `, e.StreamID, e.Type, e.Title, e.SeriesTitle, e.Season, e.Episode, e.FilePath, e.RequestedBy, e.DownloadedBytes, e.TotalBytes, e.SizeBytes, e.Status, e.CreatedAt, e.ExpiresAt, e.LastAccess)
	if err != nil {
		utils.ErrorLog("DB UpsertVODCache error: %v", err)
	}
	return err
}

// GetVODCache returns a cache entry for a stream id if exists and not expired
func (m *DBManager) GetVODCache(streamID string) (*types.VODCacheEntry, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := m.db.QueryRowContext(ctx, `SELECT stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access
        FROM vod_cache WHERE stream_id=$1 AND expires_at > CURRENT_TIMESTAMP`, streamID)
	var e types.VODCacheEntry
	if err := row.Scan(&e.StreamID, &e.Type, &e.Title, &e.SeriesTitle, &e.Season, &e.Episode, &e.FilePath, &e.RequestedBy, &e.DownloadedBytes, &e.TotalBytes, &e.SizeBytes, &e.Status, &e.CreatedAt, &e.ExpiresAt, &e.LastAccess); err != nil {
		return nil, err
	}
	return &e, nil
}

// UpdateVODProgress writes just the download counters.
//
// The downloader reports progress on a timer for the whole length of a download,
// which for a feature film is thousands of updates. Doing that through
// UpsertVODCache meant rewriting all fifteen columns each time to move two
// integers, so this narrow statement exists purely to keep that cheap.
func (m *DBManager) UpdateVODProgress(streamID string, downloaded, total int64) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// $3 is cast explicitly. Postgres infers a parameter's type from its context,
	// and comparing it against the untyped literal 0 resolves it to integer — so
	// any file over 2GB overflowed and aborted the whole statement, leaving
	// progress frozen for exactly the downloads long enough to care about. $2
	// needs no cast because assigning straight to a bigint column already pins it.
	_, err := m.db.ExecContext(ctx, `
        UPDATE vod_cache
           SET downloaded_bytes = $2::bigint,
               total_bytes = CASE WHEN $3::bigint <> 0 THEN $3::bigint ELSE total_bytes END,
               last_access = CURRENT_TIMESTAMP
         WHERE stream_id = $1`, streamID, downloaded, total)
	return err
}

// TouchVODCache updates last_access
func (m *DBManager) TouchVODCache(streamID string) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `UPDATE vod_cache SET last_access=CURRENT_TIMESTAMP WHERE stream_id=$1`, streamID)
	return err
}

// GetExpiredVODCache returns entries whose expires_at has passed, across every
// status. Callers delete the on-disk file (and its .part sibling) before the row
// so expiry never orphans a file — which the old DB-only delete did.
func (m *DBManager) GetExpiredVODCache() ([]types.VODCacheEntry, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT stream_id, file_path
        FROM vod_cache WHERE expires_at < CURRENT_TIMESTAMP`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []types.VODCacheEntry
	for rows.Next() {
		var e types.VODCacheEntry
		if err := rows.Scan(&e.StreamID, &e.FilePath); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// ListVODCacheFilePaths returns the set of non-empty file paths referenced by any
// cache row, so an orphan sweep can tell tracked files from stray ones.
func (m *DBManager) ListVODCacheFilePaths() (map[string]struct{}, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT file_path FROM vod_cache WHERE file_path <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	paths := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if p != "" {
			paths[p] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// GetStaleVODCache returns ready entries whose last_access is older than threshold.
// In-progress downloads (status != 'ready') are excluded.
func (m *DBManager) GetStaleVODCache(threshold time.Time) ([]types.VODCacheEntry, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT stream_id, file_path, last_access
        FROM vod_cache WHERE status = 'ready' AND last_access < $1`, threshold)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []types.VODCacheEntry
	for rows.Next() {
		var e types.VODCacheEntry
		if err := rows.Scan(&e.StreamID, &e.FilePath, &e.LastAccess); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteVODCacheEntry removes a single cache row by stream ID.
func (m *DBManager) DeleteVODCacheEntry(streamID string) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `DELETE FROM vod_cache WHERE stream_id = $1`, streamID)
	return err
}

// ListVODCache returns non-expired cache entries ordered by soonest expiry first. If limit<=0, returns all.
func (m *DBManager) ListVODCache(limit int) ([]types.VODCacheEntry, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = m.db.QueryContext(ctx, `SELECT stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access
            FROM vod_cache WHERE expires_at > CURRENT_TIMESTAMP ORDER BY expires_at ASC LIMIT $1`, limit)
	} else {
		rows, err = m.db.QueryContext(ctx, `SELECT stream_id, type, title, series_title, season, episode, file_path, requested_by, downloaded_bytes, total_bytes, size_bytes, status, created_at, expires_at, last_access
            FROM vod_cache WHERE expires_at > CURRENT_TIMESTAMP ORDER BY expires_at ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	list := make([]types.VODCacheEntry, 0)
	for rows.Next() {
		var e types.VODCacheEntry
		if err := rows.Scan(&e.StreamID, &e.Type, &e.Title, &e.SeriesTitle, &e.Season, &e.Episode, &e.FilePath, &e.RequestedBy, &e.DownloadedBytes, &e.TotalBytes, &e.SizeBytes, &e.Status, &e.CreatedAt, &e.ExpiresAt, &e.LastAccess); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}
