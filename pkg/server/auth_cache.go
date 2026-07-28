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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// A media player authenticates on every request, including every HTTP Range
// request of a movie — so a single playback drove hundreds of full LDAP bind +
// search round trips. Caching a successful result for a short window removes
// almost all of that.
//
// Only successes are cached. A failure is always re-checked against the
// directory, so a corrected password works immediately and a transient LDAP
// outage is never remembered as a denial.
//
// The trade-off is revocation latency: a disabled account or changed password
// keeps working until its entry expires. That window is LDAP_AUTH_CACHE_MINUTES,
// and setting it to 0 disables the cache entirely for deployments that need
// revocation to take effect at once.
var (
	ldapAuthCache sync.Map // key -> time.Time (expiry)

	// ldapCacheSalt keeps the keys meaningless outside this process: the key is
	// an HMAC of the credentials, never the password itself, and the salt is
	// regenerated on every process start.
	ldapCacheSalt     []byte
	ldapCacheSaltOnce sync.Once
	ldapCacheUsable   bool
)

// ldapCacheKey derives a per-process, non-reversible key for a credential pair.
func ldapCacheKey(username, password string) (string, bool) {
	ldapCacheSaltOnce.Do(func() {
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			// Fail closed: without a salt we will not key a credential cache.
			utils.WarnLog("LDAP auth cache: cannot generate salt (%v); caching disabled", err)
			return
		}
		ldapCacheSalt = salt
		ldapCacheUsable = true
	})
	if !ldapCacheUsable {
		return "", false
	}

	mac := hmac.New(sha256.New, ldapCacheSalt)
	mac.Write([]byte(username))
	mac.Write([]byte{0})
	mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil)), true
}

// ldapCacheTTL returns the configured lifetime of a cached success, or 0 when
// caching is off.
func (c *Config) ldapCacheTTL() time.Duration {
	if c.LDAPAuthCacheMinutes <= 0 {
		return 0
	}
	return time.Duration(c.LDAPAuthCacheMinutes) * time.Minute
}

// ldapAuthenticateCached authenticates against LDAP, reusing a recent successful
// result for the same credentials when caching is enabled.
func (c *Config) ldapAuthenticateCached(username, password string) bool {
	ttl := c.ldapCacheTTL()

	var key string
	var keyed bool
	if ttl > 0 {
		if key, keyed = ldapCacheKey(username, password); keyed {
			if v, ok := ldapAuthCache.Load(key); ok {
				if time.Now().Before(v.(time.Time)) {
					return true
				}
				ldapAuthCache.Delete(key) // expired; fall through and re-check
			}
		}
	}

	ok := ldapAuthenticate(
		c.LDAPServer,
		c.LDAPBaseDN,
		c.LDAPBindDN,
		c.LDAPBindPassword,
		c.LDAPUserAttribute,
		c.LDAPGroupAttribute,
		c.LDAPRequiredGroup,
		username,
		password,
	)

	if ok && keyed {
		ldapAuthCache.Store(key, time.Now().Add(ttl))
	}
	return ok
}

// sweepLDAPCache removes expired entries. Only successful authentications are
// stored, so the map is bounded by the number of real credential pairs in use,
// but expired entries would otherwise linger for the life of the process.
func sweepLDAPCache() {
	now := time.Now()
	ldapAuthCache.Range(func(k, v interface{}) bool {
		if exp, ok := v.(time.Time); ok && now.After(exp) {
			ldapAuthCache.Delete(k)
		}
		return true
	})
}
