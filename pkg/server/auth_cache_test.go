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
	"strings"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
)

func clearLDAPCache() {
	ldapAuthCache.Range(func(k, _ interface{}) bool {
		ldapAuthCache.Delete(k)
		return true
	})
}

func cacheConfig(minutes int) *Config {
	return &Config{ProxyConfig: &config.ProxyConfig{
		LDAPEnabled:          true,
		LDAPAuthCacheMinutes: minutes,
	}}
}

// TestLDAPCacheKeyNeverContainsCredentials is the property that matters most:
// the key must not be reversible into the password.
func TestLDAPCacheKeyNeverContainsCredentials(t *testing.T) {
	const user, pass = "alice", "sup3r-s3cret"
	key, ok := ldapCacheKey(user, pass)
	if !ok {
		t.Fatal("expected a usable cache key")
	}
	if strings.Contains(key, pass) || strings.Contains(key, user) {
		t.Fatalf("cache key leaks credentials: %q", key)
	}
	if len(key) != 64 { // hex-encoded SHA-256
		t.Fatalf("unexpected key length %d", len(key))
	}
}

// TestLDAPCacheKeyDistinguishesCredentials guards against the dangerous
// collision: a different password must never map onto an existing success.
func TestLDAPCacheKeyDistinguishesCredentials(t *testing.T) {
	base, _ := ldapCacheKey("alice", "secret")

	cases := []struct{ name, user, pass string }{
		{"different password", "alice", "secret2"},
		{"different user", "bob", "secret"},
		{"empty password", "alice", ""},
		// Without a separator, ("ali","cesecret") would concatenate to the same
		// bytes as ("alice","secret").
		{"boundary shift", "ali", "cesecret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other, _ := ldapCacheKey(tc.user, tc.pass)
			if other == base {
				t.Fatalf("key collision between (alice,secret) and (%s,%s)", tc.user, tc.pass)
			}
		})
	}
}

func TestLDAPCacheKeyStable(t *testing.T) {
	a, _ := ldapCacheKey("alice", "secret")
	b, _ := ldapCacheKey("alice", "secret")
	if a != b {
		t.Fatal("same credentials produced different keys within a process")
	}
}

// TestLDAPCacheDisabledByZero: the escape hatch must actually bypass the cache.
func TestLDAPCacheDisabledByZero(t *testing.T) {
	if ttl := cacheConfig(0).ldapCacheTTL(); ttl != 0 {
		t.Fatalf("ldapCacheTTL() = %s with 0 minutes, want 0", ttl)
	}
	if ttl := cacheConfig(5).ldapCacheTTL(); ttl != 5*time.Minute {
		t.Fatalf("ldapCacheTTL() = %s, want 5m", ttl)
	}
	if ttl := cacheConfig(-1).ldapCacheTTL(); ttl != 0 {
		t.Fatalf("negative minutes should disable the cache, got %s", ttl)
	}
}

// TestLDAPCacheHitAvoidsDirectory verifies a stored entry short-circuits, and
// that an expired one does not.
func TestLDAPCacheHitAvoidsDirectory(t *testing.T) {
	clearLDAPCache()
	defer clearLDAPCache()

	c := cacheConfig(5)
	key, ok := ldapCacheKey("alice", "secret")
	if !ok {
		t.Fatal("no cache key")
	}

	// A live entry is trusted. If it were not, this would attempt a real LDAP
	// dial against an unset server and fail.
	ldapAuthCache.Store(key, time.Now().Add(time.Minute))
	if !c.ldapAuthenticateCached("alice", "secret") {
		t.Fatal("a live cache entry should authenticate without contacting LDAP")
	}

	// An expired entry must fall through and be re-checked (which fails here,
	// since there is no directory to talk to).
	ldapAuthCache.Store(key, time.Now().Add(-time.Minute))
	if c.ldapAuthenticateCached("alice", "secret") {
		t.Fatal("an expired entry must not authenticate")
	}
	if _, still := ldapAuthCache.Load(key); still {
		t.Fatal("expired entry should have been evicted on lookup")
	}
}

// TestLDAPCacheDoesNotStoreFailures: a failed authentication must never be
// cached, so a corrected password works immediately and an LDAP outage is not
// remembered as a denial.
func TestLDAPCacheDoesNotStoreFailures(t *testing.T) {
	clearLDAPCache()
	defer clearLDAPCache()

	// No LDAP server configured, so this authentication fails.
	c := cacheConfig(5)
	if c.ldapAuthenticateCached("alice", "wrong") {
		t.Fatal("expected authentication to fail with no directory configured")
	}

	key, _ := ldapCacheKey("alice", "wrong")
	if _, found := ldapAuthCache.Load(key); found {
		t.Fatal("a failed authentication must not be cached")
	}
}

// TestLDAPCacheDisabledDoesNotStore: with the cache off, nothing accumulates.
func TestLDAPCacheDisabledDoesNotStore(t *testing.T) {
	clearLDAPCache()
	defer clearLDAPCache()

	c := cacheConfig(0)
	key, _ := ldapCacheKey("alice", "secret")
	ldapAuthCache.Store(key, time.Now().Add(time.Minute))

	// Even with an entry present, a disabled cache must consult the directory,
	// which fails here.
	if c.ldapAuthenticateCached("alice", "secret") {
		t.Fatal("disabled cache must not short-circuit on a stored entry")
	}
}

func TestSweepLDAPCacheDropsOnlyExpired(t *testing.T) {
	clearLDAPCache()
	defer clearLDAPCache()

	live, _ := ldapCacheKey("live", "x")
	dead, _ := ldapCacheKey("dead", "x")
	ldapAuthCache.Store(live, time.Now().Add(time.Hour))
	ldapAuthCache.Store(dead, time.Now().Add(-time.Hour))

	sweepLDAPCache()

	if _, ok := ldapAuthCache.Load(live); !ok {
		t.Fatal("sweep removed a live entry")
	}
	if _, ok := ldapAuthCache.Load(dead); ok {
		t.Fatal("sweep left an expired entry behind")
	}
}
