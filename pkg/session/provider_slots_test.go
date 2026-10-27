/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/provider"
)

// A live stream someone starts watching takes the provider's only connection
// from a background download (e.g. a VOD cache download nobody watches): the
// download is told to pause before the live stream dials, and gets its slot
// back once the live stream ends.
func TestLiveStreamPreemptsBackgroundDownload(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()

	slots := provider.New(func() int { return 1 })
	sm := newTestManager()
	defer sm.Stop()
	sm.SetProviderSlots(slots)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	background, err := slots.Acquire(ctx, "background download", func() bool { return false }, provider.Fresh)
	if err != nil {
		t.Fatal(err)
	}
	// The download closes its connection when asked, then waits for a slot.
	paused := make(chan struct{})
	resumed := make(chan *provider.Lease, 1)
	go func() {
		<-background.Preempted()
		close(paused)
		background.Release()
		l, aerr := slots.Acquire(ctx, "background download", func() bool { return false }, provider.Resume)
		if aerr == nil {
			resumed <- l
		}
	}()

	_, key, err := sm.RequestStream("viewer", "live-1", "live", "Live 1", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("the background download was not asked to pause")
	}
	if !receiving(sm, "live-1", key, 3*time.Second) {
		t.Fatal("the live stream should play once the slot is free")
	}
	select {
	case <-resumed:
		t.Fatal("the background download must wait while the live stream holds the slot")
	case <-time.After(400 * time.Millisecond):
	}

	removeKey(sm, "live-1", key) // the viewer leaves: the stream stops
	select {
	case l := <-resumed:
		l.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("the background download did not get its slot back after the live stream ended")
	}
}

// Without an arbiter (nil) live streams dial as before.
func TestLiveStreamWithoutArbiter(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()
	sm := newTestManager()
	defer sm.Stop()

	_, key, err := sm.RequestStream("viewer", "live-2", "live", "Live 2", mustURL(t, srv.URL))
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}
	if !receiving(sm, "live-2", key, 3*time.Second) {
		t.Fatal("the live stream should play")
	}
	removeKey(sm, "live-2", key)
}
