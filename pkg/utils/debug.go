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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	// DebugLoggingEnabled controls whether debug logs are printed
	DebugLoggingEnabled = false
)

// IsDebugLogEnabled returns whether debug logging is enabled
func IsDebugLogEnabled() bool {
	return os.Getenv("DEBUG_LOGGING") == "true"
}

// HexDump creates a hex dump of the given data for debugging purposes
func HexDump(data []byte, maxBytes int) string {
	if len(data) == 0 {
		return "[empty]"
	}

	// Limit to maxBytes
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}

	var result string
	result = fmt.Sprintf("Hex dump of %d bytes:\n", len(data))

	for i := 0; i < len(data); i += 16 {
		// Print offset
		result += fmt.Sprintf("%04x: ", i)

		// Print hex representation
		hexPart := ""
		for j := 0; j < 16; j++ {
			if i+j < len(data) {
				hexPart += fmt.Sprintf("%02x ", data[i+j])
			} else {
				hexPart += "   " // 3 spaces to align
			}

			// Extra space after 8 bytes
			if j == 7 {
				hexPart += " "
			}
		}
		result += hexPart

		// Print ASCII representation
		result += "  |"
		for j := 0; j < 16; j++ {
			if i+j < len(data) {
				b := data[i+j]
				if b >= 32 && b <= 126 { // Printable ASCII
					result += string(b)
				} else {
					result += "." // Non-printable
				}
			} else {
				result += " " // Padding
			}
		}
		result += "|\n"
	}

	return result
}

// PrettyPrintJSON returns a nicely formatted JSON string for debugging
func PrettyPrintJSON(data interface{}) string {
	if data == nil {
		return "null"
	}

	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error marshaling JSON: %v", err)
	}

	return string(jsonBytes)
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

// SaveRawResponse saves a raw API response to a file for debugging purposes
// Returns the path to the saved file or empty string if the operation failed
func SaveRawResponse(action string, data []byte) string {
	// Skip if debug logging is disabled
	if !DebugLoggingEnabled {
		return ""
	}

	// Create debug directory under system temp
	debugDir := filepath.Join(os.TempDir(), "stream-share-debug")
	if err := os.MkdirAll(debugDir, 0755); err != nil {
		ErrorLog("Failed to create debug directory: %v", err)
		return ""
	}

	// Format filename with action and timestamp
	timestamp := time.Now().Format("20060102_150405")
	actionName := action
	if actionName == "" {
		actionName = "login"
	}
	filename := filepath.Join(debugDir, fmt.Sprintf("%s_%s.json", actionName, timestamp))

	// Write data to file
	if err := os.WriteFile(filename, data, 0644); err != nil {
		ErrorLog("Failed to save debug data: %v", err)
		return ""
	}

	// For JSON data, save a prettified version for easier inspection
	var prettyData interface{}
	if json.Unmarshal(data, &prettyData) == nil {
		prettyBytes, err := json.MarshalIndent(prettyData, "", "  ")
		if err == nil {
			prettyFile := filename + ".pretty.json"
			_ = os.WriteFile(prettyFile, prettyBytes, 0644)
		}
	}

	return filename
}

// DumpStructToLog dumps the content of a struct to the debug log
func DumpStructToLog(prefix string, v interface{}) {
	if !DebugLoggingEnabled {
		return
	}

	// Marshal to JSON for easy inspection
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		DebugLog("%s: [error marshaling: %v]", prefix, err)
		return
	}

	// Log the first part (limited to avoid excessive logging)
	maxLen := 500
	strData := string(data)
	if len(strData) > maxLen {
		DebugLog("%s: %s... [truncated, full data in debug files]", prefix, strData[:maxLen])
	} else {
		DebugLog("%s: %s", prefix, strData)
	}

	// Also save to file for full inspection
	SaveRawResponse(prefix, data)
}
