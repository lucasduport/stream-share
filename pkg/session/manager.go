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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
	"strings"

	"github.com/google/uuid"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// SessionManager handles user sessions and stream multiplexing.
// Lock ordering (always acquire in this order to avoid deadlock):
//
//	userLock → streamLock
type SessionManager struct {
	userSessions     map[string]*types.UserSession     // username -> session
	streamSessions   map[string]*types.StreamSession   // streamID -> session
	streamBuffers    map[string]*StreamBuffer          // streamID -> buffer
	db               *database.DBManager
	tempLinks        map[string]*types.TemporaryLink   // token -> temp link
	userLock         sync.RWMutex
	streamLock       sync.RWMutex
	tempLinkLock     sync.RWMutex
	cleanupInterval  time.Duration
	sessionTimeout   time.Duration
	streamTimeout    time.Duration
	tempLinkTimeout  time.Duration
	httpClient       *http.Client
	stopChan         chan struct{} // closed by Stop() to terminate background goroutines
}

// StreamBuffer handles buffering and distribution of stream data
type StreamBuffer struct {
	streamID    string
	upstreamURL string
	active      bool

	// Per-client data channels and lifecycle
	clients     map[string]chan []byte
	clientDone  map[string]chan struct{}
	clientsLock sync.RWMutex

	// Stop signal for upstream reader
	stopChan chan struct{}
	stopOnce sync.Once

	// Ring buffer allowing clients to read at their own pace
	ringCap     int
	head        uint64               // next sequence number to write
	ring        [][]byte             // ring storage
	bufMu       sync.Mutex
	cond        *sync.Cond
	clientIndex map[string]uint64 // per-client next sequence to read
}

// NewSessionManager creates a new session manager
func NewSessionManager(db *database.DBManager) *SessionManager {
	manager := &SessionManager{
		userSessions:    make(map[string]*types.UserSession),
		streamSessions:  make(map[string]*types.StreamSession),
		streamBuffers:   make(map[string]*StreamBuffer),
		tempLinks:       make(map[string]*types.TemporaryLink),
		db:              db,
		cleanupInterval: 24 * time.Hour,
		sessionTimeout:  30 * time.Minute,
		streamTimeout:   2 * time.Minute,  // Time after which an unused stream is closed
		tempLinkTimeout: 24 * time.Hour,
		stopChan:        make(chan struct{}),
		httpClient: &http.Client{
			// No global Timeout: long-running streams must not be cut after 60s
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   false, // avoid HTTP/2 flow control stalls with IPTV providers
				DisableCompression:  true,  // avoid gzip on video streams
			},
		},
	}

	// Start cleanup routine (stopped by Stop())
	go manager.cleanupRoutine()

	return manager
}

// Stop terminates all background goroutines started by the session manager.
func (sm *SessionManager) Stop() {
	close(sm.stopChan)
}

// cleanupRoutine periodically removes expired sessions and links.
// It exits when sm.stopChan is closed.
func (sm *SessionManager) cleanupRoutine() {
	ticker := time.NewTicker(sm.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sm.stopChan:
			return
		case <-ticker.C:
		}
		sm.cleanupExpiredSessions()
		sm.cleanupUnusedStreams()
		
		// Also clean up expired temporary links in the database
		if sm.db != nil {
			if count, err := sm.db.CleanupExpiredLinks(); err != nil {
				utils.ErrorLog("Failed to clean expired links: %v", err)
			} else if count > 0 {
				utils.InfoLog("Cleaned %d expired temporary links", count)
			}
		}
	}
}

// cleanupExpiredSessions removes inactive user sessions
func (sm *SessionManager) cleanupExpiredSessions() {
	threshold := time.Now().Add(-sm.sessionTimeout)
	
	sm.userLock.Lock()
	defer sm.userLock.Unlock()
	
	for username, session := range sm.userSessions {
		if session.LastActive.Before(threshold) {
			utils.InfoLog("Session expired for user %s (inactive since %v)",
				username, session.LastActive)
				
			// If user was watching a stream, remove from viewers
			if session.StreamID != "" {
				sm.streamLock.Lock()
				if streamSession, exists := sm.streamSessions[session.StreamID]; exists {
					if !streamSession.RemoveViewer(username) && streamSession.Active {
						// No more viewers, stop the stream
						sm.stopStream(session.StreamID)
					}
				}
				sm.streamLock.Unlock()
			}
			
			delete(sm.userSessions, username)
		}
	}
}

