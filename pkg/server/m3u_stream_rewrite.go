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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/filter"
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
// When dedup is enabled, redundant tracks are dropped (see deduplicateTracks).
// Dropping needs the whole playlist before deciding, so the rewritten body is
// first written to a temp file while a compact record and byte range is kept
// per track; a second sequential pass then copies only the surviving ranges.
// Memory stays at a few dozen bytes per track, never the playlist size.
//
// When rules is non-nil, tracks the catalogue filter drops are left out (with
// every line attached to them); filtering happens before dedup so a dropped
// track never wins a duplicate group.
//
// Returns the number of tracks seen and the number written.
func streamRewriteM3U(resp *http.Response, destPath, xtreamUser, xtreamPassword, localUser, localPassword string, dedup bool, preferredLangs []string, rules *filter.Set) (int64, int64, error) {
	tmpPath := destPath + ".part"
	if !dedup {
		seen, written, _, err := rewriteM3UToFile(resp.Body, tmpPath, xtreamUser, xtreamPassword, localUser, localPassword, false, nil, rules)
		if err != nil {
			return seen, 0, err
		}
		if err := os.Rename(tmpPath, destPath); err != nil {
			_ = os.Remove(tmpPath)
			return seen, 0, err
		}
		return seen, written, nil
	}

	rawPath := destPath + ".raw"
	defer func() { _ = os.Remove(rawPath) }()
	seen, _, spans, err := rewriteM3UToFile(resp.Body, rawPath, xtreamUser, xtreamPassword, localUser, localPassword, true, preferredLangs, rules)
	if err != nil {
		return seen, 0, err
	}

	// Dedup decides among the tracks the filter kept only.
	recs := make([]dedupRecord, 0, len(spans))
	for i := range spans {
		if !spans[i].filtered {
			recs = append(recs, spans[i].rec)
		}
	}
	survivors := dedupKeep(recs)
	keep := make([]bool, len(spans))
	for i, j := 0, 0; i < len(spans); i++ {
		if !spans[i].filtered {
			keep[i] = survivors[j]
			j++
		}
	}

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

// trackSpan locates one track in the pass-1 file: the bytes from its first
// attached line (#EXTINF, #EXTGRP, ...) through its URL line.
type trackSpan struct {
	start, end int64
	rec        dedupRecord
	filtered   bool // dropped by the catalogue filter
}

// rewriteM3UToFile streams body into path, rewriting credentials in URL
// lines, and returns the number of tracks seen and written. When collect is
// set it writes every track and returns one trackSpan per track for
// deduplication, marking those the filter drops; otherwise tracks the filter
// drops are left out of the file directly. The lines attached to a track
// (#EXTINF, #EXTGRP, #EXTVLCOPT, ...) are held until its URL line decides it,
// so memory stays bounded by one track. On error the file is removed.
func rewriteM3UToFile(body io.Reader, filePath, xtreamUser, xtreamPassword, localUser, localPassword string, collect bool, preferredLangs []string, rules *filter.Set) (tracks, written int64, spans []trackSpan, err error) {
	f, err := os.Create(filePath)
	if err != nil {
		return 0, 0, nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(filePath)
		}
	}()

	w := bufio.NewWriterSize(f, 256*1024)
	r := bufio.NewReaderSize(body, 256*1024)
	filtering := rules.Active()

	var (
		offset  int64
		pending []string // lines attached to the next URL, not yet written
		extinf  string
	)
	write := func(line string) error {
		_, werr := w.WriteString(line)
		offset += int64(len(line))
		return werr
	}
	for {
		line, readErr := r.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			isURL := strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://")
			switch {
			case isURL:
				tracks++
				line = rewriteM3UURLLine(trimmed, xtreamUser, xtreamPassword, localUser, localPassword) + line[len(trimmed):]
				keep := true
				var group string
				if filtering || collect {
					group = extractM3UAttr(extinf, "group-title")
				}
				if filtering {
					keep = rules.Keep(filter.PlaylistKind(trimmed), group, extinfName(extinf), false)
				}
				if !keep && !collect {
					pending, extinf = pending[:0], ""
					break
				}
				start := offset
				for _, p := range pending {
					if err = write(p); err != nil {
						return tracks, written, nil, err
					}
				}
				if err = write(line); err != nil {
					return tracks, written, nil, err
				}
				if collect {
					spans = append(spans, trackSpan{
						start:    start,
						end:      offset,
						filtered: !keep,
						rec: newDedupRecord(extinfName(extinf), trimmed, group,
							extractM3UAttr(extinf, "tvg-logo") != "",
							extractM3UAttr(extinf, "tvg-id") != "",
							preferredLangs),
					})
				}
				if keep {
					written++
				}
				pending, extinf = pending[:0], ""
			case strings.HasPrefix(trimmed, "#EXTM3U") && len(pending) == 0:
				// Playlist header: belongs to no track.
				if err = write(line); err != nil {
					return tracks, written, nil, err
				}
			case strings.HasPrefix(trimmed, "#"):
				if strings.HasPrefix(trimmed, "#EXTINF") {
					extinf = trimmed
				}
				pending = append(pending, line)
			case len(pending) > 0:
				// A blank or unknown line inside a track stays with it.
				pending = append(pending, line)
			default:
				// Between tracks: belongs to none, always kept.
				if err = write(line); err != nil {
					return tracks, written, nil, err
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return tracks, written, nil, readErr
		}
	}
	// Lines after the last URL (trailing comments, blank lines) are kept.
	for _, p := range pending {
		if err = write(p); err != nil {
			return tracks, written, nil, err
		}
	}

	if err = w.Flush(); err != nil {
		return tracks, written, nil, err
	}
	if err = f.Sync(); err != nil {
		return tracks, written, nil, err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(filePath)
		return tracks, written, nil, err
	}
	return tracks, written, spans, nil
}

// copyKeptSpans copies srcPath to dstPath, dropping the spans not marked in
// keep. Spans are in increasing offset order, so the source is read once,
// sequentially; non-track bytes (the #EXTM3U header) are preserved. Returns
// the number of tracks written; on error dstPath is removed.
func copyKeptSpans(srcPath, dstPath string, spans []trackSpan, keep []bool) (written int64, err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.Create(dstPath)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = dst.Close()
			_ = os.Remove(dstPath)
		}
	}()

	r := bufio.NewReaderSize(src, 256*1024)
	w := bufio.NewWriterSize(dst, 256*1024)
	var pos int64
	// copyTo copies (or skips) bytes up to offset end.
	copyTo := func(end int64, drop bool) error {
		n := end - pos
		if n <= 0 {
			return nil
		}
		pos = end
		if drop {
			_, err := r.Discard(int(n))
			return err
		}
		_, err := io.CopyN(w, r, n)
		return err
	}
	for i, s := range spans {
		if err = copyTo(s.start, false); err != nil {
			return written, err
		}
		if err = copyTo(s.end, !keep[i]); err != nil {
			return written, err
		}
		if keep[i] {
			written++
		}
	}
	if _, err = io.Copy(w, r); err != nil {
		return written, err
	}
	if err = w.Flush(); err != nil {
		return written, err
	}
	if err = dst.Sync(); err != nil {
		return written, err
	}
	if err = dst.Close(); err != nil {
		_ = os.Remove(dstPath)
		return written, err
	}
	return written, nil
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

