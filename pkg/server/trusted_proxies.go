/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 * Copyright (C) 2026  x3n0n10
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

import "github.com/gin-gonic/gin"

// privateProxyRanges are the peers trusted to set X-Forwarded-For when
// REVERSE_PROXY_ENABLED is on: a proxy on the same host or Docker network.
var privateProxyRanges = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"}

// applyTrustedProxies makes ctx.ClientIP() honour X-Forwarded-For only from a
// private-network proxy; otherwise any client could spoof its IP.
func (c *Config) applyTrustedProxies(r *gin.Engine) error {
	if !c.ReverseProxyEnabled {
		return r.SetTrustedProxies(nil)
	}
	return r.SetTrustedProxies(privateProxyRanges)
}
