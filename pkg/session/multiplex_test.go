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
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// streamingUpstream returns a test server that emits `chunks` payloads of size
// `chunkSize`, pausing `gap` between each, then ends. It records how many bytes
// it actually managed to write so tests can assert back-pressure.
func streamingUpstream(t *testing.T, chunks, chunkSize int, gap time.Duration, written *int64) *httptest.Server {
	t.Helper()
	payload := make([]byte, chunkSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			n, err := w.Write(payload)
			if err != nil {
				return
			}
			atomic.AddInt64(written, int64(n))
			if fl != nil {
				fl.Flush()
			}
			if gap > 0 {
				time.Sleep(gap)
			}
		}
	}))
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}

// drain reads from a client channel until it closes or done fires, counting bytes.
//
// When the stream ends the pump delivers the final chunk to the client's buffered
// channel and then closes done. A plain select over {ch, done} would pick randomly
// between the ready data and the closed done, so the last buffered chunk could be
// dropped and the byte count would come up short. On done (and stop), drain any
// already-buffered chunks non-blocking before returning so every delivered byte is
// counted deterministically.
func drain(sm *SessionManager, streamID, key string, got *int64, stop <-chan struct{}) {
	ch, ok := sm.GetClientChannel(streamID, key)
	if !ok {
		return
	}
	done, _ := sm.GetClientDone(streamID, key)
	drainBuffered := func() {
		for {
			select {
			case data, ok := <-ch:
				if !ok {
					return
				}
				atomic.AddInt64(got, int64(len(data)))
			default:
				return
			}
		}
	}
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			atomic.AddInt64(got, int64(len(data)))
		case <-done:
			drainBuffered()
			return
		case <-stop:
			drainBuffered()
			return
		}
	}
}

// TestSingleViewerReceivesAll verifies a lone viewer gets the whole stream.
func TestSingleViewerReceivesAll(t *testing.T) {
	var written int64
	srv := streamingUpstream(t, 20, 4096, 2*time.Millisecond, &written)
	defer srv.Close()

	sm := NewSessionManager(nil)
	defer sm.Stop()

	streamID := "chan1"
	_, alice, err := sm.RequestStream("alice", streamID, "live", "Channel 1", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}

	var got int64
	doneDrain := make(chan struct{})
	go func() { drain(sm, streamID, alice, &got, nil); close(doneDrain) }()

	select {
	case <-doneDrain:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining single viewer")
	}

	if got != 20*4096 {
		t.Fatalf("single viewer got %d bytes, want %d", got, 20*4096)
	}
}

// TestTwoViewersShareUpstream verifies the upstream is opened once and both
// viewers receive the full stream from the shared connection.
func TestTwoViewersShareUpstream(t *testing.T) {
	var written int64
	var opens int64
	payload := make([]byte, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&opens, 1)
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 30; i++ {
			n, err := w.Write(payload)
			if err != nil {
				return
			}
			atomic.AddInt64(&written, int64(n))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(3 * time.Millisecond)
		}
	}))
	defer srv.Close()

	sm := NewSessionManager(nil)
	defer sm.Stop()

	streamID := "chan2"
	_, alice, err := sm.RequestStream("alice", streamID, "live", "Channel 2", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream alice: %v", err)
	}
	var gotA, gotB int64
	doneA := make(chan struct{})
	go func() { drain(sm, streamID, alice, &gotA, nil); close(doneA) }()

	// Second viewer joins shortly after.
	time.Sleep(10 * time.Millisecond)
	_, bob, err := sm.RequestStream("bob", streamID, "live", "Channel 2", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream bob: %v", err)
	}
	doneB := make(chan struct{})
	go func() { drain(sm, streamID, bob, &gotB, nil); close(doneB) }()

	for _, d := range []chan struct{}{doneA, doneB} {
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out draining viewers")
		}
	}

	if o := atomic.LoadInt64(&opens); o != 1 {
		t.Fatalf("upstream opened %d times, want exactly 1 (multiplexing failed)", o)
	}
	if gotA != 30*4096 {
		t.Fatalf("alice got %d bytes, want %d", gotA, 30*4096)
	}
	// Bob joined late, so he gets the live edge onward — not the whole stream,
	// but a substantial portion and never more than what was sent.
	if gotB <= 0 || gotB > 30*4096 {
		t.Fatalf("bob got %d bytes, want >0 and <= %d", gotB, 30*4096)
	}
}

