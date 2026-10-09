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

	"github.com/jamesnetherton/m3u"
)

func dedupTrack(name, uri string, tags ...m3u.Tag) m3u.Track {
	return m3u.Track{Name: name, Length: -1, URI: uri, Tags: tags}
}

func xtreamURI(kind, id string) string {
	return "http://provider.example.com/" + kind + "/xtreamuser/xtreampass/" + id
}

func TestDedupNormalizeTitle(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // a and b must map to the same key
	}{
		{"Dune (2021) 4K UHD", "dune 2021", true},
		{"Dune (2021) 4K UHD FR", "DUNE [2021]", true},
		{"Le Seigneur des Anneaux", "seigneur des anneaux", true},
		{"The Office S01E02", "the office s1e2", true},
		{"The Office S01E02", "The Office S01E03", false},
		{"Film FR", "Film EN", true},
		{"Français Accentué", "francais accentue", true},
		{"Movie (2019)", "Movie (2021)", false}, // year differs
		{"Canal+ Sport 1 HD", "Canal+ Sport 1 4K", true},
		{"TLC", "tlc", true},
	}
	for _, tc := range cases {
		ka, kb := dedupNormalizeTitle(tc.a), dedupNormalizeTitle(tc.b)
		if (ka == kb) != tc.want {
			t.Errorf("normalize(%q)=%q normalize(%q)=%q, want equal=%v", tc.a, ka, tc.b, kb, tc.want)
		}
	}
}

func TestDedupDetectMetadata(t *testing.T) {
	if q := dedupDetectQuality("Movie 4K UHD"); q != 80 {
		t.Errorf("quality 4K = %d, want 80", q)
	}
	if q := dedupDetectQuality("Movie HD 720p"); q != 60 {
		t.Errorf("quality 720p = %d, want 60", q)
	}
	if l := dedupDetectLanguage("Film VF"); l != "fra" {
		t.Errorf("lang VF = %q, want fra", l)
	}
	if l := dedupDetectLanguage("Film VOSTFR"); l != "vostfr" {
		t.Errorf("lang VOSTFR = %q, want vostfr", l)
	}
	if l := dedupDetectLanguage("Film ITA"); l != "ita" {
		t.Errorf("lang ITA = %q, want ita", l)
	}
	if l := dedupDetectLanguage("Plain Movie"); l != "" {
		t.Errorf("lang none = %q, want empty", l)
	}
	if y := dedupDetectYear("Dune (2021)"); y != 2021 {
		t.Errorf("year = %d, want 2021", y)
	}
}

func TestDeduplicateExactIDDuplicates(t *testing.T) {
	tracks := []m3u.Track{
		dedupTrack("Channel One", xtreamURI("live", "12345.ts")),
		dedupTrack("Channel One (copy)", xtreamURI("live", "12345.ts")),
		dedupTrack("Channel Two", xtreamURI("live", "67890.ts")),
	}
	out, removed := deduplicateTracks(tracks, nil)
	if removed != 1 || len(out) != 2 {
		t.Fatalf("removed=%d len=%d, want removed=1 len=2", removed, len(out))
	}
	if out[0].URI != xtreamURI("live", "12345.ts") || out[1].URI != xtreamURI("live", "67890.ts") {
		t.Errorf("unexpected survivors: %+v", out)
	}
}

func TestDeduplicateSameMovieDifferentIDs(t *testing.T) {
	// Same movie listed twice with different stream IDs, differing only by
	// language and quality: one entry must survive, the best one.
	tracks := []m3u.Track{
		dedupTrack("Dune (2021) 4K", xtreamURI("movie", "1001.mp4")),
		dedupTrack("Dune (2021) FR", xtreamURI("movie", "1002.mp4")),
		dedupTrack("Dune (2021) VF 1080p", xtreamURI("movie", "1003.mp4")),
	}
	out, removed := deduplicateTracks(tracks, []string{"fra"})
	if removed != 2 || len(out) != 1 {
		t.Fatalf("removed=%d len=%d, want removed=2 len=1", removed, len(out))
	}
	// 4K (score 80) beats VF 1080p (score 70) despite language preference.
	if out[0].URI != xtreamURI("movie", "1001.mp4") {
		t.Errorf("kept %q, want the 4K source", out[0].URI)
	}
}

