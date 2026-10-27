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

package server

import (
	"fmt"
	"strconv"
	"sync"
)

// Channel numbers ("num", the number players show next to a live channel)
// come from the cached get_live_streams catalogue. The stream id -> number
// index is rebuilt only when that cache entry changes, and never fetched: the
// status path must not wait on the provider.
var (
	liveNumMu    sync.Mutex
	liveNumEntry *catalogEntry
	liveNumIndex map[string]int
)

// liveChannelNumber returns the channel number of a live stream id, when the
// cached live catalogue knows it.
func (c *Config) liveChannelNumber(streamID string) (int, bool) {
	e := peekCatalogEntry(c.catalogKey("get_live_streams", ""))
	if e == nil {
		return 0, false
	}
	liveNumMu.Lock()
	if liveNumEntry != e {
		liveNumIndex = buildLiveNumIndex(e.data)
		liveNumEntry = e
	}
	idx := liveNumIndex
	liveNumMu.Unlock()
	n, ok := idx[normalizeStreamID(streamID)]
	return n, ok
}

// buildLiveNumIndex maps stream_id to num for a decoded get_live_streams list.
// Both fields may be strings, json.Number or floats depending on the decoder.
func buildLiveNumIndex(data interface{}) map[string]int {
	items, _ := data.([]interface{})
	idx := make(map[string]int, len(items))
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		id := normalizeStreamID(catalogString(m["stream_id"]))
		f, err := strconv.ParseFloat(catalogString(m["num"]), 64)
		if id == "" || err != nil || f <= 0 {
			continue
		}
		idx[id] = int(f)
	}
	return idx
}

// streamRef renders a stream's identifiers for status text, so similarly
// named entries can be told apart: "(n°41 · id 506638)", or "(id 506638)"
// when the channel number is unknown.
func streamRef(it activeStreamItem) string {
	id := normalizeStreamID(it.StreamID)
	if it.Num > 0 {
		return fmt.Sprintf("(n°%d · id %s)", it.Num, id)
	}
	return fmt.Sprintf("(id %s)", id)
}