// cleanupUnusedStreams stops streams that have no viewers
func (sm *SessionManager) cleanupUnusedStreams() {
	threshold := time.Now().Add(-sm.streamTimeout)
	
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	
	for streamID, session := range sm.streamSessions {
		if session.LastRequested.Before(threshold) && session.Active {
			utils.InfoLog("Stream %s has been inactive for %v, stopping",
				streamID, sm.streamTimeout)
			sm.stopStream(streamID)
		}
	}
}

// RegisterUser creates or updates a user session
func (sm *SessionManager) RegisterUser(username, ip, userAgent string) *types.UserSession {
	sm.userLock.Lock()
	defer sm.userLock.Unlock()
	
	now := time.Now()
	
	// Check if user already has a session
	if session, exists := sm.userSessions[username]; exists {
		session.LastActive = now
		session.IPAddress = ip
		session.UserAgent = userAgent
		return session
	}
	
	// Create new session
	session := &types.UserSession{
		Username:   username,
		StartTime:  now,
		LastActive: now,
		IPAddress:  ip,
		UserAgent:  userAgent,
	}
	
	sm.userSessions[username] = session
	
	// Try to get Discord info if available
	if sm.db != nil {
		discordID, discordName, err := sm.db.GetDiscordByLDAPUser(username)
		if err == nil {
			session.DiscordID = discordID
			session.DiscordName = discordName
			utils.DebugLog("Linked Discord account %s to user %s", discordName, username)
		}
	}
	
	utils.InfoLog("New session registered for user %s from %s", username, ip)
	return session
}

// GetUserSession retrieves a user session if it exists
func (sm *SessionManager) GetUserSession(username string) *types.UserSession {
	// Use Lock (not RLock) because we mutate LastActive below.
	sm.userLock.Lock()
	defer sm.userLock.Unlock()

	session, exists := sm.userSessions[username]
	if !exists {
		return nil
	}

	session.LastActive = time.Now()
	return session
}

