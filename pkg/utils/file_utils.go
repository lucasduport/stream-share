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
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func WriteResponseToFileWithOverwrite(ctx *gin.Context, resp interface{}, overwrite bool, contentType string, optionalURL ...string) {
	// Define the cache directory
	cacheDir := config.CacheFolder
	if cacheDir == "" {
		// No where to save the files.
		return
	}

	// Ensure the cache directory exists
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		log.Printf("Error creating cache directory: %v", err)
		return
	}

	// Determine which URL to use
	var urlString string
	if len(optionalURL) > 0 && optionalURL[0] != "" {
		urlString = optionalURL[0]
	} else {
		urlString = ctx.Request.URL.String()
	}

	// Determine file extension based on response type
	var extension string
	switch contentType {
	case "application/json":
		extension = ".json"
	case "application/xml", "text/xml":
		extension = ".xml"
	case "text/plain":
		extension = ".txt"
	case "application/x-mpegURL", "application/vnd.apple.mpegurl":
		extension = ".m3u8"
	case "audio/x-mpegurl":
		extension = ".m3u"
	default:
		extension = ".json"
	}

	// Generate filename with correct extension
	filename := filepath.Join(cacheDir, url.QueryEscape(urlString) + extension)

	// Convert the response to a string
	respString := ConvertResponseToString(resp)

	// Check if the file exists
	_, err := os.Stat(filename)
	fileExists := !os.IsNotExist(err)

	if !fileExists || (fileExists && overwrite) {
		// Create or overwrite the file
		file, err := os.Create(filename)
		if err != nil {
			log.Printf("Error creating/opening file: %v", err)
			return
		}
		defer file.Close()

		if _, err := file.WriteString(respString); err != nil {
			log.Printf("Error writing to file: %v", err)
		} else {
			if fileExists {
				DebugLog("File overwritten: %s", filename)
			} else {
				DebugLog("Response written to new file: %s", filename)
			}
		}
	}
}

// ConvertResponseToString converts an interface response to a string
func ConvertResponseToString(resp interface{}) string {
	var respString string
	switch v := resp.(type) {
	case string:
		respString = v
	case []byte:
		respString = string(v)
	default:
		respString = fmt.Sprintf("%v", v)
	}

	return respString
}