// TestSlowClientDroppedProtectsOthers verifies that a viewer who stops reading
// is dropped after the stall timeout and the healthy viewer then receives the
// rest of the stream from the shared upstream connection.
func TestSlowClientDroppedProtectsOthers(t *testing.T) {
	const chunks, chunkSize = 200, 64 * 1024
	var written int64
	srv := streamingUpstream(t, chunks, chunkSize, 0, &written) // fast upstream
	defer srv.Close()

	sm := NewSessionManager(nil)
	sm.SetClientStallTimeout(300 * time.Millisecond) // keep the test quick
	defer sm.Stop()

	streamID := "chan3"
	_, fast, err := sm.RequestStream("fast", streamID, "live", "Channel 3", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream fast: %v", err)
	}
	if _, _, err := sm.RequestStream("slow", streamID, "live", "Channel 3", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream slow: %v", err)
	}

	// "slow" never reads its channel; "fast" drains continuously. Once slow's
	// buffer fills, the pump blocks on it until the stall timeout drops it, after
	// which fast receives the remainder.
	var gotFast int64
	doneFast := make(chan struct{})
	go func() { drain(sm, streamID, fast, &gotFast, nil); close(doneFast) }()

	select {
	case <-doneFast:
		// Fast viewer completed: the slow client was dropped and did not
		// permanently stall the stream.
		if got := atomic.LoadInt64(&gotFast); got != chunks*chunkSize {
			t.Fatalf("fast viewer got %d bytes, want %d", got, chunks*chunkSize)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("fast viewer never completed (got %d bytes) — slow client likely stalled the stream",
			atomic.LoadInt64(&gotFast))
	}
}

// TestConcurrentJoinLeave hammers join/leave to surface races under -race.
func TestConcurrentJoinLeave(t *testing.T) {
	var written int64
	srv := streamingUpstream(t, 1000, 16*1024, time.Millisecond, &written)
	defer srv.Close()

	sm := NewSessionManager(nil)
	defer sm.Stop()

	streamID := "chan4"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			user := fmt.Sprintf("user%d", n)
			_, key, err := sm.RequestStream(user, streamID, "live", "Channel 4", mustURL(t, srv.URL))
			if err != nil {
				return
			}
			var got int64
			stop := make(chan struct{})
			d := make(chan struct{})
			go func() { drain(sm, streamID, key, &got, stop); close(d) }()
			time.Sleep(time.Duration(20+n*10) * time.Millisecond)
			sm.RemoveClient(streamID, key)
			close(stop)
			<-d
		}(i)
	}
	wg.Wait()
}

// countingUpstream serves an endless stream of small chunks and counts how many
// upstream connections were opened, for multi-device tests.
func countingUpstream(t *testing.T, opens *int64) *httptest.Server {
	t.Helper()
	payload := make([]byte, 4096)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(opens, 1)
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for {
			if _, err := w.Write(payload); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}))
}

// receiving reports whether the client connection key receives data within d.
func receiving(sm *SessionManager, streamID, key string, d time.Duration) bool {
	ch, ok := sm.GetClientChannel(streamID, key)
	if !ok {
		return false
	}
	done, _ := sm.GetClientDone(streamID, key)
	deadline := time.After(d)
	for {
		select {
		case data, open := <-ch:
			if !open {
				return false
			}
			if len(data) > 0 {
				return true
			}
		case <-done:
			return false
		case <-deadline:
			return false
		}
	}
}

// keepDraining reads key's channel until stop closes, so the pump never blocks
// on it.
func keepDraining(sm *SessionManager, streamID, key string, stop <-chan struct{}) {
	var n int64
	drain(sm, streamID, key, &n, stop)
}

func streamActive(sm *SessionManager, streamID string) bool {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()
	b, ok := sm.streamBuffers[streamID]
	return ok && b.active
}

