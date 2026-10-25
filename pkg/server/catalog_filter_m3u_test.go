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
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/filter"
)

// The functions below are the get.php rewrite as it was before the catalogue
// filter, kept verbatim so the tests can prove that with no filter configured
// the output is byte-identical.

func legacyRewriteM3UToFile(body io.Reader, filePath, xtreamUser, xtreamPassword, localUser, localPassword string, collect bool, preferredLangs []string) (tracks int64, spans []trackSpan, err error) {
	f, err := os.Create(filePath)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(filePath)
		}
	}()

	w := bufio.NewWriterSize(f, 256*1024)
	r := bufio.NewReaderSize(body, 256*1024)

	var (
		offset     int64
		trackStart int64 = -1 // offset of the first line attached to the next URL
		extinf     string
	)
	for {
		line, readErr := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			isURL := strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://")
			if isURL {
				tracks++
				line = rewriteM3UURLLine(trimmed, xtreamUser, xtreamPassword, localUser, localPassword) + line[len(trimmed):]
			}
			if collect {
				switch {
				case isURL:
					start := trackStart
					if start < 0 {
						start = offset
					}
					spans = append(spans, trackSpan{
						start: start,
						end:   offset + int64(len(line)),
						rec: newDedupRecord(extinfName(extinf), trimmed,
							extractM3UAttr(extinf, "group-title"),
							extractM3UAttr(extinf, "tvg-logo") != "",
							extractM3UAttr(extinf, "tvg-id") != "",
							preferredLangs),
					})
					trackStart, extinf = -1, ""
				case strings.HasPrefix(trimmed, "#EXTM3U"):
					// Playlist header: belongs to no track.
				case strings.HasPrefix(trimmed, "#"):
					if trackStart < 0 {
						trackStart = offset
					}
					if strings.HasPrefix(trimmed, "#EXTINF") {
						extinf = trimmed
					}
				}
			}
			if _, err = w.WriteString(line); err != nil {
				return tracks, nil, err
			}
			offset += int64(len(line))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return tracks, nil, readErr
		}
	}

	if err = w.Flush(); err != nil {
		return tracks, nil, err
	}
	if err = f.Sync(); err != nil {
		return tracks, nil, err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(filePath)
		return tracks, nil, err
	}
	return tracks, spans, nil
}

func legacyStreamRewriteM3U(resp *http.Response, destPath, xtreamUser, xtreamPassword, localUser, localPassword string, dedup bool, preferredLangs []string) (int64, int64, error) {
	tmpPath := destPath + ".part"
	if !dedup {
		seen, _, err := legacyRewriteM3UToFile(resp.Body, tmpPath, xtreamUser, xtreamPassword, localUser, localPassword, false, nil)
		if err != nil {
			return seen, 0, err
		}
		if err := os.Rename(tmpPath, destPath); err != nil {
			_ = os.Remove(tmpPath)
			return seen, 0, err
		}
		return seen, seen, nil
	}

	rawPath := destPath + ".raw"
	defer func() { _ = os.Remove(rawPath) }()
	seen, spans, err := legacyRewriteM3UToFile(resp.Body, rawPath, xtreamUser, xtreamPassword, localUser, localPassword, true, preferredLangs)
	if err != nil {
		return seen, 0, err
	}

	recs := make([]dedupRecord, len(spans))
	for i := range spans {
		recs[i] = spans[i].rec
	}
	keep := dedupKeep(recs)

	written, err := copyKeptSpans(rawPath, tmpPath, spans, keep)
	if err != nil {
		return seen, written, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return seen, written, err
	}
	return seen, written, nil
}

// m3uFixtures exercise the shapes a get.php body can take: header with
// attributes, attached directives, blank lines inside and between tracks,
// CRLF, a trailing line without newline, tracks without #EXTINF, and a plain
// (non-Xtream) URL.
var m3uFixtures = map[string]string{
	"simple": "#EXTM3U\n" +
		"#EXTINF:-1 tvg-id=\"a\" group-title=\"A| News\",A| One\n" +
		"http://p/live/xu/xp/1.ts\n" +
		"#EXTINF:-1 group-title=\"B| Sport\",B| Two\n" +
		"http://p/live/xu/xp/2.ts\n",
	"directives": "#EXTM3U url-tvg=\"http://p/xmltv.php\"\n\n" +
		"#EXTINF:-1 group-title=\"A| News\",A| One\n#EXTGRP:A\n#EXTVLCOPT:http-user-agent=x\n" +
		"http://p/live/xu/xp/1.ts\n\n" +
		"#EXTINF:0,B| Movie\n\n#EXTGRP:B\n" +
		"http://p/movie/xu/xp/9.mkv\n" +
		"http://p/series/xu/xp/7.mp4\n" +
		"# trailing comment\n",
	"crlf":  "#EXTM3U\r\n#EXTINF:-1,A| One\r\nhttp://p/xu/xp/1\r\n#EXTINF:-1,B| Two\r\nhttps://p/live/xu/xp/2.m3u8",
	"plain": "#EXTM3U\n#EXTINF:-1 group-title=\"X\",Plain\nhttp://other.example/stream.m3u8?token=1\n",
}

