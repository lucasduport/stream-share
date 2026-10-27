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
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---- helpers ----

func newTestManager() *SessionManager {
	sm := NewSessionManager(nil)
	sm.SetClientStallTimeout(200 * time.Millisecond)
	// Synthetic VOD views are torn down a streamTimeout after their last range
	// request; keep that short so tests don't wait the 2-minute default.
	sm.SetStreamTimeout(50 * time.Millisecond)
	return sm
}

// ---- RegisterUser / GetUserSession ----

func TestRegisterUser_createsSession(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sess := sm.RegisterUser("alice", "1.2.3.4", "TestAgent/1.0")
	if sess == nil {
		t.Fatal("expected session, got nil")
		return
	}
	if sess.Username != "alice" {
		t.Fatalf("want username alice, got %q", sess.Username)
	}
}

func TestRegisterUser_idempotent(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "A")
	time.Sleep(5 * time.Millisecond)
	sm.RegisterUser("alice", "5.6.7.8", "B") // second call updates IP/UA

	got := sm.GetUserSession("alice")
	if got == nil {
		t.Fatal("session not found after second register")
		return
	}
	if got.IPAddress != "5.6.7.8" {
		t.Fatalf("expected IP to be updated to 5.6.7.8, got %q", got.IPAddress)
	}

	// Should still be only one session.
	all := sm.GetAllSessions()
	if len(all) != 1 {
		t.Fatalf("expected 1 session, got %d", len(all))
	}
}

func TestGetUserSession_unknown(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	if got := sm.GetUserSession("nobody"); got != nil {
		t.Fatalf("expected nil for unknown user, got %+v", got)
	}
}

// ---- VOD view tracking ----

func TestRegisterVODView_appearsInStreams(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")
	sm.RegisterVODView("alice", "vod123", "movie", "Test Movie")

	info, ok := sm.GetStreamInfo("vod123")
	if !ok {
		t.Fatal("stream not found after RegisterVODView")
	}
	if !info.Active {
		t.Fatal("stream should be active")
	}
	if _, viewing := info.Viewers["alice"]; !viewing {
		t.Fatal("alice should be listed as a viewer")
	}
}

// TestRegisterVODView_adoptsResolvedTitle: the first Range request of a view
// can lose the race to resolve a title and pass the raw id; a later request
// carrying the real title must be adopted (ported from x3n0n10/stream-share#46).
func TestRegisterVODView_adoptsResolvedTitle(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")
	sm.RegisterVODView("alice", "327914", "movie", "327914")
	if info, ok := sm.GetStreamInfo("327914"); !ok || info.StreamTitle != "327914" {
		t.Fatalf("expected fallback title, got %+v", info)
	}

	sm.RegisterVODView("alice", "327914", "movie", "The Real Movie Title")
	if info, ok := sm.GetStreamInfo("327914"); !ok || info.StreamTitle != "The Real Movie Title" {
		t.Fatalf("expected resolved title to be adopted, got %+v", info)
	}
}

// adoptVODHistoryTitle decides when an open history row must be rewritten:
// only while it still holds a fallback, per (stream, user) view, and it returns
// 0 while the row's insert is in flight so recordVODHistory applies it later.
func TestAdoptVODHistoryTitle(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()
	key := streamUserKey("42", "alice")

	// No open row for this view: nothing to correct.
	if id := sm.adoptVODHistoryTitle("alice", "42", "Movie"); id != 0 {
		t.Fatalf("no row: id = %d", id)
	}

	// Row inserted with the raw id as title.
	sm.vodHistoryIDs[key] = 7
	sm.vodHistoryTitles[key] = "42"

	// Fallback titles never trigger a rewrite.
	for _, title := range []string{"", "42"} {
		if id := sm.adoptVODHistoryTitle("alice", "42", title); id != 0 {
			t.Fatalf("fallback %q: id = %d", title, id)
		}
	}
	if id := sm.adoptVODHistoryTitle("alice", "42", "Movie"); id != 7 {
		t.Fatalf("resolved title: id = %d, want 7", id)
	}
	if sm.vodHistoryTitles[key] != "Movie" {
		t.Fatalf("wanted title not recorded: %q", sm.vodHistoryTitles[key])
	}
	// Once corrected, the row is left alone (no rewrite per Range request).
	if id := sm.adoptVODHistoryTitle("alice", "42", "Movie"); id != 0 {
		t.Fatalf("already corrected: id = %d", id)
	}
	if id := sm.adoptVODHistoryTitle("alice", "42", "Other"); id != 0 {
		t.Fatalf("real title must not be replaced: id = %d", id)
	}

	// Another viewer's row is tracked independently: the stream-level title
	// is already resolved, but bob's row still holds the fallback.
	bob := streamUserKey("42", "bob")
	sm.vodHistoryIDs[bob] = 0 // insert still in flight
	sm.vodHistoryTitles[bob] = ""
	if id := sm.adoptVODHistoryTitle("bob", "42", "Movie"); id != 0 {
		t.Fatalf("in-flight insert: id = %d, want 0", id)
	}
	if sm.vodHistoryTitles[bob] != "Movie" {
		t.Fatalf("in-flight insert must still record the wanted title, got %q", sm.vodHistoryTitles[bob])
	}
}

