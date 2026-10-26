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

// Package provider arbitrates the media connections opened to the upstream
// provider.
//
// Providers limit how many connections an account may hold at once, often to a
// single one, and cut the oldest when a new one opens. Without arbitration a
// background cache download (an item nobody watches any more) and what a
// viewer is watching (a live channel, another movie) each reopen their
// connection after being cut, and keep cutting each other.
//
// An Arbiter hands out slots up to the provider's limit. Every holder declares,
// live, whether it is in the foreground (someone is watching it) or in the
// background. A foreground request that finds no free slot preempts a
// background holder: the holder is asked to close its connection, waits for a
// slot again and resumes where it stopped. Background requests never preempt
// anything; they get free slots in arrival order.
package provider

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// Mode says how far an Acquire may go to get a slot.
type Mode int

const (
	// Resume may preempt background holders (when the caller is in the
	// foreground) but never another foreground holder. Used when a holder
	// that was preempted or retrying wants its slot back, so two foreground
	// consumers cannot take the slot from each other in turn.
	Resume Mode = iota
	// Fresh is a new consumer: in the foreground it may also preempt the
	// oldest foreground holder when no background one is left. The provider
	// cannot serve both anyway, and would cut the older connection itself;
	// the most recent request (e.g. the item a viewer just switched to) wins.
	Fresh
)

// preemptGrace is how long a foreground request waits for a preempted holder
// to give its slot back before taking a slot anyway. Holders close their
// connection at their next read, so this only matters if one is stuck.
var preemptGrace = 10 * time.Second

// pollInterval re-evaluates waiting requests, whose foreground state can
// change without the arbiter being told (a viewer arriving or leaving).
var pollInterval = 250 * time.Millisecond

// Arbiter hands out provider connection slots. A nil *Arbiter grants every
// request at once.
type Arbiter struct {
	capacity func() int // <= 0: no limit

	mu      sync.Mutex
	seq     uint64
	held    map[*Lease]struct{}
	waiters []*waiter
	changed chan struct{}
}

type waiter struct {
	seq        uint64
	label      string
	foreground func() bool
	mode       Mode
}

// Lease is a granted slot. Release it when the connection is closed for good
// or before waiting for a new slot.
type Lease struct {
	a           *Arbiter
	label       string
	foreground  func() bool
	seq         uint64
	since       time.Time
	preempt     chan struct{}
	preemptOnce sync.Once
	preemptedAt time.Time // guarded by a.mu
}

// New returns an arbiter whose capacity (the most slots held at once) is read
// from capacity on every decision, so a limit learned later applies at once.
func New(capacity func() int) *Arbiter {
	return &Arbiter{capacity: capacity, held: map[*Lease]struct{}{}, changed: make(chan struct{})}
}

// noopLease is what a nil arbiter grants: never preempted.
func noopLease(label string) *Lease {
	return &Lease{label: label, preempt: make(chan struct{})}
}

// Acquire waits for a slot for the connection named label. foreground reports,
// whenever asked, whether something is being watched through it. It returns an
// error only when ctx ends first.
func (a *Arbiter) Acquire(ctx context.Context, label string, foreground func() bool, mode Mode) (*Lease, error) {
	if a == nil {
		return noopLease(label), nil
	}
	if foreground == nil {
		foreground = func() bool { return false }
	}
	a.mu.Lock()
	a.seq++
	w := &waiter{seq: a.seq, label: label, foreground: foreground, mode: mode}
	a.waiters = append(a.waiters, w)
	var waitingSince time.Time
	for {
		capacity := a.capacity()
		fg := foreground()
		if capacity <= 0 || (len(a.held) < capacity && a.firstInLineLocked(w, fg)) {
			l := a.grantLocked(w)
			a.mu.Unlock()
			return l, nil
		}
		if fg {
			victim := a.backgroundHolderLocked()
			if victim == nil && mode == Fresh {
				victim = a.oldestForegroundHolderLocked()
				if victim != nil && victim.preemptedAt.IsZero() {
					utils.WarnLog("Provider slot: %s and %s are both being watched but the provider allows %d connection(s); %s takes over",
						victim.label, label, capacity, label)
				}
			}
			if victim != nil {
				a.preemptLocked(victim, label)
				if waitingSince.IsZero() {
					waitingSince = time.Now()
				}
			}
			// A holder that does not give its slot back in time must not
			// leave a viewer waiting forever: take a slot over the limit.
			if !waitingSince.IsZero() && time.Since(waitingSince) > preemptGrace && a.firstInLineLocked(w, fg) {
				utils.WarnLog("Provider slot: a preempted connection did not close within %s; opening %s anyway", preemptGrace, label)
				l := a.grantLocked(w)
				a.mu.Unlock()
				return l, nil
			}
		}
		ch := a.changed
		a.mu.Unlock()
		timer := time.NewTimer(pollInterval)
		select {
		case <-ch:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			a.mu.Lock()
			a.removeWaiterLocked(w)
			a.notifyLocked()
			a.mu.Unlock()
			return nil, ctx.Err()
		}
		timer.Stop()
		a.mu.Lock()
	}
}

