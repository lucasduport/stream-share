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
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// IPAlias is a friendly display name assigned to a client IP address.
type IPAlias struct {
	IPAddress string    `json:"ip_address"`
	Alias     string    `json:"alias"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrIPAliasInUse is returned by UpsertIPAlias when the alias is already
// assigned to a different IP address (aliases are unique, case-insensitively,
// so they can be looked up in reverse — e.g. Discord's /history alias option).
var ErrIPAliasInUse = errors.New("alias is already assigned to another IP address")

// UpsertIPAlias creates the alias for an IP address, or replaces its existing
// one (one alias per IP). Returns ErrIPAliasInUse if another IP already uses
// this alias.
func (m *DBManager) UpsertIPAlias(ip, alias string) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `
        INSERT INTO ip_aliases (ip_address, alias, created_at, updated_at)
        VALUES ($1, $2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
        ON CONFLICT (ip_address) DO UPDATE SET alias = EXCLUDED.alias, updated_at = CURRENT_TIMESTAMP
    `, ip, alias)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return ErrIPAliasInUse
		}
		utils.ErrorLog("DB UpsertIPAlias error: %v", err)
	}
	return err
}

// DeleteIPAlias removes the alias for an IP address, if one exists.
func (m *DBManager) DeleteIPAlias(ip string) error {
	if m == nil || m.db == nil {
		return fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(ctx, `DELETE FROM ip_aliases WHERE ip_address = $1`, ip)
	return err
}

// ListIPAliases returns every configured alias, most recently updated first.
func (m *DBManager) ListIPAliases() ([]IPAlias, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT ip_address, alias, created_at, updated_at FROM ip_aliases ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	list := make([]IPAlias, 0)
	for rows.Next() {
		var a IPAlias
		if err := rows.Scan(&a.IPAddress, &a.Alias, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// GetIPByAlias resolves an alias back to its IP address (case-insensitive).
// The bool is false when no IP has been assigned that alias.
func (m *DBManager) GetIPByAlias(alias string) (string, bool, error) {
	if m == nil || m.db == nil {
		return "", false, fmt.Errorf("database not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ip string
	err := m.db.QueryRowContext(ctx, `SELECT ip_address FROM ip_aliases WHERE LOWER(alias) = LOWER($1)`, alias).Scan(&ip)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ip, true, nil
}

// GetIPAliasMap loads every alias into an ip -> alias map, for resolving many
// viewer display names in one query instead of one lookup per viewer.
func (m *DBManager) GetIPAliasMap() (map[string]string, error) {
	list, err := m.ListIPAliases()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, a := range list {
		out[a.IPAddress] = a.Alias
	}
	return out, nil
}
