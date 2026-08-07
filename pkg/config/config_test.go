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

package config_test

import (
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/utils"
)

func newConfig(host string, port, advertised int, https, reverseProxy bool, publicBase string) *config.ProxyConfig {
	return &config.ProxyConfig{
		HostConfig:          &config.HostConfiguration{Hostname: host, Port: port},
		AdvertisedPort:      advertised,
		HTTPS:               https,
		ReverseProxyEnabled: reverseProxy,
		PublicBaseURL:       publicBase,
	}
}

func TestScheme(t *testing.T) {
	if got := (&config.ProxyConfig{HTTPS: true}).Scheme(); got != "https" {
		t.Fatalf("HTTPS=true: want https, got %q", got)
	}
	if got := (&config.ProxyConfig{HTTPS: false}).Scheme(); got != "http" {
		t.Fatalf("HTTPS=false: want http, got %q", got)
	}
}

func TestHostPort(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.ProxyConfig
		want string
	}{
		{"http direct uses listen port", newConfig("tv.example.com", 8080, 0, false, false, ""), "tv.example.com:8080"},
		// The server only serves plain HTTP, so HTTPS implies a TLS proxy in front
		// and the internal port must not appear — this is the issue #23 case.
		{"https without advertised port drops internal port", newConfig("tv.example.com", 8080, 0, true, false, ""), "tv.example.com"},
		{"https reverse proxy drops port", newConfig("tv.example.com", 8080, 0, true, true, ""), "tv.example.com"},
		{"http reverse proxy drops port", newConfig("tv.example.com", 8080, 0, false, true, ""), "tv.example.com"},
		{"explicit https default port dropped", newConfig("tv.example.com", 8080, 443, true, false, ""), "tv.example.com"},
		{"explicit http default port dropped", newConfig("tv.example.com", 8080, 80, false, false, ""), "tv.example.com"},
		{"explicit https non-default port kept", newConfig("tv.example.com", 8080, 8443, true, false, ""), "tv.example.com:8443"},
		{"explicit port wins over reverse proxy", newConfig("tv.example.com", 8080, 8080, false, true, ""), "tv.example.com:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.HostPort(); got != tc.want {
				t.Fatalf("HostPort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublicURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.ProxyConfig
		path string
		want string
	}{
		{"derived https no advertised port", newConfig("tv.example.com", 8080, 0, true, false, ""), "/download/x", "https://tv.example.com/download/x"},
		{"derived https explicit 443", newConfig("tv.example.com", 8080, 443, true, false, ""), "/download/x", "https://tv.example.com/download/x"},
		{"derived http with port", newConfig("tv.example.com", 8080, 0, false, false, ""), "/download/x", "http://tv.example.com:8080/download/x"},
		{"public base wins", newConfig("tv.example.com", 8080, 8080, false, false, "https://cdn.example.com"), "/download/x", "https://cdn.example.com/download/x"},
		{"public base trailing slash trimmed", newConfig("tv.example.com", 8080, 8080, false, false, "https://cdn.example.com/"), "/download/x", "https://cdn.example.com/download/x"},
		{"public base subpath", newConfig("tv.example.com", 8080, 8080, false, false, "https://cdn.example.com/stream-share"), "/download/x", "https://cdn.example.com/stream-share/download/x"},
		{"public base missing scheme gets one", newConfig("tv.example.com", 8080, 8080, true, false, "cdn.example.com"), "/download/x", "https://cdn.example.com/download/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.PublicURL(tc.path); got != tc.want {
				t.Fatalf("PublicURL(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestPublicURL_issue23 reproduces the end-to-end path from GitHub issue #23: an
// operator sets HOSTNAME to a scheme-carrying value, which previously produced
// links like "http://https//tv.lucasduport.cc:8080/download/...". After the
// hostname is normalized (as cmd does at load) and the URL is built via the
// shared builder, the result is always a single, well-formed URL.
func TestPublicURL_issue23(t *testing.T) {
	const token = "vod-download-token"
	cases := []struct {
		name         string
		rawHostname  string
		advertised   int
		reverseProxy bool
		want         string
	}{
		// The operator's real config: scheme in HOSTNAME, no PROXY flag, no
		// ADVERTISED_PORT. The internal 8080 must not appear.
		{"missing colon typo, nothing else set", "https//tv.lucasduport.cc", 0, false, "https://tv.lucasduport.cc/download/" + token},
		{"full scheme, nothing else set", "https://tv.lucasduport.cc", 0, false, "https://tv.lucasduport.cc/download/" + token},
		{"full scheme, reverse proxy", "https://tv.lucasduport.cc", 0, true, "https://tv.lucasduport.cc/download/" + token},
		{"explicit non-default https port kept", "https://tv.lucasduport.cc", 8443, false, "https://tv.lucasduport.cc:8443/download/" + token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configFromRawHostname(tc.rawHostname, tc.advertised, tc.reverseProxy)
			got := cfg.PublicURL("/download/" + token)
			if got != tc.want {
				t.Fatalf("PublicURL = %q, want %q", got, tc.want)
			}
			// Guard against regressions of the exact bug shape.
			if strings.Count(got, "://") != 1 {
				t.Errorf("expected exactly one scheme separator in %q", got)
			}
			if strings.Contains(got, "https//") || strings.Contains(got, "http://https") {
				t.Errorf("malformed scheme in %q", got)
			}
		})
	}
}

// TestPublicURL_hostnameWithPort covers a port embedded in HOSTNAME. It must be
// honored, never duplicated with the listening port (the "host:8443:8080" bug),
// and dropped when it is the scheme default. An explicit ADVERTISED_PORT wins.
func TestPublicURL_hostnameWithPort(t *testing.T) {
	const token = "tok"
	cases := []struct {
		name        string
		rawHostname string
		advertised  int
		want        string
	}{
		{"http host with port", "tv.example.com:9000", 0, "http://tv.example.com:9000/download/" + token},
		{"https scheme and port", "https://tv.example.com:8443", 0, "https://tv.example.com:8443/download/" + token},
		{"https scheme with default 443 dropped", "https://tv.example.com:443", 0, "https://tv.example.com/download/" + token},
		{"explicit advertised overrides hostname port", "https://tv.example.com:9000", 8443, "https://tv.example.com:8443/download/" + token},
		{"bracketed ipv6 with port", "https://[2001:db8::1]:8443", 0, "https://[2001:db8::1]:8443/download/" + token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := configFromRawHostname(tc.rawHostname, tc.advertised, false)
			got := cfg.PublicURL("/download/" + token)
			if got != tc.want {
				t.Fatalf("PublicURL = %q, want %q", got, tc.want)
			}
			// No doubled port such as "host:9000:8080" in the authority.
			authority := got[len(cfg.Scheme())+len("://"):]
			if i := strings.IndexByte(authority, '/'); i >= 0 {
				authority = authority[:i]
			}
			if !strings.HasPrefix(authority, "[") && strings.Count(authority, ":") > 1 {
				t.Errorf("doubled port in authority %q (from %q)", authority, got)
			}
		})
	}
}

// configFromRawHostname mirrors cmd/root.go: normalize HOSTNAME, infer HTTPS from
// its scheme, and lift an embedded port into the advertised port when one wasn't
// set explicitly.
func configFromRawHostname(rawHostname string, advertised int, reverseProxy bool) *config.ProxyConfig {
	host, hostPort, https := utils.NormalizeHostname(rawHostname)
	if advertised == 0 {
		advertised = hostPort
	}
	return newConfig(host, 8080, advertised, https, reverseProxy, "")
}
