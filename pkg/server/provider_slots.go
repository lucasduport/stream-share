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

// providerSlotCapacity is how many media connections may be open to the
// provider at once (see provider.Arbiter): PROVIDER_MAX_CONNECTIONS when
// positive, none when negative, otherwise the limit the provider reports in
// its account info, read from the cache only (this runs on every arbitration
// decision and must never wait on the provider). 0 means no limit, which is
// also the answer while the provider's limit is not known yet, so deployments
// whose provider reports nothing behave as before.
func (c *Config) providerSlotCapacity() int {
	switch n := c.ProviderMaxConnections; {
	case n < 0:
		return 0
	case n > 0:
		return n
	}
	if snap := c.providerAccountCached(); snap.Info != nil && snap.Info.MaxConnections > 0 {
		return snap.Info.MaxConnections
	}
	return 0
}
