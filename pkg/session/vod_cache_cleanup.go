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

package session

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// vodCacheContains reports whether path is safely inside the VOD cache directory,
// guarding every deletion against a stray absolute path in a DB row.
func vodCacheContains(cacheDir, path string) bool {
	return strings.HasPrefix(filepath.Clean(path), cacheDir+string(os.PathSeparator))
}

// removeVODFile deletes a cached VOD media file and its sibling ".part" (left by
// an interrupted download), but only when they live inside the cache directory.
func removeVODFile(cacheDir, path string) {
	if path == "" {
		return
	}
	if !vodCacheContains(cacheDir, path) {
		utils.WarnLog("Refusing to delete out-of-cache-dir path: %s", path)
		return
	}
	for _, p := range []string{path, path + ".part"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Could not delete VOD file %s: %v", p, err)
		}
	}
}

// cleanupExpiredVODFiles removes entries whose expires_at has passed, deleting
// the file (and its .part sibling) before the DB row so expiry never orphans a
// file. Unlike the stale sweep this spans every status, so an expired failed or
// in-progress entry is cleaned up too.
func (sm *SessionManager) cleanupExpiredVODFiles() {
	cacheDir := filepath.Clean(utils.VODCacheDir())
	entries, err := sm.db.GetExpiredVODCache()
	if err != nil {
		utils.ErrorLog("Failed to query expired VOD cache: %v", err)
		return
	}
	for _, e := range entries {
		removeVODFile(cacheDir, e.FilePath)
		if err := sm.db.DeleteVODCacheEntry(e.StreamID); err != nil {
			utils.ErrorLog("Failed to remove expired VOD cache row for %s: %v", e.StreamID, err)
		}
	}
	if len(entries) > 0 {
		utils.InfoLog("Removed %d expired VOD cache entry(ies)", len(entries))
	}
}

// cleanupStaleVODFiles deletes cached VOD files (and their DB rows) that have
// not been accessed within vodCacheStaleAge. In-progress downloads are skipped.
func (sm *SessionManager) cleanupStaleVODFiles() {
	cacheDir := filepath.Clean(utils.VODCacheDir())

	threshold := time.Now().Add(-sm.vodCacheStaleAge)
	entries, err := sm.db.GetStaleVODCache(threshold)
	if err != nil {
		utils.ErrorLog("Failed to query stale VOD cache: %v", err)
		return
	}
	for _, e := range entries {
		removeVODFile(cacheDir, e.FilePath)
		if err := sm.db.DeleteVODCacheEntry(e.StreamID); err != nil {
			utils.ErrorLog("Failed to remove stale VOD cache row for %s: %v", e.StreamID, err)
		} else {
			utils.InfoLog("Removed stale VOD cache entry %s (last accessed %s ago)", e.StreamID, utils.HumanDuration(time.Since(e.LastAccess)))
		}
	}
}

// reapVODOrphans removes files in the VOD cache directory that no row references,
// plus leftover ".part" files from failed or interrupted downloads. It only
// deletes files idle for at least vodCacheStaleAge: an active download writes its
// ".part" continuously, so the mtime grace keeps a live transfer safe while a
// dead one eventually ages past the threshold.
func (sm *SessionManager) reapVODOrphans() {
	cacheDir := filepath.Clean(utils.VODCacheDir())
	dirEntries, err := os.ReadDir(cacheDir)
	if err != nil {
		if !os.IsNotExist(err) {
			utils.WarnLog("VOD orphan sweep: cannot read %s: %v", cacheDir, err)
		}
		return
	}
	known, err := sm.db.ListVODCacheFilePaths()
	if err != nil {
		utils.ErrorLog("VOD orphan sweep: cannot list referenced files: %v", err)
		return
	}
	cutoff := time.Now().Add(-sm.vodCacheStaleAge)
	removed := 0
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		full := filepath.Join(cacheDir, de.Name())
		// A finished media file referenced by a row is governed by that row's
		// expiry/staleness, not this sweep. ".part" files are never referenced
		// (rows track the final path), so they always fall through to the age check.
		if !strings.HasSuffix(de.Name(), ".part") {
			if _, ok := known[full]; ok {
				continue
			}
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // too fresh - may be an active download or a just-written file
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("VOD orphan sweep: could not delete %s: %v", full, err)
			continue
		}
		removed++
	}

	if removed > 0 {
		utils.InfoLog("VOD orphan sweep: removed %d unreferenced file(s)", removed)
	}
}