func TestDeduplicateLanguagePreference(t *testing.T) {
	// Equal quality: the preferred language wins.
	tracks := []m3u.Track{
		dedupTrack("Dune (2021) EN 1080p", xtreamURI("movie", "1001.mp4")),
		dedupTrack("Dune (2021) FR 1080p", xtreamURI("movie", "1002.mp4")),
	}
	out, removed := deduplicateTracks(tracks, []string{"fra", "eng"})
	if removed != 1 || len(out) != 1 {
		t.Fatalf("removed=%d len=%d, want removed=1 len=1", removed, len(out))
	}
	if out[0].URI != xtreamURI("movie", "1002.mp4") {
		t.Errorf("kept %q, want the FR source", out[0].URI)
	}
}

func TestDeduplicateNeverMergesEpisodes(t *testing.T) {
	// Two episodes of the same series share the series title prefix but have
	// distinct SxxEyy markers: both must survive.
	tracks := []m3u.Track{
		dedupTrack("The Office S01E01", xtreamURI("series", "5001.mkv")),
		dedupTrack("The Office S01E02", xtreamURI("series", "5002.mkv")),
	}
	out, removed := deduplicateTracks(tracks, nil)
	if removed != 0 || len(out) != 2 {
		t.Fatalf("removed=%d len=%d, want removed=0 len=2", removed, len(out))
	}
}

func TestDeduplicateNeverMergesDifferentYears(t *testing.T) {
	tracks := []m3u.Track{
		dedupTrack("Dune (1984)", xtreamURI("movie", "2001.mp4")),
		dedupTrack("Dune (2021)", xtreamURI("movie", "2002.mp4")),
	}
	out, removed := deduplicateTracks(tracks, nil)
	if removed != 0 || len(out) != 2 {
		t.Fatalf("removed=%d len=%d, want removed=0 len=2", removed, len(out))
	}
}

func TestDeduplicateKeepsDistinctChannels(t *testing.T) {
	tracks := []m3u.Track{
		dedupTrack("Channel One", xtreamURI("live", "1.ts")),
		dedupTrack("Channel Two", xtreamURI("live", "2.ts")),
		dedupTrack("Channel Three", xtreamURI("live", "3.ts")),
	}
	out, removed := deduplicateTracks(tracks, nil)
	if removed != 0 || len(out) != 3 {
		t.Fatalf("removed=%d len=%d, want 0/3", removed, len(out))
	}
}

func TestDeduplicatePrefersRicherMetadata(t *testing.T) {
	// Same ID twice: the copy with logo + EPG id + group wins over the bare one.
	tracks := []m3u.Track{
		dedupTrack("Channel One", xtreamURI("live", "42.ts")),
		dedupTrack("Channel One", xtreamURI("live", "42.ts"),
			m3u.Tag{Name: "tvg-logo", Value: "http://logo/42.png"},
			m3u.Tag{Name: "tvg-id", Value: "channel.one"},
			m3u.Tag{Name: "group-title", Value: "Sports"},
		),
	}
	out, removed := deduplicateTracks(tracks, nil)
	if removed != 1 || len(out) != 1 {
		t.Fatalf("removed=%d len=%d, want removed=1 len=1", removed, len(out))
	}
	if len(out[0].Tags) != 3 {
		t.Errorf("kept entry has %d tags, want the richer 3-tag copy", len(out[0].Tags))
	}
}

func TestDeduplicatePlainM3UExactURL(t *testing.T) {
	// Plain M3U (no Xtream type segment): exact same URL is deduplicated, but
	// same-named files from different hosts are not.
	tracks := []m3u.Track{
		dedupTrack("Movie", "http://a.example.com/dir/movie.mp4"),
		dedupTrack("Movie", "http://a.example.com/dir/movie.mp4"),
		dedupTrack("Movie", "http://b.example.com/dir/movie.mp4"),
	}
	out, removed := deduplicateTracks(tracks, nil)
	if removed != 1 || len(out) != 2 {
		t.Fatalf("removed=%d len=%d, want removed=1 len=2", removed, len(out))
	}
}

