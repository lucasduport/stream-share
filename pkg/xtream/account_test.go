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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The string-typed spelling used by most Xtream panels.
const stringyLogin = `{
  "user_info": {
    "username": "someuser", "password": "somepass", "message": "Welcome",
    "auth": 1, "status": "Active", "exp_date": "1793491200", "is_trial": "0",
    "active_cons": "2", "created_at": "1690000000", "max_connections": "4",
    "allowed_output_formats": ["m3u8", "ts"]
  },
  "server_info": {"timezone": "Europe/Amsterdam", "time_now": "2026-08-07 12:00:00"}
}`

func TestParseAccountInfoStringTypedFields(t *testing.T) {
	info, err := parseAccountInfo([]byte(stringyLogin))
	if err != nil {
		t.Fatalf("parseAccountInfo returned error: %v", err)
	}
	if !info.Auth {
		t.Error("Auth = false, want true")
	}
	if info.Status != "Active" {
		t.Errorf("Status = %q, want %q", info.Status, "Active")
	}
	if info.Message != "Welcome" {
		t.Errorf("Message = %q, want %q", info.Message, "Welcome")
	}
	if info.IsTrial {
		t.Error("IsTrial = true, want false")
	}
	if info.ActiveConnections != 2 || info.MaxConnections != 4 {
		t.Errorf("connections = %d/%d, want 2/4", info.ActiveConnections, info.MaxConnections)
	}
	if info.ExpiresAt == nil || info.ExpiresAt.Unix() != 1793491200 {
		t.Errorf("ExpiresAt = %v, want unix 1793491200", info.ExpiresAt)
	}
	if info.CreatedAt == nil || info.CreatedAt.Unix() != 1690000000 {
		t.Errorf("CreatedAt = %v, want unix 1690000000", info.CreatedAt)
	}
	if len(info.AllowedOutputFormats) != 2 || info.AllowedOutputFormats[0] != "m3u8" {
		t.Errorf("AllowedOutputFormats = %v, want [m3u8 ts]", info.AllowedOutputFormats)
	}
	if info.ServerTimezone != "Europe/Amsterdam" || info.ServerTime != "2026-08-07 12:00:00" {
		t.Errorf("server info = %q/%q, want Europe/Amsterdam and the given time",
			info.ServerTimezone, info.ServerTime)
	}
	if info.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero, want the parse time")
	}
}

func TestParseAccountInfoNumberTypedFields(t *testing.T) {
	body := `{"user_info": {
		"auth": true, "status": "Active", "exp_date": 1793491200, "is_trial": 1,
		"active_cons": 1, "max_connections": 2,
		"allowed_output_formats": "m3u8, ts , rtmp"
	}}`
	info, err := parseAccountInfo([]byte(body))
	if err != nil {
		t.Fatalf("parseAccountInfo returned error: %v", err)
	}
	if !info.Auth || !info.IsTrial {
		t.Errorf("Auth=%v IsTrial=%v, want both true", info.Auth, info.IsTrial)
	}
	if info.ActiveConnections != 1 || info.MaxConnections != 2 {
		t.Errorf("connections = %d/%d, want 1/2", info.ActiveConnections, info.MaxConnections)
	}
	if info.ExpiresAt == nil || info.ExpiresAt.Unix() != 1793491200 {
		t.Errorf("ExpiresAt = %v, want unix 1793491200", info.ExpiresAt)
	}
	want := []string{"m3u8", "ts", "rtmp"}
	if len(info.AllowedOutputFormats) != len(want) {
		t.Fatalf("AllowedOutputFormats = %v, want %v", info.AllowedOutputFormats, want)
	}
	for i := range want {
		if info.AllowedOutputFormats[i] != want[i] {
			t.Errorf("AllowedOutputFormats[%d] = %q, want %q", i, info.AllowedOutputFormats[i], want[i])
		}
	}
}

func TestParseAccountInfoUnsetExpiryMeansUnlimited(t *testing.T) {
	for _, raw := range []string{`null`, `""`, `"0"`, `0`, `"unlimited"`} {
		body := `{"user_info": {"auth": 1, "status": "Active", "exp_date": ` + raw + `}}`
		info, err := parseAccountInfo([]byte(body))
		if err != nil {
			t.Fatalf("exp_date %s: parseAccountInfo returned error: %v", raw, err)
		}
		if info.ExpiresAt != nil {
			t.Errorf("exp_date %s: ExpiresAt = %v, want nil (unlimited)", raw, info.ExpiresAt)
		}
		if info.Expired() {
			t.Errorf("exp_date %s: Expired() = true, want false for an unlimited account", raw)
		}
	}
}

func TestParseAccountInfoDateFormattedExpiry(t *testing.T) {
	info, err := parseAccountInfo([]byte(`{"user_info": {"exp_date": "2026-12-31 23:59:59"}}`))
	if err != nil {
		t.Fatalf("parseAccountInfo returned error: %v", err)
	}
	want := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	if info.ExpiresAt == nil || !info.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", info.ExpiresAt, want)
	}
}

func TestAccountInfoExpired(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	if (&AccountInfo{ExpiresAt: &past}).Expired() != true {
		t.Error("past expiry: Expired() = false, want true")
	}
	if (&AccountInfo{ExpiresAt: &future}).Expired() != false {
		t.Error("future expiry: Expired() = true, want false")
	}
	if (&AccountInfo{}).Expired() != false {
		t.Error("no expiry: Expired() = true, want false")
	}
	var nilInfo *AccountInfo
	if nilInfo.Expired() {
		t.Error("nil AccountInfo: Expired() = true, want false")
	}
}

func TestParseAccountInfoRejectsUnusableResponses(t *testing.T) {
	cases := map[string]string{
		"not JSON":         `<html>blocked</html>`,
		"no user_info":     `{"server_info": {"timezone": "UTC"}}`,
		"empty body":       ``,
		"user_info a list": `{"user_info": []}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAccountInfo([]byte(body)); err == nil {
				t.Errorf("parseAccountInfo(%q) returned no error, want one", body)
			}
		})
	}
}

func TestClientAccountInfoFetchesLoginPayload(t *testing.T) {
	var gotPath, gotAction, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAction = r.URL.Query().Get("action")
		gotUser = r.URL.Query().Get("username")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(stringyLogin))
	}))
	defer srv.Close()

	client, err := New("someuser", "somepass", srv.URL, "")
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	info, err := client.AccountInfo(context.Background())
	if err != nil {
		t.Fatalf("AccountInfo returned error: %v", err)
	}
	if gotPath != "/player_api.php" {
		t.Errorf("requested path = %q, want /player_api.php", gotPath)
	}
	if gotAction != "" {
		t.Errorf("requested action = %q, want it omitted (login payload)", gotAction)
	}
	if gotUser != "someuser" {
		t.Errorf("requested username = %q, want someuser", gotUser)
	}
	if info.MaxConnections != 4 {
		t.Errorf("MaxConnections = %d, want 4", info.MaxConnections)
	}
}

func TestClientAccountInfoReportsUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(456)
	}))
	defer srv.Close()

	client, err := New("u", "p", srv.URL, "")
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if _, err := client.AccountInfo(context.Background()); err == nil {
		t.Fatal("AccountInfo returned no error for an upstream 456, want one")
	}
}

func TestClientAccountInfoHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(stringyLogin))
	}))
	defer srv.Close()

	client, err := New("u", "p", srv.URL, "")
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = client.AccountInfo(ctx)
	if err == nil {
		t.Fatal("AccountInfo returned no error for a canceled context, want one")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the context deadline to surface", err)
	}
}
