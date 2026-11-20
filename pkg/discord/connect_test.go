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

package discord

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

func TestIsPermanentGatewayError(t *testing.T) {
	for _, code := range []int{4004, 4010, 4011, 4012, 4013, 4014} {
		if !isPermanentGatewayError(&websocket.CloseError{Code: code}) {
			t.Errorf("code %d: want permanent", code)
		}
	}
	for _, err := range []error{&websocket.CloseError{Code: 4000}, &websocket.CloseError{Code: 1006}, errors.New("dial tcp: no such host"), nil} {
		if isPermanentGatewayError(err) {
			t.Errorf("%v: want retryable", err)
		}
	}
}

func TestConnectWithRetry_RetriesUntilSuccess(t *testing.T) {
	calls := 0
	open := func() error {
		calls++
		if calls < 3 {
			return errors.New("unreachable")
		}
		return nil
	}
	if !connectWithRetry(open, make(chan struct{}), time.Millisecond) || calls != 3 {
		t.Fatalf("got calls=%d, want success after 3", calls)
	}
}

func TestConnectWithRetry_StopsOnPermanentError(t *testing.T) {
	calls := 0
	open := func() error { calls++; return &websocket.CloseError{Code: 4004} }
	if connectWithRetry(open, make(chan struct{}), time.Millisecond) || calls != 1 {
		t.Fatalf("got calls=%d, want 1 attempt and false", calls)
	}
}

func TestConnectWithRetry_AlreadyOpenIsSuccess(t *testing.T) {
	calls := 0
	open := func() error { calls++; return discordgo.ErrWSAlreadyOpen }
	if !connectWithRetry(open, make(chan struct{}), time.Millisecond) || calls != 1 {
		t.Fatalf("got calls=%d, want 1 attempt and true", calls)
	}
}

func TestConnectWithRetry_StopEndsLoop(t *testing.T) {
	stop := make(chan struct{})
	open := func() error { return errors.New("unreachable") }
	done := make(chan bool)
	go func() { done <- connectWithRetry(open, stop, time.Hour) }()
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("want false after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("loop did not exit after stop")
	}
}
