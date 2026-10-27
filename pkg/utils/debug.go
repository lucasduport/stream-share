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
)

// IsDebugLogEnabled returns whether debug logging is enabled
func IsDebugLogEnabled() bool {
	return os.Getenv("LOG_DEBUG_ENABLED") == "true"
}

// WriteResponseToFile writes the response to a file for debugging
func WriteResponseToFile(filename string, data []byte, contentType string) {
	if cacheFolder := os.Getenv("CACHE_FOLDER"); cacheFolder != "" {
		_ = os.MkdirAll(cacheFolder, 0755)
		filePath := filepath.Join(cacheFolder, filename)

		err := os.WriteFile(filePath, data, 0644)
		if err != nil {
			ErrorLog("Failed to write response to file: %v", err)
		} else {
			DebugLog("Wrote response to file: %s", filePath)
		}
	}
}