// RequestStream handles a new stream request and implements connection multiplexing
func (sm *SessionManager) RequestStream(username, streamID, streamType, streamTitle string,
	upstreamURL *url.URL) (*StreamBuffer, error) {

	// Get user session, creating if necessary
	var userSession *types.UserSession
	sm.userLock.Lock()
	if session, exists := sm.userSessions[username]; exists {
		userSession = session
	} else {
		userSession = &types.UserSession{
			Username:   username,
			StartTime:  time.Now(),
			LastActive: time.Now(),
		}
		sm.userSessions[username] = userSession
	}
	
	// Update user session with stream info
	prevStreamID := userSession.StreamID
	userSession.StreamID = streamID
	userSession.StreamType = streamType
	userSession.LastActive = time.Now()
	sm.userLock.Unlock()
	
	// Handle case where user switches streams
	if prevStreamID != "" && prevStreamID != streamID {
		sm.streamLock.Lock()
		if prevStream, exists := sm.streamSessions[prevStreamID]; exists {
			if !prevStream.RemoveViewer(username) && prevStream.Active {
				// If no more viewers, stop the previous stream
				sm.stopStream(prevStreamID)
			}
		}
		sm.streamLock.Unlock()
	}
	
	// Check if this stream is already active
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	var streamBuffer *StreamBuffer

	// If this stream already exists, add the user as a viewer and start a per-client reader
	if existingBuffer, exists := sm.streamBuffers[streamID]; exists && existingBuffer.active {
		utils.InfoLog("User %s joined existing stream %s", username, streamID)

		if streamSession, exists := sm.streamSessions[streamID]; exists {
			streamSession.AddViewer(username)
			streamSession.LastRequested = time.Now()
		}

		// Add user as a client
		clientChan := make(chan []byte, 256) // larger buffer to smooth jitter
		existingBuffer.clientsLock.Lock()
		if existingBuffer.clientDone == nil {
			existingBuffer.clientDone = make(map[string]chan struct{})
		}
		existingBuffer.clients[username] = clientChan
		existingBuffer.clientDone[username] = make(chan struct{})
		// Start client goroutine at current head
		existingBuffer.bufMu.Lock()
		if existingBuffer.clientIndex == nil {
			existingBuffer.clientIndex = make(map[string]uint64)
		}
		existingBuffer.clientIndex[username] = existingBuffer.head
		existingBuffer.bufMu.Unlock()
		existingBuffer.clientsLock.Unlock()

		go sm.serveClient(existingBuffer, username)

		return existingBuffer, nil
	}

	// Create a new stream session
	streamSession := &types.StreamSession{
		StreamID:      streamID,
		StreamType:    streamType,
		StreamTitle:   streamTitle,
		UpstreamURL:   upstreamURL.String(),
		StartTime:     time.Now(),
		LastRequested: time.Now(),
		Viewers:       make(map[string]time.Time),
		Active:        true,
	}
	streamSession.AddViewer(username)
	sm.streamSessions[streamID] = streamSession

	// Create a new stream buffer
	streamBuffer = &StreamBuffer{
		streamID:    streamID,
		upstreamURL: upstreamURL.String(),
		active:      true,
		clients:     make(map[string]chan []byte),
		clientDone:  make(map[string]chan struct{}),
		stopChan:    make(chan struct{}),
		ringCap:     256,                         // last 256 chunks retained
		ring:        make([][]byte, 256),         // preallocate
		clientIndex: make(map[string]uint64),
	}
	streamBuffer.cond = sync.NewCond(&streamBuffer.bufMu)

	// Add the requesting user as the first client
	clientChan := make(chan []byte, 256)
	streamBuffer.clients[username] = clientChan
	streamBuffer.clientDone[username] = make(chan struct{})
	streamBuffer.clientIndex[username] = 0 // will follow head as it grows

	sm.streamBuffers[streamID] = streamBuffer

	// Start the upstream reader goroutine
	go sm.streamToClients(streamBuffer, upstreamURL)
	// Start the per-client reader
	go sm.serveClient(streamBuffer, username)

	// Record in database
	if sm.db != nil {
		_, err := sm.db.AddStreamHistory(
			username, streamID, streamType, streamTitle,
			userSession.IPAddress, userSession.UserAgent,
		)
		if err != nil {
			utils.ErrorLog("Failed to record stream history: %v", err)
		}
	}

	utils.InfoLog("Started new stream %s for user %s", streamID, username)
	return streamBuffer, nil
}

