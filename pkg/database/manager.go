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
    "time"

    "github.com/lucasduport/stream-share/pkg/utils"
    _ "github.com/lib/pq"
)

// DBManager handles database operations
type DBManager struct {
    db          *sql.DB
    initialized bool
}

// NewDBManager creates a new database manager
func NewDBManager(_ string) (*DBManager, error) {
    utils.InfoLog("Initializing PostgreSQL database connection")

    host := utils.GetEnvOrDefault("DB_HOST", "localhost")
    port := utils.GetEnvOrDefault("DB_PORT", "5432")
    dbName := utils.GetEnvOrDefault("DB_NAME", "iptvproxy")
    user := utils.GetEnvOrDefault("DB_USER", "postgres")
    password := utils.GetEnvOrDefault("DB_PASSWORD", "")
    sslMode := utils.GetEnvOrDefault("DB_SSLMODE", "disable")

    connStr := fmt.Sprintf(
        "host=%s port=%s dbname=%s user=%s password=%s sslmode=%s",
        host, port, dbName, user, password, sslMode,
    )

    utils.DebugLog("Connecting to PostgreSQL: host=%s port=%s dbname=%s user=%s", host, port, dbName, user)

    db, err := sql.Open("postgres", connStr)
    if err != nil {
        return nil, fmt.Errorf("failed to open PostgreSQL database: %w", err)
    }

    if err := db.Ping(); err != nil {
        utils.ErrorLog("Failed to connect to database: %v", err)
        return nil, fmt.Errorf("database connection test failed: %w", err)
    }
    utils.InfoLog("Database connection successful")

    db.SetMaxOpenConns(10)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(time.Hour)

    manager := &DBManager{db: db}
    if err := manager.initSchema(); err != nil {
        db.Close()
        return nil, err
    }

    manager.initialized = true
    return manager, nil
}

// IsInitialized returns whether the database is initialized
func (m *DBManager) IsInitialized() bool {
    return m != nil && m.initialized && m.db != nil
}

// Close closes the database connection
func (m *DBManager) Close() error {
    if m == nil || m.db == nil {
        return nil
    }
    utils.InfoLog("Closing database connection")
    return m.db.Close()
}
