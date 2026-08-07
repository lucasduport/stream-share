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

package utils

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// MaskString masks sensitive parts of strings for logging.
func MaskString(s string) string {
	switch {
	case len(s) == 0:
		return "[empty]"
	case len(s) <= 8:
		return s[:1] + "******"
	default:
		return s[:4] + "..." + s[len(s)-4:]
	}
}

// MaskURL masks sensitive parts of URLs for logging.
// It follows the same logic as the original server package helper.
func MaskURL(urlStr string) string {
	parts := strings.Split(urlStr, "/")
	if len(parts) >= 7 {
		// For URLs like http://host/path/user/pass/id
		parts[5] = MaskString(parts[5]) // Password
		parts[4] = MaskString(parts[4]) // Username
	}
	return strings.Join(parts, "/")
}

// NormalizeHostname sanitizes an operator-supplied hostname so it can be safely
// composed into generated URLs of the form "<scheme>://<host>[:<port>]...".
// Operators frequently set HOSTNAME (or --hostname) to a full URL such as
// "https://tv.example.com:8443/whatever"; composing that naively yields
// malformed links — a duplicated scheme ("http://https://...") or a duplicated
// port ("host:8443:8080"). It strips a leading scheme (tolerating the "https//"
// missing-colon typo), discards any path/query/fragment, and splits off a
// trailing port. It returns the bare host (IPv6 literals keep their brackets),
// the port (0 when none was present), and whether the removed scheme was https,
// so callers can honor the operator's evident intent to serve over TLS and on
// that port.
func NormalizeHostname(raw string) (host string, port int, https bool) {
	host = strings.TrimSpace(raw)
	switch lower := strings.ToLower(host); {
	case strings.HasPrefix(lower, "https://"):
		host, https = host[len("https://"):], true
	case strings.HasPrefix(lower, "https//"):
		host, https = host[len("https//"):], true
	case strings.HasPrefix(lower, "http://"):
		host = host[len("http://"):]
	case strings.HasPrefix(lower, "http//"):
		host = host[len("http//"):]
	}
	// Discard any path, query, or fragment; only the authority is wanted.
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	host, port = splitHostPort(strings.TrimSpace(host))
	return host, port, https
}

// splitHostPort separates a trailing numeric port from a "host:port" authority.
// It returns the host (IPv6 literals keep their brackets) and the port, or the
// input unchanged with port 0 when there is no numeric port — including a bare,
// unbracketed IPv6 literal, whose colons are not a port separator.
func splitHostPort(authority string) (host string, port int) {
	if authority == "" {
		return "", 0
	}
	h, p, err := net.SplitHostPort(authority)
	if err != nil {
		return authority, 0
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return authority, 0
	}
	// net.SplitHostPort strips the brackets from an IPv6 literal; restore them so
	// the host recomposes correctly as "[host]:port".
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return h, n
}

// HumanDuration formats a duration into a short, human-friendly string (e.g., "2 minutes", "3 hours").
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// HumanBytes formats a byte count into a short, human-friendly string (e.g., 1.2 GB)
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	pre := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	if exp >= len(pre) {
		exp = len(pre) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), pre[exp])
}