// TestSameUserTwoDevicesShareStream covers two devices on one account watching
// the same channel: both receive data over a single upstream connection, one
// leaving does not cut the other, and the stream stops once both are gone.
func TestSameUserTwoDevicesShareStream(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()

	sm := newTestManager()
	defer sm.Stop()

	const streamID = "same-user"
	_, tv, err := sm.RequestStream("lucas", streamID, "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream tv: %v", err)
	}
	_, phone, err := sm.RequestStream("lucas", streamID, "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream phone: %v", err)
	}
	if tv == phone {
		t.Fatalf("both devices got client key %q", tv)
	}

	if !receiving(sm, streamID, tv, 2*time.Second) || !receiving(sm, streamID, phone, 2*time.Second) {
		t.Fatal("both devices should receive data")
	}
	if o := atomic.LoadInt64(&opens); o != 1 {
		t.Fatalf("upstream opened %d times, want 1", o)
	}
	if info, ok := sm.GetStreamInfo(streamID); !ok || len(info.GetViewers()) != 1 {
		t.Fatalf("viewers should list lucas once, got %v", info.GetViewers())
	}

	// The phone leaves: the TV keeps playing on the same upstream.
	stopTV := make(chan struct{})
	go keepDraining(sm, streamID, tv, stopTV)
	sm.RemoveClient(streamID, phone)
	time.Sleep(50 * time.Millisecond)
	if !streamActive(sm, streamID) {
		t.Fatal("stream stopped although the TV is still watching")
	}
	if info, ok := sm.GetStreamInfo(streamID); !ok || len(info.GetViewers()) != 1 {
		t.Fatal("lucas should still be a viewer while one device watches")
	}
	close(stopTV)
	if !receiving(sm, streamID, tv, 2*time.Second) {
		t.Fatal("TV stopped receiving after the phone left")
	}
	if o := atomic.LoadInt64(&opens); o != 1 {
		t.Fatalf("upstream reopened (%d opens) after one device left", o)
	}

	// The TV leaves too: nobody is left, the stream stops.
	sm.RemoveClient(streamID, tv)
	if streamActive(sm, streamID) {
		t.Fatal("stream should stop once its last device leaves")
	}
	if info, ok := sm.GetStreamInfo(streamID); ok && len(info.GetViewers()) != 0 {
		t.Fatalf("no viewers should remain, got %v", info.GetViewers())
	}
}

// TestSameUserTwoDevicesDifferentChannels covers a device leaving a channel it
// shares with another device of the same account for a new channel: the shared
// channel must stay up for the device still on it.
func TestSameUserTwoDevicesDifferentChannels(t *testing.T) {
	var opens1, opens2 int64
	srv1 := countingUpstream(t, &opens1)
	defer srv1.Close()
	srv2 := countingUpstream(t, &opens2)
	defer srv2.Close()

	sm := newTestManager()
	defer sm.Stop()

	_, tv, err := sm.RequestStream("lucas", "ch-a", "live", "A", mustURL(t, srv1.URL))
	if err != nil {
		t.Fatalf("RequestStream tv: %v", err)
	}
	// While the TV is watching ch-a, the phone is the switch-detection
	// candidate's "previous" stream owner: put a second lucas client on ch-a
	// first, so ch-a has two clients of the same user when the phone moves on.
	_, phone, err := sm.RequestStream("lucas", "ch-a", "live", "A", mustURL(t, srv1.URL))
	if err != nil {
		t.Fatalf("RequestStream phone on ch-a: %v", err)
	}
	stopTV := make(chan struct{})
	defer close(stopTV)
	go keepDraining(sm, "ch-a", tv, stopTV)

	// The phone zaps to ch-b, opening it before its ch-a connection closes.
	_, phoneB, err := sm.RequestStream("lucas", "ch-b", "live", "B", mustURL(t, srv2.URL))
	if err != nil {
		t.Fatalf("RequestStream phone on ch-b: %v", err)
	}
	sm.RemoveClient("ch-a", phone)

	if !streamActive(sm, "ch-a") {
		t.Fatal("ch-a stopped although the TV still watches it")
	}
	if !receiving(sm, "ch-b", phoneB, 2*time.Second) {
		t.Fatal("phone should receive ch-b")
	}
	if !streamActive(sm, "ch-a") || !streamActive(sm, "ch-b") {
		t.Fatal("both channels should stay up")
	}
	sm.RemoveClient("ch-b", phoneB)
	sm.RemoveClient("ch-a", tv)
}

// TestSameUserNewChannelReplacesOtherDevicesChannel pins down what happens when
// a second device of the same account opens a different channel while the
// first device is alone on its channel: the server cannot tell that from one
// device zapping (same user, usually the same IP), so the first channel is
// released for the new one. Providers commonly allow a single connection per
// account, so two channels at once could not both play anyway.
func TestSameUserNewChannelReplacesOtherDevicesChannel(t *testing.T) {
	var opens1, opens2 int64
	srv1 := countingUpstream(t, &opens1)
	defer srv1.Close()
	srv2 := countingUpstream(t, &opens2)
	defer srv2.Close()

	sm := newTestManager()
	defer sm.Stop()

	_, tv, err := sm.RequestStream("lucas", "ch-a", "live", "A", mustURL(t, srv1.URL))
	if err != nil {
		t.Fatalf("RequestStream tv: %v", err)
	}
	_, phone, err := sm.RequestStream("lucas", "ch-b", "live", "B", mustURL(t, srv2.URL))
	if err != nil {
		t.Fatalf("RequestStream phone: %v", err)
	}
	if streamActive(sm, "ch-a") {
		t.Fatal("ch-a should be released for the account's new channel")
	}
	if !receiving(sm, "ch-b", phone, 2*time.Second) {
		t.Fatal("the new channel should play")
	}
	sm.RemoveClient("ch-a", tv)
	if !streamActive(sm, "ch-b") {
		t.Fatal("the TV's handler exiting must not stop ch-b")
	}
	sm.RemoveClient("ch-b", phone)
}

