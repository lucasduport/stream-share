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

package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/types"
)

// The channel number comes from the cached live catalogue, whatever type the
// decoder gave its fields, and follows a refreshed catalogue.
func TestLiveChannelNumberFromCachedCatalogue(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig("http://provider.invalid")
	key := c.catalogKey("get_live_streams", "")

	if _, ok := c.liveChannelNumber("506638"); ok {
		t.Fatal("no catalogue cached: number must be unknown")
	}

	storeCachedCatalog(key, []interface{}{
		map[string]interface{}{"num": json.Number("41"), "stream_id": json.Number("506638")},
		map[string]interface{}{"num": float64(7), "stream_id": "9"},
		map[string]interface{}{"num": "12", "stream_id": "10"},
		map[string]interface{}{"num": "", "stream_id": "11"},
	})
	for id, want := range map[string]int{"506638": 41, "506638.ts": 41, "9": 7, "10": 12} {
		if n, ok := c.liveChannelNumber(id); !ok || n != want {
			t.Errorf("liveChannelNumber(%q) = %d, %v; want %d", id, n, ok, want)
		}
	}
	if _, ok := c.liveChannelNumber("11"); ok {
		t.Error("an entry without a number must stay unknown")
	}

	storeCachedCatalog(key, []interface{}{
		map[string]interface{}{"num": json.Number("42"), "stream_id": json.Number("506638")},
	})
	if n, _ := c.liveChannelNumber("506638"); n != 42 {
		t.Fatalf("after a catalogue refresh got %d, want 42", n)
	}
}

func TestStreamRef(t *testing.T) {
	if got := streamRef(activeStreamItem{StreamID: "506638", Num: 41}); got != "(n°41 · id 506638)" {
		t.Errorf("with number: %q", got)
	}
	if got := streamRef(activeStreamItem{StreamID: "1717919.mkv"}); got != "(id 1717919)" {
		t.Errorf("without number: %q", got)
	}
}

// A live stream's status entry carries its channel number when the catalogue
// knows it.
func TestActiveStreamItemLiveNumber(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig("http://provider.invalid")
	storeCachedCatalog(c.catalogKey("get_live_streams", ""), []interface{}{
		map[string]interface{}{"num": json.Number("41"), "stream_id": json.Number("506638")},
	})
	s := &types.StreamSession{StreamID: "506638", StreamType: "live", StreamTitle: "XX| CHANNEL FHD",
		StartTime: time.Now(), Viewers: map[string]time.Time{}}
	if it := c.buildActiveStreamItem(s, nil); it.Num != 41 {
		t.Fatalf("live item num = %d, want 41", it.Num)
	}
	s.StreamID = "999"
	if it := c.buildActiveStreamItem(s, nil); it.Num != 0 {
		t.Fatalf("unknown live id: num = %d, want 0 (omitted)", it.Num)
	}
}

// The status text shows the stream id (and the number when known) next to the
// title; a VOD view has no channel number.
func TestStatusTextShowsStreamIdentifiers(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig("http://provider.invalid")
	sm := session.NewSessionManager(nil)
	defer sm.Stop()
	c.sessionManager = sm
	sm.RegisterVODView("lucas", "1717919", "series", "Some Show — S05E02")

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest("GET", "/api/internal/status", nil)
	c.statusSummary(ctx)

	var resp struct {
		Data struct {
			Text    string                   `json:"text"`
			Summary []map[string]interface{} `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Data.Text, "Some Show — S05E02 (id 1717919)") {
		t.Fatalf("status text missing identifiers:\n%s", resp.Data.Text)
	}
	if len(resp.Data.Summary) != 1 || resp.Data.Summary[0]["stream_id"] != "1717919" {
		t.Fatalf("summary = %v", resp.Data.Summary)
	}
	if _, has := resp.Data.Summary[0]["num"]; has {
		t.Fatal("num must be omitted when unknown")
	}
}
