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
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

// VOD titles are resolved lazily, one item at a time via the Xtream
// get_vod_info endpoint, rather than pulling the (potentially huge) full VOD
// catalog. Results are cached so each id is fetched at most once. A cached
// empty string means "looked up, no title" so confirmed misses are not retried.
var (
	vodNameMu    sync.RWMutex
	vodNameIndex = map[string]string{} // normalized vod_id -> title ("" = confirmed no title)
)

// vodNameCached returns a previously resolved VOD title without performing any
// network call. It is safe to call from hot/locked paths (e.g. session-manager
// logging) because it only reads the local cache.
func vodNameCached(streamID string) (string, bool) {
	id := normalizeStreamID(streamID)
	vodNameMu.RLock()
	name, ok := vodNameIndex[id]
	vodNameMu.RUnlock()
	return name, ok && name != ""
}

// vodTitleByID lazily resolves a movie title via get_vod_info, caching the
// result (including confirmed misses). It performs a network call on a cache
// miss, so it must NOT be called while holding a lock the stream hot path needs.
func (c *Config) vodTitleByID(streamID string) (string, bool) {
	id := normalizeStreamID(streamID)
	if id == "" {
		return "", false
	}

	vodNameMu.RLock()
	name, cached := vodNameIndex[id]
	vodNameMu.RUnlock()
	if cached {
		return name, name != ""
	}

	title, resolved := c.fetchVODTitle(id)
	if !resolved {
		return "", false // transient failure: do not cache, allow a later retry
	}

	vodNameMu.Lock()
	vodNameIndex[id] = title // title may be "" = confirmed no title
	vodNameMu.Unlock()
	if title != "" {
		if err := c.db.UpsertStreamName(id, "vod", title, ""); err != nil {
			utils.WarnLog("stream_names: failed to persist VOD name for %s: %v", id, err)
		}
	}
	return title, title != ""
}

// fetchVODTitle calls get_vod_info for a single movie id. The bool return is
// false only for transient failures (so the caller can retry later); it is true
// when the upstream answered, even if no usable title was present.
func (c *Config) fetchVODTitle(vodID string) (title string, resolved bool) {
	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, "")
	if err != nil {
		utils.WarnLog("VOD title: failed to create Xtream client: %v", err)
		return "", false
	}
	resp, _, _, err := client.Action(c.ProxyConfig, "get_vod_info", url.Values{"vod_id": {vodID}})
	if err != nil {
		utils.DebugLog("VOD title: get_vod_info failed for vod_id=%s: %v", vodID, err)
		return "", false
	}
	m, ok := resp.(map[string]interface{})
	if !ok {
		return "", true // upstream answered but not a usable object → confirmed empty
	}
	// Xtream returns the title under movie_data.name, with info.name as a fallback.
	if md, ok := m["movie_data"].(map[string]interface{}); ok {
		if n, ok := md["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n), true
		}
	}
	if info, ok := m["info"].(map[string]interface{}); ok {
		if n, ok := info["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n), true
		}
	}
	return "", true
}

// resolveStreamName resolves a human title for any stream id without performing
// network I/O: live channel index first, then the local VOD title cache. It is
// injected into the session manager as the name resolver, so it is safe to call
// while holding streamLock.
func (c *Config) resolveStreamName(streamID string) (string, bool) {
	if name, ok := c.getChannelNameByID(streamID); ok && strings.TrimSpace(name) != "" {
		return name, true
	}
	return vodNameCached(streamID)
}

// titleMissRetryAfter is how long a failed VOD title lookup is remembered before
// it is attempted again. Long enough that a playback's Range requests never
// rescan, short enough that a title appearing in a refreshed catalogue is picked
// up without a restart.
const titleMissRetryAfter = 30 * time.Minute

// titleAttempt is a memoised outcome of resolveTitleAtStart. A hit is kept
// indefinitely (titles do not change); a miss expires so it can be retried.
type titleAttempt struct {
	title   string
	expires time.Time // zero for hits, which never expire
}

var (
	titleResolvedMu    sync.RWMutex
	titleResolvedIndex = map[string]titleAttempt{} // normalized id -> outcome
)

