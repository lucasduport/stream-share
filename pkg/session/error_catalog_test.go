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
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func writeCatalog(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "error-messages.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return path
}

// TestLookupMeaningFromStatusText is the reason standard codes need no
// maintenance: the meaning comes from the stdlib, not the table.
func TestLookupMeaningFromStatusText(t *testing.T) {
	c := LoadCatalog("")
	code, meaning, message := c.Lookup(&UpstreamError{Key: "404", StatusCode: 404})

	if code != "404" {
		t.Fatalf("code = %q, want 404", code)
	}
	if meaning != "Not Found" {
		t.Fatalf("meaning = %q, want Not Found (from http.StatusText)", meaning)
	}
	if message == "" || message == genericErrorMessage {
		t.Fatalf("message = %q, want the built-in custom message", message)
	}
}

// TestLookupNonStandardCode covers the codes http.StatusText cannot help with,
// which is exactly what the built-in table exists for.
func TestLookupNonStandardCode(t *testing.T) {
	c := LoadCatalog("")
	code, meaning, message := c.Lookup(&UpstreamError{Key: "456", StatusCode: 456})

	if code != "456" {
		t.Fatalf("code = %q, want 456", code)
	}
	if meaning != "Connection Limit" {
		t.Fatalf("meaning = %q, want Connection Limit from the built-in table", meaning)
	}
	if message != "All provider connections are in use." {
		t.Fatalf("message = %q, unexpected", message)
	}
}

// TestLookupSyntheticKeyHasNoCode: transport failures have no HTTP status, so
// the slate must not render a stray "Error " with a blank number.
func TestLookupSyntheticKeyHasNoCode(t *testing.T) {
	c := LoadCatalog("")
	code, meaning, message := c.Lookup(&UpstreamError{Key: KeyTimeout})

	if code != "" {
		t.Fatalf("code = %q, want empty for a synthetic key", code)
	}
	if meaning != "Timed Out" {
		t.Fatalf("meaning = %q, want Timed Out", meaning)
	}
	if message == "" {
		t.Fatal("message should not be empty")
	}
}

func TestLookupUnknownKeyFallsBack(t *testing.T) {
	c := LoadCatalog("")
	code, meaning, message := c.Lookup(&UpstreamError{Key: "418", StatusCode: 418})

	if code != "418" {
		t.Fatalf("code = %q, want 418", code)
	}
	// No table entry, but the stdlib still knows this one.
	if meaning != "I'm a teapot" {
		t.Fatalf("meaning = %q, want I'm a teapot", meaning)
	}
	if message != genericErrorMessage {
		t.Fatalf("message = %q, want the generic fallback", message)
	}
}

func TestLookupNilError(t *testing.T) {
	c := LoadCatalog("")
	code, meaning, message := c.Lookup(nil)
	if code != "" || meaning != "" || message != genericErrorMessage {
		t.Fatalf("Lookup(nil) = (%q,%q,%q), want empty/empty/generic", code, meaning, message)
	}
}

// TestOverrideMessageKeepsMeaning is the merge behaviour that makes the common
// case (retune the wording, keep the official meaning) a one-field edit.
func TestOverrideMessageKeepsMeaning(t *testing.T) {
	path := writeCatalog(t, `{"403": {"message": "Check your subscription."}}`)
	c := LoadCatalog(path)

	_, meaning, message := c.Lookup(&UpstreamError{Key: "403", StatusCode: 403})
	if meaning != "Forbidden" {
		t.Fatalf("meaning = %q, want Forbidden retained from http.StatusText", meaning)
	}
	if message != "Check your subscription." {
		t.Fatalf("message = %q, want the override", message)
	}
}

func TestOverrideBothFields(t *testing.T) {
	path := writeCatalog(t, `{"456": {"meaning": "Slots Full", "message": "Try later."}}`)
	c := LoadCatalog(path)

	_, meaning, message := c.Lookup(&UpstreamError{Key: "456", StatusCode: 456})
	if meaning != "Slots Full" || message != "Try later." {
		t.Fatalf("got (%q,%q), want (Slots Full, Try later.)", meaning, message)
	}
}

// TestOverrideAddsNewKey: a provider-specific code can be handled without a
// code change, which is the point of the file being data.
func TestOverrideAddsNewKey(t *testing.T) {
	path := writeCatalog(t, `{"499": {"meaning": "Provider Quirk", "message": "Known issue."}}`)
	c := LoadCatalog(path)

	code, meaning, message := c.Lookup(&UpstreamError{Key: "499", StatusCode: 499})
	if code != "499" || meaning != "Provider Quirk" || message != "Known issue." {
		t.Fatalf("got (%q,%q,%q), want 499/Provider Quirk/Known issue.", code, meaning, message)
	}
}

func TestOverrideSyntheticKey(t *testing.T) {
	path := writeCatalog(t, `{"UNREACHABLE": {"message": "Provider is down."}}`)
	c := LoadCatalog(path)

	_, meaning, message := c.Lookup(&UpstreamError{Key: KeyUnreachable})
	if meaning != "Unreachable" {
		t.Fatalf("meaning = %q, want the built-in Unreachable retained", meaning)
	}
	if message != "Provider is down." {
		t.Fatalf("message = %q, want the override", message)
	}
}

// TestBadCatalogFilesFallBack: operator config must never be fatal.
func TestBadCatalogFilesFallBack(t *testing.T) {
	cases := map[string]string{
		"malformed json": `{"403": {`,
		"wrong shape":    `["not", "an", "object"]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := LoadCatalog(writeCatalog(t, body))
			_, meaning, message := c.Lookup(&UpstreamError{Key: "403", StatusCode: 403})
			if meaning != "Forbidden" || message == "" {
				t.Fatalf("built-ins not intact after bad file: (%q,%q)", meaning, message)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		c := LoadCatalog(filepath.Join(t.TempDir(), "does-not-exist.json"))
		if _, meaning, _ := c.Lookup(&UpstreamError{Key: "403", StatusCode: 403}); meaning != "Forbidden" {
			t.Fatalf("built-ins not intact after missing file: %q", meaning)
		}
	})
}

func TestClassifyUpstream(t *testing.T) {
	if got := classifyUpstream(nil, 403); got.Key != "403" || got.StatusCode != 403 {
		t.Fatalf("status 403 classified as %+v", got)
	}
	if got := classifyUpstream(errors.New("connection refused"), 0); got.Key != KeyUnreachable {
		t.Fatalf("generic dial error classified as %q, want %q", got.Key, KeyUnreachable)
	}
	if got := classifyUpstream(&net.DNSError{Err: "no such host"}, 0); got.Key != KeyDNS {
		t.Fatalf("DNS error classified as %q, want %q", got.Key, KeyDNS)
	}
	// A timeout that is also a DNS error must read as a timeout.
	if got := classifyUpstream(&net.DNSError{Err: "timeout", IsTimeout: true}, 0); got.Key != KeyTimeout {
		t.Fatalf("DNS timeout classified as %q, want %q", got.Key, KeyTimeout)
	}
	// Transport error wins over a status, since it happened first.
	if got := classifyUpstream(errors.New("boom"), 500); got.Key != KeyUnreachable {
		t.Fatalf("err+status classified as %q, want the transport key", got.Key)
	}
}