// firstInLineLocked reports whether no other waiter should get a free slot
// before w: foreground before background, then arrival order.
func (a *Arbiter) firstInLineLocked(w *waiter, fg bool) bool {
	for _, o := range a.waiters {
		if o == w {
			continue
		}
		ofg := o.foreground()
		if ofg && !fg {
			return false
		}
		if ofg == fg && o.seq < w.seq {
			return false
		}
	}
	return true
}

func (a *Arbiter) grantLocked(w *waiter) *Lease {
	a.removeWaiterLocked(w)
	l := &Lease{a: a, label: w.label, foreground: w.foreground, seq: w.seq, since: time.Now(), preempt: make(chan struct{})}
	a.held[l] = struct{}{}
	a.notifyLocked()
	return l
}

func (a *Arbiter) removeWaiterLocked(w *waiter) {
	for i, o := range a.waiters {
		if o == w {
			a.waiters = append(a.waiters[:i], a.waiters[i+1:]...)
			return
		}
	}
}

// backgroundHolderLocked returns a holder nobody watches, preferring one that
// is already being preempted (its slot is about to free), then the newest.
func (a *Arbiter) backgroundHolderLocked() *Lease {
	var pick *Lease
	for l := range a.held {
		if l.foreground() {
			continue
		}
		if !l.preemptedAt.IsZero() {
			return l
		}
		if pick == nil || l.seq > pick.seq {
			pick = l
		}
	}
	return pick
}

// oldestForegroundHolderLocked returns the foreground holder granted first,
// preferring one already being preempted.
func (a *Arbiter) oldestForegroundHolderLocked() *Lease {
	var pick *Lease
	for l := range a.held {
		if !l.preemptedAt.IsZero() {
			return l
		}
		if pick == nil || l.seq < pick.seq {
			pick = l
		}
	}
	return pick
}

func (a *Arbiter) preemptLocked(l *Lease, forLabel string) {
	if !l.preemptedAt.IsZero() {
		return
	}
	l.preemptedAt = time.Now()
	utils.InfoLog("Provider slot: pausing %s for %s", l.label, forLabel)
	l.preemptOnce.Do(func() { close(l.preempt) })
}

func (a *Arbiter) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

// Preempted is closed when the arbiter wants this slot back: the holder must
// close its connection and Release.
func (l *Lease) Preempted() <-chan struct{} {
	return l.preempt
}

// IsPreempted reports whether Preempted is closed.
func (l *Lease) IsPreempted() bool {
	select {
	case <-l.preempt:
		return true
	default:
		return false
	}
}

// Release gives the slot back. Safe to call more than once.
func (l *Lease) Release() {
	if l == nil || l.a == nil {
		return
	}
	a := l.a
	a.mu.Lock()
	if _, ok := a.held[l]; ok {
		delete(a.held, l)
		a.notifyLocked()
	}
	a.mu.Unlock()
}

// SlotState describes one holder or waiter, for status pages.
type SlotState struct {
	Label      string    `json:"label"`
	Foreground bool      `json:"foreground"`
	Since      time.Time `json:"since,omitempty"`
	Pausing    bool      `json:"pausing,omitempty"`
}

// Snapshot is the arbiter's state, for status pages.
type Snapshot struct {
	Capacity int         `json:"capacity"` // 0: no limit
	Held     []SlotState `json:"held"`
	Waiting  []SlotState `json:"waiting"`
}

// Snapshot returns the current holders (oldest first) and waiters.
func (a *Arbiter) Snapshot() Snapshot {
	if a == nil {
		return Snapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Snapshot{Capacity: a.capacity(), Held: []SlotState{}, Waiting: []SlotState{}}
	if s.Capacity < 0 {
		s.Capacity = 0
	}
	held := make([]*Lease, 0, len(a.held))
	for l := range a.held {
		held = append(held, l)
	}
	sort.Slice(held, func(i, j int) bool { return held[i].seq < held[j].seq })
	for _, l := range held {
		s.Held = append(s.Held, SlotState{Label: l.label, Foreground: l.foreground(), Since: l.since, Pausing: !l.preemptedAt.IsZero()})
	}
	for _, w := range a.waiters {
		s.Waiting = append(s.Waiting, SlotState{Label: w.label, Foreground: w.foreground()})
	}
	return s
}