// getPHPStallTimeout aborts a get.php download that has received no bytes for
// this long.
const getPHPStallTimeout = 60 * time.Second

// getPHPClient downloads get.php playlists. It has no overall timeout (see
// getPHPStallTimeout); providers can take a while to start answering for a big
// playlist. Accept-Encoding is left to the transport so it asks for gzip and
// decompresses transparently: M3U text shrinks ~10x on the wire, which
// dominates download time on a slow provider.
var getPHPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 3 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

// fetchAndRewriteXtreamM3U downloads the provider's get.php M3U and writes a
// credential-rewritten copy to destPath using stream rewriting (constant
// memory). It returns the number of tracks and any error.
func (c *Config) fetchAndRewriteXtreamM3U(rawURL, destPath string) (int64, error) {
	utils.InfoLog("Streaming M3U from Xtream (constant-memory rewrite): %s", utils.MaskURL(rawURL))

	// No overall deadline (a big playlist from a slow provider takes minutes
	// while still making progress), but a body that stops arriving is aborted:
	// otherwise a stalled provider would leave the refresh hanging forever.
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Accept", "*/*")
	// Accept-Encoding is left to the transport so it asks for gzip and
	// decompresses transparently: M3U text shrinks ~10x on the wire, which
	// dominates download time on a slow provider.

	resp, err := getPHPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("M3U fetch failed: %w", stripURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	stall := time.AfterFunc(getPHPStallTimeout, cancel)
	defer stall.Stop()
	resp.Body = struct {
		io.Reader
		io.Closer
	}{&stallReader{r: resp.Body, timer: stall, idle: getPHPStallTimeout}, resp.Body}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("backend returned %d for M3U request", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, err
	}

	seen, written, err := streamRewriteM3U(resp, destPath,
		c.XtreamUser.String(), c.XtreamPassword.String(),
		c.User.String(), c.Password.String(),
		c.M3UDedupEnabled, dedupPreferredLanguages(c.M3UDedupPreferredLangs),
		c.catalogFilter)
	if err != nil {
		return seen, err
	}
	if (c.M3UDedupEnabled || c.catalogFilter != nil) && written < seen {
		utils.InfoLog("M3U filter/dedup: %d tracks -> %d kept (%d removed)", seen, written, seen-written)
	}
	if written == 0 {
		return written, fmt.Errorf("empty playlist returned by Xtream backend")
	}
	utils.InfoLog("Streamed and rewrote M3U: %d tracks -> %s", written, destPath)
	return written, nil
}
