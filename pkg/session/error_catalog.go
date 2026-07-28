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

package session

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// genericErrorMessage is shown when a key has no entry anywhere.
const genericErrorMessage = "The provider returned an unexpected error."

// ErrorInfo is one catalog entry. Meaning is optional: when empty, the official
// meaning falls back to http.StatusText for standard codes, which is why the
// built-in table only spells out meanings for non-standard IPTV codes.
type ErrorInfo struct {
	Meaning string `json:"meaning"`
	Message string `json:"message"`
}

// Catalog maps upstream error keys to the text shown on the error slate.
type Catalog struct {
	entries map[string]ErrorInfo
}

// builtinCatalog is the default wording, overridable per-key by the JSON file.
func builtinCatalog() map[string]ErrorInfo {
	return map[string]ErrorInfo{
		// Standard HTTP codes: meaning comes from http.StatusText.
		"401": {Message: "The provider rejected our credentials."},
		"403": {Message: "The provider refused access to this channel."},
		"404": {Message: "This channel is not available from the provider."},
		"429": {Message: "The provider is rate-limiting this account."},
		"500": {Message: "The provider is having problems. Retrying..."},
		"502": {Message: "The provider is having problems. Retrying..."},
		"503": {Message: "The provider is temporarily unavailable. Retrying..."},
		"504": {Message: "The provider timed out upstream. Retrying..."},

		// Non-standard codes used by Xtream providers: http.StatusText is empty
		// for these, so the meaning has to be spelled out here.
		"456": {Meaning: "Connection Limit", Message: "All provider connections are in use."},
		"461": {Meaning: "Connection Limit", Message: "All provider connections are in use."},
		"512": {Meaning: "Provider Error", Message: "The provider reported an internal error."},

		// Transport failures, which never produced an HTTP status.
		KeyUnreachable: {Meaning: "Unreachable", Message: "Cannot reach the provider."},
		KeyTimeout:     {Meaning: "Timed Out", Message: "The provider did not respond in time."},
		KeyDNS:         {Meaning: "DNS Failure", Message: "Could not resolve the provider address."},
		KeyTLS:         {Meaning: "TLS Failure", Message: "Could not establish a secure connection to the provider."},
	}
}

// LoadCatalog returns the built-in catalog, overlaid with a JSON file when path
// is non-empty. A missing or malformed file is logged and ignored — bad operator
// config must never stop streams from being served.
func LoadCatalog(path string) *Catalog {
	c := &Catalog{entries: builtinCatalog()}

	path = strings.TrimSpace(path)
	if path == "" {
		return c
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		utils.WarnLog("Error catalog: cannot read %s (%v); using built-in messages", path, err)
		return c
	}

	var overrides map[string]ErrorInfo
	if err := json.Unmarshal(raw, &overrides); err != nil {
		utils.WarnLog("Error catalog: %s is not valid JSON (%v); using built-in messages", path, err)
		return c
	}

	applied := 0
	for key, override := range overrides {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		// Merge per-field so a file that only sets "message" keeps the built-in
		// (or http.StatusText) meaning rather than blanking it.
		entry := c.entries[key]
		if strings.TrimSpace(override.Meaning) != "" {
			entry.Meaning = override.Meaning
		}
		if strings.TrimSpace(override.Message) != "" {
			entry.Message = override.Message
		}
		c.entries[key] = entry
		applied++
	}

	utils.InfoLog("Error catalog: loaded %d override(s) from %s", applied, path)
	return c
}

// Lookup resolves the three fields shown on screen for an upstream failure.
//
//	code    - the numeric HTTP status, empty for transport failures
//	meaning - official meaning: catalog entry, else http.StatusText, else empty
//	message - the custom message: catalog entry, else a generic fallback
func (c *Catalog) Lookup(e *UpstreamError) (code, meaning, message string) {
	if e == nil {
		return "", "", genericErrorMessage
	}

	if e.StatusCode > 0 {
		code = strconv.Itoa(e.StatusCode)
	}

	var entry ErrorInfo
	if c != nil && c.entries != nil {
		entry = c.entries[e.Key]
	}

	meaning = strings.TrimSpace(entry.Meaning)
	if meaning == "" && e.StatusCode > 0 {
		meaning = http.StatusText(e.StatusCode)
	}

	message = strings.TrimSpace(entry.Message)
	if message == "" {
		message = genericErrorMessage
	}

	return code, meaning, message
}