// TestSingleDeviceSwitchStopsPrevious keeps the provider-friendly behavior for
// one device: opening a new channel releases the previous upstream at once,
// before the old connection's handler has noticed it was left.
func TestSingleDeviceSwitchStopsPrevious(t *testing.T) {
	var opens1, opens2 int64
	srv1 := countingUpstream(t, &opens1)
	defer srv1.Close()
	srv2 := countingUpstream(t, &opens2)
	defer srv2.Close()

	sm := newTestManager()
	defer sm.Stop()

	_, onA, err := sm.RequestStream("lucas", "ch-a", "live", "A", mustURL(t, srv1.URL))
	if err != nil {
		t.Fatalf("RequestStream ch-a: %v", err)
	}
	_, onB, err := sm.RequestStream("lucas", "ch-b", "live", "B", mustURL(t, srv2.URL))
	if err != nil {
		t.Fatalf("RequestStream ch-b: %v", err)
	}
	if streamActive(sm, "ch-a") {
		t.Fatal("previous channel should be stopped immediately on a single-device switch")
	}
	if !receiving(sm, "ch-b", onB, 2*time.Second) {
		t.Fatal("new channel should play")
	}
	// The old handler exits later and releases its own (already gone) client:
	// this must not disturb the new channel.
	sm.RemoveClient("ch-a", onA)
	if !streamActive(sm, "ch-b") {
		t.Fatal("releasing the old connection stopped the new channel")
	}
	sm.RemoveClient("ch-b", onB)
}

// TestReconnectDoesNotStopStream covers a device reconnecting to the channel it
// is watching: the new connection joins before the old handler exits, and the
// old one releasing its client must leave the stream (and new connection) up.
func TestReconnectDoesNotStopStream(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()

	sm := newTestManager()
	defer sm.Stop()

	const streamID = "reconnect"
	_, oldConn, err := sm.RequestStream("lucas", streamID, "live", "C", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream old: %v", err)
	}
	_, newConn, err := sm.RequestStream("lucas", streamID, "live", "C", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream new: %v", err)
	}
	sm.RemoveClient(streamID, oldConn)

	if !streamActive(sm, streamID) {
		t.Fatal("stream stopped when the old connection was released")
	}
	if !receiving(sm, streamID, newConn, 2*time.Second) {
		t.Fatal("new connection should keep receiving")
	}
	if o := atomic.LoadInt64(&opens); o != 1 {
		t.Fatalf("upstream opened %d times, want 1", o)
	}
	sm.RemoveClient(streamID, newConn)
}

// TestDisconnectUserDropsEveryDevice checks the admin disconnect still removes
// all of a user's connections.
func TestDisconnectUserDropsEveryDevice(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()

	sm := newTestManager()
	defer sm.Stop()

	const streamID = "kick"
	sm.RegisterUser("lucas", "10.0.0.1", "")
	if _, _, err := sm.RequestStream("lucas", streamID, "live", "C", mustURL(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sm.RequestStream("lucas", streamID, "live", "C", mustURL(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	sm.DisconnectUser("lucas")
	if streamActive(sm, streamID) {
		t.Fatal("stream should stop once every device of the user is disconnected")
	}
}

// TestSessionExpiryKeepsConnectedViewer checks that an old user session is not
// reaped while the user is still connected to a live stream.
func TestSessionExpiryKeepsConnectedViewer(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()

	sm := newTestManager()
	sm.SetSessionTimeout(20 * time.Millisecond)
	defer sm.Stop()

	const streamID = "long-watch"
	_, key, err := sm.RequestStream("lucas", streamID, "live", "C", mustURL(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, streamID, key, stop)

	time.Sleep(50 * time.Millisecond)
	sm.cleanupExpiredSessions()
	if !streamActive(sm, streamID) {
		t.Fatal("expiry sweep stopped a stream the user is still connected to")
	}
	if sm.GetUserSession("lucas") == nil {
		t.Fatal("connected user's session should not be reaped")
	}
	sm.RemoveClient(streamID, key)
}
