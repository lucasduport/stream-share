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
	"os"
	"testing"
	"time"
)

// ---- MaskString ----

func TestMaskString_empty(t *testing.T) {
	if got := MaskString(""); got != "[empty]" {
		t.Fatalf("want [empty], got %q", got)
	}
}

func TestMaskString_short(t *testing.T) {
	// ≤8 chars: first char + "******"
	got := MaskString("abc")
	if got != "a******" {
		t.Fatalf("want %q, got %q", "a******", got)
	}
	got = MaskString("12345678")
	if got != "1******" {
		t.Fatalf("want %q, got %q", "1******", got)
	}
}

func TestMaskString_long(t *testing.T) {
	// >8 chars: first 4 + "..." + last 4
	got := MaskString("supersecretpassword")
	if got != "supe...word" {
		t.Fatalf("want %q, got %q", "supe...word", got)
	}
}

// ---- MaskURL ----

func TestMaskURL_standard(t *testing.T) {
	url := "http://host/path/alice/secret123/channel.ts"
	got := MaskURL(url)
	// parts[4]=alice parts[5]=secret123 should both be masked
	if got == url {
		t.Fatal("MaskURL returned input unchanged")
	}
	// must not contain the full credentials in plaintext
	for _, plain := range []string{"alice", "secret123"} {
		if len(plain) > 4 {
			// Only flag if the full value is present (short strings are shown partially)
			if contains(got, plain) {
				t.Errorf("MaskURL left %q unmasked in output %q", plain, got)
			}
		}
	}
}

func TestMaskURL_short(t *testing.T) {
	// URLs with fewer than 7 segments are returned unchanged
	short := "http://host/only"
	if got := MaskURL(short); got != short {
		t.Fatalf("want %q unchanged, got %q", short, got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---- NormalizeHostname ----

func TestNormalizeHostname(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantHost  string
		wantPort  int
		wantHTTPS bool
	}{
		{"bare host", "tv.example.com", "tv.example.com", 0, false},
		{"empty", "", "", 0, false},
		{"https scheme", "https://tv.example.com", "tv.example.com", 0, true},
		{"http scheme", "http://tv.example.com", "tv.example.com", 0, false},
		// The exact typo from issue #23: missing colon after the scheme.
		{"https missing colon", "https//tv.example.com", "tv.example.com", 0, true},
		{"http missing colon", "http//tv.example.com", "tv.example.com", 0, false},
		{"scheme with port", "https://tv.example.com:8443", "tv.example.com", 8443, true},
		{"bare host with port", "tv.example.com:8080", "tv.example.com", 8080, false},
		{"scheme host port and path", "https://tv.example.com:8443/foo", "tv.example.com", 8443, true},
		{"trailing slash", "https://tv.example.com/", "tv.example.com", 0, true},
		{"scheme with path", "https://tv.example.com/download/x", "tv.example.com", 0, true},
		{"query and fragment", "https://tv.example.com?a=b#c", "tv.example.com", 0, true},
		{"surrounding whitespace", "  https://tv.example.com  ", "tv.example.com", 0, true},
		{"uppercase scheme", "HTTPS://tv.example.com", "tv.example.com", 0, true},
		{"bracketed ipv6 with port", "http://[2001:db8::1]:8080", "[2001:db8::1]", 8080, false},
		{"bracketed ipv6 no port", "[2001:db8::1]", "[2001:db8::1]", 0, false},
		{"bare ipv6 left intact", "2001:db8::1", "2001:db8::1", 0, false},
		{"non-numeric port left intact", "tv.example.com:abc", "tv.example.com:abc", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotHost, gotPort, gotHTTPS := NormalizeHostname(tc.in)
			if gotHost != tc.wantHost || gotPort != tc.wantPort || gotHTTPS != tc.wantHTTPS {
				t.Fatalf("NormalizeHostname(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.in, gotHost, gotPort, gotHTTPS, tc.wantHost, tc.wantPort, tc.wantHTTPS)
			}
		})
	}
}

// ---- HumanDuration ----

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30 seconds"},
		{59 * time.Second, "59 seconds"},
		{1 * time.Minute, "1 minutes"},
		{90 * time.Second, "1 minutes"},
		{2 * time.Hour, "2 hours"},
		{23 * time.Hour, "23 hours"},
		{24 * time.Hour, "1 days"},
		{48 * time.Hour, "2 days"},
	}
	for _, tc := range cases {
		if got := HumanDuration(tc.d); got != tc.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ---- HumanBytes ----

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, tc := range cases {
		if got := HumanBytes(tc.b); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.b, got, tc.want)
		}
	}
}

// ---- GetEnvOrDefault ----

func TestGetEnvOrDefault_set(t *testing.T) {
	t.Setenv("TEST_KEY_XYZ", "hello")
	if got := GetEnvOrDefault("TEST_KEY_XYZ", "fallback"); got != "hello" {
		t.Fatalf("want %q, got %q", "hello", got)
	}
}

func TestGetEnvOrDefault_unset(t *testing.T) {
	_ = os.Unsetenv("TEST_KEY_XYZ_UNSET")
	if got := GetEnvOrDefault("TEST_KEY_XYZ_UNSET", "fallback"); got != "fallback" {
		t.Fatalf("want %q, got %q", "fallback", got)
	}
}

func TestGetEnvOrDefault_empty(t *testing.T) {
	// An explicitly empty env var should still return the default (GetEnvOrDefault
	// treats "" as unset).
	t.Setenv("TEST_KEY_EMPTY", "")
	if got := GetEnvOrDefault("TEST_KEY_EMPTY", "fallback"); got != "fallback" {
		t.Fatalf("want %q, got %q", "fallback", got)
	}
}