// titleResolvedLookup returns a previously resolved outcome, if still valid.
func titleResolvedLookup(streamID string) (string, bool) {
	id := normalizeStreamID(streamID)
	titleResolvedMu.RLock()
	a, ok := titleResolvedIndex[id]
	titleResolvedMu.RUnlock()
	if !ok {
		return "", false
	}
	if a.title == "" && !a.expires.IsZero() && time.Now().After(a.expires) {
		return "", false // stale miss: worth another look
	}
	return a.title, true
}

// titleResolvedStore memoises an outcome, expiring misses so they get retried.
func titleResolvedStore(streamID, title string) {
	id := normalizeStreamID(streamID)
	a := titleAttempt{title: title}
	if title == "" {
		a.expires = time.Now().Add(titleMissRetryAfter)
	}
	titleResolvedMu.Lock()
	titleResolvedIndex[id] = a
	titleResolvedMu.Unlock()
}

// resolveTitleAtStart resolves a stream's title when it first starts, allowed to
// hit the network for VOD. Used off the lock to populate StreamTitle and warm the
// VOD cache so subsequent (locked) log lookups resolve from cache.
//
// The VOD branches are memoised, misses included. Both are expensive — a
// get_vod_info round trip, or a linear scan of the provider's full catalogue —
// and this runs per HTTP Range request, of which a single playback issues
// hundreds. Without memoising the miss, a title that cannot be found is searched
// for again on every one of them.
func (c *Config) resolveTitleAtStart(streamID, streamType string) (string, bool) {
	if name, ok := c.getChannelNameByID(streamID); ok && strings.TrimSpace(name) != "" {
		return name, true
	}

	// Only VOD is memoised here. Live names come from the channel index above,
	// which fills in asynchronously — caching a miss for a live stream would
	// pin an empty title that never recovers.
	if streamType != "movie" && streamType != "series" {
		return "", false
	}

	if title, done := titleResolvedLookup(streamID); done {
		return title, title != ""
	}

	title := ""
	switch streamType {
	case "movie":
		if name, ok := c.vodTitleByID(streamID); ok {
			title = strings.TrimSpace(name)
		} else if t := c.findVODTitleInCache("movie", streamID); strings.TrimSpace(t) != "" {
			// The API gave us nothing usable; fall back to the cached VOD M3U.
			title = strings.TrimSpace(t)
		}
	case "series":
		// get_vod_info is keyed by a movie vod_id and cannot resolve a series
		// episode id, so series rely on the cached VOD M3U title when available.
		title = strings.TrimSpace(c.findVODTitleInCache("series", streamID))
	}

	titleResolvedStore(streamID, title)
	if title == "" {
		return "", false
	}
	c.cacheVODName(streamID, title)
	return title, true
}

// cacheVODName stores a resolved title in the VOD cache (e.g. one found via the
// M3U cache) so locked log lookups can later resolve it without network I/O.
func (c *Config) cacheVODName(streamID, title string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	id := normalizeStreamID(streamID)

	// Skip the write when we already hold this exact title: the row would be
	// identical, and this is reached from per-request paths.
	vodNameMu.Lock()
	unchanged := vodNameIndex[id] == title
	vodNameIndex[id] = title
	vodNameMu.Unlock()
	if unchanged {
		return
	}

	if err := c.db.UpsertStreamName(id, "vod", title, ""); err != nil {
		utils.WarnLog("stream_names: failed to persist VOD name for %s: %v", id, err)
	}
}

// vodLabel formats a VOD stream for logging as "Title (Stream <id>)", resolving
// the title via get_vod_info (cached) when needed. Safe in per-request handlers
// (not under streamLock).
func (c *Config) vodLabel(streamID string) string {
	id := normalizeStreamID(streamID)
	if name, ok := c.vodTitleByID(streamID); ok && strings.TrimSpace(name) != "" {
		return fmt.Sprintf("%s (Stream %s)", strings.TrimSpace(name), id)
	}
	return fmt.Sprintf("Stream %s", id)
}