func runRewrite(t *testing.T, body string, dedup bool, rules *filter.Set) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "out.m3u")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	if _, _, err := streamRewriteM3U(resp, dest, "xu", "xp", "lu", "lp", dedup, nil, rules); err != nil {
		t.Fatalf("streamRewriteM3U: %v", err)
	}
	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func runLegacyRewrite(t *testing.T, body string, dedup bool) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "legacy.m3u")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	if _, _, err := legacyStreamRewriteM3U(resp, dest, "xu", "xp", "lu", "lp", dedup, nil); err != nil {
		t.Fatalf("legacyStreamRewriteM3U: %v", err)
	}
	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestRewriteWithoutFilterIsByteIdentical: no filter configured means the
// get.php output is exactly what it was before the filter existed.
func TestRewriteWithoutFilterIsByteIdentical(t *testing.T) {
	for name, body := range m3uFixtures {
		for _, dedup := range []bool{false, true} {
			got := runRewrite(t, body, dedup, nil)
			want := runLegacyRewrite(t, body, dedup)
			if got != want {
				t.Errorf("%s dedup=%v: output differs\n got: %q\nwant: %q", name, dedup, got, want)
			}
		}
	}
}

func mustFilterSet(t *testing.T, cfg filter.Config) *filter.Set {
	t.Helper()
	s, err := filter.NewSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRewriteFilterDropsWholeTracks: a dropped track loses every attached
// line; kept tracks, the header and lines between tracks are untouched.
func TestRewriteFilterDropsWholeTracks(t *testing.T) {
	rules := mustFilterSet(t, filter.Config{All: filter.Patterns{Name: `^A\|`}})
	for _, dedup := range []bool{false, true} {
		got := runRewrite(t, m3uFixtures["directives"], dedup, rules)
		want := "#EXTM3U url-tvg=\"http://p/xmltv.php\"\n\n" +
			"#EXTINF:-1 group-title=\"A| News\",A| One\n#EXTGRP:A\n#EXTVLCOPT:http-user-agent=x\n" +
			"http://p/live/lu/lp/1.ts\n\n" +
			"# trailing comment\n"
		if got != want {
			t.Errorf("dedup=%v:\n got: %q\nwant: %q", dedup, got, want)
		}
	}
}

// TestRewriteFilterPerKind: kind rules follow the URL path; a group include
// does not drop uncategorized movies, which are judged on their name.
func TestRewriteFilterPerKind(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXTINF:-1 group-title=\"A| News\",A| One\nhttp://p/live/xu/xp/1.ts\n" +
		"#EXTINF:-1 group-title=\"C| News\",C| Two\nhttp://p/live/xu/xp/2.ts\n" +
		"#EXTINF:-1,A| Film\nhttp://p/movie/xu/xp/3.mkv\n" +
		"#EXTINF:-1,C| Film\nhttp://p/movie/xu/xp/4.mkv\n" +
		"#EXTINF:-1,C| Show S01E01\nhttp://p/series/xu/xp/5.mp4\n"
	rules := mustFilterSet(t, filter.Config{
		All:   filter.Patterns{Group: `^A\|`},
		Movie: filter.Patterns{Name: `^A\|`},
	})
	got := runRewrite(t, body, false, rules)
	for _, keep := range []string{"/1.ts", "/3.mkv", "/5.mp4"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s should be kept:\n%s", keep, got)
		}
	}
	for _, drop := range []string{"/2.ts", "/4.mkv"} {
		if strings.Contains(got, drop) {
			t.Errorf("%s should be dropped:\n%s", drop, got)
		}
	}
}

// TestRewriteFilterBeforeDedup: a filtered track never wins a duplicate group
// over a kept one.
func TestRewriteFilterBeforeDedup(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXTINF:-1 tvg-logo=\"l\" tvg-id=\"x\" group-title=\"C| 4K\",C| Dune (2021) 4K\nhttp://p/movie/xu/xp/1.mkv\n" +
		"#EXTINF:-1,A| Dune (2021)\nhttp://p/movie/xu/xp/2.mkv\n"
	rules := mustFilterSet(t, filter.Config{Movie: filter.Patterns{NameExclude: `^C\|`}})
	got := runRewrite(t, body, true, rules)
	if !strings.Contains(got, "/2.mkv") || strings.Contains(got, "/1.mkv") {
		t.Fatalf("the kept copy must survive dedup:\n%s", got)
	}
}

