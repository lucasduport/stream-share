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

package types

import (
	"sync"
	"time"
)

// UserSession represents an active user session
type UserSession struct {
	Username    string    // LDAP/local username
	DiscordID   string    // Linked Discord ID (if available)
	DiscordName string    // Discord username for display
	StreamID    string    // Current stream ID
	StreamType  string    // "live", "vod", "series"
	StartTime   time.Time // Session start time
	LastActive  time.Time // Last activity time
	IPAddress   string    // User's IP address
	UserAgent   string    // User's device/agent
}

// StreamSession represents a shared stream with multiple viewers
type StreamSession struct {
	StreamID      string               // Stream identifier
	StreamType    string               // Type of content (live, vod, series)
	StreamTitle   string               // Content title
	ProxyURL      string               // Internal proxy URL
	UpstreamURL   string               // Upstream provider URL
	StartTime     time.Time            // When the stream started
	LastRequested time.Time            // Last time any user requested this stream
	Viewers       map[string]time.Time // Map of usernames to their last activity time
	Active        bool                 // Whether the stream is currently active
	lock          sync.RWMutex         // Lock for concurrent access
}

// AddViewer adds a viewer to the stream session
func (s *StreamSession) AddViewer(username string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	s.Viewers[username] = time.Now()
	s.LastRequested = time.Now()
}

// RemoveViewer removes a viewer from the stream session
func (s *StreamSession) RemoveViewer(username string) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	
	delete(s.Viewers, username)
	
	// Return whether the stream still has viewers
	return len(s.Viewers) > 0
}

// GetViewers returns a copy of the current viewers map
func (s *StreamSession) GetViewers() map[string]time.Time {
	s.lock.RLock()
	defer s.lock.RUnlock()
	
	// Create a copy of the viewers map to avoid race conditions
	viewers := make(map[string]time.Time, len(s.Viewers))
	for k, v := range s.Viewers {
		viewers[k] = v
	}
	
	return viewers
}

// VODRequest represents a VOD search request and response
type VODRequest struct {
	Username  string
	Query     string
	Results   []VODResult
	CreatedAt time.Time
	ExpiresAt time.Time
	Token     string  // Unique token for this request
}

// VODResult represents a single VOD search result
type VODResult struct {
	ID       string
	Title    string
	Category string
	Duration string
	Year     string
	Rating   string
	StreamID string // The stream ID needed to retrieve this content
	// Optional size information (in bytes and human-friendly)
	SizeBytes int64
	Size      string
	// Type of result: "movie" or "series"
	StreamType string
	// Series metadata when StreamType is "series"
	SeriesTitle   string
	Season        int
	Episode       int
	EpisodeTitle  string
}

// TemporaryLink represents a generated temporary download link
type TemporaryLink struct {
	Token     string
	Username  string
	URL       string
	ExpiresAt time.Time
	StreamID  string
	Title     string
}

// APIResponse is a standardized API response structure
type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// VODCacheEntry tracks cached VOD or series episode stored on disk
type VODCacheEntry struct {
	StreamID    string    `json:"stream_id"`
	Type        string    `json:"type"` // movie or series
	Title       string    `json:"title,omitempty"`
	SeriesTitle string    `json:"series_title,omitempty"`
	Season      int       `json:"season,omitempty"`
	Episode     int       `json:"episode,omitempty"`
	FilePath    string    `json:"file_path"`
	RequestedBy string    `json:"requested_by,omitempty"`
	// Live progress
	DownloadedBytes int64     `json:"downloaded_bytes,omitempty"`
	TotalBytes      int64     `json:"total_bytes,omitempty"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	Status      string    `json:"status"` // downloading, ready, failed
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	LastAccess  time.Time `json:"last_access,omitempty"`
}
