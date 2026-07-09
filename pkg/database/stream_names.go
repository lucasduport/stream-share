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

import "time"

const upsertStreamNameSQL = `
    INSERT INTO stream_names (stream_id, source, name, epg_channel_id, updated_at)
    VALUES ($1, $2, $3, $4, $5)
    ON CONFLICT (stream_id, source) DO UPDATE
        SET name = EXCLUDED.name, epg_channel_id = EXCLUDED.epg_channel_id, updated_at = EXCLUDED.updated_at
`

// UpsertStreamName upserts a single stream name with an optional EPG channel ID.
func (m *DBManager) UpsertStreamName(streamID, source, name, epgChannelID string) error {
	epgIDs := map[string]string{}
	if epgChannelID != "" {
		epgIDs[streamID] = epgChannelID
	}
	return m.UpsertStreamNames(map[string]string{streamID: name}, epgIDs, source)
}

// UpsertStreamNames batch-upserts id→name pairs (and optional EPG channel IDs) for the given source.
func (m *DBManager) UpsertStreamNames(names map[string]string, epgIDs map[string]string, source string) error {
	if m == nil || m.db == nil || len(names) == 0 {
		return nil
	}
	now := time.Now()
	for id, name := range names {
		epgID := ""
		if epgIDs != nil {
			epgID = epgIDs[id]
		}
		if _, err := m.db.Exec(upsertStreamNameSQL, id, source, name, epgID, now); err != nil {
			return err
		}
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
