/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License
 * as published by the Free Software Foundation, either version 3 of the
 * Free Software Foundation, either version 3 of the License, or
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
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/lucasduport/stream-share/pkg/types"
)

func newTestBot() *Bot {
	return &Bot{
		pendingRetry: make(map[string]*retryContext),
	}
}

func TestRetryContextTTLExpiry(t *testing.T) {
	b := newTestBot()
	b.pendingRetry["msg1"] = &retryContext{Kind: retrySearch, Created: time.Now().Add(-retryTTL - time.Minute)}
	b.pendingRetry["msg2"] = &retryContext{Kind: retryDownload, Created: time.Now()}

	b.cleanupExpiredRetries()

	if _, ok := b.pendingRetry["msg1"]; ok {
		t.Fatal("expected expired retry context to be removed")
	}
	if _, ok := b.pendingRetry["msg2"]; !ok {
		t.Fatal("expected fresh retry context to be kept")
	}
}

func TestClearRetry(t *testing.T) {
	b := newTestBot()
	b.pendingRetry["msg1"] = &retryContext{Kind: retryCache, Created: time.Now()}

	b.clearRetry(nil) // must not panic
	b.clearRetry(&discordgo.Message{ID: "msg1", ChannelID: "c1"})

	if _, ok := b.pendingRetry["msg1"]; ok {
		t.Fatal("expected retry context to be cleared")
	}
}

func TestAPIErrorMessage(t *testing.T) {
	// err takes priority
	got := apiErrorMessage("Failed to create download", errTest{}, map[string]interface{}{"Error": "api says no"})
	if got != "Failed to create download: boom" {
		t.Fatalf("expected err to win, got %q", got)
	}
	// falls back to API Error field
	got = apiErrorMessage("Failed to start caching", nil, map[string]interface{}{"Error": "timeout"})
	if got != "Failed to start caching: timeout" {
		t.Fatalf("expected API error, got %q", got)
	}
	// base only when neither present
	got = apiErrorMessage("Failed", nil, nil)
	if got != "Failed" {
		t.Fatalf("expected base only, got %q", got)
	}
	got = apiErrorMessage("Failed", nil, map[string]interface{}{"Error": 42})
	if got != "Failed" {
		t.Fatalf("expected base only for non-string Error, got %q", got)
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom" }

func TestRetryGuardDenials(t *testing.T) {
	b := newTestBot()
	// A retry from the same user (Member.User.ID matches ctx.UserID).
	inter := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Member: &discordgo.Member{User: &discordgo.User{ID: "u1"}},
	}}

	// InFlight guard
	ctx := &retryContext{Kind: retrySearch, UserID: "u1", Created: time.Now(), InFlight: true}
	if deny := b.retryGuard(ctx, inter); deny == "" {
		t.Fatal("expected denial for InFlight context")
	}

	// Max attempts guard
	ctx = &retryContext{Kind: retrySearch, UserID: "u1", Created: time.Now(), Attempts: retryMaxAttempts}
	if deny := b.retryGuard(ctx, inter); deny == "" {
		t.Fatal("expected denial for max attempts")
	}

	// Backoff guard
	ctx = &retryContext{Kind: retrySearch, UserID: "u1", Created: time.Now(), LastAttempt: time.Now()}
	if deny := b.retryGuard(ctx, inter); deny == "" {
		t.Fatal("expected denial inside backoff window")
	}

	// Wrong user guard
	ctx = &retryContext{Kind: retrySearch, UserID: "u1", Created: time.Now()}
	other := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Member: &discordgo.Member{User: &discordgo.User{ID: "u2"}},
	}}
	if deny := b.retryGuard(ctx, other); deny == "" {
		t.Fatal("expected denial for wrong user")
	}

	// Fresh context: no denial
	if deny := b.retryGuard(ctx, inter); deny != "" {
		t.Fatalf("expected no denial for fresh context, got %q", deny)
	}
}

func TestRetryContextStoresSearchParams(t *testing.T) {
	ctx := &retryContext{Kind: retrySearch, UserID: "u1", ChannelID: "c1", Query: "game of thrones", Days: 3}
	if ctx.Kind != retrySearch || ctx.Query != "game of thrones" || ctx.Days != 3 {
		t.Fatalf("unexpected context: %+v", ctx)
	}
}

func TestRetryContextStoresSelection(t *testing.T) {
	sel := types.VODResult{StreamID: "42", Title: "Movie", StreamType: "movie"}
	ctx := &retryContext{Kind: retryDownload, UserID: "u1", ChannelID: "c1", Selected: sel}
	if ctx.Selected.StreamID != "42" {
		t.Fatalf("unexpected selection: %+v", ctx.Selected)
	}
}

func TestRetryButtonRow(t *testing.T) {
	row := retryButtonRow(true)
	if len(row) != 1 {
		t.Fatalf("expected 1 action row, got %d", len(row))
	}
	ar, ok := row[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow, got %T", row[0])
	}
	if len(ar.Components) != 1 {
		t.Fatalf("expected 1 button, got %d", len(ar.Components))
	}
	btn, ok := ar.Components[0].(discordgo.Button)
	if !ok {
		t.Fatalf("expected Button, got %T", ar.Components[0])
	}
	if btn.CustomID != "retry" {
		t.Fatalf("expected customID=retry, got %q", btn.CustomID)
	}
	if !btn.Disabled {
		t.Fatal("expected button to be disabled")
	}

	enabled := retryButtonRow(false)
	ar2 := enabled[0].(discordgo.ActionsRow)
	btn2 := ar2.Components[0].(discordgo.Button)
	if btn2.Disabled {
		t.Fatal("expected button to be enabled")
	}
}

func TestRetryBackoffWindow(t *testing.T) {
	// A retry clicked within retryBackoff of the previous attempt must be
	// rejected by the handler guard; verify the guard condition directly.
	ctx := &retryContext{Kind: retrySearch, Created: time.Now()}
	ctx.LastAttempt = time.Now()
	if time.Since(ctx.LastAttempt) >= retryBackoff {
		t.Fatal("expected last attempt to be within backoff window")
	}
	ctx.LastAttempt = time.Now().Add(-retryBackoff - time.Second)
	if time.Since(ctx.LastAttempt) < retryBackoff {
		t.Fatal("expected last attempt to be outside backoff window")
	}
}

func TestRetryMaxAttempts(t *testing.T) {
	ctx := &retryContext{Kind: retryCache, Created: time.Now()}
	for i := 0; i < retryMaxAttempts; i++ {
		if ctx.Attempts >= retryMaxAttempts {
			t.Fatalf("attempt %d should still be allowed", i)
		}
		ctx.Attempts++
	}
	if ctx.Attempts < retryMaxAttempts {
		t.Fatal("expected attempts to reach the cap")
	}
}
