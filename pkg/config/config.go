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
}
