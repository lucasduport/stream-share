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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/slate"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// SessionManager handles user sessions and stream multiplexing.
// Lock ordering (always acquire in this order to avoid deadlock):
//
//	userLock → streamLock
type SessionManager struct {
	userSessions       map[string]*types.UserSession   // username -> session
	streamSessions     map[string]*types.StreamSession // streamID -> session
	streamBuffers      map[string]*StreamBuffer        // streamID -> buffer
	db                 *database.DBManager
	tempLinks          map[string]*types.TemporaryLink // token -> temp link
	userLock           sync.RWMutex
	streamLock         sync.RWMutex
	tempLinkLock       sync.RWMutex
	cleanupInterval    time.Duration
	sessionTimeout     time.Duration
	streamTimeout      time.Duration
	tempLinkTimeout    time.Duration
	vodCacheStaleAge   time.Duration
	slateCacheDir      string        // dir holding rendered slate clips; "" disables slate pruning
	slateStaleAge      time.Duration // prune slate clips not modified within this; 0 disables
	clientStallTimeout time.Duration // drop a multiplexed client whose buffer stays full this long
	httpClient         *http.Client
	stopChan           chan struct{} // closed by Stop() to terminate background goroutines
	catchupManager     *catchup.Manager
	nameResolver       func(streamID string) (string, bool) // optional channel-name lookup for logs

	// Error slate: when the upstream fails before delivering a byte, live
	// viewers are shown a rendered explanation instead of a dropped connection,
	// while the pump keeps retrying the provider. slateProvider is nil when the
	// feature is disabled or ffmpeg is unavailable.
	slateProvider SlateProvider
	errorCatalog  *Catalog
	slateRetryMax time.Duration

	// pauseGrace controls how long a catchup-enabled live stream keeps its
	// upstream connection (and disk recording) alive after its last viewer
	// disconnects, so a TiviMate "pause" followed by a timeshift-based resume
	// has continuous buffered content with no gap. 0 disables the behavior
	// (streams stop immediately, as before). Guarded by streamLock.
	pauseGrace   time.Duration
	pendingStops map[string]chan struct{} // streamID -> cancel channel for a scheduled stop

	// vodViewTimers holds grace timers that delay tearing down a synthetic VOD
	// view after its last range request. Cached VOD is served via many short
	// Range requests with gaps between them while the player drains its buffer;
	// without a grace window the synthetic session would flicker out of /status
	// between requests. A new request cancels the pending timer. Keyed by
	// streamID + "\x00" + username. Guarded by its own mutex.
	vodViewTimersMu sync.Mutex
	vodViewTimers   map[string]*time.Timer
	// vodHistoryIDs maps an active VOD view (streamID + "\x00" + username) to its
	// open stream_history row id, so the row is written once per viewing session
	// (not once per Range request) and closed when the view ends. Guarded by
	// vodViewTimersMu.
	vodHistoryIDs map[string]int64

	// liveHistoryIDs maps an active live viewer (streamID + "\x00" + username) to
	// its open stream_history row id. A row is opened when a user starts watching
	// a live stream (including multiplexed co-viewers) and closed when that user
	// leaves, so durations reflect actual watch time. Guarded by liveHistoryMu.
	// A sentinel value of 0 means "insert in flight" (see recordLiveHistory).
	liveHistoryMu  sync.Mutex
	liveHistoryIDs map[string]int64
}

// Stream multiplexing tuning.
//
// The upstream is read once and fanned out to every client attached to the same
// stream. Delivery is back-pressured: the pump does not advance to the next
// upstream chunk until every client has accepted the current one (or been
// dropped). With a single client this reproduces a direct proxy — the upstream
// read rate is gated by how fast that client drains, so TCP back-pressure flows
// all the way to the provider and the stream stays smooth. With multiple clients
// the pump runs at the rate of the slowest healthy client; the per-client buffer
// absorbs jitter, and a client that stalls longer than clientStallTimeout is
// dropped so it cannot freeze the shared connection for everyone else.
const (
	streamChunkSize           = 128 * 1024       // upstream read size
	clientBufferChunks        = 32               // per-client jitter buffer (~4MB)
	defaultClientStallTimeout = 30 * time.Second // default for SessionManager.clientStallTimeout
)

// streamClient is a single viewer attached to a StreamBuffer.
type streamClient struct {
	ch       chan []byte   // buffered video chunks awaiting the HTTP writer
	done     chan struct{} // closed once when the client leaves or is dropped
	doneOnce sync.Once
}

// close signals the client to terminate. Safe to call multiple times and from
// either the HTTP side (client disconnected) or the pump (slow-client drop).
func (c *streamClient) close() {
	c.doneOnce.Do(func() { close(c.done) })
}

