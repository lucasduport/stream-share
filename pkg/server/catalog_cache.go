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
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// catalogEntry is a cached, fully-parsed provider catalog response.
type catalogEntry struct {
	data      interface{}
	fetchedAt time.Time
	// failedAt is when the last refresh of this entry failed; guarded by
	// catalogCacheMu. It spaces out retries while the provider is down.
	failedAt time.Time

	// The JSON a player receives for this catalog, encoded once per entry so
	// cache hits don't re-encode tens of MB on every request.
	renderOnce sync.Once
	rendered   []byte
	renderErr  error
}

// renderedJSON returns render(e.data), computed once for this entry. An entry
// is never mutated after it is stored (a refresh stores a new one), so the
// memoised bytes stay valid for the entry's whole life.
func (e *catalogEntry) renderedJSON(render func(interface{}) ([]byte, error)) ([]byte, error) {
	e.renderOnce.Do(func() { e.rendered, e.renderErr = render(e.data) })
	return e.rendered, e.renderErr
}

// catalogCache memoises parsed Xtream catalog responses (get_vod_streams,
// get_series, get_live_streams, the category lists) so player refreshes,
// searches and warm-ups do not re-download and re-decode multi-tens-of-MB JSON
// bodies on every query.
//
// An entry is fresh for catalogCacheTTL. Past that it is still served (the
// provider's last good answer) while a single background refresh replaces it,
// so a slow or failing provider never makes a player wait once a catalog has
// been fetched. Concurrent fetches for the same key are coalesced via per-key
// in-flight tracking, so a burst of requests triggers exactly one provider
// round-trip.
var (
	catalogCacheMu sync.RWMutex
	catalogCache   = map[string]*catalogEntry{}

	// catalogInFlight tracks keys with an active fetch so concurrent callers
	// wait on the same result instead of each starting their own request.
	catalogInFlight   = map[string]*catalogFetch{}
	catalogInFlightMu sync.Mutex
)

// catalogRetryBackoff is how long a failed refresh of an expired entry is
// remembered before another one is attempted; the last good entry is served
// in between.
const catalogRetryBackoff = time.Minute

// catalogFetch represents an in-flight catalog fetch. Waiters block on done
// and then read result/err.
type catalogFetch struct {
	done   chan struct{}
	result *catalogEntry
	err    error
}

// catalogResultError reports a provider answer that must not be cached: a
// transport error (err != nil), or an empty / unusable body the provider
// returned successfully (err == nil). data is what the uncached path would
// have served, so callers with nothing cached can keep that behavior.
type catalogResultError struct {
	data interface{}
	code int
	err  error
}

func (e *catalogResultError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "provider returned an empty or unusable catalog"
}

func (e *catalogResultError) Unwrap() error { return e.err }

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
func storeCachedCatalog(key string, data interface{}) *catalogEntry {
	e := &catalogEntry{data: data, fetchedAt: time.Now()}
	catalogCacheMu.Lock()
	catalogCache[key] = e
	catalogCacheMu.Unlock()
	return e
}

// fetchCatalogOnce returns the catalog for key (see fetchCatalogEntry). A
// provider answer that was empty but not an error is returned as-is, uncached,
// as it was before last-good fallback existed.
func fetchCatalogOnce(key string, fetchFn func() (interface{}, error)) (interface{}, error) {
	e, _, err := fetchCatalogEntry(key, false, fetchFn)
	if err != nil {
		var re *catalogResultError
		if errors.As(err, &re) && re.err == nil {
			return re.data, nil
		}
		return nil, err
	}
	return e.data, nil
}

