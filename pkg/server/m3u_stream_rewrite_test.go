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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteM3UURLLine(t *testing.T) {
	cases := []struct {
		name                          string
		line                          string
		xtreamUser, xtreamPass        string
		localUser, localPass          string
		want                          string
	}{
		{
			name:           "path credentials",
			line:           "http://provider.example.com/live/xtreamuser/xtreampass/12345.ts",
			xtreamUser:     "xtreamuser",
			xtreamPass:     "xtreampass",
			localUser:      "localuser",
			localPass:      "localpass",
			want:           "http://provider.example.com/live/localuser/localpass/12345.ts",
		},
		{
			name:           "query credentials",
			line:           "http://provider.example.com/get.php?username=xtreamuser&password=xtreampass&type=m3u",
			xtreamUser:     "xtreamuser",
			xtreamPass:     "xtreampass",
			localUser:      "localuser",
			localPass:      "localpass",
			want:           "http://provider.example.com/get.php?password=localpass&type=m3u&username=localuser",
		},
		{
			name:           "no credentials to replace",
			line:           "http://provider.example.com/live/other/pass/123.ts",
			xtreamUser:     "xtreamuser",
			xtreamPass:     "xtreampass",
			localUser:      "localuser",
			localPass:      "localpass",
			want:           "http://provider.example.com/live/other/pass/123.ts",
		},
		{
			name:           "empty xtream creds passthrough",
			line:           "http://provider.example.com/live/u/p/1.ts",
			xtreamUser:     "",
			xtreamPass:     "",
			localUser:      "localuser",
			localPass:      "localpass",
			want:           "http://provider.example.com/live/u/p/1.ts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteM3UURLLine(tc.line, tc.xtreamUser, tc.xtreamPass, tc.localUser, tc.localPass)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStreamRewriteM3U(t *testing.T) {
	body := `#EXTM3U
#EXTINF:-1 tvg-id="ch1",Channel One
http://provider.example.com/live/xtreamuser/xtreampass/12345.ts
#EXTINF:-1,Movie
http://provider.example.com/movie/xtreamuser/xtreampass/67890.mp4
`
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.m3u")

	tracks, err := streamRewriteM3U(resp, dest, "xtreamuser", "xtreampass", "localuser", "localpass")
	if err != nil {
		t.Fatal(err)
	}
	if tracks != 2 {
		t.Fatalf("tracks = %d, want 2", tracks)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)

	if !strings.Contains(out, "http://provider.example.com/live/localuser/localpass/12345.ts") {
		t.Errorf("live URL not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "http://provider.example.com/movie/localuser/localpass/67890.mp4") {
		t.Errorf("movie URL not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "#EXTINF:-1 tvg-id=\"ch1\",Channel One") {
		t.Errorf("EXTINF line not preserved:\n%s", out)
	}
	// Ensure the original credentials are gone.
	if strings.Contains(out, "xtreamuser") || strings.Contains(out, "xtreampass") {
		t.Errorf("provider credentials leaked into output:\n%s", out)
	}
	// No .part file should remain.
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("temp .part file was not renamed away")
	}
}

func TestStreamRewriteM3UEmpty(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("#EXTM3U\n")),
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "empty.m3u")

	tracks, err := streamRewriteM3U(resp, dest, "u", "p", "lu", "lp")
	if err != nil {
		t.Fatal(err)
	}
	if tracks != 0 {
		t.Fatalf("tracks = %d, want 0", tracks)
	}
}
