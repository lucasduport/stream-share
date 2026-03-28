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
    "os"
    "path/filepath"
    "sync"
    "time"

    "github.com/jamesnetherton/m3u"
    "github.com/lucasduport/stream-share/pkg/utils"
    uuid "github.com/satori/go.uuid"
)

type cacheMeta struct {
    string
    time.Time
}

var xtreamM3uCache map[string]cacheMeta = map[string]cacheMeta{}
var xtreamM3uCacheLock = sync.RWMutex{}

// cacheXtreamM3u stores a generated Xtream playlist to a temp file for reuse.
func (c *Config) cacheXtreamM3u(playlist *m3u.Playlist, cacheName string) error {
    xtreamM3uCacheLock.Lock()
    defer xtreamM3uCacheLock.Unlock()

    // Temporarily swap the playlist to avoid copying a mutex-containing struct.
    origPlaylist := c.playlist
    c.playlist = playlist
    defer func() { c.playlist = origPlaylist }()

    path := filepath.Join(os.TempDir(), uuid.NewV4().String()+".stream-share.m3u")
    f, err := os.Create(path)
    if err != nil {
        return err
    }
    defer f.Close()

    if err := c.marshallInto(f, true); err != nil {
        return err
    }
    xtreamM3uCache[cacheName] = cacheMeta{path, time.Now()}
    utils.DebugLog("Cached Xtream M3U at %s for key %s", path, cacheName)
    return nil
}
