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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// streamRewriteM3U downloads a provider M3U (get.php) and rewrites it to a
// local cache file with line-level credential replacement, without ever
// materializing the full playlist in RAM. This replaces the previous
// m3u.Parse(url) approach, which on a 424 MB / 1.45M-track catalog peaked at
// ~1.3 GiB heap and took 1-3 minutes.
//
// The rewrite replaces the provider's username/password in URLs with the local
// proxy credentials, preserving all other URL structure. Lines that are not
// http(s) URLs are passed through unchanged.
//
// When dedup is enabled, redundant tracks (the same Xtream stream ID listed
// twice, or same-title copies that differ only by language/quality) are
// dropped. Dropping requires seeing the whole playlist before deciding, so
// dedup uses a two-pass strategy: pass 1 streams the body to a temporary file
// while recording each track's byte range and metadata; pass 2 re-reads the
// temp file and emits only the surviving tracks. Peak memory stays
// proportional to the number of tracks, never to the playlist size.
//
// Returns the number of tracks seen (before dedup) and the number written.
func streamRewriteM3U(resp *http.Response, destPath, xtreamUser, xtreamPassword, localUser, localPassword string, dedup bool, preferredLangs []string) (int64, int64, error) {
	if dedup {
		return streamRewriteM3UDedup(resp, destPath, xtreamUser, xtreamPassword, localUser, localPassword, preferredLangs)
	}
	return streamRewriteM3UStream(resp, destPath, xtreamUser, xtreamPassword, localUser, localPassword)
}

// streamRewriteM3UStream is the single-pass, constant-memory rewrite used when
// dedup is disabled: every track line is credential-rewritten and written
// straight through.
func streamRewriteM3UStream(resp *http.Response, destPath, xtreamUser, xtreamPassword, localUser, localPassword string) (int64, int64, error) {
	tmpPath := destPath + ".part"
	f, err := os.Create(tmpPath)
	if err != nil {
		return 0, 0, err
	}

	w := bufio.NewWriterSize(f, 256*1024)
	r := bufio.NewReaderSize(resp.Body, 256*1024)

	var tracks int64

	for {
		line, readErr := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
				tracks++
				rewritten := rewriteM3UURLLine(trimmed, xtreamUser, xtreamPassword, localUser, localPassword)
				if _, werr := w.WriteString(rewritten); werr != nil {
					_ = f.Close()
					_ = os.Remove(tmpPath)
					return tracks, 0, werr
				}
				if line[len(line)-1] == '\n' {
					if _, werr := w.WriteString("\n"); werr != nil {
						_ = f.Close()
						_ = os.Remove(tmpPath)
						return tracks, 0, werr
					}
				}
			} else {
				if _, werr := w.WriteString(line); werr != nil {
					_ = f.Close()
					_ = os.Remove(tmpPath)
					return tracks, 0, werr
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return tracks, 0, readErr
		}
	}

	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return tracks, 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return tracks, 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return tracks, 0, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return tracks, 0, err
	}
	return tracks, tracks, nil
}

// dedupTrackRef records where one track lives in the pass-1 raw file so pass 2
// can re-emit only the surviving tracks.
type dedupTrackRef struct {
	extinfOff int64 // byte offset of the track's #EXTINF line, -1 when absent
	urlOff     int64 // byte offset of the URL line
	urlLen     int64 // length of the URL line including its newline
	track      m3u.Track
}

