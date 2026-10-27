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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/lucasduport/stream-share/pkg/filter"
)

// Catalogue filtering for player_api lists. The catalog cache always holds the
// provider's full answer (the /watch search and the channel-name index use it
// unfiltered); the filter is applied when the JSON sent to players is rendered.

// catalogAction describes a player_api catalogue action for filtering: the
// kind of its entries, whether it lists categories rather than entries, and
// the other action of the pair (categories <-> entries).
type catalogAction struct {
	kind       filter.Kind
	categories bool
	pair       string
}

var catalogActions = map[string]catalogAction{
	"get_live_streams":      {filter.Live, false, "get_live_categories"},
	"get_vod_streams":       {filter.Movie, false, "get_vod_categories"},
	"get_series":            {filter.Series, false, "get_series_categories"},
	"get_live_categories":   {filter.Live, true, "get_live_streams"},
	"get_vod_categories":    {filter.Movie, true, "get_vod_streams"},
	"get_series_categories": {filter.Series, true, "get_series"},
}

// filterPlayerAPICatalog returns the player_api answer for action with the
// catalogue filter applied, or resp itself when nothing is filtered for that
// kind. resp is the processed provider answer; it is never modified.
func (c *Config) filterPlayerAPICatalog(action string, resp interface{}) interface{} {
	a, ok := catalogActions[action]
	if !ok || !c.catalogFilter.ActiveFor(a.kind) {
		return resp
	}
	items, ok := resp.([]interface{})
	if !ok {
		return resp
	}
	if a.categories {
		return c.filterCategories(a, items)
	}
	return c.filterEntries(a.kind, items, c.categoryNames(a.pair))
}

// filterEntries keeps the stream/series entries that pass the filter, judged
// on their category name (looked up in names), their name and adult flag.
func (c *Config) filterEntries(kind filter.Kind, items []interface{}, names map[string]string) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			out = append(out, item)
			continue
		}
		if c.catalogFilter.Keep(kind, names[catalogString(m["category_id"])], catalogString(m["name"]), catalogFlag(m["is_adult"])) {
			out = append(out, item)
		}
	}
	return out
}

// filterCategories keeps the categories whose name passes the group patterns.
// When the matching entry list is already cached, categories left without any
// kept entry are dropped too, so players do not show empty folders; the list
// is never fetched for this.
func (c *Config) filterCategories(a catalogAction, items []interface{}) []interface{} {
	var used map[string]bool
	if entries, ok := peekCatalog(c.catalogKey(a.pair, "")); ok {
		if list, ok := entries.([]interface{}); ok {
			names := categoryNamesFrom(items)
			used = make(map[string]bool)
			for _, e := range c.filterEntries(a.kind, list, names) {
				if m, ok := e.(map[string]interface{}); ok {
					used[catalogString(m["category_id"])] = true
				}
			}
		}
	}
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			out = append(out, item)
			continue
		}
		if !c.catalogFilter.KeepGroup(a.kind, catalogString(m["category_name"])) {
			continue
		}
		if used != nil && !used[catalogString(m["category_id"])] {
			continue
		}
		out = append(out, item)
	}
	return out
}

// categoryNames returns category_id -> category_name for a categories action,
// from the catalog cache (fetching it if needed). On failure it is empty, and
// entries are then judged on their name alone.
func (c *Config) categoryNames(categoriesAction string) map[string]string {
	if !c.catalogFilter.Active() {
		return nil
	}
	resp, err := fetchCatalogOnce(c.catalogKey(categoriesAction, ""), c.catalogFetcher(categoriesAction, nil))
	if err != nil {
		return nil
	}
	items, _ := resp.([]interface{})
	return categoryNamesFrom(items)
}

func categoryNamesFrom(items []interface{}) map[string]string {
	names := make(map[string]string, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			names[catalogString(m["category_id"])] = catalogString(m["category_name"])
		}
	}
	return names
}

// catalogString renders a decoded JSON scalar (string or number) as a string,
// "" for nil.
func catalogString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}

// catalogFlag reads a provider boolean that may come as 1, "1", true or "true".
func catalogFlag(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case json.Number:
		return t.String() == "1"
	case float64:
		return t == 1
	case int:
		return t == 1
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	}
	return false
}

// liveStreamFiltered reports whether a live stream id is known and dropped by
// the catalogue filter, from the cached live catalogue only. Unknown ids (or
// no cached catalogue) are allowed: the filter fails open for streams.
func (c *Config) liveStreamFiltered(streamID string) bool {
	if !c.catalogFilter.ActiveFor(filter.Live) {
		return false
	}
	set := c.keptLiveIDs()
	if set == nil {
		return false
	}
	kept, known := set.ids[normalizeStreamID(streamID)]
	return known && !kept
}

// liveKeptSet is the outcome of the live filter over one cached live
// catalogue: every known stream id with whether it is kept, and the EPG
// channel ids of the kept streams.
type liveKeptSet struct {
	src    *catalogEntry
	cats   *catalogEntry
	ids    map[string]bool
	epgIDs map[string]bool

	epgKeyOnce sync.Once
	epgKeyVal  string
}

// epgKey fingerprints the kept EPG channel ids, computed once per set: two
// sets with the same key keep the same guide channels.
func (s *liveKeptSet) epgKey() string {
	s.epgKeyOnce.Do(func() {
		ids := make([]string, 0, len(s.epgIDs))
		for id := range s.epgIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		h := sha256.New()
		for _, id := range ids {
			h.Write([]byte(id))
			h.Write([]byte{0})
		}
		s.epgKeyVal = hex.EncodeToString(h.Sum(nil)[:8])
	})
	return s.epgKeyVal
}

var (
	liveKeptMu  sync.Mutex
	liveKeptMem *liveKeptSet
)

// keptLiveIDs returns the live filter's outcome over the cached live
// catalogue and categories, recomputed only when either changes. It never
// fetches (it runs on the stream request path): nil when no live catalogue is
// cached yet; without cached categories, channels are judged on their name.
func (c *Config) keptLiveIDs() *liveKeptSet {
	e := peekCatalogEntry(c.catalogKey("get_live_streams", ""))
	if e == nil {
		return nil
	}
	cats := peekCatalogEntry(c.catalogKey("get_live_categories", ""))
	liveKeptMu.Lock()
	defer liveKeptMu.Unlock()
	if liveKeptMem != nil && liveKeptMem.src == e && liveKeptMem.cats == cats {
		return liveKeptMem
	}
	items, _ := e.data.([]interface{})
	var names map[string]string
	if cats != nil {
		catItems, _ := cats.data.([]interface{})
		names = categoryNamesFrom(catItems)
	}
	set := &liveKeptSet{src: e, cats: cats, ids: make(map[string]bool, len(items)), epgIDs: make(map[string]bool)}
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := normalizeStreamID(catalogString(m["stream_id"]))
		if id == "" {
			continue
		}
		kept := c.catalogFilter.Keep(filter.Live, names[catalogString(m["category_id"])], catalogString(m["name"]), catalogFlag(m["is_adult"]))
		set.ids[id] = kept
		if epg := catalogString(m["epg_channel_id"]); kept && epg != "" {
			set.epgIDs[epg] = true
		}
	}
	liveKeptMem = set
	return set
}
