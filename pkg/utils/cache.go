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

package utils

import (
	"os"
	"path/filepath"
	"strings"
)

// Cache layout. A single CACHE_FOLDER env var defines the root; everything else
// lives in a purpose-specific subdirectory underneath it so the different
// cleanup routines never touch each other's files (e.g. catchup's blanket
// *.ts sweep can only ever affect the catchup subfolder).
const (
	cacheRootEnv     = "CACHE_FOLDER"
	cacheRootDefault = "stream-share"
	vodSubdir        = "vod"
	catchupSubdir    = "catchup"
	slateSubdir      = "slate"
)

// CacheRoot returns the root cache directory: CACHE_FOLDER when set, otherwise a
// stable default under the system temp directory.
func CacheRoot() string {
	if dir := strings.TrimSpace(os.Getenv(cacheRootEnv)); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), cacheRootDefault)
}

// VODCacheDir returns the subdirectory holding cached VOD media files.
func VODCacheDir() string {
	return filepath.Join(CacheRoot(), vodSubdir)
}

// CatchupBufferDir returns the subdirectory holding live catchup buffer files.
func CatchupBufferDir() string {
	return filepath.Join(CacheRoot(), catchupSubdir)
}

// SlateCacheDir returns the subdirectory holding generated error-slate clips.
func SlateCacheDir() string {
	return filepath.Join(CacheRoot(), slateSubdir)
}