// StreamBuffer fans a single upstream connection out to multiple clients.
type StreamBuffer struct {
	streamID    string
	upstreamURL string
	active      bool

	// Attached clients, keyed by username.
	clients     map[string]*streamClient
	clientsLock sync.RWMutex

	// Stop signal for the upstream pump.
	stopChan chan struct{}
	stopOnce sync.Once

	// Optional disk buffer for local catchup (nil when catchup is disabled)
	diskBuffer *catchup.DiskBuffer

	// probeTaps are one-shot listeners that sample bytes already flowing through
	// this buffer's upstream pump (e.g. for technical stream-info probing),
	// without opening any additional connection to the provider.
	probeTaps     []*probeTap
	probeTapsLock sync.Mutex

	// ready is closed once the pump knows whether the upstream came up, so the
	// HTTP handler can decide what to send before it commits to a 200. startErr
	// is nil on success and set when the upstream failed before delivering any
	// byte. slateOK records whether this stream type may be replaced by an error
	// slate (live/timeshift only — VOD is served over byte ranges).
	ready      chan struct{}
	readyOnce  sync.Once
	startErrMu sync.Mutex
	startErr   *UpstreamError
	slateOK    bool
}

// markReady records the upstream outcome and releases anyone waiting on Ready.
// Only the first call counts: later transitions (e.g. a mid-stream failure) must
// not retroactively change what the handler already decided.
func (b *StreamBuffer) markReady(err *UpstreamError) {
	b.readyOnce.Do(func() {
		b.startErrMu.Lock()
		b.startErr = err
		b.startErrMu.Unlock()
		close(b.ready)
	})
}

// Ready returns a channel closed once the upstream outcome is known.
func (b *StreamBuffer) Ready() <-chan struct{} { return b.ready }

// StartError returns the upstream failure recorded before any byte was
// delivered, or nil if the stream started successfully.
func (b *StreamBuffer) StartError() *UpstreamError {
	b.startErrMu.Lock()
	defer b.startErrMu.Unlock()
	return b.startErr
}

// probeTap collects up to maxBytes of upstream data fed to it by the pump, then
// closes done. Safe for concurrent feed (from the pump goroutine) and snapshot
// (from whoever requested the sample), even after a timeout races a feed.
type probeTap struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	maxBytes  int
	done      chan struct{}
	closeOnce sync.Once
}

func newProbeTap(maxBytes int) *probeTap {
	return &probeTap{maxBytes: maxBytes, done: make(chan struct{})}
}

func (t *probeTap) feed(chunk []byte) {
	select {
	case <-t.done:
		return
	default:
	}
	t.mu.Lock()
	if t.buf.Len() < t.maxBytes {
		t.buf.Write(chunk)
	}
	full := t.buf.Len() >= t.maxBytes
	t.mu.Unlock()
	if full {
		t.closeOnce.Do(func() { close(t.done) })
	}
}

func (t *probeTap) snapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]byte, t.buf.Len())
	copy(out, t.buf.Bytes())
	return out
}

