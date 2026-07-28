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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/slate"
)

// fakeSlate stands in for the ffmpeg-backed generator so these tests run
// anywhere. The fixture is recognisable bytes rather than real MPEG-TS.
type fakeSlate struct {
	path      string
	available bool
	calls     int32
	lastView  atomic.Value // slate.View
}

func newFakeSlate(t *testing.T, payload []byte) *fakeSlate {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slate.ts")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return &fakeSlate{path: path, available: true}
}

func (f *fakeSlate) Available() bool { return f.available }

func (f *fakeSlate) Clip(v slate.View) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastView.Store(v)
	return f.path, nil
}

// slateManager builds a manager wired for fast, deterministic slate tests.
func slateManager(t *testing.T, fake *fakeSlate) *SessionManager {
	t.Helper()
	sm := NewSessionManager(nil)
	sm.SetStreamTimeout(50 * time.Millisecond)
	sm.SetErrorCatalog(LoadCatalog(""))
	if fake != nil {
		sm.SetSlateProvider(fake)
		sm.SetSlateRetryMax(2 * time.Second)
	}
	return sm
}

// TestSlateServedOnUpstreamFailure is the core promise of the feature: a viewer
// whose provider returned 403 keeps receiving bytes instead of a dead channel.
func TestSlateServedOnUpstreamFailure(t *testing.T) {
	payload := bytes.Repeat([]byte("SLATE"), 4096)
	fake := newFakeSlate(t, payload)
	sm := slateManager(t, fake)
	defer sm.Stop()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	streamID := "chan-403"
	buffer, err := sm.RequestStream("viewer", streamID, "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}

	select {
	case <-buffer.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("buffer never became ready")
	}

	startErr := buffer.StartError()
	if startErr == nil || startErr.Key != "403" {
		t.Fatalf("StartError = %+v, want key 403", startErr)
	}
	if !sm.ServesSlateFor(streamID) {
		t.Fatal("ServesSlateFor should be true for a live stream with a slate provider")
	}

	var got int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { drain(sm, streamID, "viewer", &got, stop); close(done) }()

	// Give the pump time to render and start pacing the slate out.
	time.Sleep(700 * time.Millisecond)
	close(stop)
	<-done

	if atomic.LoadInt64(&got) == 0 {
		t.Fatal("viewer received no slate bytes; the stream was dropped instead")
	}
	if n := atomic.LoadInt32(&fake.calls); n != 1 {
		t.Fatalf("slate rendered %d times, want exactly 1 for one failed stream", n)
	}

	view, _ := fake.lastView.Load().(slate.View)
	if view.Code != "403" {
		t.Fatalf("slate view code = %q, want 403", view.Code)
	}
	if view.Meaning != "Forbidden" {
		t.Fatalf("slate view meaning = %q, want Forbidden", view.Meaning)
	}
	if view.Message == "" {
		t.Fatal("slate view message should not be empty")
	}
}

// TestSlateSkippedForVOD guards the scope decision: VOD is served over byte
// ranges, so splicing a TS clip into it would corrupt the response.
func TestSlateSkippedForVOD(t *testing.T) {
	fake := newFakeSlate(t, []byte("SLATE"))
	sm := slateManager(t, fake)
	defer sm.Stop()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	streamID := "movie-403"
	buffer, err := sm.RequestStream("viewer", streamID, "movie", "A Movie", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}

	select {
	case <-buffer.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("buffer never became ready")
	}

	if sm.ServesSlateFor(streamID) {
		t.Fatal("VOD streams must not be served an error slate")
	}
	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt32(&fake.calls); n != 0 {
		t.Fatalf("slate rendered %d times for VOD, want 0", n)
	}
}

// TestSlateDisabledKeepsOldBehaviour is the regression guard: with no provider
// configured, a failed stream tears down exactly as it did before.
func TestSlateDisabledKeepsOldBehaviour(t *testing.T) {
	sm := slateManager(t, nil) // no slate provider
	defer sm.Stop()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	streamID := "chan-nofallback"
	buffer, err := sm.RequestStream("viewer", streamID, "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}

	select {
	case <-buffer.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("buffer never became ready")
	}
	if sm.ServesSlateFor(streamID) {
		t.Fatal("ServesSlateFor should be false with no slate provider")
	}

	// The old behavior is that the buffer is deactivated and the viewer's
	// channel goes away, which is what GetClientChannel reports. (stopStream
	// deliberately leaves the StreamSession in place with Active=false, so
	// checking GetStreamInfo would not capture this.)
	deadline := time.Now().Add(2 * time.Second)
	dropped := false
	for time.Now().Before(deadline) {
		if _, ok := sm.GetClientChannel(streamID, "viewer"); !ok {
			dropped = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !dropped {
		t.Fatal("viewer should have been dropped when no slate is configured")
	}
}

// TestSlateResumesWhenUpstreamRecovers covers the retry/resume handoff: the
// viewer is on the slate, the provider comes back, and real payload flows.
func TestSlateResumesWhenUpstreamRecovers(t *testing.T) {
	fake := newFakeSlate(t, bytes.Repeat([]byte("S"), 2048))
	sm := slateManager(t, fake)
	sm.SetSlateRetryMax(20 * time.Second)
	defer sm.Stop()

	var healthy atomic.Bool
	realPayload := bytes.Repeat([]byte("R"), 64*1024)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			if _, err := w.Write(realPayload); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer srv.Close()

	streamID := "chan-recover"
	if _, err := sm.RequestStream("viewer", streamID, "live", "Channel", mustURL(t, srv.URL)); err != nil {
		t.Fatalf("RequestStream: %v", err)
	}

	// Collect everything the viewer receives so we can look for real payload.
	received := make(chan []byte, 4096)
	stop := make(chan struct{})
	go func() {
		ch, ok := sm.GetClientChannel(streamID, "viewer")
		if !ok {
			return
		}
		done, _ := sm.GetClientDone(streamID, "viewer")
		for {
			select {
			case data, open := <-ch:
				if !open {
					return
				}
				select {
				case received <- data:
				default:
				}
			case <-done:
				return
			case <-stop:
				return
			}
		}
	}()

	time.Sleep(400 * time.Millisecond) // let the slate start
	healthy.Store(true)                // provider comes back

	deadline := time.After(15 * time.Second)
	sawReal := false
	for !sawReal {
		select {
		case data := <-received:
			if bytes.Contains(data, []byte("RRRR")) {
				sawReal = true
			}
		case <-deadline:
			close(stop)
			t.Fatal("never resumed real video after the upstream recovered")
		}
	}
	close(stop)
}