func TestUnregisterVODView_removesSession(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")
	sm.RegisterVODView("alice", "vod123", "movie", "Test Movie")
	sm.UnregisterVODView("alice", "vod123")

	// Removal is deferred by a grace window (streamTimeout) so the session
	// survives the gaps between a player's range requests. It should still be
	// present immediately after unregister, then gone once the grace elapses.
	if _, ok := sm.GetStreamInfo("vod123"); !ok {
		t.Fatal("stream should survive the grace window after last range request")
	}
	if !waitForStreamGone(sm, "vod123", time.Second) {
		t.Fatal("stream should be removed after the grace window elapses")
	}
}

// waitForStreamGone polls until the stream session is gone or the deadline hits.
func waitForStreamGone(sm *SessionManager, streamID string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, ok := sm.GetStreamInfo(streamID); !ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, ok := sm.GetStreamInfo(streamID)
	return !ok
}

func TestVODView_multipleViewers(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	for _, u := range []string{"alice", "bob"} {
		sm.RegisterUser(u, "1.2.3.4", "")
		sm.RegisterVODView(u, "vod123", "movie", "Test Movie")
	}

	// Only alice leaves — stream should still exist for bob (and survive alice's
	// grace window, since bob is still an active viewer).
	sm.UnregisterVODView("alice", "vod123")
	time.Sleep(100 * time.Millisecond) // longer than the grace window
	if _, ok := sm.GetStreamInfo("vod123"); !ok {
		t.Fatal("stream should still exist while bob is watching")
	}

	// Bob leaves — stream should be gone once his grace window elapses.
	sm.UnregisterVODView("bob", "vod123")
	if !waitForStreamGone(sm, "vod123", time.Second) {
		t.Fatal("stream should be removed after all viewers leave")
	}
}

// ---- Temporary links ----

