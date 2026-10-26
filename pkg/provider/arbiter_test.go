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

package provider

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func fixed(n int) func() int { return func() int { return n } }

func flag(v bool) (func() bool, *atomic.Bool) {
	var b atomic.Bool
	b.Store(v)
	return b.Load, &b
}

// acquireAsync starts an Acquire and returns a channel receiving its lease.
func acquireAsync(t *testing.T, a *Arbiter, ctx context.Context, label string, fg func() bool, mode Mode) <-chan *Lease {
	t.Helper()
	ch := make(chan *Lease, 1)
	go func() {
		l, err := a.Acquire(ctx, label, fg, mode)
		if err == nil {
			ch <- l
		}
	}()
	return ch
}

func got(ch <-chan *Lease, d time.Duration) *Lease {
	select {
	case l := <-ch:
		return l
	case <-time.After(d):
		return nil
	}
}

func preempted(l *Lease, d time.Duration) bool {
	select {
	case <-l.Preempted():
		return true
	case <-time.After(d):
		return false
	}
}

func TestNilAndUnlimitedArbitersGrantAtOnce(t *testing.T) {
	var nilArb *Arbiter
	l, err := nilArb.Acquire(context.Background(), "x", nil, Fresh)
	if err != nil || l == nil || l.IsPreempted() {
		t.Fatalf("nil arbiter: %v %v", l, err)
	}
	l.Release()

	a := New(fixed(0))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := a.Acquire(ctx, "x", nil, Fresh); err != nil {
			t.Fatal(err)
		}
	}
	if s := a.Snapshot(); len(s.Held) != 5 || s.Capacity != 0 {
		t.Fatalf("unlimited snapshot: %+v", s)
	}
}

// Background requests never preempt; they get freed slots in arrival order.
func TestBackgroundWaitsInOrderAndNeverPreempts(t *testing.T) {
	a := New(fixed(1))
	ctx := context.Background()
	first, _ := a.Acquire(ctx, "first", nil, Fresh)
	second := acquireAsync(t, a, ctx, "second", nil, Fresh)
	time.Sleep(50 * time.Millisecond)
	third := acquireAsync(t, a, ctx, "third", nil, Fresh)

	if preempted(first, 400*time.Millisecond) {
		t.Fatal("a background request preempted a holder")
	}
	first.Release()
	l2 := got(second, time.Second)
	if l2 == nil {
		t.Fatal("second did not get the freed slot")
	}
	if got(third, 300*time.Millisecond) != nil {
		t.Fatal("third got a slot while second holds the only one")
	}
	l2.Release()
	if l3 := got(third, time.Second); l3 == nil {
		t.Fatal("third did not get the slot")
	} else {
		l3.Release()
	}
}

// A foreground request preempts a background holder and gets its slot once it
// is released.
func TestForegroundPreemptsBackground(t *testing.T) {
	a := New(fixed(1))
	ctx := context.Background()
	bg, _ := a.Acquire(ctx, "download", nil, Fresh)
	fgFn, _ := flag(true)
	fg := acquireAsync(t, a, ctx, "live", fgFn, Fresh)
	if !preempted(bg, time.Second) {
		t.Fatal("background holder was not preempted")
	}
	if got(fg, 200*time.Millisecond) != nil {
		t.Fatal("foreground granted before the background slot was released")
	}
	bg.Release()
	if l := got(fg, time.Second); l == nil {
		t.Fatal("foreground did not get the slot")
	}
}

// Fresh foreground takes the slot from an older foreground holder; Resume
// foreground does not, so two watched streams cannot take it back in turn.
func TestFreshForegroundWinsResumeWaits(t *testing.T) {
	a := New(fixed(1))
	ctx := context.Background()
	fgFn, _ := flag(true)
	older, _ := a.Acquire(ctx, "older", fgFn, Fresh)

	resume := acquireAsync(t, a, ctx, "resume", fgFn, Resume)
	if preempted(older, 400*time.Millisecond) {
		t.Fatal("a Resume request preempted a foreground holder")
	}
	newer := acquireAsync(t, a, ctx, "newer", fgFn, Fresh)
	if !preempted(older, time.Second) {
		t.Fatal("a Fresh foreground request did not preempt the older foreground holder")
	}
	older.Release()
	// The Resume request was first in line among foreground waiters.
	first := got(resume, time.Second)
	if first == nil {
		t.Fatal("no waiter got the freed slot")
	}
	first.Release()
	if l := got(newer, time.Second); l == nil {
		t.Fatal("newer never got a slot")
	}
}

// A waiter that becomes foreground while waiting starts preempting.
func TestWaiterTurningForegroundPreempts(t *testing.T) {
	a := New(fixed(1))
	ctx := context.Background()
	bg, _ := a.Acquire(ctx, "download A", nil, Fresh)
	fn, watched := flag(false)
	waiting := acquireAsync(t, a, ctx, "download B", fn, Resume)
	if preempted(bg, 400*time.Millisecond) {
		t.Fatal("a background waiter preempted")
	}
	watched.Store(true) // a viewer arrives for B
	if !preempted(bg, time.Second) {
		t.Fatal("B, now watched, did not preempt A")
	}
	bg.Release()
	if l := got(waiting, time.Second); l == nil {
		t.Fatal("B did not get the slot")
	}
}

// Foreground waiters go before background waiters when a slot frees.
func TestForegroundWaitersFirst(t *testing.T) {
	a := New(fixed(1))
	ctx := context.Background()
	fgFn, _ := flag(true)
	holder, _ := a.Acquire(ctx, "holder", fgFn, Fresh)
	bgWaiter := acquireAsync(t, a, ctx, "background", nil, Fresh)
	time.Sleep(50 * time.Millisecond)
	fgWaiter := acquireAsync(t, a, ctx, "watched", fgFn, Resume)
	time.Sleep(50 * time.Millisecond)
	holder.Release()
	if l := got(fgWaiter, time.Second); l == nil {
		t.Fatal("the watched waiter should get the slot first")
	} else {
		l.Release()
	}
	if l := got(bgWaiter, time.Second); l == nil {
		t.Fatal("the background waiter should get the slot next")
	}
}

// A cancelled wait leaves the queue and does not block others.
func TestCancelledWaitLeavesQueue(t *testing.T) {
	a := New(fixed(1))
	holder, _ := a.Acquire(context.Background(), "holder", nil, Fresh)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.Acquire(ctx, "gone", nil, Fresh)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled Acquire should fail")
	}
	next := acquireAsync(t, a, context.Background(), "next", nil, Fresh)
	holder.Release()
	if l := got(next, time.Second); l == nil {
		t.Fatal("the cancelled waiter blocked the queue")
	}
	if s := a.Snapshot(); len(s.Waiting) != 0 {
		t.Fatalf("waiters left: %+v", s.Waiting)
	}
}

// A preempted holder that never releases cannot leave a viewer waiting
// forever: after the grace the foreground request gets a slot anyway.
func TestStuckHolderDoesNotBlockForeverAfterGrace(t *testing.T) {
	old := preemptGrace
	preemptGrace = 200 * time.Millisecond
	t.Cleanup(func() { preemptGrace = old })
	a := New(fixed(1))
	stuck, _ := a.Acquire(context.Background(), "stuck", nil, Fresh)
	fgFn, _ := flag(true)
	start := time.Now()
	l, err := a.Acquire(context.Background(), "watched", fgFn, Fresh)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < preemptGrace {
		t.Fatalf("granted after %v, before the grace", d)
	}
	if !stuck.IsPreempted() {
		t.Fatal("stuck holder was not asked to pause")
	}
}
