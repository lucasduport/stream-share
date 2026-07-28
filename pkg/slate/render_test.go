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

package slate

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireFFmpeg skips when the toolchain is absent, so the suite still runs on a
// machine without ffmpeg (the feature degrades the same way at runtime).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	if fontFile() == "" {
		t.Skip("no DejaVu font installed")
	}
}

// TestRenderProducesPlayableTS is the round trip that unit tests of the filter
// string cannot give: it proves the assembled ffmpeg command actually runs and
// yields MPEG-TS a player will accept. A silent audio track is included because
// many IPTV players stall on a video-only stream.
func TestRenderProducesPlayableTS(t *testing.T) {
	requireFFmpeg(t)

	g := New(t.TempDir())
	if !g.Available() {
		t.Fatal("generator should be available when ffmpeg and a font are present")
	}

	cases := []struct {
		name string
		v    View
	}{
		{"typical", View{Code: "403", Meaning: "Forbidden", Message: "The provider refused access to this channel.", Channel: "NL Sport 1"}},
		{"synthetic key", View{Meaning: "Timed Out", Message: "The provider did not respond in time."}},
		{"long message wraps", View{Code: "503", Meaning: "Service Unavailable", Message: strings.Repeat("far too long to fit on one line ", 8), Channel: "Some Channel"}},
		{"empty view", View{}},
		// The important one: characters that are ffmpeg filter syntax must not
		// break the graph. Without sanitising, this fails to render at all.
		{"hostile text", View{Code: "456", Meaning: "Connection Limit", Message: `Nasty: 'quotes' 100% \ back:slash [brackets]`, Channel: "Ch: 1 & 2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := g.Clip(tc.v)
			if err != nil {
				t.Fatalf("Clip failed: %v", err)
			}

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat output: %v", err)
			}
			if fi.Size() == 0 {
				t.Fatal("clip is empty")
			}

			out, err := exec.Command("ffprobe", "-v", "error",
				"-show_entries", "stream=codec_name,codec_type",
				"-of", "csv=p=0", path).CombinedOutput()
			if err != nil {
				t.Fatalf("ffprobe rejected the clip: %v: %s", err, out)
			}
			streams := string(out)
			if !strings.Contains(streams, "h264") {
				t.Fatalf("no h264 video stream: %s", streams)
			}
			if !strings.Contains(streams, "aac") {
				t.Fatalf("no aac audio stream (players expect one): %s", streams)
			}
		})
	}
}

// TestClipIsCached guards the design decision that a repeated error does not
// re-run ffmpeg.
func TestClipIsCached(t *testing.T) {
	requireFFmpeg(t)

	g := New(t.TempDir())
	v := View{Code: "403", Meaning: "Forbidden", Message: "Denied.", Channel: "Ch"}

	first, err := g.Clip(v)
	if err != nil {
		t.Fatalf("first Clip: %v", err)
	}
	firstStat, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat first: %v", err)
	}

	second, err := g.Clip(v)
	if err != nil {
		t.Fatalf("second Clip: %v", err)
	}
	secondStat, err := os.Stat(second)
	if err != nil {
		t.Fatalf("stat second: %v", err)
	}

	if first != second {
		t.Fatalf("cache key differs across identical views: %s vs %s", first, second)
	}
	if !firstStat.ModTime().Equal(secondStat.ModTime()) {
		t.Fatal("clip was re-rendered instead of reused from cache")
	}
}

// TestDifferentViewsDifferentClips ensures the cache key covers every displayed
// field, so changing the wording actually changes what viewers see.
func TestDifferentViewsDifferentClips(t *testing.T) {
	requireFFmpeg(t)

	g := New(t.TempDir())
	base := View{Code: "403", Meaning: "Forbidden", Message: "Denied.", Channel: "Ch"}

	basePath, err := g.Clip(base)
	if err != nil {
		t.Fatalf("base Clip: %v", err)
	}

	changed := base
	changed.Message = "Different wording."
	changedPath, err := g.Clip(changed)
	if err != nil {
		t.Fatalf("changed Clip: %v", err)
	}

	if basePath == changedPath {
		t.Fatal("different messages produced the same cached clip")
	}
}
