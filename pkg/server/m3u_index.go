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

package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

var (
	channelIndexMu    sync.RWMutex
	channelIndex      map[string]string
	channelIndexPath  string
	channelIndexMTime time.Time

	// apiChannelIndex maps normalized stream IDs to channel names harvested from
	// the Xtream get_live_streams API response. It is populated whenever a player
	// fetches the live stream list, so name resolution works even when the
	// proxified M3U playlist was never generated (pure Xtream API mode).
	apiChannelIndexMu sync.RWMutex
	apiChannelIndex   map[string]string

	// epgIndex maps normalized stream IDs to EPG channel IDs (tvg-id / epg_channel_id).
	epgIndexMu sync.RWMutex
	epgIndex   map[string]string // normalized stream_id → tvg-id
)

// persistInFlight tracks which sources currently have a persist running.
var persistInFlight sync.Map // source -> struct{}

// lastPersisted records the content hash of the last index successfully written
// per source, so an unchanged index is not rewritten.
var lastPersisted sync.Map // source -> string

// streamNamesHash fingerprints the exact content that would be written. Keys are
// sorted so the hash is stable across map iteration order.
func streamNamesHash(names map[string]string, epgIDs map[string]string) string {
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(names[id]))
		h.Write([]byte{0})
		h.Write([]byte(epgIDs[id]))
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// persistStreamNamesAsync writes the name index to the database off the calling
// goroutine.
//
// This must never run inline: updateAPIChannelIndex is reached from the
// get_live_streams handler, and a full channel list is thousands of rows. Doing
// that write synchronously kept the player waiting until it gave up with a
// broken pipe — and because a disconnected client does not stop the handler, its
// retry stacked another write on top, saturating the connection pool until
// unrelated things (the Discord bot's heartbeat) starved too. The in-flight
// guard is what stops that pile-up; the in-memory index is still updated
// synchronously by the caller, so name resolution is unaffected.
// It also skips the write entirely when the index is byte-for-byte what was last
// written. Players refetch the channel list routinely and the background
// refresher runs on a timer, but the names themselves change rarely — so the
// common case is rewriting thousands of identical rows for nothing. Hashing a
// few thousand entries costs microseconds against the round trips it saves.
func (c *Config) persistStreamNamesAsync(names map[string]string, epgIDs map[string]string, source string) {
	if c.db == nil || len(names) == 0 {
		return
	}

	hash := streamNamesHash(names, epgIDs)
	if prev, ok := lastPersisted.Load(source); ok && prev.(string) == hash {
		utils.DebugLog("stream_names: %s index unchanged (%d entries); skipping write", source, len(names))
		return
	}

	if _, busy := persistInFlight.LoadOrStore(source, struct{}{}); busy {
		utils.DebugLog("stream_names: persist for source=%s already running; skipping this round", source)
		return
	}
	go func() {
		defer persistInFlight.Delete(source)
		if err := c.db.UpsertStreamNames(names, epgIDs, source); err != nil {
			utils.WarnLog("stream_names: failed to persist %s channel index: %v", source, err)
			return
		}
		// Only remember the hash on success, so a failed write is retried next time.
		lastPersisted.Store(source, hash)
		utils.DebugLog("stream_names: persisted %d %s entries", len(names), source)
	}()
}

// updateAPIChannelIndex replaces the API-sourced name index with id→name pairs and optional EPG IDs.
func (c *Config) updateAPIChannelIndex(names map[string]string, epgIDs map[string]string) {
	if len(names) == 0 {
		return
	}
	apiChannelIndexMu.Lock()
	apiChannelIndex = names
	apiChannelIndexMu.Unlock()

	if len(epgIDs) > 0 {
		epgIndexMu.Lock()
		epgIndex = epgIDs // API is authoritative for EPG IDs
		epgIndexMu.Unlock()
	}

	c.persistStreamNamesAsync(names, epgIDs, "api")
}

// lookupEPGChannelID returns the EPG channel ID (tvg-id) for a normalized stream ID.
func lookupEPGChannelID(normalizedID string) (string, bool) {
	epgIndexMu.RLock()
	defer epgIndexMu.RUnlock()
	if epgIndex == nil {
		return "", false
	}
	id, ok := epgIndex[normalizedID]
	return id, ok && id != ""
}

// lookupAPIChannelName returns the channel name for a normalized stream ID.
func lookupAPIChannelName(normalizedID string) (string, bool) {
	apiChannelIndexMu.RLock()
	defer apiChannelIndexMu.RUnlock()
	if apiChannelIndex == nil {
		return "", false
	}
	name, ok := apiChannelIndex[normalizedID]
	return name, ok
}

// normalizeStreamID trims common file extensions from the last path segment
func normalizeStreamID(id string) string {
	if i := strings.Index(id, "."); i > 0 {
		return id[:i]
	}
	return id
}

