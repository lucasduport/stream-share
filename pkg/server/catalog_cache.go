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
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// catalogEntry is a cached, fully-parsed provider catalog response.
type catalogEntry struct {
	data      interface{}
	fetchedAt time.Time
}

// catalogCache memoises parsed Xtream catalog responses (get_vod_streams,
// get_series, get_live_streams) so repeated searches and warm-ups do not
// re-download and re-decode multi-tens-of-MB JSON bodies on every query.
//
// Entries live for catalogCacheTTL; a background refresh keeps them warm on
// the provider's refresh cadence. Concurrent fetches for the same key are
// coalesced via per-key in-flight tracking, so a burst of searches triggers
// exactly one provider round-trip.
var (
	catalogCacheMu sync.RWMutex
	catalogCache   = map[string]*catalogEntry{}

	// catalogInFlight tracks keys with an active fetch so concurrent callers
	// wait on the same result instead of each starting their own request.
	catalogInFlight   = map[string]*catalogFetch{}
	catalogInFlightMu sync.Mutex
)

// catalogFetch represents an in-flight catalog fetch. Waiters block on done
// and then read result/err.
type catalogFetch struct {
	done   chan struct{}
	result interface{}
	err    error
}

// catalogCacheTTL is how long a parsed catalog is considered fresh. It tracks
// the M3U cache expiration cadence (default 1h) but is kept independent so it
// can be tuned separately via env if needed.
func catalogCacheTTL() time.Duration {
	if v := utils.GetenvInt("CATALOG_CACHE_TTL_MINUTES"); v > 0 {
		return time.Duration(v) * time.Minute
	}
	return time.Hour
}

// getCachedCatalog returns the cached parsed catalog for key if it exists and
// is still fresh.
func getCachedCatalog(key string) (interface{}, bool) {
	catalogCacheMu.RLock()
	e, ok := catalogCache[key]
	catalogCacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(e.fetchedAt) >= catalogCacheTTL() {
		return nil, false
	}
	return e.data, true
}

// storeCachedCatalog saves a parsed catalog under key.
func storeCachedCatalog(key string, data interface{}) {
	catalogCacheMu.Lock()
	catalogCache[key] = &catalogEntry{data: data, fetchedAt: time.Now()}
	catalogCacheMu.Unlock()
}

// fetchCatalogOnce returns the cached catalog for key, or fetches it via
// fetchFn. Concurrent callers for the same key share a single fetch.
// The fetcher receives a boolean indicating whether it is the leader (true)
// or a waiter (false); waiters should return the shared result immediately.
//
// On fetch error the error is returned to all waiters and nothing is cached,
// so the next caller retries.
func fetchCatalogOnce(key string, fetchFn func() (interface{}, error)) (interface{}, error) {
	// Fast path: fresh cache hit.
	if data, ok := getCachedCatalog(key); ok {
		return data, nil
	}

	catalogInFlightMu.Lock()
	if f, busy := catalogInFlight[key]; busy {
		// Another goroutine is fetching; wait for it.
		catalogInFlightMu.Unlock()
		<-f.done
		return f.result, f.err
	}
	// Become the leader.
	f := &catalogFetch{done: make(chan struct{})}
	catalogInFlight[key] = f
	catalogInFlightMu.Unlock()

	// Double-check cache after acquiring leadership (another fetch may have
	// completed between the fast-path check and lock acquisition).
	if data, ok := getCachedCatalog(key); ok {
		f.result = data
		close(f.done)
		catalogInFlightMu.Lock()
		delete(catalogInFlight, key)
		catalogInFlightMu.Unlock()
		return data, nil
	}

	data, err := fetchFn()

	catalogInFlightMu.Lock()
	delete(catalogInFlight, key)
	catalogInFlightMu.Unlock()

	f.result = data
	f.err = err
	close(f.done)

	if err == nil && data != nil {
		storeCachedCatalog(key, data)
	}
	return data, err
}

// invalidateCatalogCache drops all cached catalogs. Called when the provider
// configuration changes or on explicit refresh.
func invalidateCatalogCache() {
	catalogCacheMu.Lock()
	catalogCache = map[string]*catalogEntry{}
	catalogCacheMu.Unlock()
}

// invalidateCatalogCacheKey drops a single cached catalog entry, forcing the
// next access to re-fetch from the provider.
func invalidateCatalogCacheKey(key string) {
	catalogCacheMu.Lock()
	delete(catalogCache, key)
	catalogCacheMu.Unlock()
}
