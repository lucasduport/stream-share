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
	"fmt"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// upsertBatchRows is how many rows go into a single multi-row INSERT. Postgres
// caps a statement at 65535 bound parameters and each row binds 5, so the hard
// ceiling is ~13k; 500 keeps statements small while still turning a 10k-channel
// playlist into 20 round trips instead of 10,000.
const upsertBatchRows = 500

// upsertStreamNamesTimeout bounds a full persist so a slow or wedged database
// cannot pin a connection indefinitely.
const upsertStreamNamesTimeout = 30 * time.Second

const upsertStreamNamesSuffix = `
    ON CONFLICT (stream_id, source) DO UPDATE
        SET name = EXCLUDED.name, epg_channel_id = EXCLUDED.epg_channel_id, updated_at = EXCLUDED.updated_at`

// UpsertStreamName upserts a single stream name with an optional EPG channel ID.
func (m *DBManager) UpsertStreamName(streamID, source, name, epgChannelID string) error {
	epgIDs := map[string]string{}
	if epgChannelID != "" {
		epgIDs[streamID] = epgChannelID
	}
	return m.UpsertStreamNames(map[string]string{streamID: name}, epgIDs, source)
}

// UpsertStreamNames batch-upserts id→name pairs (and optional EPG channel IDs)
// for the given source.
//
// Rows are written in multi-row batches rather than one statement per name: a
// full channel list runs to thousands of entries, and a per-row loop meant
// thousands of sequential round trips, which is slow enough to stall whatever
// called it and to tie up a pooled connection for minutes.
func (m *DBManager) UpsertStreamNames(names map[string]string, epgIDs map[string]string, source string) error {
	if m == nil || m.db == nil || len(names) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), upsertStreamNamesTimeout)
	defer cancel()

	now := time.Now()
	started := now

	// Flatten to a stable slice so batching is straightforward.
	type row struct{ id, name, epgID string }
	rows := make([]row, 0, len(names))
	for id, name := range names {
		epgID := ""
		if epgIDs != nil {
			epgID = epgIDs[id]
		}
		rows = append(rows, row{id: id, name: name, epgID: epgID})
	}

	for start := 0; start < len(rows); start += upsertBatchRows {
		end := start + upsertBatchRows
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]

		var sb strings.Builder
		sb.WriteString("INSERT INTO stream_names (stream_id, source, name, epg_channel_id, updated_at) VALUES ")
		args := make([]interface{}, 0, len(batch)*5)
		for i, r := range batch {
			if i > 0 {
				sb.WriteString(",")
			}
			n := i * 5
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5)
			args = append(args, r.id, source, r.name, r.epgID, now)
		}
		sb.WriteString(upsertStreamNamesSuffix)

		if _, err := m.db.ExecContext(ctx, sb.String(), args...); err != nil {
			utils.ErrorLog("Database error upserting stream names (source=%s, batch %d-%d of %d): %v",
				source, start, end, len(rows), err)
			return err
		}
	}

	if elapsed := time.Since(started); elapsed > time.Second {
		utils.DebugLog("Database: upserted %d stream name(s) for source=%s in %s",
			len(rows), source, utils.HumanDuration(elapsed))
	}
	return nil
}

// LoadStreamNames loads all stream names grouped by source, plus a merged EPG channel ID index.
// Returns (map[source]map[streamID]name, map[streamID]epgChannelID, error).
// The EPG index merges across sources; last non-empty value wins (api > m3u).
func (m *DBManager) LoadStreamNames() (map[string]map[string]string, map[string]string, error) {
	bySource := map[string]map[string]string{}
	epgIndex := map[string]string{}
	if m == nil || m.db == nil {
		return bySource, epgIndex, nil
	}
	rows, err := m.db.Query(`SELECT stream_id, source, name, epg_channel_id FROM stream_names`)
	if err != nil {
		return bySource, epgIndex, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, source, name, epgID string
		if err := rows.Scan(&id, &source, &name, &epgID); err != nil {
			continue
		}
		if bySource[source] == nil {
			bySource[source] = map[string]string{}
		}
		bySource[source][id] = name
		if epgID != "" {
			epgIndex[id] = epgID
		}
	}
	return bySource, epgIndex, rows.Err()
}