// serveClient reads from the ring buffer and sends to a specific client's channel
func (sm *SessionManager) serveClient(buffer *StreamBuffer, username string) {
	ch := func() chan []byte {
		buffer.clientsLock.RLock()
		defer buffer.clientsLock.RUnlock()
		return buffer.clients[username]
	}()
	done := func() chan struct{} {
		buffer.clientsLock.RLock()
		defer buffer.clientsLock.RUnlock()
		return buffer.clientDone[username]
	}()

	var next uint64
	buffer.bufMu.Lock()
	next = buffer.clientIndex[username]
	buffer.bufMu.Unlock()

	for {
		// Wait for data availability or done
		buffer.bufMu.Lock()
		for next == buffer.head && buffer.active {
			buffer.cond.Wait()
		}
		if !buffer.active {
			buffer.bufMu.Unlock()
			break
		}
		// Handle overflow: if ring wrapped and client is too far behind, fast-forward
		if buffer.head > uint64(buffer.ringCap) && next < buffer.head-uint64(buffer.ringCap) {
			next = buffer.head - uint64(buffer.ringCap)
		}
		chunk := buffer.ring[next%uint64(buffer.ringCap)]
		next++
		buffer.clientIndex[username] = next
		buffer.bufMu.Unlock()

		// Check if client asked to stop
		select {
		case <-done:
			goto EXIT
		default:
		}

		// Deliver chunk (block if client is slow; independent from other clients)
		out := ch
		if out == nil {
			goto EXIT
		}
		select {
		case out <- chunk:
			// ok
		case <-done:
			goto EXIT
		}
	}

EXIT:
	// Close the outgoing data channel to signal the HTTP writer to finish.
	// Use the locally captured `ch` reference rather than a map lookup: RemoveClient
	// deletes the entry from clients without closing, so a map lookup would miss it
	// and the HTTP writer goroutine would block forever.
	buffer.clientsLock.Lock()
	if ch != nil {
		if _, stillInMap := buffer.clients[username]; stillInMap {
			close(ch)
		}
		// Always remove from map regardless; if already removed that is a no-op.
		delete(buffer.clients, username)
	}
	// Clean up done channel entry. Do NOT close it here — it may have already been
	// closed by RemoveClient or stopStream; just remove the map entry.
	delete(buffer.clientDone, username)
	buffer.clientsLock.Unlock()
}

// streamToClients fetches the stream from upstream and fills the ring buffer
func (sm *SessionManager) streamToClients(buffer *StreamBuffer, upstreamURL *url.URL) {
	utils.DebugLog("Starting stream from %s", upstreamURL.String())

	// Create a context that cancels when the stream is stopped
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-buffer.stopChan
		cancel()
	}()

	// Bind the upstream request to the cancelable context
	req, err := http.NewRequestWithContext(ctx, "GET", upstreamURL.String(), nil)
	if err != nil {
		utils.ErrorLog("Failed to create request: %v", err)
		return
	}

	// Set headers; for VOD/series use a strict whitelist header set
	isVOD := strings.Contains(upstreamURL.Path, "/movie/") || strings.Contains(upstreamURL.Path, "/series/")
	if isVOD {
		h := http.Header{}
		h.Set("User-Agent", utils.GetIPTVUserAgent())
		h.Set("Accept", "*/*")
	h.Set("Accept-Language", utils.GetLanguageHeader())
		h.Set("Accept-Encoding", "identity")
		h.Set("Connection", "keep-alive")
		h.Set("Range", "bytes=0-")
		req.Header = h
	} else {
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Connection", "keep-alive")
	}

	resp, err := sm.httpClient.Do(req)
	if err != nil {
		utils.ErrorLog("Failed to connect to upstream: %v", err)
		sm.stopStream(buffer.streamID)
		return
	}
	defer resp.Body.Close()

	// Check if response is successful. For VOD with Range requests, 206 is valid.
	if isVOD {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			utils.ErrorLog("Upstream returned status %d for VOD stream %s",
				resp.StatusCode, buffer.streamID)
			sm.stopStream(buffer.streamID)
			return
		}
	} else {
		if resp.StatusCode != http.StatusOK {
			utils.ErrorLog("Upstream returned status %d for stream %s",
				resp.StatusCode, buffer.streamID)
			sm.stopStream(buffer.streamID)
			return
		}
	}

	// Stream data into ring buffer
	buffer.active = true

	const chunkSize = 128 * 1024 // was 64KB; larger chunks reduce per-write overhead
	dataBuffer := make([]byte, chunkSize)

	for {
		// Stop requested
		select {
		case <-buffer.stopChan:
			utils.DebugLog("Stream %s stopped", buffer.streamID)
			return
		default:
		}

		n, rerr := resp.Body.Read(dataBuffer)
		if rerr != nil {
			if rerr != io.EOF && ctx.Err() == nil {
				utils.ErrorLog("Error reading from upstream: %v", rerr)
			}
			sm.stopStream(buffer.streamID)
			return
		}
		if n <= 0 {
			continue
		}

		// Copy to ring buffer
		chunk := make([]byte, n)
		copy(chunk, dataBuffer[:n])

		// Append to ring and notify clients
		buffer.bufMu.Lock()
		buffer.ring[buffer.head%uint64(buffer.ringCap)] = chunk
		buffer.head++
		buffer.bufMu.Unlock()
		buffer.cond.Broadcast()

		// Touch stream LastRequested to avoid cleanup timeout while data flows
		sm.streamLock.Lock()
		if ss, ok := sm.streamSessions[buffer.streamID]; ok {
			ss.LastRequested = time.Now()
		}
		sm.streamLock.Unlock()
	}
}

