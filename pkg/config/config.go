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

package config

import (
	"net/url"
)

// Add Debugging Logging option
var DebugLoggingEnabled bool

// Set a Cache Folder to save responses into
var CacheFolder string

// CredentialString represents an stream-share credential.
type CredentialString string

// PathEscape escapes the credential for an url path.
func (c CredentialString) PathEscape() string {
	return url.PathEscape(string(c))
}

// String returns the credential string.
func (c CredentialString) String() string {
	return string(c)
}

// HostConfiguration containt host infos
type HostConfiguration struct {
	Hostname string
	Port     int
}

// ProxyConfig Contain original m3u playlist and HostConfiguration
type ProxyConfig struct {
	HostConfig           *HostConfiguration
	XtreamUser           CredentialString
	XtreamPassword       CredentialString
	XtreamBaseURL        string
	XtreamGenerateApiGet bool
	M3UCacheExpiration   int
	M3UFileName          string
	CustomEndpoint       string
	CustomId             string
	RemoteURL            *url.URL
	AdvertisedPort       int
	HTTPS                bool
	User, Password       CredentialString
	// LDAP authentication fields
	LDAPEnabled        bool
	LDAPServer         string
	LDAPBaseDN         string
	LDAPBindDN         string
	LDAPBindPassword   string
	LDAPUserAttribute  string
	LDAPGroupAttribute string
	LDAPRequiredGroup  string
	// LDAPAuthCacheMinutes caches successful LDAP authentications for this long.
	// 0 disables the cache, so every request re-checks the directory.
	LDAPAuthCacheMinutes int

	// Reverse proxy / public URL configuration
	ReverseProxyEnabled bool
	PublicBaseURL       string

	// VOD caching configuration
	VODCacheEnabled    bool
	VODCacheStaleHours int
	VODExtOrder        string
	VODExtProbeEnabled bool

	// Catchup configuration
	CatchupEnabled           bool
	CatchupDurationHours     int
	CatchupPauseGraceMinutes int

	// Error slate configuration: on upstream failure, show the reason on screen
	// instead of dropping the stream. Requires ffmpeg in the image.
	ErrorSlateEnabled         bool
	ErrorSlateRetryMaxMinutes int
	ErrorSlateMessagesFile    string

	// Session / stream timeout configuration
	SessionTimeoutMinutes        int
	StreamTimeoutMinutes         int
	TempLinkHours                int
	MultiplexStallTimeoutSeconds int

	// Internal API / Discord configuration
	InternalAPIKey     string
	DiscordBotToken    string
	DiscordAdminRoleID string
	DiscordAPIURL      string

	// Dashboard configuration
	// InstanceName identifies this deployment when its API data is combined with
	// other stream-share instances in an external dashboard. Defaults to the
	// machine hostname when unset.
	InstanceName string

	// StreamTechProbeEnabled turns on best-effort audio/video technical info
	// (codec, resolution, bitrate, ...) for active live streams, exposed via the
	// dashboard API. It samples bytes already flowing through the existing
	// shared upstream connection (no extra connection to the provider) and
	// analyzes them with ffprobe, which must be present in the runtime image.
	StreamTechProbeEnabled bool

	// Provider health check. When enabled, stream-share periodically probes one
	// configured live channel and reports whether the IPTV provider is serving us
	// or blocking our current egress IP (HTTP 456 — typical when the VPN server's
	// IP has been banned). It surfaces this via /healthz (driving the Docker
	// HEALTHCHECK) and an authenticated force-probe at /api/internal/health.
	//
	// stream-share only *reports* health. Reconnecting the VPN on a bad result is
	// intentionally left to an external service so this app carries no dependency
	// on gluetun or any particular VPN. See docker-compose.yml for a worked
	// example of such a watchdog.
	HealthCheckEnabled bool
	// HealthCheckStreamID is the live channel id to probe (as it appears in a
	// stream URL, e.g. "12345" or "12345.ts").
	HealthCheckStreamID string
	// HealthCheckBlockedCodes is a comma-separated list of upstream HTTP status
	// codes that mean "our egress IP is blocked" (reported as "blocked" rather
	// than a generic "error"). Defaults to "456", which many Xtream providers
	// use — but that code is outside the HTTP standard, so providers may use
	// others; hence it is configurable rather than hardcoded.
	HealthCheckBlockedCodes string
	// HealthCheckTimes is an optional comma-separated list of local wall-clock
	// times (HH:MM, in the container's TZ) at which to self-probe, e.g.
	// "04:00,16:00". When empty, probes only happen at startup and on demand via
	// the force-probe endpoint, leaving all scheduling to the external watchdog.
	HealthCheckTimes string
	// HealthCheckTimeoutSeconds bounds a single probe. Defaults to 15 when unset.
	HealthCheckTimeoutSeconds int
	// HealthCheckMinIntervalSeconds is the minimum spacing between real provider
	// probes, so the force-probe endpoint cannot be used to hammer the provider
	// (which is exactly what gets an IP blocked). Defaults to 60 when unset.
	HealthCheckMinIntervalSeconds int
	// HealthCheckStaleMinutes, when > 0, makes /healthz report unhealthy if the
	// last probe is older than this — catching a probing loop that has silently
	// stalled. Disabled (0) by default.
	HealthCheckStaleMinutes int
}
