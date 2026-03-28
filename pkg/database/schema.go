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

    "github.com/lucasduport/stream-share/pkg/utils"
)

// initSchema creates database tables if they don't exist
func (m *DBManager) initSchema() error {
    utils.InfoLog("Initializing database schema")

    if m == nil || m.db == nil {
        return fmt.Errorf("database not initialized")
    }

    if _, err := m.db.Exec(`
        CREATE TABLE IF NOT EXISTS discord_ldap_mapping (
            discord_id TEXT PRIMARY KEY,
            discord_name TEXT NOT NULL,
            ldap_username TEXT NOT NULL UNIQUE,
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            last_active TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        )
    `); err != nil {
        utils.ErrorLog("Failed to create discord_ldap_mapping table: %v", err)
        return fmt.Errorf("failed to create discord_ldap_mapping table: %w", err)
    }

    if _, err := m.db.Exec(`
        CREATE TABLE IF NOT EXISTS stream_history (
            id SERIAL PRIMARY KEY,
            username TEXT NOT NULL,
            discord_id TEXT,
            stream_id TEXT NOT NULL,
            stream_type TEXT NOT NULL,
            stream_title TEXT,
            start_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            end_time TIMESTAMP,
            ip_address TEXT,
            user_agent TEXT
        )
    `); err != nil {
        utils.ErrorLog("Failed to create stream_history table: %v", err)
        return fmt.Errorf("failed to create stream_history table: %w", err)
    }

    if _, err := m.db.Exec(`
        CREATE TABLE IF NOT EXISTS temporary_links (
            token TEXT PRIMARY KEY,
            username TEXT NOT NULL,
            url TEXT NOT NULL,
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            expires_at TIMESTAMP NOT NULL,
            stream_id TEXT,
            title TEXT
        )
    `); err != nil {
        utils.ErrorLog("Failed to create temporary_links table: %v", err)
        return fmt.Errorf("failed to create temporary_links table: %w", err)
    }

    if _, err := m.db.Exec(`
        CREATE TABLE IF NOT EXISTS vod_cache (
            stream_id TEXT PRIMARY KEY,
            type TEXT NOT NULL,
            title TEXT,
            series_title TEXT,
            season INTEGER,
            episode INTEGER,
            file_path TEXT NOT NULL,
            requested_by TEXT,
            downloaded_bytes BIGINT DEFAULT 0,
            total_bytes BIGINT DEFAULT 0,
            size_bytes BIGINT DEFAULT 0,
            status TEXT NOT NULL,
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            expires_at TIMESTAMP NOT NULL,
            last_access TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        )
    `); err != nil {
        utils.ErrorLog("Failed to create vod_cache table: %v", err)
        return fmt.Errorf("failed to create vod_cache table: %w", err)
    }

    utils.InfoLog("Database schema initialized successfully")
    return nil
}