// GetClientChannel retrieves the data channel for a specific client
func (sm *SessionManager) GetClientChannel(streamID, username string) (chan []byte, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return nil, false
	}

	buffer.clientsLock.RLock()
	defer buffer.clientsLock.RUnlock()

	channel, exists := buffer.clients[username]
	return channel, exists
}

// RemoveClient removes a client from a stream
func (sm *SessionManager) RemoveClient(streamID, username string) {
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	// Update user session
	sm.userLock.Lock()
	if userSession, exists := sm.userSessions[username]; exists && userSession.StreamID == streamID {
		userSession.StreamID = ""
		userSession.StreamType = ""
	}
	sm.userLock.Unlock()

	// Signal client goroutine to stop; it will close the data channel
	buffer, exists := sm.streamBuffers[streamID]
	if !exists {
		return
	}

	buffer.clientsLock.Lock()
	if d, ok := buffer.clientDone[username]; ok {
		close(d)
		delete(buffer.clientDone, username)
	}
	// don’t close buffer.clients[username] here; goroutine closes it
	delete(buffer.clients, username)
	buffer.clientsLock.Unlock()

	// Remove from stream session and stop the stream if last viewer
	streamSession, exists := sm.streamSessions[streamID]
	if !exists {
		return
	}
	if !streamSession.RemoveViewer(username) && buffer.active {
		sm.stopStream(streamID)
	}

	utils.InfoLog("User %s removed from stream %s", username, streamID)
}

// stopStream stops an active stream
func (sm *SessionManager) stopStream(streamID string) {
	utils.InfoLog("Stopping stream %s", streamID)

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return
	}

	// Signal upstream goroutine to stop (Once prevents double-close panic)
	buffer.stopOnce.Do(func() { close(buffer.stopChan) })
	buffer.active = false

	// Signal all clients to stop; each goroutine closes its data channel
	buffer.clientsLock.Lock()
	for username, d := range buffer.clientDone {
		close(d)
		delete(buffer.clientDone, username)
	}
	buffer.clients = make(map[string]chan []byte)
	buffer.clientsLock.Unlock()

	// Update the stream session
	if streamSession, exists := sm.streamSessions[streamID]; exists {
		streamSession.Active = false
	}

	utils.InfoLog("Stream %s stopped and all clients disconnected", streamID)
}

// GenerateTemporaryLink creates a temporary download link
func (sm *SessionManager) GenerateTemporaryLink(username, streamID, title, rawURL string) (string, error) {
	token := uuid.New().String()
	expiresAt := time.Now().Add(sm.tempLinkTimeout)
	
	tempLink := &types.TemporaryLink{
		Token:     token,
		Username:  username,
		URL:       rawURL,
		ExpiresAt: expiresAt,
		StreamID:  streamID,
		Title:     title,
	}
	
	// Store in memory
	sm.tempLinkLock.Lock()
	sm.tempLinks[token] = tempLink
	sm.tempLinkLock.Unlock()
	
	// Store in database if available
	if sm.db != nil {
		if err := sm.db.CreateTemporaryLink(token, username, rawURL, streamID, title, expiresAt); err != nil {
			utils.ErrorLog("Failed to store temporary link in database: %v", err)
		}
	}
	
	utils.InfoLog("Generated temporary link for user %s, expires at %v", username, expiresAt)
	return token, nil
}