// streamRewriteM3UDedup is the two-pass deduplicating variant of
// streamRewriteM3U. Pass 1 streams the provider body to a raw temp file
// (credential-rewritten) while recording each track's byte range and
// metadata. Pass 2 runs deduplicateTrackIndices over the collected tracks and
// copies the surviving byte ranges into the final file. Memory is O(number of
// tracks); disk cost is one extra temp file, removed on return.
func streamRewriteM3UDedup(resp *http.Response, destPath, xtreamUser, xtreamPassword, localUser, localPassword string, preferredLangs []string) (int64, int64, error) {
	tmpPath := destPath + ".part"
	rawPath := destPath + ".raw"
	defer func() { _ = os.Remove(rawPath) }()

	raw, err := os.Create(rawPath)
	if err != nil {
		return 0, 0, err
	}
	w := bufio.NewWriterSize(raw, 256*1024)
	r := bufio.NewReaderSize(resp.Body, 256*1024)

	var (
		tracks int64
		offset int64
		refs   []dedupTrackRef

		pendingExtinfOff int64 = -1
		lastName         string
		lastLen          = -1
		lastTags         []m3u.Tag
	)

	for {
		line, readErr := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(trimmed, "#EXTINF"):
				pendingExtinfOff = offset
				lastName, lastLen, lastTags = parseExtinfLine(trimmed)
				// Offsets track bytes actually written, which for URL lines
				// below is the rewritten line, not the raw one.
				offset += writeRawLine(w, line)
			case strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://"):
				tracks++
				urlOff := offset
				rewritten := rewriteM3UURLLine(trimmed, xtreamUser, xtreamPassword, localUser, localPassword)
				written := int64(len(rewritten))
				w.WriteString(rewritten) // nolint: errcheck
				if line[len(line)-1] == '\n' {
					written++
					w.WriteString("\n") // nolint: errcheck
				}
				offset += written
				refs = append(refs, dedupTrackRef{
					extinfOff: pendingExtinfOff,
					urlOff:     urlOff,
					urlLen:     written,
					track:      m3u.Track{Name: lastName, Length: lastLen, URI: trimmed, Tags: lastTags},
				})
				pendingExtinfOff = -1
				lastName, lastLen, lastTags = "", -1, nil
			default:
				offset += writeRawLine(w, line)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			_ = raw.Close()
			return tracks, 0, readErr
		}
	}

	if err := w.Flush(); err != nil {
		_ = raw.Close()
		return tracks, 0, err
	}
	if err := raw.Sync(); err != nil {
		_ = raw.Close()
		return tracks, 0, err
	}
	if err := raw.Close(); err != nil {
		return tracks, 0, err
	}

	// Decide which track indices survive. The index mapping is exact even
	// when duplicate entries share URI and title but differ in tags.
	m3uTracks := make([]m3u.Track, len(refs))
	for i, ref := range refs {
		m3uTracks[i] = ref.track
	}
	keepIdx := deduplicateTrackIndices(m3uTracks, preferredLangs)
	keepSet := make([]bool, len(refs))
	for _, i := range keepIdx {
		keepSet[i] = true
	}

	// Pass 2: emit header + surviving tracks by copying byte ranges.
	out, err := os.Create(tmpPath)
	if err != nil {
		return tracks, 0, err
	}
	ow := bufio.NewWriterSize(out, 256*1024)
	ow.WriteString("#EXTM3U\n") // nolint: errcheck

	rawIn, err := os.Open(rawPath)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return tracks, 0, err
	}
	defer func() { _ = rawIn.Close() }()

	var written int64
	buf := make([]byte, 256*1024)
	for i, ref := range refs {
		if !keepSet[i] {
			continue
		}
		if ref.extinfOff >= 0 {
			if err := copyRange(ow, rawIn, ref.extinfOff, ref.urlOff-ref.extinfOff, buf); err != nil {
				_ = out.Close()
				_ = os.Remove(tmpPath)
				return tracks, written, err
			}
		}
		if err := copyRange(ow, rawIn, ref.urlOff, ref.urlLen, buf); err != nil {
			_ = out.Close()
			_ = os.Remove(tmpPath)
			return tracks, written, err
		}
		written++
	}

	if err := ow.Flush(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return tracks, written, err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return tracks, written, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return tracks, written, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return tracks, written, err
	}
	return tracks, written, nil
}

// writeRawLine writes line verbatim and returns the number of bytes written.
func writeRawLine(w *bufio.Writer, line string) int64 {
	n, _ := w.WriteString(line)
	return int64(n)
}

