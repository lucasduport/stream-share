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
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// streamUserKey identifies a (stream, user) pair for the per-view history and
// grace-timer maps.
func streamUserKey(streamID, username string) string { return streamID + "\x00" + username }

// reserveHistorySlot atomically checks m for key and, if absent, reserves it
// with the sentinel 0 (insert in flight). Returns true if the caller reserved
// the slot (and must later set the real id or delete it on failure), false if
// the slot was already present.
func reserveHistorySlot(mu *sync.Mutex, m map[string]int64, key string) bool {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := m[key]; exists {
		return false
	}
	m[key] = 0
	return true
}

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

	if !reserveHistorySlot(&sm.vodViewTimersMu, sm.vodHistoryIDs, key) {
		return
	}

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

	if !reserveHistorySlot(&sm.liveHistoryMu, sm.liveHistoryIDs, key) {
		return
	}

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
	id := sm.captureLiveHistoryID(username, streamID)
	sm.closeHistoryRow(id)
}

// captureLiveHistoryID removes and returns the history row ID for a (stream,
// user) pair, or 0 if none was open. Safe to call under streamLock — it only
// touches liveHistoryMu, not the database.
func (sm *SessionManager) captureLiveHistoryID(username, streamID string) int64 {
	key := streamUserKey(streamID, username)
	sm.liveHistoryMu.Lock()
	id, ok := sm.liveHistoryIDs[key]
	if ok {
		delete(sm.liveHistoryIDs, key)
	}
	sm.liveHistoryMu.Unlock()
	if ok {
		return id
	}
	return 0
}

// closeHistoryRow closes a stream_history row by ID. No-op for id <= 0. Must
// NOT be called under streamLock — it performs a synchronous DB write.
func (sm *SessionManager) closeHistoryRow(id int64) {
	if id <= 0 || sm.db == nil {
		return
	}
	if err := sm.db.CloseStreamHistory(id); err != nil {
		utils.WarnLog("Failed to close live stream history %d: %v", id, err)
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