// NewSessionManager creates a new session manager
func NewSessionManager(db *database.DBManager) *SessionManager {
	manager := &SessionManager{
		userSessions:       make(map[string]*types.UserSession),
		streamSessions:     make(map[string]*types.StreamSession),
		streamBuffers:      make(map[string]*StreamBuffer),
		tempLinks:          make(map[string]*types.TemporaryLink),
		db:                 db,
		cleanupInterval:    24 * time.Hour,
		sessionTimeout:     30 * time.Minute,
		streamTimeout:      2 * time.Minute,
		tempLinkTimeout:    24 * time.Hour,
		vodCacheStaleAge:   24 * time.Hour,
		clientStallTimeout: defaultClientStallTimeout,
		pendingStops:       make(map[string]chan struct{}),
		vodViewTimers:      make(map[string]*time.Timer),
		vodHistoryIDs:      make(map[string]int64),
		liveHistoryIDs:     make(map[string]int64),
		stopChan:           make(chan struct{}),
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

	sm.vodViewTimersMu.Lock()
	for key, t := range sm.vodViewTimers {
		t.Stop()
		delete(sm.vodViewTimers, key)
	}
	sm.vodViewTimersMu.Unlock()
}

// SetCatchupManager attaches a catchup manager for local disk buffering of live streams.
func (sm *SessionManager) SetCatchupManager(m *catchup.Manager) {
	sm.catchupManager = m
}

// SetNameResolver attaches a channel-name lookup used to label streams in logs.
func (sm *SessionManager) SetNameResolver(f func(streamID string) (string, bool)) {
	sm.nameResolver = f
}

// streamLabel formats a stream for logging as "Channel Name (Stream <id>)",
// falling back to "Stream <id>" when no name is known. The id is reported
// without its file extension for readability.
func (sm *SessionManager) streamLabel(streamID string) string {
	id := strings.TrimSuffix(streamID, path.Ext(streamID))
	if sm.nameResolver != nil {
		if name, ok := sm.nameResolver(streamID); ok && strings.TrimSpace(name) != "" {
			return fmt.Sprintf("%s (Stream %s)", strings.TrimSpace(name), id)
		}
	}
	return fmt.Sprintf("Stream %s", id)
}

// sessionSweepInterval controls how often idle sessions and streams are reaped.
// It is deliberately short so a stalled viewer that halts its stream's pump
// releases the shared upstream connection promptly (within streamTimeout of going
// idle), rather than waiting for the daily cache sweep.
const sessionSweepInterval = 30 * time.Second

// cleanupRoutine reaps idle sessions/streams on a short cadence and expired
// links/VOD cache on the (daily) cleanupInterval. It exits when stopChan closes.
func (sm *SessionManager) cleanupRoutine() {
	sessionTicker := time.NewTicker(sessionSweepInterval)
	defer sessionTicker.Stop()
	cacheTicker := time.NewTicker(sm.cleanupInterval)
	defer cacheTicker.Stop()

	// Run cache maintenance shortly after startup as well as on the daily ticker,
	// so a container that restarts more often than once a day is not left with an
	// ever-growing cache. The short delay lets bootstrap finish applying config
	// (stale ages, slate dir) before the first pass.
	initial := time.NewTimer(30 * time.Second)
	defer initial.Stop()

	for {
		select {
		case <-sm.stopChan:
			return
		case <-sessionTicker.C:
			sm.cleanupExpiredSessions()
			sm.cleanupUnusedStreams()
		case <-initial.C:
			sm.runCacheMaintenance()
		case <-cacheTicker.C:
			sm.runCacheMaintenance()
		}
	}
}

// runCacheMaintenance reaps everything that accumulates on disk: expired temp
// links, expired and stale VOD cache files (plus their .part siblings), orphaned
// VOD files no longer referenced by any row, and stale slate clips.
func (sm *SessionManager) runCacheMaintenance() {
	if sm.db != nil {
		if count, err := sm.db.CleanupExpiredLinks(); err != nil {
			utils.ErrorLog("Failed to clean expired links: %v", err)
		} else if count > 0 {
			utils.InfoLog("Cleaned %d expired temporary links", count)
		}
		// Order matters: delete expired/stale entries' files *before* their rows,
		// then reap anything left unreferenced (e.g. failed downloads' .part files).
		sm.cleanupExpiredVODFiles()
		sm.cleanupStaleVODFiles()
		sm.reapVODOrphans()
	}
	if sm.slateStaleAge > 0 && sm.slateCacheDir != "" {
		if n, err := slate.PruneCache(sm.slateCacheDir, sm.slateStaleAge); err != nil {
			utils.WarnLog("Slate cache prune failed: %v", err)
		} else if n > 0 {
			utils.InfoLog("Pruned %d stale slate clip(s)", n)
		}
	}
}

// cleanupExpiredSessions removes inactive user sessions
func (sm *SessionManager) cleanupExpiredSessions() {
	threshold := time.Now().Add(-sm.sessionTimeout)

	// Collect (username, streamID) pairs to close after releasing the locks, so
	// the history DB round-trips don't run while holding userLock.
	type departed struct{ username, streamID string }
	var toClose []departed

	sm.userLock.Lock()
	for username, session := range sm.userSessions {
		if session.LastActive.Before(threshold) {
			utils.InfoLog("Session expired for user %s (inactive for %s)",
				username, utils.HumanDuration(time.Since(session.LastActive)))

			// If user was watching a stream, remove from viewers
			if session.StreamID != "" {
				streamID := session.StreamID
				sm.streamLock.Lock()
				if streamSession, exists := sm.streamSessions[streamID]; exists {
					if !streamSession.RemoveViewer(username) && streamSession.Active {
						// No more viewers, stop the stream
						sm.stopStream(streamID)
					}
				}
				sm.streamLock.Unlock()
				toClose = append(toClose, departed{username, streamID})
			}

			delete(sm.userSessions, username)
		}
	}
	sm.userLock.Unlock()

	// Close each departed viewer's live history row (no-op for VOD, which is
	// closed via its own grace path).
	for _, d := range toClose {
		sm.closeLiveHistory(d.username, d.streamID)
	}
}

// cleanupUnusedStreams stops streams that have no viewers
func (sm *SessionManager) cleanupUnusedStreams() {
	threshold := time.Now().Add(-sm.streamTimeout)

	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	for streamID, session := range sm.streamSessions {
		if session.LastRequested.Before(threshold) && session.Active {
			utils.DebugLog("%s has been inactive for %s, stopping",
				sm.streamLabel(streamID), utils.HumanDuration(time.Since(session.LastRequested)))
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
		// Close the departing viewer's history row for the previous stream.
		sm.closeLiveHistory(username, prevStreamID)
	}

	// Check if this stream is already active
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	var streamBuffer *StreamBuffer

	// If this stream already exists, attach the user as an additional viewer of the
	// shared upstream connection.
	if existingBuffer, exists := sm.streamBuffers[streamID]; exists && existingBuffer.active {
		utils.InfoLog("User %s joined existing %s (multiplexed)", username, sm.streamLabel(streamID))

		// A viewer returned — cancel any pending pause-grace stop.
		if sm.cancelPendingStop(streamID) {
			utils.DebugLog("%s resumed before pause grace expired; continuing uninterrupted", sm.streamLabel(streamID))
		}

		joinTitle := streamType
		if streamSession, exists := sm.streamSessions[streamID]; exists {
			streamSession.AddViewer(username)
			streamSession.LastRequested = time.Now()
			joinTitle = streamSession.StreamTitle
		}
		// Record this co-viewer's own history row (multiplexed joiners were
		// previously never recorded).
		sm.recordLiveHistory(username, streamID, streamType, joinTitle, userSession.IPAddress, userSession.UserAgent)

		existingBuffer.clientsLock.Lock()
		// If the user already has a client attached (reconnect), drop the old one so
		// its HTTP handler exits cleanly before we install the replacement.
		if old, alreadyClient := existingBuffer.clients[username]; alreadyClient {
			old.close()
			delete(existingBuffer.clients, username)
			utils.DebugLog("User %s reconnected to %s; replaced stale client", username, sm.streamLabel(streamID))
		}
		existingBuffer.clients[username] = newStreamClient()
		existingBuffer.clientsLock.Unlock()

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

	// Create a new stream buffer with the requesting user as the first client
	streamBuffer = &StreamBuffer{
		streamID:    streamID,
		upstreamURL: upstreamURL.String(),
		active:      true,
		clients:     map[string]*streamClient{username: newStreamClient()},
		stopChan:    make(chan struct{}),
		ready:       make(chan struct{}),
		slateOK:     slateEligible(streamType),
	}

	// Start local disk buffer for live streams when catchup is enabled
	if sm.catchupManager != nil && sm.catchupManager.IsEnabled() && streamType == "live" {
		bareID := strings.TrimSuffix(path.Base(upstreamURL.Path), path.Ext(upstreamURL.Path))
		streamBuffer.diskBuffer = sm.catchupManager.StartBuffer(bareID)
	}

	sm.streamBuffers[streamID] = streamBuffer

	// Start the single upstream pump that fans out to all clients
	go sm.streamToClients(streamBuffer, upstreamURL)

	// Record this viewer's history row (opened now, closed when they leave).
	sm.recordLiveHistory(username, streamID, streamType, streamTitle, userSession.IPAddress, userSession.UserAgent)

	utils.InfoLog("Started new %s for user %s", sm.streamLabel(streamID), username)
	return streamBuffer, nil
}

// newStreamClient allocates a client with its jitter buffer and done signal.
func newStreamClient() *streamClient {
	return &streamClient{
		ch:   make(chan []byte, clientBufferChunks),
		done: make(chan struct{}),
	}
}

// deliver enqueues a chunk for one client, applying back-pressure.
//
// When sole is true (the only viewer) it blocks until the client accepts the
// chunk or disconnects — reproducing a direct connection's TCP back-pressure.
// With multiple viewers it still blocks (so the pump tracks the slowest client),
// but a client whose buffer stays full past clientStallTimeout is dropped so it
// cannot freeze the shared upstream for everyone else. Returns true if dropped.
func (sm *SessionManager) deliver(buffer *StreamBuffer, cl *streamClient, chunk []byte, sole bool) (dropped bool) {
	if sole {
		select {
		case cl.ch <- chunk:
		case <-cl.done:
		case <-buffer.stopChan:
		}
		return false
	}
	// Use an explicit timer (stopped on the fast path) rather than time.After,
	// which would otherwise leave one live timer per chunk until it fires.
	stall := time.NewTimer(sm.clientStallTimeout)
	defer stall.Stop()
	select {
	case cl.ch <- chunk:
		return false
	case <-cl.done:
		return false
	case <-buffer.stopChan:
		return false
	case <-stall.C:
		cl.close()
		return true
	}
}

// streamToClients pumps the single upstream connection and fans each chunk out
// to every attached client. It is the only reader of the upstream body.
func (sm *SessionManager) streamToClients(buffer *StreamBuffer, upstreamURL *url.URL) {
	utils.DebugLog("Starting stream from %s", upstreamURL.String())

	// Create a context that cancels when the stream is stopped
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-buffer.stopChan
		cancel()
	}()

	// Dial the upstream. dialUpstream sets the same headers as before: never
	// inject Range, so the upstream returns a natural 200 with Content-Length
	// rather than a 206 that may lack a Content-Range total (which causes
	// avformat errors in media players). 200 and 206 are both accepted, since
	// some providers return 206 unconditionally.
	resp, uerr := sm.dialUpstream(ctx, upstreamURL)

	// Release the HTTP handler waiting to learn whether this stream came up, so
	// it can choose a response before committing to a 200.
	buffer.markReady(uerr)

	if uerr != nil {
		utils.ErrorLog("Upstream failed for %s: %s", sm.streamLabel(buffer.streamID), uerr.Error())

		if !sm.slateEnabled(buffer) {
			sm.stopStreamLocking(buffer.streamID)
			return
		}
		// Show the viewer what went wrong and keep retrying the provider. This
		// returns a live response only if the upstream came back.
		if resp = sm.serveSlateAndRetry(ctx, buffer, upstreamURL, uerr); resp == nil {
			sm.stopStreamLocking(buffer.streamID)
			return
		}
	}

	// resp is reassigned when a slate retry succeeds, so close whatever is
	// current at return rather than capturing the original.
	defer func() {
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()

	// The buffer was created active; the pump does not flip the flag itself so
	// that every read/write of buffer.active stays under sm.streamLock.
	dataBuffer := make([]byte, streamChunkSize)

	// LastRequested only needs to stay fresh relative to streamTimeout (minutes),
	// so throttle the streamLock update rather than taking the lock per chunk.
	var lastTouch time.Time

	for {
		// Stop requested
		select {
		case <-buffer.stopChan:
			utils.DebugLog("%s stopped", sm.streamLabel(buffer.streamID))
			return
		default:
		}

		n, rerr := resp.Body.Read(dataBuffer)
		if n > 0 {
			// Each client reads concurrently, so the chunk must be its own copy
			// (dataBuffer is reused on the next read).
			chunk := make([]byte, n)
			copy(chunk, dataBuffer[:n])
			sm.fanOut(buffer, chunk)

			// Async disk write for local catchup (non-blocking, never stalls the pump)
			if buffer.diskBuffer != nil {
				buffer.diskBuffer.Write(chunk)
			}

			// Feed any attached probe taps (e.g. technical-info sampling). Almost
			// always empty, so this is a cheap lock+iterate over nothing.
			buffer.probeTapsLock.Lock()
			taps := buffer.probeTaps
			buffer.probeTapsLock.Unlock()
			for _, t := range taps {
				t.feed(chunk)
			}

			// Touch stream LastRequested to avoid cleanup timeout while data flows.
			if now := time.Now(); now.Sub(lastTouch) >= time.Second {
				lastTouch = now
				sm.streamLock.Lock()
				if ss, ok := sm.streamSessions[buffer.streamID]; ok {
					ss.LastRequested = now
				}
				sm.streamLock.Unlock()
			}
		}
		if rerr != nil {
			if rerr != io.EOF && ctx.Err() == nil {
				utils.ErrorLog("Error reading from upstream: %v", rerr)
			}
			sm.stopStreamLocking(buffer.streamID)
			return
		}
	}
}

// fanOut delivers one chunk to every attached client in parallel and waits for
// all of them to accept it (or be dropped) before returning. This is what gives
// the pump its back-pressure: it cannot read the next upstream chunk until the
// slowest healthy client has taken the current one.
func (sm *SessionManager) fanOut(buffer *StreamBuffer, chunk []byte) {
	buffer.clientsLock.RLock()
	names := make([]string, 0, len(buffer.clients))
	targets := make([]*streamClient, 0, len(buffer.clients))
	for name, cl := range buffer.clients {
		names = append(names, name)
		targets = append(targets, cl)
	}
	buffer.clientsLock.RUnlock()

	switch len(targets) {
	case 0:
		return
	case 1:
		// Sole viewer: deliver inline on the pump goroutine (no fan-out
		// goroutine or WaitGroup). deliver never drops the sole client, so
		// there is nothing to remove afterwards.
		sm.deliver(buffer, targets[0], chunk, true)
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i := range targets {
		go func(name string, cl *streamClient) {
			defer wg.Done()
			if sm.deliver(buffer, cl, chunk, false) {
				utils.WarnLog("Dropping slow client %s from %s (buffer stalled)", name, sm.streamLabel(buffer.streamID))
				sm.RemoveClient(buffer.streamID, name)
			}
		}(names[i], targets[i])
	}
	wg.Wait()
}

// GetClientChannel retrieves the data channel for a specific client.
func (sm *SessionManager) GetClientChannel(streamID, username string) (chan []byte, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return nil, false
	}

	buffer.clientsLock.RLock()
	defer buffer.clientsLock.RUnlock()

	cl, exists := buffer.clients[username]
	if !exists {
		return nil, false
	}
	return cl.ch, true
}

// GetClientDone returns the termination signal for a specific client. The HTTP
// handler selects on it so it exits when the pump drops the client (slow viewer)
// or the stream stops.
func (sm *SessionManager) GetClientDone(streamID, username string) (<-chan struct{}, bool) {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return nil, false
	}

	buffer.clientsLock.RLock()
	defer buffer.clientsLock.RUnlock()

	cl, exists := buffer.clients[username]
	if !exists {
		return nil, false
	}
	return cl.done, true
}

// RemoveClient removes a client from a stream.
//
// Deliberately does NOT clear userSession.StreamID/StreamType here: a closed
// HTTP connection looks identical whether the user is switching channels or
// "pausing" (TiviMate disconnects, then resumes later via timeshift). Leaving
// the previous stream ID in place lets RequestStream's switch-detection work
// correctly regardless of request ordering — it overwrites StreamID when the
// user starts a genuinely different stream, and converts any pending
// pause-grace stop on the old stream into an immediate one. Idle sessions are
// fully reaped by cleanupExpiredSessions/DisconnectUser regardless.
func (sm *SessionManager) RemoveClient(streamID, username string) {
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()

	// Signal the client's HTTP handler to finish, then detach it.
	buffer, exists := sm.streamBuffers[streamID]
	if !exists {
		return
	}

	buffer.clientsLock.Lock()
	if cl, ok := buffer.clients[username]; ok {
		cl.close()
		delete(buffer.clients, username)
	}
	buffer.clientsLock.Unlock()

	// This viewer is leaving the stream — close their live history row. A later
	// resume (e.g. after a catchup pause) opens a fresh row via RequestStream.
	sm.closeLiveHistory(username, streamID)

	// Remove from stream session and stop the stream if last viewer
	streamSession, exists := sm.streamSessions[streamID]
	if !exists {
		return
	}
	if !streamSession.RemoveViewer(username) && buffer.active {
		// Catchup-enabled live streams get a grace window before stopping for
		// real, so a TiviMate "pause" (which looks like a disconnect to us)
		// followed by a resume keeps recording with no gap. A genuine channel
		// switch is detected and stopped immediately in RequestStream instead.
		if buffer.diskBuffer != nil && sm.pauseGrace > 0 {
			sm.schedulePendingStop(streamID)
		} else {
			sm.stopStream(streamID)
		}
	}

	utils.InfoLog("User %s removed from %s", username, sm.streamLabel(streamID))
}

// stopStreamLocking acquires streamLock and stops the stream. Used by the
// upstream pump goroutine, which does not otherwise hold the lock.
func (sm *SessionManager) stopStreamLocking(streamID string) {
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	sm.stopStream(streamID)
}

// schedulePendingStop delays stopping streamID by sm.pauseGrace instead of
// stopping it immediately, keeping the upstream pump (and catchup recording)
// alive in case the viewer resumes — e.g. TiviMate "pauses" by disconnecting
// and later resumes via a timeshift request. The caller must hold streamLock.
func (sm *SessionManager) schedulePendingStop(streamID string) {
	if _, exists := sm.pendingStops[streamID]; exists {
		return
	}
	cancel := make(chan struct{})
	sm.pendingStops[streamID] = cancel
	utils.DebugLog("%s has no viewers; keeping it alive for up to %s in case of resume (pause/timeshift)",
		sm.streamLabel(streamID), utils.HumanDuration(sm.pauseGrace))

	go func() {
		select {
		case <-time.After(sm.pauseGrace):
		case <-cancel:
			return
		}
		sm.streamLock.Lock()
		defer sm.streamLock.Unlock()
		if current, ok := sm.pendingStops[streamID]; ok && current == cancel {
			delete(sm.pendingStops, streamID)
			utils.DebugLog("%s pause grace expired with no viewers returning; stopping", sm.streamLabel(streamID))
			sm.stopStream(streamID)
		}
	}()
}

// cancelPendingStop cancels a scheduled stop for streamID, if any (silently —
// callers that represent a genuine "viewer returned" event should log that
// themselves; this is also called from stopStream itself, where logging
// "resumed" would be misleading). The caller must hold streamLock.
func (sm *SessionManager) cancelPendingStop(streamID string) bool {
	if cancel, ok := sm.pendingStops[streamID]; ok {
		close(cancel)
		delete(sm.pendingStops, streamID)
		return true
	}
	return false
}

// NotifyCatchupActivity cancels any pending stop for streamID, keeping the
// upstream connection and disk recording alive while a client is actively
// reading from its catchup buffer (e.g. rewinding). Safe to call whether or
// not a stop was actually pending.
func (sm *SessionManager) NotifyCatchupActivity(streamID string) {
	sm.streamLock.Lock()
	defer sm.streamLock.Unlock()
	if sm.cancelPendingStop(streamID) {
		utils.DebugLog("%s resumed via catchup before pause grace expired; continuing uninterrupted", sm.streamLabel(streamID))
	}
}

// stopStream stops an active stream and disconnects all of its clients.
// The caller must hold sm.streamLock.
func (sm *SessionManager) stopStream(streamID string) {
	// Cancel any scheduled pause-grace stop — we're stopping for real now
	// (e.g. the viewer switched to a different channel).
	sm.cancelPendingStop(streamID)

	utils.DebugLog("Stopping %s", sm.streamLabel(streamID))

	buffer, exists := sm.streamBuffers[streamID]
	if !exists || !buffer.active {
		return
	}

	// Signal the upstream pump to stop (Once prevents double-close panic)
	buffer.stopOnce.Do(func() { close(buffer.stopChan) })
	buffer.active = false

	// Release any handler still waiting to learn whether this stream came up.
	// Normally the pump has already marked it ready and this is a no-op; it only
	// matters when a stream is torn down before the upstream dial finished, where
	// otherwise the handler would block until its own timeout.
	buffer.markReady(&UpstreamError{Key: KeyUnreachable})

	// Signal every client's HTTP handler to finish
	buffer.clientsLock.Lock()
	for _, cl := range buffer.clients {
		cl.close()
	}
	buffer.clients = make(map[string]*streamClient)
	buffer.clientsLock.Unlock()

	// Stop disk buffer with grace period so in-flight timeshift readers can finish.
	// TiviMate closes the live connection BEFORE opening timeshift, so we must keep
	// the file alive briefly.
	if buffer.diskBuffer != nil && sm.catchupManager != nil {
		sm.catchupManager.StopBuffer(buffer.diskBuffer.StreamID())
	}

	// Update the stream session
	if streamSession, exists := sm.streamSessions[streamID]; exists {
		streamSession.Active = false
	}

	utils.DebugLog("%s stopped and all clients disconnected", sm.streamLabel(streamID))
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

// streamUserKey identifies a (stream, user) pair for the per-view history and
// grace-timer maps.
func streamUserKey(streamID, username string) string { return streamID + "\x00" + username }

// RegisterVODView creates a synthetic stream session so status commands see users
// watching local files. Cached VOD is served as many short Range requests, so any
// pending grace-period teardown for this view is cancelled here: as long as the
// player keeps requesting, the session stays visible in /status.
func (sm *SessionManager) RegisterVODView(username, streamID, streamType, title string) {
	sm.cancelVODViewTimer(streamID, username)

	// Record a stream_history row once per viewing session. RegisterVODView is
	// called on every Range request; recordVODHistory de-duplicates so only the
	// first request for a (user, stream) view inserts a row.
	sm.recordVODHistory(username, streamID, streamType, title)

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
		ss.Active = true
		// The title passed on the very first request may have been an
		// unresolved fallback (e.g. a transient lookup failure). Later calls
		// (RegisterVODView runs on every Range request) can carry a properly
		// resolved title, so adopt it once available instead of sticking with
		// the initial fallback for the rest of the session.
		if (ss.StreamTitle == "" || ss.StreamTitle == streamID) && title != "" && title != streamID {
			ss.StreamTitle = title
		}
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

// UnregisterVODView schedules removal of a synthetic VOD view after a grace
// period rather than tearing it down immediately. Players fetch cached files in
// short Range requests with gaps in between while the local buffer plays; a
// grace window keeps the session in /status across those gaps. A subsequent
// RegisterVODView (i.e. the next range request) cancels the pending removal.
func (sm *SessionManager) UnregisterVODView(username, streamID string) {
	key := streamUserKey(streamID, username)
	sm.vodViewTimersMu.Lock()
	if t, ok := sm.vodViewTimers[key]; ok {
		t.Stop()
	}
	sm.vodViewTimers[key] = time.AfterFunc(sm.streamTimeout, func() {
		sm.vodViewTimersMu.Lock()
		delete(sm.vodViewTimers, key)
		sm.vodViewTimersMu.Unlock()
		sm.removeVODView(username, streamID)
	})
	sm.vodViewTimersMu.Unlock()
}

// recordVODHistory inserts a stream_history row for a VOD view the first time it
// is seen, keyed by (stream, user). Repeat calls (subsequent Range requests) are
// no-ops while the view is active. The row is closed in removeVODView.
func (sm *SessionManager) recordVODHistory(username, streamID, streamType, title string) {
	if sm.db == nil {
		return
	}
	key := streamUserKey(streamID, username)

	sm.vodViewTimersMu.Lock()
	if _, exists := sm.vodHistoryIDs[key]; exists {
		sm.vodViewTimersMu.Unlock()
		return
	}
	// Reserve the slot so a concurrent Range request cannot insert a duplicate row.
	sm.vodHistoryIDs[key] = 0
	sm.vodViewTimersMu.Unlock()

	ip, ua := "", ""
	sm.userLock.RLock()
	if sess, ok := sm.userSessions[username]; ok {
		ip, ua = sess.IPAddress, sess.UserAgent
	}
	sm.userLock.RUnlock()

	id, err := sm.db.AddStreamHistory(username, streamID, streamType, title, ip, ua)
	if err != nil {
		utils.ErrorLog("Failed to record VOD stream history: %v", err)
		sm.vodViewTimersMu.Lock()
		delete(sm.vodHistoryIDs, key)
		sm.vodViewTimersMu.Unlock()
		return
	}
	sm.vodViewTimersMu.Lock()
	sm.vodHistoryIDs[key] = id
	sm.vodViewTimersMu.Unlock()
}

// closeVODHistory marks a VOD view's stream_history row as ended, if one is open.
func (sm *SessionManager) closeVODHistory(username, streamID string) {
	key := streamUserKey(streamID, username)
	sm.vodViewTimersMu.Lock()
	id, ok := sm.vodHistoryIDs[key]
	if ok {
		delete(sm.vodHistoryIDs, key)
	}
	sm.vodViewTimersMu.Unlock()
	if ok && id > 0 && sm.db != nil {
		if err := sm.db.CloseStreamHistory(id); err != nil {
			utils.WarnLog("Failed to close VOD stream history %d: %v", id, err)
		}
	}
}

// recordLiveHistory opens a stream_history row for a live viewer the first time
// they are seen on a stream, keyed by (stream, user). The DB insert runs off the
// caller's goroutine (callers hold streamLock) so the streaming hot path is not
// blocked on database latency. If the viewer leaves before the insert completes,
// the row is closed immediately when the id lands.
func (sm *SessionManager) recordLiveHistory(username, streamID, streamType, title, ip, ua string) {
	if sm.db == nil {
		return
	}
	key := streamUserKey(streamID, username)

	sm.liveHistoryMu.Lock()
	if _, exists := sm.liveHistoryIDs[key]; exists {
		sm.liveHistoryMu.Unlock()
		return
	}
	// Reserve the slot (sentinel 0 = insert in flight) so a concurrent join for the
	// same viewer cannot open a duplicate row.
	sm.liveHistoryIDs[key] = 0
	sm.liveHistoryMu.Unlock()

	go func() {
		id, err := sm.db.AddStreamHistory(username, streamID, streamType, title, ip, ua)
		if err != nil {
			utils.ErrorLog("Failed to record live stream history: %v", err)
			sm.liveHistoryMu.Lock()
			if v, ok := sm.liveHistoryIDs[key]; ok && v == 0 {
				delete(sm.liveHistoryIDs, key)
			}
			sm.liveHistoryMu.Unlock()
			return
		}
		sm.liveHistoryMu.Lock()
		if v, ok := sm.liveHistoryIDs[key]; ok && v == 0 {
			sm.liveHistoryIDs[key] = id
			sm.liveHistoryMu.Unlock()
			return
		}
		// The viewer already left before the insert completed (slot was deleted):
		// close the freshly-created row so it does not stay open forever.
		sm.liveHistoryMu.Unlock()
		if err := sm.db.CloseStreamHistory(id); err != nil {
			utils.WarnLog("Failed to close orphaned live stream history %d: %v", id, err)
		}
	}()
}

// closeLiveHistory marks a live viewer's stream_history row as ended, if one is
// open. Safe to call for non-live streams and unknown viewers (no-op). Deleting a
// still-reserved slot (id 0) signals recordLiveHistory's goroutine to close the
// row as soon as its insert completes.
func (sm *SessionManager) closeLiveHistory(username, streamID string) {
	key := streamUserKey(streamID, username)
	sm.liveHistoryMu.Lock()
	id, ok := sm.liveHistoryIDs[key]
	if ok {
		delete(sm.liveHistoryIDs, key)
	}
	sm.liveHistoryMu.Unlock()
	if ok && id > 0 && sm.db != nil {
		if err := sm.db.CloseStreamHistory(id); err != nil {
			utils.WarnLog("Failed to close live stream history %d: %v", id, err)
		}
	}
}

// cancelVODViewTimer stops any pending grace-period removal for a VOD view.
func (sm *SessionManager) cancelVODViewTimer(streamID, username string) {
	key := streamUserKey(streamID, username)
	sm.vodViewTimersMu.Lock()
	if t, ok := sm.vodViewTimers[key]; ok {
		t.Stop()
		delete(sm.vodViewTimers, key)
	}
	sm.vodViewTimersMu.Unlock()
}

// removeVODView detaches a user from a synthetic VOD viewing session once the
// grace period has elapsed without further range requests.
func (sm *SessionManager) removeVODView(username, streamID string) {
	sm.closeVODHistory(username, streamID)

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

// CaptureStreamSample returns up to maxBytes of upstream data for an actively
// buffered (multiplexed live) stream, sampled from bytes already flowing
// through its existing pump — it does NOT open any additional connection to
// the provider. Returns an error if the stream has no active shared buffer
// (e.g. it is a VOD view, which has no persistent upstream connection to tap).
// If the timeout elapses before maxBytes is reached, whatever was captured so
// far is returned (which may be a usable, if smaller, sample).
func (sm *SessionManager) CaptureStreamSample(streamID string, maxBytes int, timeout time.Duration) ([]byte, error) {
	sm.streamLock.RLock()
	buffer, exists := sm.streamBuffers[streamID]
	sm.streamLock.RUnlock()
	if !exists || !buffer.active {
		return nil, fmt.Errorf("no active shared upstream connection for stream %s", streamID)
	}

	tap := newProbeTap(maxBytes)
	buffer.probeTapsLock.Lock()
	buffer.probeTaps = append(buffer.probeTaps, tap)
	buffer.probeTapsLock.Unlock()
	defer func() {
		buffer.probeTapsLock.Lock()
		for i, t := range buffer.probeTaps {
			if t == tap {
				buffer.probeTaps = append(buffer.probeTaps[:i], buffer.probeTaps[i+1:]...)
				break
			}
		}
		buffer.probeTapsLock.Unlock()
	}()

	select {
	case <-tap.done:
		return tap.snapshot(), nil
	case <-time.After(timeout):
		if sample := tap.snapshot(); len(sample) > 0 {
			return sample, nil
		}
		return nil, fmt.Errorf("timed out sampling stream %s", streamID)
	}
}

// SetSessionTimeout sets the user session timeout duration
func (sm *SessionManager) SetSessionTimeout(timeout time.Duration) {
	sm.sessionTimeout = timeout
}

// SetStreamTimeout sets the unused stream timeout duration
func (sm *SessionManager) SetStreamTimeout(timeout time.Duration) {
	sm.streamTimeout = timeout
}

// SetPauseGrace sets how long a catchup-enabled live stream stays alive
// (upstream connection open, disk recording continuing) after its last
// viewer disconnects, so a pause/resume via timeshift has no recording gap.
// 0 disables the behavior.
func (sm *SessionManager) SetPauseGrace(d time.Duration) {
	sm.pauseGrace = d
}

// SetTempLinkTimeout sets the temporary link expiration duration
func (sm *SessionManager) SetTempLinkTimeout(timeout time.Duration) {
	sm.tempLinkTimeout = timeout
}

// SetVODCacheStaleAge sets how long a cached file can go unaccessed before cleanup.
func (sm *SessionManager) SetVODCacheStaleAge(d time.Duration) {
	sm.vodCacheStaleAge = d
}

// SetClientStallTimeout sets how long the pump waits on a multiplexed client
// whose buffer is full before dropping it (to protect the other viewers).
func (sm *SessionManager) SetClientStallTimeout(d time.Duration) {
	if d > 0 {
		sm.clientStallTimeout = d
	}
}

// SetSlateCache configures pruning of the on-disk slate clip cache: clips not
// modified within age are deleted (they are regenerated on demand). An empty dir
// or a non-positive age disables pruning.
func (sm *SessionManager) SetSlateCache(dir string, age time.Duration) {
	sm.slateCacheDir = dir
	sm.slateStaleAge = age
}

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
			continue // too fresh — may be an active download or a just-written file
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