// TestFilterXMLTV: dropped channels lose their <channel> and every
// <programme>; the rest is copied byte for byte, whatever the line layout.
func TestFilterXMLTV(t *testing.T) {
	in := `<?xml version="1.0" encoding="utf-8" ?><!DOCTYPE tv SYSTEM "xmltv.dtd"><tv generator-info-name="g">` +
		`<channel id="keep.a"><display-name>A</display-name><icon src="x" /></channel>` +
		`<channel id="drop.b"><display-name>B</display-name></channel>` + "\n" +
		`<programme start="1" stop="2" channel="keep.a"><title>T &amp; U</title></programme>` +
		"\n  " + `<programme start="1" channel="drop.b"><title>gone</title><desc>x < y</desc></programme>` +
		`<programme channel="keep.a" start="3"/>` +
		`<channel	id="a&amp;b"><display-name>AB</display-name></channel>` +
		`</tv>` + "\n"
	var out bytes.Buffer
	kept, dropped, err := filterXMLTV(bufio.NewReaderSize(strings.NewReader(in), 16), &out, map[string]bool{"keep.a": true, "a&b": true})
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="utf-8" ?><!DOCTYPE tv SYSTEM "xmltv.dtd"><tv generator-info-name="g">` +
		`<channel id="keep.a"><display-name>A</display-name><icon src="x" /></channel>` +
		"\n" +
		`<programme start="1" stop="2" channel="keep.a"><title>T &amp; U</title></programme>` +
		"\n  " +
		`<programme channel="keep.a" start="3"/>` +
		`<channel	id="a&amp;b"><display-name>AB</display-name></channel>` +
		`</tv>` + "\n"
	if out.String() != want {
		t.Fatalf("filtered guide differs\n got: %q\nwant: %q", out.String(), want)
	}
	if kept != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 2 and 1", kept, dropped)
	}
}

// TestFilterXMLTVTruncated: a guide cut inside an element is an error, never
// a silently shortened copy.
func TestFilterXMLTVTruncated(t *testing.T) {
	in := `<tv><channel id="a"><display-name>A</display-name>`
	_, _, err := filterXMLTV(bufio.NewReader(strings.NewReader(in)), io.Discard, map[string]bool{"a": true})
	if err == nil {
		t.Fatal("truncated element must fail")
	}
}

// syntheticM3U builds a get.php-like playlist of n tracks (2n+1 lines) across
// live, movie and series, with a prefix in names and live groups.
func syntheticM3U(n int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	prefixes := []string{"A", "B", "C", "D", "E"}
	for i := 0; i < n; i++ {
		p := prefixes[i%len(prefixes)]
		switch i % 3 {
		case 0:
			b.WriteString("#EXTINF:-1 tvg-id=\"c" + p + "\" tvg-logo=\"http://img/x.png\" group-title=\"" + p + "| Group\"," + p + "| Channel\n")
			b.WriteString("http://p/live/xu/xp/" + strings.Repeat("1", 6) + ".ts\n")
		case 1:
			b.WriteString("#EXTINF:-1 tvg-logo=\"http://img/x.png\" group-title=\"\"," + p + "| Some Film (2021)\n")
			b.WriteString("http://p/movie/xu/xp/" + strings.Repeat("2", 6) + ".mkv\n")
		default:
			b.WriteString("#EXTINF:-1 group-title=\"\"," + p + "| Some Show S01 E02\n")
			b.WriteString("http://p/series/xu/xp/" + strings.Repeat("3", 7) + ".mp4\n")
		}
	}
	return b.String()
}

func benchRewrite(b *testing.B, rules *filter.Set) {
	body := syntheticM3U(500_000) // ~1M lines
	dest := filepath.Join(b.TempDir(), "out.m3u")
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
		if _, _, err := streamRewriteM3U(resp, dest, "xu", "xp", "lu", "lp", false, nil, rules); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteM3UNoFilter(b *testing.B) { benchRewrite(b, nil) }

func BenchmarkRewriteM3UFiltered(b *testing.B) {
	rules, _ := filter.NewSet(filter.Config{
		All:   filter.Patterns{Group: `^(A|B)\|`, Name: `^(A|B)\|`},
		Movie: filter.Patterns{NameExclude: `(?i)xxx`},
	})
	benchRewrite(b, rules)
}

func BenchmarkRewriteM3ULegacy(b *testing.B) {
	body := syntheticM3U(500_000)
	dest := filepath.Join(b.TempDir(), "out.m3u")
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
		if _, _, err := legacyStreamRewriteM3U(resp, dest, "xu", "xp", "lu", "lp", false, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFilterXMLTV(b *testing.B) {
	var g strings.Builder
	g.WriteString(`<?xml version="1.0" encoding="utf-8" ?><tv>`)
	keep := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		id := "ch" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
		if i%3 == 0 {
			keep[id] = true
		}
		g.WriteString(`<channel id="` + id + `"><display-name>Name</display-name><icon src="http://img/x.png" /></channel>`)
	}
	for i := 0; i < 400_000; i++ {
		id := "ch" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
		g.WriteString(`<programme start="20261025000000 +0000" stop="20261025010000 +0000" channel="` + id + `"><title>Some title here</title><desc>` + strings.Repeat("Description words. ", 8) + `</desc></programme>`)
	}
	g.WriteString(`</tv>`)
	in := g.String()
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := filterXMLTV(bufio.NewReaderSize(strings.NewReader(in), 256*1024), io.Discard, keep); err != nil {
			b.Fatal(err)
		}
	}
}
