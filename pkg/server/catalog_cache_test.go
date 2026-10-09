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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogCacheStoresAndRetrieves(t *testing.T) {
	invalidateCatalogCache()
	key := "test:store"
	want := []interface{}{"a", "b"}

	storeCachedCatalog(key, want)
	got, ok := getCachedCatalog(key)
	if !ok {
		t.Fatal("expected cache hit after store")
	}
	arr, ok := got.([]interface{})
	if !ok || len(arr) != 2 {
		t.Fatalf("unexpected cached value: %T %v", got, got)
	}
}

func TestCatalogCacheExpiry(t *testing.T) {
	invalidateCatalogCache()
	key := "test:expiry"

	catalogCacheMu.Lock()
	catalogCache[key] = &catalogEntry{data: "old", fetchedAt: time.Now().Add(-2 * time.Hour)}
	catalogCacheMu.Unlock()

	if _, ok := getCachedCatalog(key); ok {
		t.Fatal("expected expired entry to be a miss")
	}
}

func TestFetchCatalogOnceCoalesces(t *testing.T) {
	invalidateCatalogCache()
	key := "test:coalesce"

	var calls int32
	fetchFn := func() (interface{}, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return "data", nil
	}

	const goroutines = 10
	var wg sync.WaitGroup
	results := make([]interface{}, goroutines)
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = fetchCatalogOnce(key, fetchFn)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", got)
	}
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d got error: %v", i, errs[i])
		}
		if results[i] != "data" {
			t.Fatalf("goroutine %d got %v, want %q", i, results[i], "data")
		}
	}
}

func TestFetchCatalogOnceErrorNotCached(t *testing.T) {
	invalidateCatalogCache()
	key := "test:error"

	wantErr := errors.New("provider down")
	var calls int32
	fetchFn := func() (interface{}, error) {
		atomic.AddInt32(&calls, 1)
		return nil, wantErr
	}

	if _, err := fetchCatalogOnce(key, fetchFn); err == nil {
		t.Fatal("expected error on first fetch")
	}
	if _, ok := getCachedCatalog(key); ok {
		t.Fatal("error result must not be cached")
	}

	// Second call should retry (calls == 2), not return the cached error.
	if _, err := fetchCatalogOnce(key, fetchFn); err == nil {
		t.Fatal("expected error on second fetch")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 fetch attempts, got %d", got)
	}
}

func TestInvalidateCatalogCacheKey(t *testing.T) {
	invalidateCatalogCache()
	storeCachedCatalog("k1", "v1")
	storeCachedCatalog("k2", "v2")

	invalidateCatalogCacheKey("k1")

	if _, ok := getCachedCatalog("k1"); ok {
		t.Fatal("k1 should be invalidated")
	}
	if _, ok := getCachedCatalog("k2"); !ok {
		t.Fatal("k2 should still be cached")
	}
}
