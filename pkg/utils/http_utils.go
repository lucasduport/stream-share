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

import "os"

// GetIPTVUserAgent returns the user agent to use for IPTV upstream requests
// Uses the USER_AGENT environment variable if set, otherwise defaults to "IPTVSmartersPro"
func GetIPTVUserAgent() string {
	userAgent := os.Getenv("USER_AGENT")
	if userAgent == "" {
		return "IPTVSmartersPro"
	}
	return userAgent
}

// GetLanguageHeader returns the preferred Accept-Language header, defaulting to en_US
func GetLanguageHeader() string {
	if v := os.Getenv("ACCEPT_LANGUAGE"); v != "" {
		return v
	}
	return "en_US"
}