func TestStreamRewriteM3UDedup(t *testing.T) {
	body := `#EXTM3U
#EXTINF:-1 tvg-id="ch1" group-title="Sports",Channel One
http://provider.example.com/live/xtreamuser/xtreampass/12345.ts
#EXTINF:-1,Channel One (copy)
http://provider.example.com/live/xtreamuser/xtreampass/12345.ts
#EXTINF:-1 group-title="Movies",Dune (2021) 4K
http://provider.example.com/movie/xtreamuser/xtreampass/1001.mp4
#EXTINF:-1 group-title="Movies",Dune (2021) VF 1080p
http://provider.example.com/movie/xtreamuser/xtreampass/1002.mp4
#EXTINF:-1,The Office S01E01
http://provider.example.com/series/xtreamuser/xtreampass/5001.mkv
#EXTINF:-1,The Office S01E02
http://provider.example.com/series/xtreamuser/xtreampass/5002.mkv
`
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.m3u")

	seen, written, err := streamRewriteM3U(resp, dest, "xtreamuser", "xtreampass", "localuser", "localpass", true, []string{"fra"})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 5 {
		t.Fatalf("seen = %d, want 5", seen)
	}
	// 5 seen -> 3 kept: duplicate channel, one of the two Dune copies; both
	// episodes survive.
	if written != 3 {
		t.Fatalf("written = %d, want 3", written)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)

	if !strings.HasPrefix(out, "#EXTM3U\n") {
		t.Errorf("output missing #EXTM3U header:\n%s", out)
	}
	// 4K Dune wins over VF 1080p.
	if !strings.Contains(out, "http://provider.example.com/movie/localuser/localpass/1001.mp4") {
		t.Errorf("4K Dune source missing:\n%s", out)
	}
	if strings.Contains(out, "xtreampass/1002.mp4") {
		t.Errorf("redundant VF Dune source was not removed:\n%s", out)
	}
	// Exactly one copy of the duplicated channel.
	if got := strings.Count(out, "localpass/12345.ts"); got != 1 {
		t.Errorf("channel 12345 appears %d times, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `tvg-id="ch1"`) {
		t.Errorf("EXTINF tags of the kept channel copy lost:\n%s", out)
	}
	// Both episodes survive.
	if !strings.Contains(out, "localpass/5001.mkv") || !strings.Contains(out, "localpass/5002.mkv") {
		t.Errorf("episodes missing from output:\n%s", out)
	}
	// No provider credentials leak.
	if strings.Contains(out, "xtreamuser") || strings.Contains(out, "xtreampass") {
		t.Errorf("provider credentials leaked:\n%s", out)
	}
	// Temp files are cleaned up.
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part file left behind")
	}
	if _, err := os.Stat(dest + ".raw"); !os.IsNotExist(err) {
		t.Error(".raw file left behind")
	}
}

func TestStreamRewriteM3UDedupKeepsOrder(t *testing.T) {
	body := `#EXTM3U
#EXTINF:-1,First
http://provider.example.com/live/u/p/1.ts
#EXTINF:-1,Second
http://provider.example.com/live/u/p/2.ts
#EXTINF:-1,First (dup)
http://provider.example.com/live/u/p/1.ts
#EXTINF:-1,Third
http://provider.example.com/live/u/p/3.ts
`
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.m3u")

	_, written, err := streamRewriteM3U(resp, dest, "u", "p", "lu", "lp", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if written != 3 {
		t.Fatalf("written = %d, want 3", written)
	}
	data, _ := os.ReadFile(dest)
	out := string(data)
	i1 := strings.Index(out, "lu/lp/1.ts")
	i2 := strings.Index(out, "lu/lp/2.ts")
	i3 := strings.Index(out, "lu/lp/3.ts")
	if !(i1 != -1 && i1 < i2 && i2 < i3) {
		t.Errorf("survivors out of order:\n%s", out)
	}
	if got := strings.Count(out, "lu/lp/1.ts"); got != 1 {
		t.Errorf("track 1 appears %d times, want 1", got)
	}
}

func TestParseExtinfLine(t *testing.T) {
	name, length, tags := parseExtinfLine(`#EXTINF:-1 tvg-id="ch1" group-title="Sports",Channel One`)
	if name != "Channel One" {
		t.Errorf("name = %q", name)
	}
	if length != -1 {
		t.Errorf("length = %d", length)
	}
	if dedupTagValue(tags, "tvg-id") != "ch1" || dedupTagValue(tags, "group-title") != "Sports" {
		t.Errorf("tags = %+v", tags)
	}
}

func TestDedupPreferredLanguages(t *testing.T) {
	got := dedupPreferredLanguages(" FRA , eng ,, ITA ")
	want := []string{"fra", "eng", "ita"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
