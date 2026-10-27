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

package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Discord rejects commands whose required options come after optional ones.
func TestShareCommandSpecs(t *testing.T) {
	b := &Bot{}
	found := map[string]*discordgo.ApplicationCommand{}
	for _, c := range b.commandSpecs() {
		found[c.Name] = c
	}
	share, ok := found["share"]
	if !ok || found["shares"] == nil {
		t.Fatal("/share and /shares must be registered")
	}
	seenOptional := false
	for _, o := range share.Options {
		if !o.Required {
			seenOptional = true
		} else if seenOptional {
			t.Fatalf("required option %q after an optional one", o.Name)
		}
	}
}

func TestShareMessages(t *testing.T) {
	created := map[string]interface{}{
		"token": "abcdefghijklmnopqrstuv", "title": "XX| Some Channel", "state": "scheduled",
		"starts_at": "2026-10-28T20:00:00Z", "ends_at": "2026-10-28T22:00:00Z", "created_by": "alice",
		"urls": map[string]interface{}{"playlist": "https://tv.example/share/abcdefghijklmnopqrstuv.m3u", "stream": "https://tv.example/share/abcdefghijklmnopqrstuv/live.ts"},
	}
	msg := shareCreatedMessage(created, "")
	for _, want := range []string{"XX| Some Channel", "https://tv.example/share/abcdefghijklmnopqrstuv.m3u", "/live.ts", "VLC", "3 devices"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("created message lacks %q:\n%s", want, msg)
		}
	}

	ended := map[string]interface{}{"token": "zzzzzz", "title": "Old", "state": "ended",
		"starts_at": "2026-10-20T20:00:00Z", "ends_at": "2026-10-20T22:00:00Z", "created_by": "alice"}
	content, buttons := renderShareList([]interface{}{created, ended})
	if len(buttons) != 1 || buttons[0].CustomID != shareRevokePrefix+"abcdefghijklmnopqrstuv" {
		t.Fatalf("buttons = %+v (only links not over yet get one)", buttons)
	}
	if !strings.Contains(content, "1. XX| Some Channel") || !strings.Contains(content, "2. Old") || strings.Contains(content, "zzzzzz") {
		t.Fatalf("list:\n%s", content)
	}

	many := make([]discordgo.Button, 12)
	if rows := shareButtonRows(many); len(rows) != 3 {
		t.Fatalf("12 buttons → %d rows, want 3 (5 per row)", len(rows))
	}
}
