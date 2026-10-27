/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"net/url"
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
)

func TestRedactRequestURIMasksCredentials(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{
		User: "proxyuser", Password: "proxypass",
		XtreamUser: "provuser", XtreamPassword: "provpass",
	}}
	cases := []struct {
		name, uri, route string
		secrets          []string
		keep             string
	}{
		{"provider creds in path", "/live/provuser/provpass/123.ts", "/live/provuser/provpass/:id", []string{"provuser", "provpass"}, "123.ts"},
		{"ldap creds by route param", "/movie/alice/s3cret/42.mkv", "/movie/:username/:password/:id", []string{"s3cret"}, "42.mkv"},
		{"share token", "/share/abcdefghijklmnopqrstuv/live.ts", "/share/:token/live.ts", []string{"abcdefghijklmnopqrstuv"}, "/live.ts"},
		{"share playlist token", "/share/abcdefghijklmnopqrstuv.m3u", "/share/:token", []string{"abcdefghijklmnopqrstuv"}, "/share/"},
		{"download token", "/download/abcdef123456", "/download/:token", []string{"abcdef123456"}, "/download/"},
		{"query creds", "/player_api.php?username=alice&password=s3cret&action=get_vod_streams", "/player_api.php", []string{"alice", "s3cret"}, "get_vod_streams"},
		{"unmatched route still masks known secrets", "/nope/provuser/provpass", "", []string{"provuser", "provpass"}, "/nope/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.uri)
			if err != nil {
				t.Fatal(err)
			}
			got := c.redactRequestURI(u, tc.route)
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Fatalf("redacted %q still contains %q", got, s)
				}
			}
			if !strings.Contains(got, tc.keep) {
				t.Fatalf("redacted %q lost %q", got, tc.keep)
			}
		})
	}
}
