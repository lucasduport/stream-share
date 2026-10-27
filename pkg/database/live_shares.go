/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
)

// CreateLiveShare stores a new live share link.
func (m *DBManager) CreateLiveShare(s *types.LiveShare) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `
        INSERT INTO live_shares (token, stream_id, title, created_by, starts_at, ends_at, created_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
    `, s.Token, s.StreamID, s.Title, s.CreatedBy, s.StartsAt.UTC(), s.EndsAt.UTC(), s.CreatedAt.UTC())
	return err
}

// ListLiveShares returns the share links that ended after since (scheduled and
// active ones included), newest first.
func (m *DBManager) ListLiveShares(since time.Time) ([]*types.LiveShare, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `
        SELECT token, stream_id, title, created_by, starts_at, ends_at, revoked_at, created_at
        FROM live_shares WHERE ends_at > $1 ORDER BY created_at DESC
    `, since.UTC())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.LiveShare
	for rows.Next() {
		s := &types.LiveShare{}
		var revoked sql.NullTime
		if err := rows.Scan(&s.Token, &s.StreamID, &s.Title, &s.CreatedBy, &s.StartsAt, &s.EndsAt, &revoked, &s.CreatedAt); err != nil {
			return nil, err
		}
		if revoked.Valid {
			t := revoked.Time
			s.RevokedAt = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeLiveShare marks a share link revoked at the given time. Revoking an
// already revoked link keeps its first revocation time.
func (m *DBManager) RevokeLiveShare(token string, at time.Time) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `UPDATE live_shares SET revoked_at = COALESCE(revoked_at, $2) WHERE token = $1`, token, at.UTC())
	return err
}

// DeleteLiveSharesEndedBefore removes share links that ended before cutoff.
func (m *DBManager) DeleteLiveSharesEndedBefore(cutoff time.Time) (int64, error) {
	if m == nil || m.db == nil {
		return 0, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := m.db.ExecContext(ctx, `DELETE FROM live_shares WHERE ends_at < $1`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