// GetTemporaryLink retrieves a temporary link by token
func (sm *SessionManager) GetTemporaryLink(token string) (*types.TemporaryLink, error) {
	// First check in memory
	sm.tempLinkLock.RLock()
	tempLink, exists := sm.tempLinks[token]
	sm.tempLinkLock.RUnlock()
	
	if exists && time.Now().Before(tempLink.ExpiresAt) {
		return tempLink, nil
	}
	
	// If not in memory or expired, try the database
	if sm.db != nil {
		return sm.db.GetTemporaryLink(token)
	}
	
	return nil, fmt.Errorf("temporary link not found or expired")
}

// GetAllSessions returns all current user sessions
func (sm *SessionManager) GetAllSessions() []*types.UserSession {
	sm.userLock.RLock()
	defer sm.userLock.RUnlock()
	
	sessions := make([]*types.UserSession, 0, len(sm.userSessions))
	for _, session := range sm.userSessions {
		sessions = append(sessions, session)
	}
	
	return sessions
}

// GetAllStreams returns all active stream sessions
func (sm *SessionManager) GetAllStreams() []*types.StreamSession {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()
	
	streams := make([]*types.StreamSession, 0, len(sm.streamSessions))
	for _, stream := range sm.streamSessions {
		if stream.Active {
			streams = append(streams, stream)
		}
	}
	
	return streams
}

// DisconnectUser forcibly disconnects all streams for a user
func (sm *SessionManager) DisconnectUser(username string) {
	sm.userLock.Lock()
	userSession, exists := sm.userSessions[username]
	if !exists {
		sm.userLock.Unlock()
		return
	}
	
	streamID := userSession.StreamID
	userSession.StreamID = ""
	userSession.StreamType = ""
	sm.userLock.Unlock()
	
	// If user was watching a stream, remove them
	if streamID != "" {
		sm.RemoveClient(streamID, username)
	}
	
	utils.InfoLog("User %s forcibly disconnected", username)
}

// RegisterVODView creates a synthetic stream session so status commands see users watching local files.
func (sm *SessionManager) RegisterVODView(username, streamID, streamType, title string) {
	sm.userLock.Lock()
	if sess, exists := sm.userSessions[username]; exists {
		sess.StreamID = streamID
		sess.StreamType = streamType
		sess.LastActive = time.Now()
	}
	sm.userLock.Unlock()

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	if ss, exists := sm.streamSessions[streamID]; exists {
		ss.AddViewer(username)
		ss.LastRequested = time.Now()
	} else {
		ss := &types.StreamSession{
			StreamID: streamID, StreamType: streamType, StreamTitle: title,
			StartTime: time.Now(), LastRequested: time.Now(),
			Viewers: make(map[string]time.Time), Active: true,
		}
		ss.AddViewer(username)
		sm.streamSessions[streamID] = ss
	}
}

// UnregisterVODView removes a user from a synthetic VOD viewing session.
func (sm *SessionManager) UnregisterVODView(username, streamID string) {
	sm.userLock.Lock()
	if sess, exists := sm.userSessions[username]; exists && sess.StreamID == streamID {
		sess.StreamID = ""
		sess.StreamType = ""
	}
	sm.userLock.Unlock()

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	if ss, exists := sm.streamSessions[streamID]; exists {
		if !ss.RemoveViewer(username) {
			ss.Active = false
			delete(sm.streamSessions, streamID)
		}
	}
}

// GetStreamInfo gets information about a specific stream
func (sm *SessionManager) GetStreamInfo(streamID string) (*types.StreamSession, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()
	
	session, exists := sm.streamSessions[streamID]
	return session, exists
}

// SetSessionTimeout sets the user session timeout duration
func (sm *SessionManager) SetSessionTimeout(timeout time.Duration) {
	sm.sessionTimeout = timeout
}

// SetStreamTimeout sets the unused stream timeout duration
func (sm *SessionManager) SetStreamTimeout(timeout time.Duration) {
	sm.streamTimeout = timeout
}

// SetTempLinkTimeout sets the temporary link expiration duration
func (sm *SessionManager) SetTempLinkTimeout(timeout time.Duration) {
	sm.tempLinkTimeout = timeout
}