func TestGenerateAndGetTemporaryLink(t *testing.T) {
	sm := newTestManager()
	sm.SetTempLinkTimeout(1 * time.Hour)
	defer sm.Stop()

	token, err := sm.GenerateTemporaryLink("alice", "stream1", "My Stream", "http://example.com/stream")
	if err != nil {
		t.Fatalf("GenerateTemporaryLink: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	link, err := sm.GetTemporaryLink(token)
	if err != nil {
		t.Fatalf("GetTemporaryLink: %v", err)
	}
	if link.Username != "alice" {
		t.Fatalf("want username alice, got %q", link.Username)
	}
}

func TestGetTemporaryLink_expired(t *testing.T) {
	sm := newTestManager()
	sm.SetTempLinkTimeout(1 * time.Millisecond) // expire almost immediately
	defer sm.Stop()

	token, _ := sm.GenerateTemporaryLink("alice", "s1", "T", "http://x.com")
	time.Sleep(10 * time.Millisecond)

	if _, err := sm.GetTemporaryLink(token); err == nil {
		t.Fatal("expected error for expired link, got nil")
	}
}

func TestGetTemporaryLink_notFound(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	if _, err := sm.GetTemporaryLink("nonexistent-token"); err == nil {
		t.Fatal("expected error for unknown token")
	}
}

// ---- Session expiry (cleanupExpiredSessions) ----

func TestCleanupExpiredSessions(t *testing.T) {
	sm := newTestManager()
	sm.SetSessionTimeout(50 * time.Millisecond)
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")

	time.Sleep(100 * time.Millisecond)
	sm.cleanupExpiredSessions()

	if sm.GetUserSession("alice") != nil {
		t.Fatal("expired session should have been removed")
	}
}

func TestCleanupExpiredSessions_activeNotRemoved(t *testing.T) {
	sm := newTestManager()
	sm.SetSessionTimeout(200 * time.Millisecond)
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")
	sm.GetUserSession("alice") // bumps LastActive

	time.Sleep(100 * time.Millisecond)
	sm.cleanupExpiredSessions()

	if sm.GetUserSession("alice") == nil {
		t.Fatal("active session should NOT be removed before timeout")
	}
}

// ---- DisconnectUser ----

func TestDisconnectUser(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")

	// DisconnectUser clears the stream association on the user session but does
	// not delete the session itself (it is a soft disconnect for reconnect purposes).
	sm.DisconnectUser("alice")

	sess := sm.GetUserSession("alice")
	if sess == nil {
		t.Fatal("user session should still exist after DisconnectUser (soft disconnect)")
		return
	}
	if sess.StreamID != "" {
		t.Fatalf("stream association should be empty, got %q", sess.StreamID)
	}
}

// ---- GetAllSessions / GetAllStreams ----

func TestGetAllSessions(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	for _, u := range []string{"alice", "bob", "carol"} {
		sm.RegisterUser(u, "1.2.3.4", "")
	}

	all := sm.GetAllSessions()
	if len(all) != 3 {
		t.Fatalf("want 3 sessions, got %d", len(all))
	}
}

func TestGetAllStreams_onlyActive(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()

	sm.RegisterUser("alice", "1.2.3.4", "")
	sm.RegisterVODView("alice", "vod1", "movie", "M1")
	sm.RegisterVODView("alice", "vod2", "movie", "M2") // alice switches to vod2

	all := sm.GetAllStreams()
	// vod1 will have alice removed when she switched; vod2 should be active.
	for _, s := range all {
		if !s.Active {
			t.Errorf("GetAllStreams returned inactive stream %q", s.StreamID)
		}
	}
}

// ---- RequestStream — user switch ----

func TestRequestStream_userSwitchesChannel(t *testing.T) {
	var w1, w2 int64
	srv1 := streamingUpstream(t, 50, 4096, 2*time.Millisecond, &w1)
	defer srv1.Close()
	srv2 := streamingUpstream(t, 50, 4096, 2*time.Millisecond, &w2)
	defer srv2.Close()
	_, _ = w1, w2

	sm := newTestManager()
	defer sm.Stop()

	// Alice starts on channel 1.
	u1 := mustURL(t, srv1.URL)
	if _, _, err := sm.RequestStream("alice", "ch1", "live", "Channel 1", u1); err != nil {
		t.Fatalf("RequestStream ch1: %v", err)
	}

	// Alice switches to channel 2.
	u2 := mustURL(t, srv2.URL)
	_, aliceCh2, err := sm.RequestStream("alice", "ch2", "live", "Channel 2", u2)
	if err != nil {
		t.Fatalf("RequestStream ch2: %v", err)
	}

	// ch1 should no longer be tracked as having alice.
	if info, ok := sm.GetStreamInfo("ch1"); ok {
		if _, still := info.Viewers["alice"]; still {
			t.Error("alice should have been removed from ch1 after switching to ch2")
		}
	}
	// ch2 should show alice.
	if info, ok := sm.GetStreamInfo("ch2"); ok {
		if _, viewing := info.Viewers["alice"]; !viewing {
			t.Error("alice should be a viewer on ch2")
		}
	}

	removeKey(sm, "ch2", aliceCh2)
}

// ---- RemoveClient under concurrent access ----

func TestRemoveClient_concurrent(t *testing.T) {
	var w int64
	srv := streamingUpstream(t, 200, 4096, time.Millisecond, &w)
	defer srv.Close()

	sm := newTestManager()
	defer sm.Stop()

	const n = 5
	u := mustURL(t, srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		user := fmt.Sprintf("u%d", i)
		_, key, err := sm.RequestStream(user, "concurrent", "live", "C", u)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			time.Sleep(20 * time.Millisecond)
			removeKey(sm, "concurrent", k)
		}(key)
	}
	wg.Wait()
}

// HasVODViewer is false for the first request of a VOD view and true for the
// following Range requests of the same view, which is what keeps per-request
// logging to one line per playback.
func TestHasVODViewerOnlyFirstRequestIsNew(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()
	if sm.HasVODViewer("alice", "42") {
		t.Fatal("no view yet")
	}
	sm.RegisterVODView("alice", "42", "series", "Some Show")
	if !sm.HasVODViewer("alice", "42") {
		t.Fatal("second request of the view should find the viewer")
	}
	if sm.HasVODViewer("bob", "42") {
		t.Fatal("another user's first request is a new view")
	}
}
