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
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// streamIndexEntry maps a normalized stream ID to its extension and display
// title, as found in an M3U playlist.
type streamIndexEntry struct {
	Ext   string
	Title string
}

// m3uStreamIndex is a per-file in-memory index replacing per-request linear
// scans of multi-hundred-MB M3U files. It is built once when the file changes
// (keyed on path + mtime) and serves O(1) lookups for extension and title.
//
// Keys are basePath+"\x00"+streamID (e.g. "movie\x00012345"), matching the
// semantics of the legacy findExtInM3U/findTitleInM3U scanners.
var (
	m3uStreamIndexMu sync.RWMutex
	m3uStreamIndex   = map[string]*streamIndexState{} // filePath -> state
)

type streamIndexState struct {
	mtime time.Time
	index map[string]streamIndexEntry
}

// buildStreamIndex scans an M3U file once and returns a map of
// basePath+"\x00"+streamID -> {ext, title}. It is a single linear pass with
// O(1) map inserts, replacing the previous per-request O(file) scans.
func buildStreamIndex(r io.Reader) map[string]streamIndexEntry {
	sc := bufio.NewScanner(r)
	// M3U lines can be long (URLs with query strings); raise the buffer.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	idx := make(map[string]streamIndexEntry, 4096)
	lastExtinf := ""

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF") {
			if i := strings.LastIndex(line, ","); i != -1 && i+1 < len(line) {
				lastExtinf = strings.TrimSpace(line[i+1:])
			} else {
				lastExtinf = ""
			}
			continue
		}
		if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			continue
		}
		// Determine the basePath segment: /movie/, /series/, /live/, etc.
		basePath := extractBasePath(u.Path)
		last := path.Base(u.Path)
		id := last
		ext := path.Ext(last)
		if ext != "" {
			id = strings.TrimSuffix(last, ext)
		}
		if id == "" {
			continue
		}
		key := basePath + "\x00" + id
		if _, exists := idx[key]; !exists {
			idx[key] = streamIndexEntry{Ext: ext, Title: lastExtinf}
		}
		lastExtinf = ""
	}
	return idx
}

// extractBasePath returns the stream-type segment from a URL path, e.g.
// "/movie/user/pass/12345.mp4" -> "movie". Returns "" when no known segment
// is present.
func extractBasePath(urlPath string) string {
	segments := strings.Split(strings.Trim(urlPath, "/"), "/")
	for _, seg := range segments {
		switch seg {
		case "movie", "series", "live", "timeshift":
			return seg
		}
	}
	return ""
}

// getStreamIndex returns the cached index for filePath, rebuilding it when the
// file's mtime has changed. Returns nil when the file cannot be stat'd or read.
func getStreamIndex(filePath string) map[string]streamIndexEntry {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil
	}

	m3uStreamIndexMu.RLock()
	st, ok := m3uStreamIndex[filePath]
	m3uStreamIndexMu.RUnlock()
	if ok && st.mtime.Equal(info.ModTime()) {
		return st.index
	}

	// Rebuild under write lock (double-check inside).
	m3uStreamIndexMu.Lock()
	defer m3uStreamIndexMu.Unlock()
	if st, ok := m3uStreamIndex[filePath]; ok && st.mtime.Equal(info.ModTime()) {
		return st.index
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	idx := buildStreamIndex(f)
	m3uStreamIndex[filePath] = &streamIndexState{mtime: info.ModTime(), index: idx}
	utils.DebugLog("stream index: built %d entries for %s", len(idx), filePath)
	return idx
}

// lookupStreamIndex returns the extension and title for a given basePath +
// streamID from the indexed M3U file. O(1) map lookup, no file scan.
func lookupStreamIndex(filePath, basePath, streamID string) (ext, title string, found bool) {
	idx := getStreamIndex(filePath)
	if idx == nil {
		return "", "", false
	}
	key := basePath + "\x00" + streamID
	e, ok := idx[key]
	if !ok {
		return "", "", false
	}
	return e.Ext, e.Title, true
}