// ensureChannelIndex parses c.proxyfiedM3UPath and builds/refreshes the channelIndex cache
func (c *Config) ensureChannelIndex() {
	m3uPath := c.proxyfiedM3UPath
	if strings.TrimSpace(m3uPath) == "" {
		return
	}
	info, err := os.Stat(m3uPath)
	if err != nil {
		return
	}

	// Fast path: unchanged
	channelIndexMu.RLock()
	same := channelIndex != nil && channelIndexPath == m3uPath && channelIndexMTime.Equal(info.ModTime())
	channelIndexMu.RUnlock()
	if same {
		return
	}

	// Rebuild under write lock (double-check inside)
	channelIndexMu.Lock()
	defer channelIndexMu.Unlock()

	if channelIndex != nil && channelIndexPath == m3uPath && channelIndexMTime.Equal(info.ModTime()) {
		return
	}

	f, err := os.Open(m3uPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	lastTitle := ""
	lastEPGID := ""
	newIndex := make(map[string]string, 4096)
	newEPGIndex := make(map[string]string, 4096)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if idx := strings.LastIndex(line, ","); idx != -1 && idx+1 < len(line) {
				lastTitle = strings.TrimSpace(line[idx+1:])
			} else {
				lastTitle = ""
			}
			// Extract tvg-id attribute
			lastEPGID = extractM3UAttr(line, "tvg-id")
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			u, err := url.Parse(line)
			if err == nil {
				base := path.Base(u.Path)
				id := normalizeStreamID(base)
				if lastTitle != "" {
					newIndex[id] = lastTitle
				}
				if lastEPGID != "" {
					newEPGIndex[id] = lastEPGID
				}
			}
			lastTitle = ""
			lastEPGID = ""
		}
	}
	// best-effort index

	epgIndexMu.Lock()
	epgIndex = newEPGIndex
	epgIndexMu.Unlock()

	// Write to DB off this goroutine (best-effort, non-fatal). ensureChannelIndex
	// runs under the channelIndex write lock and is reached from name lookups on
	// hot paths, so the write must not block it.
	c.persistStreamNamesAsync(newIndex, newEPGIndex, "m3u")

	channelIndex = newIndex
	channelIndexPath = m3uPath
	channelIndexMTime = info.ModTime()
}

// warmChannelIndexFromDB seeds the in-memory indices from the database on startup.
func (c *Config) warmChannelIndexFromDB() {
	bySource, dbEPGIndex, err := c.db.LoadStreamNames()
	if err != nil {
		utils.WarnLog("stream_names: failed to load from DB: %v", err)
		return
	}
	if m3uNames := bySource["m3u"]; len(m3uNames) > 0 {
		channelIndexMu.Lock()
		if channelIndex == nil {
			channelIndex = m3uNames
		}
		channelIndexMu.Unlock()
	}
	if apiNames := bySource["api"]; len(apiNames) > 0 {
		apiChannelIndexMu.Lock()
		if apiChannelIndex == nil {
			apiChannelIndex = apiNames
		}
		apiChannelIndexMu.Unlock()
	}
	if vodNames := bySource["vod"]; len(vodNames) > 0 {
		vodNameMu.Lock()
		for id, name := range vodNames {
			if _, exists := vodNameIndex[id]; !exists {
				vodNameIndex[id] = name
			}
		}
		vodNameMu.Unlock()
	}
	if len(dbEPGIndex) > 0 {
		epgIndexMu.Lock()
		if epgIndex == nil {
			epgIndex = dbEPGIndex
		}
		epgIndexMu.Unlock()
	}
	total := len(bySource["m3u"]) + len(bySource["api"]) + len(bySource["vod"])
	if total > 0 {
		utils.InfoLog("stream_names: loaded %d names from DB (m3u=%d, api=%d, vod=%d)",
			total, len(bySource["m3u"]), len(bySource["api"]), len(bySource["vod"]))
	}
}

// getChannelNameByID returns the channel name for a given stream ID if known.
// It prefers the proxified M3U index and falls back to names harvested from the
// Xtream get_live_streams API, so resolution works in both M3U and API modes.
func (c *Config) getChannelNameByID(streamID string) (string, bool) {
	id := normalizeStreamID(streamID)

	c.ensureChannelIndex()
	channelIndexMu.RLock()
	if channelIndex != nil {
		if name, ok := channelIndex[id]; ok {
			channelIndexMu.RUnlock()
			return name, true
		}
	}
	channelIndexMu.RUnlock()

	return lookupAPIChannelName(id)
}

// streamLabel formats a stream for logging as "Name (Stream <id>)", falling back
// to "Stream <id>" when no name is known. It resolves live channel names and any
// already-cached VOD titles without network I/O. The id is reported without its
// file extension for readability.
func (c *Config) streamLabel(streamID string) string {
	id := normalizeStreamID(streamID)
	if name, ok := c.resolveStreamName(streamID); ok && strings.TrimSpace(name) != "" {
		return fmt.Sprintf("%s (Stream %s)", strings.TrimSpace(name), id)
	}
	return fmt.Sprintf("Stream %s", id)
}

// extractM3UAttr extracts the value of a key="value" attribute from an #EXTINF line.
func extractM3UAttr(line, key string) string {
	prefix := key + `="`
	start := strings.Index(line, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(line[start:], `"`)
	if end < 0 {
		return ""
	}
	return line[start : start+end]
}