// fetchCatalogEntry returns the cached entry for key, fetching it via fetchFn
// when absent (or always, when force is set). stale reports that the entry is
// the provider's last good answer rather than a fresh one:
//   - an expired entry is returned immediately and refreshed in the
//     background (one refresh at a time, spaced by catalogRetryBackoff after
//     a failure);
//   - when a blocking fetch fails, the previous entry, if any, is returned
//     instead of the error.
//
// Failures are never cached: with nothing to fall back on, the error is
// returned and the next caller retries.
func fetchCatalogEntry(key string, force bool, fetchFn func() (interface{}, error)) (e *catalogEntry, stale bool, err error) {
	catalogCacheMu.RLock()
	prev := catalogCache[key]
	refreshDue := prev != nil && (prev.failedAt.IsZero() || time.Since(prev.failedAt) >= catalogRetryBackoff)
	catalogCacheMu.RUnlock()

	if prev != nil && !force {
		if time.Since(prev.fetchedAt) < catalogCacheTTL() {
			return prev, false, nil
		}
		if refreshDue {
			startCatalogRefresh(key, fetchFn)
		}
		return prev, true, nil
	}

	e, err = runCatalogFetch(key, force, fetchFn)
	if err == nil {
		return e, false, nil
	}
	if prev != nil {
		utils.WarnLog("Catalog %s: refresh failed, serving last good copy from %s ago: %v",
			catalogKeyLabel(key), time.Since(prev.fetchedAt).Round(time.Second), err)
		return prev, true, nil
	}
	return nil, false, err
}

// startCatalogRefresh refreshes key in the background unless a fetch for it
// is already running.
func startCatalogRefresh(key string, fetchFn func() (interface{}, error)) {
	catalogInFlightMu.Lock()
	if _, busy := catalogInFlight[key]; busy {
		catalogInFlightMu.Unlock()
		return
	}
	f := &catalogFetch{done: make(chan struct{})}
	catalogInFlight[key] = f
	catalogInFlightMu.Unlock()
	go func() {
		if _, err := leadCatalogFetch(key, f, fetchFn); err != nil {
			utils.WarnLog("Catalog %s: background refresh failed, keeping last good copy: %v", catalogKeyLabel(key), err)
		}
	}()
}

// runCatalogFetch fetches key, or joins the fetch already running for it.
func runCatalogFetch(key string, force bool, fetchFn func() (interface{}, error)) (*catalogEntry, error) {
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

	// Double-check after acquiring leadership: another fetch may have
	// completed between the caller's cache check and lock acquisition.
	if !force {
		catalogCacheMu.RLock()
		e := catalogCache[key]
		catalogCacheMu.RUnlock()
		if e != nil && time.Since(e.fetchedAt) < catalogCacheTTL() {
			finishCatalogFetch(key, f, e, nil)
			return e, nil
		}
	}
	return leadCatalogFetch(key, f, fetchFn)
}

// leadCatalogFetch runs fetchFn as the leader of f, stores a successful
// result, records a failure on the existing entry, and releases waiters.
func leadCatalogFetch(key string, f *catalogFetch, fetchFn func() (interface{}, error)) (*catalogEntry, error) {
	data, err := fetchFn()
	if err == nil && data == nil {
		err = &catalogResultError{}
	}
	var e *catalogEntry
	if err == nil {
		e = storeCachedCatalog(key, data)
	} else {
		catalogCacheMu.Lock()
		if prev := catalogCache[key]; prev != nil {
			prev.failedAt = time.Now()
		}
		catalogCacheMu.Unlock()
	}
	finishCatalogFetch(key, f, e, err)
	return e, err
}

func finishCatalogFetch(key string, f *catalogFetch, e *catalogEntry, err error) {
	catalogInFlightMu.Lock()
	delete(catalogInFlight, key)
	catalogInFlightMu.Unlock()
	f.result, f.err = e, err
	close(f.done)
}

// catalogKeyLabel returns the action part of a cache key ("vod_streams") so
// logs never carry the base URL or account that follow it.
func catalogKeyLabel(key string) string {
	if i := strings.Index(key, ":"); i > 0 {
		return key[:i]
	}
	return key
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