// copyRange copies length bytes starting at offset from src to w.
func copyRange(w *bufio.Writer, src *os.File, offset, length int64, buf []byte) error {
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	remaining := length
	for remaining > 0 {
		n := int64(len(buf))
		if n > remaining {
			n = remaining
		}
		rn, err := src.Read(buf[:n])
		if rn > 0 {
			if _, werr := w.Write(buf[:rn]); werr != nil {
				return werr
			}
			remaining -= int64(rn)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	return nil
}

// parseExtinfLine extracts the display name, length and tags from a raw
// #EXTINF line, mirroring the semantics of the jamesnetherton/m3u parser.
func parseExtinfLine(line string) (name string, length int, tags []m3u.Tag) {
	length = -1
	body := strings.TrimPrefix(line, "#EXTINF:")
	if idx := strings.LastIndex(body, ","); idx != -1 {
		name = strings.TrimSpace(body[idx+1:])
		fields := strings.Fields(body[:idx])
		if len(fields) > 0 {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				length = n
			}
		}
	} else {
		name = strings.TrimSpace(body)
	}
	for _, m := range reDedupTag.FindAllStringSubmatch(line, -1) {
		if len(m) == 3 {
			tags = append(tags, m3u.Tag{Name: m[1], Value: m[2]})
		}
	}
	return name, length, tags
}

// rewriteM3UURLLine replaces provider credentials embedded in an M3U URL line
// with the local proxy credentials. It handles both path-embedded credentials
// (Xtream style: /live/user/pass/id.ts) and query-string credentials
// (?username=...&password=...).
func rewriteM3UURLLine(line, xtreamUser, xtreamPassword, localUser, localPassword string) string {
	if xtreamUser == "" && xtreamPassword == "" {
		return line
	}
	out := line
	// Path-embedded: replace /xtreamUser/xtreamPassword/ segments.
	if xtreamUser != "" {
		out = strings.ReplaceAll(out, "/"+xtreamUser+"/", "/"+localUser+"/")
	}
	if xtreamPassword != "" {
		out = strings.ReplaceAll(out, "/"+xtreamPassword+"/", "/"+localPassword+"/")
	}
	// Query-string credentials.
	if u, err := url.Parse(out); err == nil {
		q := u.Query()
		if q.Get("username") == xtreamUser && xtreamUser != "" {
			q.Set("username", localUser)
		}
		if q.Get("password") == xtreamPassword && xtreamPassword != "" {
			q.Set("password", localPassword)
		}
		u.RawQuery = q.Encode()
		out = u.String()
	}
	return out
}

// fetchAndRewriteXtreamM3U downloads the provider's get.php M3U and writes a
// credential-rewritten copy to destPath using stream rewriting (constant
// memory). It returns the number of tracks and any error.
func (c *Config) fetchAndRewriteXtreamM3U(rawURL, destPath string) (int64, error) {
	utils.InfoLog("Streaming M3U from Xtream (constant-memory rewrite): %s", utils.MaskURL(rawURL))

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")

	client := &http.Client{
		Timeout: 0, // no global timeout; the download may take minutes for 400+ MB
		Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("M3U fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("backend returned %d for M3U request", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, err
	}

	seen, written, err := streamRewriteM3U(resp, destPath,
		c.XtreamUser.String(), c.XtreamPassword.String(),
		c.User.String(), c.Password.String(),
		c.M3UDedupEnabled, dedupPreferredLanguages(c.M3UDedupPreferredLangs))
	if err != nil {
		return seen, err
	}
	if c.M3UDedupEnabled && written < seen {
		utils.InfoLog("M3U dedup: %d tracks -> %d kept (%d redundant removed)", seen, written, seen-written)
	}
	if written == 0 {
		return written, fmt.Errorf("empty playlist returned by Xtream backend")
	}
	utils.InfoLog("Streamed and rewrote M3U: %d tracks -> %s", written, destPath)
	return written, nil
}
