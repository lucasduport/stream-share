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
	"strings"
	"time"

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
// Returns the path of the written cache file and the number of tracks seen.
func streamRewriteM3U(resp *http.Response, destPath, xtreamUser, xtreamPassword, localUser, localPassword string) (int64, error) {
	tmpPath := destPath + ".part"
	f, err := os.Create(tmpPath)
	if err != nil {
		return 0, err
	}

	w := bufio.NewWriterSize(f, 256*1024)
	r := bufio.NewReaderSize(resp.Body, 256*1024)

	var tracks int64
	lineNum := 0

	for {
		line, readErr := r.ReadString('\n')
		if len(line) > 0 {
			lineNum++
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
				tracks++
				rewritten := rewriteM3UURLLine(trimmed, xtreamUser, xtreamPassword, localUser, localPassword)
				if _, werr := w.WriteString(rewritten); werr != nil {
					_ = f.Close()
					_ = os.Remove(tmpPath)
					return tracks, werr
				}
				if line[len(line)-1] == '\n' {
					if _, werr := w.WriteString("\n"); werr != nil {
						_ = f.Close()
						_ = os.Remove(tmpPath)
						return tracks, werr
					}
				}
			} else {
				if _, werr := w.WriteString(line); werr != nil {
					_ = f.Close()
					_ = os.Remove(tmpPath)
					return tracks, werr
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return tracks, readErr
		}
	}

	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return tracks, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return tracks, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return tracks, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return tracks, err
	}
	return tracks, nil
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

	tracks, err := streamRewriteM3U(resp, destPath,
		c.XtreamUser.String(), c.XtreamPassword.String(),
		c.User.String(), c.Password.String())
	if err != nil {
		return tracks, err
	}
	if tracks == 0 {
		return tracks, fmt.Errorf("empty playlist returned by Xtream backend")
	}
	utils.InfoLog("Streamed and rewrote M3U: %d tracks -> %s", tracks, destPath)
	return tracks, nil
}
