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

package server

import (
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
)

// TestReplaceURL_hostPort verifies that generated M3U/playlist URLs share the
// same host/port rules as the download link: a default port for the scheme is
// dropped, a non-default one is kept, and the scheme is never duplicated.
func TestReplaceURL_hostPort(t *testing.T) {
	cases := []struct {
		name         string
		https        bool
		advertised   int
		wantContains string
		wantAbsent   string
	}{
		{"https without advertised port drops internal port", true, 0, "https://tv.example.com/", ":8080"},
		{"https on default port drops port", true, 443, "https://tv.example.com/", ":443"},
		{"http direct keeps listen port", false, 0, "http://tv.example.com:8080/", ""},
		{"http explicit non-default port kept", false, 9000, "http://tv.example.com:9000/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{
				ProxyConfig: &config.ProxyConfig{
					HostConfig:     &config.HostConfiguration{Hostname: "tv.example.com", Port: 8080},
					AdvertisedPort: tc.advertised,
					HTTPS:          tc.https,
					User:           config.CredentialString("localuser"),
					Password:       config.CredentialString("localpass"),
				},
				endpointAntiColision: "abc123",
			}
			got, err := c.replaceURL("http://provider.tld/live/pu/pp/42.ts", 42, false)
			if err != nil {
				t.Fatalf("replaceURL returned error: %v", err)
			}
			if !strings.HasPrefix(got, tc.wantContains) {
				t.Errorf("replaceURL = %q, want prefix %q", got, tc.wantContains)
			}
			if tc.wantAbsent != "" && strings.Contains(got, tc.wantAbsent) {
				t.Errorf("replaceURL = %q, should not contain %q", got, tc.wantAbsent)
			}
			if strings.Count(got, "://") != 1 {
				t.Errorf("expected exactly one scheme separator in %q", got)
			}
		})
	}
}
