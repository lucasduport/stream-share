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

package server

import (
	"bufio"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

var (
	channelIndexMu    sync.RWMutex
	channelIndex      map[string]string
	channelIndexPath  string
	channelIndexMTime time.Time
)

// normalizeStreamID trims common file extensions from the last path segment
func normalizeStreamID(id string) string {
	if i := strings.Index(id, "."); i > 0 {
		return id[:i]
	}
	return id
}

// ensureChannelIndex parses c.proxyfiedM3UPath and builds/refreshes the channelIndex cache
func (c *Config) ensureChannelIndex() {
	m3uPath := c.proxyfiedM3UPath
	if strings.TrimSpace(m3uPath) == "" {
		return
	}
	info, err := os.Stat(m3uPath)
	if err != nil {
		return
	}

	// Fast path: unchanged
	channelIndexMu.RLock()
	same := channelIndex != nil && channelIndexPath == m3uPath && channelIndexMTime.Equal(info.ModTime())
	channelIndexMu.RUnlock()
	if same {
		return
	}

	// Rebuild under write lock (double-check inside)
	channelIndexMu.Lock()
	defer channelIndexMu.Unlock()

	if channelIndex != nil && channelIndexPath == m3uPath && channelIndexMTime.Equal(info.ModTime()) {
		return
	}

	f, err := os.Open(m3uPath)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lastTitle := ""
	newIndex := make(map[string]string, 4096)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if idx := strings.LastIndex(line, ","); idx != -1 && idx+1 < len(line) {
				lastTitle = strings.TrimSpace(line[idx+1:])
			} else {
				lastTitle = ""
			}
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			u, err := url.Parse(line)
			if err == nil {
				base := path.Base(u.Path)
				id := normalizeStreamID(base)
				if lastTitle != "" {
					newIndex[id] = lastTitle
				}
			}
			lastTitle = ""
		}
	}
	// best-effort index

	channelIndex = newIndex
	channelIndexPath = m3uPath
	channelIndexMTime = info.ModTime()
}

// getChannelNameByID returns the channel name for a given stream ID if known
func (c *Config) getChannelNameByID(streamID string) (string, bool) {
	c.ensureChannelIndex()
	channelIndexMu.RLock()
	defer channelIndexMu.RUnlock()
	if channelIndex == nil {
		return "", false
	}
	name, ok := channelIndex[normalizeStreamID(streamID)]
	return name, ok
}
