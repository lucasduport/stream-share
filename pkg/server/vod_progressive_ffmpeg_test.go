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
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end tests with real demuxers: ffprobe and ffmpeg open a movie being
// downloaded through the progressive path, the way players built on ffmpeg do.
// Containers whose index sits at the end of the file (matroska written by most
// muxers, MP4 without faststart) make the demuxer jump to the end before it
// can play; that jump must not wait for the whole download.
//
// They need ffmpeg and ffprobe on PATH and are skipped otherwise (CI installs
// them).

// ffmpegTools skips the test unless ffmpeg and ffprobe are available.
func ffmpegTools(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
}

// makeFixture encodes a short test movie into path with ffmpeg.
func makeFixture(t *testing.T, path string, extra ...string) []byte {
	t.Helper()
	args := []string{"-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "30", "-c:v", "mpeg4", "-q:v", "3", "-g", "25"}
	args = append(args, extra...)
	args = append(args, path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mp4TopLevelBoxes lists the types of an MP4 file's top-level boxes, in order.
func mp4TopLevelBoxes(data []byte) []string {
	var types []string
	for off := uint64(0); off+8 <= uint64(len(data)); {
		size := uint64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		header := uint64(8)
		switch size {
		case 1:
			if off+16 > uint64(len(data)) {
				return types
			}
			size, header = binary.BigEndian.Uint64(data[off+8:]), 16
		case 0:
			size = uint64(len(data)) - off
		}
		if size < header {
			return types
		}
		types = append(types, typ)
		off += size
	}
	return types
}

// probeWhileDownloading serves fixture from a provider paced so that the whole
// download takes about 15 s, then checks that ffprobe identifies the streams
// and ffmpeg decodes a frame after a seek to the middle, both well before the
// download completes, over a single provider connection, and that the cached
// file is byte-identical in the end.
func probeWhileDownloading(t *testing.T, fixture []byte, id string) {
	t.Helper()
	const chunk = 64 << 10
	delay := 15 * time.Second * chunk / time.Duration(len(fixture))
	p := &fakeVODProvider{data: fixture, chunk: chunk, delay: delay}
	c, _, front := progressiveTestServer(t, p, true)
	url := front.URL + "/series/" + id
	idRaw := strings.TrimSuffix(id, filepath.Ext(id))
	total := int64(len(fixture))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", url).CombinedOutput()
	probeTook := time.Since(start)
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "video") || !strings.Contains(string(out), "audio") {
		t.Fatalf("ffprobe found no video/audio stream: %q", out)
	}
	st := downloadState(c, idRaw)
	if st != nil {
		t.Logf("ffprobe identified the streams in %v, download at %d/%d bytes", probeTook, st.written, total)
	}
	if probeTook > 8*time.Second {
		t.Fatalf("ffprobe took %v: it waited for the download (~15 s) instead of being served the index", probeTook)
	}
	if st == nil || st.written >= total {
		t.Fatal("the download already finished: the test did not exercise a read ahead of it")
	}

	start = time.Now()
	out, err = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-ss", "15", "-i", url,
		"-frames:v", "1", "-f", "null", "-").CombinedOutput()
	seekTook := time.Since(start)
	if err != nil {
		t.Fatalf("ffmpeg seek+decode: %v\n%s", err, out)
	}
	t.Logf("ffmpeg decoded a frame at 15 s in %v", seekTook)
	if seekTook > 10*time.Second {
		t.Fatalf("decoding a frame mid-file took %v", seekTook)
	}

	deadline := time.Now().Add(90 * time.Second)
	for downloadState(c, idRaw) != nil {
		if time.Now().After(deadline) {
			t.Fatal("download did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := p.maxConcurrent(50 * time.Millisecond); n != 1 {
		t.Fatalf("provider had %d connections open at once, want 1 (ranges %v)", n, p.rangesSeen())
	}
	got, err := os.ReadFile(filepath.Join(os.Getenv("CACHE_FOLDER"), "vod", id))
	if err != nil || !bytes.Equal(got, fixture) {
		t.Fatalf("cached file differs from the source: %v", err)
	}
}

// Matroska with its index (cues) at the end, as ffmpeg's and mkvmerge's
// muxers write it.
func TestProgressiveFFprobeMatroskaIndexAtEnd(t *testing.T) {
	ffmpegTools(t)
	fixture := makeFixture(t, filepath.Join(t.TempDir(), "fixture.mkv"), "-c:a", "pcm_s16le")
	cues := []byte{0x1C, 0x53, 0xBB, 0x6B} // Cues element ID
	if pos := bytes.LastIndex(fixture, cues); pos < len(fixture)/2 {
		t.Fatalf("fixture's cues are not at the end (last at %d of %d)", pos, len(fixture))
	}
	probeWhileDownloading(t, fixture, "301.mkv")
}

// MP4 without faststart: the moov box (the index) comes after the media data.
func TestProgressiveFFprobeMP4MoovAtEnd(t *testing.T) {
	ffmpegTools(t)
	fixture := makeFixture(t, filepath.Join(t.TempDir(), "fixture.mp4"), "-c:a", "aac", "-b:a", "128k")
	boxes := strings.Join(mp4TopLevelBoxes(fixture), ",")
	if mdat, moov := strings.Index(boxes, "mdat"), strings.Index(boxes, "moov"); mdat < 0 || moov < mdat {
		t.Fatalf("fixture's moov is not after mdat: %s", boxes)
	}
	probeWhileDownloading(t, fixture, "302.mp4")
}
