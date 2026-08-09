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

package xtream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// AccountInfo is the upstream account's subscription state, normalized from the
// provider's player_api.php login response.
//
// The provider's own credentials are deliberately dropped during parsing: the
// login payload echoes them back, and nothing downstream (dashboard, Discord)
// has any use for them.
type AccountInfo struct {
	// Auth reports whether the provider accepted our credentials (auth=1).
	// A false value with no transport error means the account itself is being
	// rejected — the single most useful thing to surface on a dashboard.
	Auth bool
	// Status is the provider's own wording, typically "Active", "Expired",
	// "Banned" or "Disabled". It is passed through unmodified because panels
	// differ and an operator recognizes their provider's phrasing.
	Status  string
	Message string
	IsTrial bool
	// ExpiresAt is nil when the provider reports no expiry (unlimited).
	ExpiresAt *time.Time
	CreatedAt *time.Time
	// ActiveConnections is the provider's count across every device using this
	// account, not just this stream-share instance.
	ActiveConnections int
	// MaxConnections is 0 when the provider does not report a limit.
	MaxConnections       int
	AllowedOutputFormats []string
	ServerTimezone       string
	ServerTime           string
	// FetchedAt is when this snapshot was read from the provider.
	FetchedAt time.Time
}

// Expired reports whether the subscription's expiry date has passed. An account
// with no expiry date is never expired.
func (a *AccountInfo) Expired() bool {
	return a != nil && a.ExpiresAt != nil && time.Now().After(*a.ExpiresAt)
}

// AccountInfo fetches the upstream subscription state from the provider.
//
// Calling player_api.php with credentials but no action returns the panel's
// login payload, whose user_info block carries the subscription facts — expiry,
// connection limit and current connection count, trial flag — that no other
// Xtream action exposes. Unlike Action, this returns errors rather than an empty
// fallback: a failed read here is itself information worth reporting.
func (c *Client) AccountInfo(ctx context.Context) (*AccountInfo, error) {
	u, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/player_api.php")
	if err != nil {
		return nil, utils.PrintErrorAndReturn(err)
	}
	params := url.Values{}
	params.Set("username", c.Username)
	params.Set("password", c.Password)
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, utils.PrintErrorAndReturn(err)
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("provider returned HTTP %d for the account info request", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read account info response: %w", err)
	}
	return parseAccountInfo(body)
}

// parseAccountInfo normalizes a player_api.php login payload. Panels are
// inconsistent about types — the same field arrives as a JSON string on one
// provider and a number on the next — so every field goes through a coercing
// accessor rather than a typed unmarshal.
func parseAccountInfo(body []byte) (*AccountInfo, error) {
	var payload struct {
		UserInfo   map[string]interface{} `json:"user_info"`
		ServerInfo map[string]interface{} `json:"server_info"`
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(body)))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("provider login response is not valid JSON: %w", err)
	}
	if payload.UserInfo == nil {
		return nil, fmt.Errorf("provider login response contains no user_info block")
	}

	ui := payload.UserInfo
	info := &AccountInfo{
		Auth:                 fieldBool(ui, "auth"),
		Status:               strings.TrimSpace(fieldString(ui, "status")),
		Message:              strings.TrimSpace(fieldString(ui, "message")),
		IsTrial:              fieldBool(ui, "is_trial"),
		ExpiresAt:            fieldTime(ui, "exp_date"),
		CreatedAt:            fieldTime(ui, "created_at"),
		ActiveConnections:    fieldInt(ui, "active_cons"),
		MaxConnections:       fieldInt(ui, "max_connections"),
		AllowedOutputFormats: fieldStrings(ui, "allowed_output_formats"),
		FetchedAt:            time.Now(),
	}
	if si := payload.ServerInfo; si != nil {
		info.ServerTimezone = strings.TrimSpace(fieldString(si, "timezone"))
		info.ServerTime = strings.TrimSpace(fieldString(si, "time_now"))
	}
	return info, nil
}

// fieldString reads a field as text, accepting the JSON number and boolean
// spellings providers also use.
func fieldString(m map[string]interface{}, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// fieldInt reads a numeric field that may arrive as a number or a string.
// Anything unparseable (including a panel's "unlimited") reads as 0.
func fieldInt(m map[string]interface{}, key string) int {
	s := strings.TrimSpace(fieldString(m, key))
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return int(n)
	}
	return 0
}

// fieldBool reads a flag that may arrive as a bool, a 0/1 number, or their
// string spellings.
func fieldBool(m map[string]interface{}, key string) bool {
	if b, ok := m[key].(bool); ok {
		return b
	}
	s := strings.ToLower(strings.TrimSpace(fieldString(m, key)))
	switch s {
	case "", "0", "false", "no", "null":
		return false
	case "1", "true", "yes":
		return true
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n != 0
	}
	return false
}

// fieldTime reads a timestamp field, which is a Unix epoch on nearly every
// panel but occasionally a formatted date. A missing, null, empty or zero value
// means "not set" (an unlimited subscription, for exp_date) and yields nil.
func fieldTime(m map[string]interface{}, key string) *time.Time {
	s := strings.TrimSpace(fieldString(m, key))
	if s == "" || s == "0" || strings.EqualFold(s, "null") || strings.EqualFold(s, "unlimited") {
		return nil
	}
	if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
		if epoch <= 0 {
			return nil
		}
		t := time.Unix(epoch, 0).UTC()
		return &t
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// fieldStrings reads a list field, tolerating the comma-separated string form
// some panels use instead of a JSON array.
func fieldStrings(m map[string]interface{}, key string) []string {
	switch v := m[key].(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(fmt.Sprintf("%v", item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
