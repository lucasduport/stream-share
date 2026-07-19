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
func drain(sm *SessionManager, streamID, user string, got *int64, stop <-chan struct{}) {
	ch, ok := sm.GetClientChannel(streamID, user)
	if !ok {
		return
	}
	done, _ := sm.GetClientDone(streamID, user)
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
	if _, err := sm.RequestStream("alice", streamID, "live", "Channel 1", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream: %v", err)
	}

	var got int64
	doneDrain := make(chan struct{})
	go func() { drain(sm, streamID, "alice", &got, nil); close(doneDrain) }()

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
	if _, err := sm.RequestStream("alice", streamID, "live", "Channel 2", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream alice: %v", err)
	}
	var gotA, gotB int64
	doneA := make(chan struct{})
	go func() { drain(sm, streamID, "alice", &gotA, nil); close(doneA) }()

	// Second viewer joins shortly after.
	time.Sleep(10 * time.Millisecond)
	if _, err := sm.RequestStream("bob", streamID, "live", "Channel 2", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream bob: %v", err)
	}
	doneB := make(chan struct{})
	go func() { drain(sm, streamID, "bob", &gotB, nil); close(doneB) }()

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
	if _, err := sm.RequestStream("fast", streamID, "live", "Channel 3", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream fast: %v", err)
	}
	if _, err := sm.RequestStream("slow", streamID, "live", "Channel 3", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream slow: %v", err)
	}

	// "slow" never reads its channel; "fast" drains continuously. Once slow's
	// buffer fills, the pump blocks on it until the stall timeout drops it, after
	// which fast receives the remainder.
	var gotFast int64
	doneFast := make(chan struct{})
	go func() { drain(sm, streamID, "fast", &gotFast, nil); close(doneFast) }()

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
			if _, err := sm.RequestStream(user, streamID, "live", "Channel 4", mustURL(t, srv.URL)); err != nil {
				return
			}
			var got int64
			stop := make(chan struct{})
			d := make(chan struct{})
			go func() { drain(sm, streamID, user, &got, stop); close(d) }()
			time.Sleep(time.Duration(20+n*10) * time.Millisecond)
			sm.RemoveClient(streamID, user)
			close(stop)
			<-d
		}(i)
	}
	wg.Wait()
}
