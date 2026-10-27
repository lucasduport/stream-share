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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/filter"
	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/types"
)

// liveProvider serves a live catalogue and endless live streams, counting how
// many stream connections are open at once.
type liveProvider struct {
	open, maxOpen, total int32
}

func (p *liveProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/player_api.php") {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("action") {
		case "get_live_categories":
			_, _ = w.Write([]byte(`[{"category_id":"10","category_name":"A| News","parent_id":0},{"category_id":"20","category_name":"B| News","parent_id":0}]`))
		case "get_live_streams":
			_, _ = w.Write([]byte(`[{"num":1,"stream_id":101,"name":"A| Channel One","category_id":"10","epg_channel_id":"one.a","stream_icon":"http://logo/one.png","is_adult":0},` +
				`{"num":2,"stream_id":102,"name":"A| Channel Two","category_id":"10","epg_channel_id":"two.a","is_adult":0},` +
				`{"num":3,"stream_id":103,"name":"B| Channel Three","category_id":"20","epg_channel_id":"three.b","is_adult":0}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
		return
	}
	n := atomic.AddInt32(&p.open, 1)
	atomic.AddInt32(&p.total, 1)
	defer atomic.AddInt32(&p.open, -1)
	for {
		m := atomic.LoadInt32(&p.maxOpen)
		if n <= m || atomic.CompareAndSwapInt32(&p.maxOpen, m, n) {
			break
		}
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.WriteHeader(http.StatusOK)
	chunk := make([]byte, 188*20)
	for i := range chunk {
		chunk[i] = 0x47
	}
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type shareEnv struct {
	c     *Config
	p     *liveProvider
	front *httptest.Server
}

// shareTestServer runs the share routes and a household live route (as user
// "lucas") in front of a fake provider, with the given connection capacity.
func shareTestServer(t *testing.T, capacity int, rules *filter.Config) *shareEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	invalidateCatalogCache()
	p := &liveProvider{}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)

	c := &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: up.URL, XtreamUser: "prov", XtreamPassword: "secret"}}
	c.PublicBaseURL = "https://tv.example"
	if rules != nil {
		set, err := filter.NewSet(*rules)
		if err != nil {
			t.Fatal(err)
		}
		c.catalogFilter = set
	}
	c.providerSlots = provider.New(func() int { return capacity })
	c.sessionManager = session.NewSessionManager(nil)
	c.sessionManager.SetProviderSlots(c.providerSlots)
	t.Cleanup(c.sessionManager.Stop)
	c.shares = newShareStore()

	r := gin.New()
	r.GET("/share/:token", c.handleSharePlaylist)
	r.GET("/share/:token/live.ts", c.handleShareStream)
	r.GET("/household/:id", func(ctx *gin.Context) {
		ctx.Set("username", "lucas")
		c.xtreamStreamHandler(ctx)
	})
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return &shareEnv{c: c, p: p, front: front}
}

func (e *shareEnv) share(t *testing.T, streamID string, start, end time.Time) *types.LiveShare {
	t.Helper()
	sh, _, err := e.c.createLiveShare("lucas", "", streamID, start, end, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

// watch opens url and reads from it in the background; it returns once the
// response headers arrived, with the status, a counter of bytes read and a
// channel closed when the body ends.
func watch(t *testing.T, ctx context.Context, url string) (int, *int64, <-chan struct{}) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	var n int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, 32<<10)
		for {
			k, err := resp.Body.Read(buf)
			atomic.AddInt64(&n, int64(k))
			if err != nil {
				return
			}
		}
	}()
	return resp.StatusCode, &n, done
}

func getBody(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, string(b)
}

func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// Links answer by state: the playlist is available before the window (so a
// guest can set it up), the stream only inside it; ended and revoked links are
// gone, unknown ones not found. Nothing is cacheable.
func TestShareLinkStates(t *testing.T) {
	e := shareTestServer(t, 1, nil)
	now := time.Now()
	later := e.share(t, "101", now.Add(time.Hour), now.Add(2*time.Hour))

	code, h, body := getBody(t, e.front.URL+"/share/"+later.Token+".m3u")
	if code != http.StatusOK || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("scheduled playlist: %d %q", code, h.Get("Cache-Control"))
	}
	for _, want := range []string{"#EXTM3U", `tvg-id="one.a"`, `tvg-logo="http://logo/one.png"`, ",A| Channel One",
		"https://tv.example/share/" + later.Token + "/live.ts"} {
		if !strings.Contains(body, want) {
			t.Fatalf("playlist lacks %q:\n%s", want, body)
		}
	}
	if code, _, body := getBody(t, e.front.URL+"/share/"+later.Token+"/live.ts"); code != http.StatusForbidden || !strings.Contains(body, "not active yet") {
		t.Fatalf("scheduled stream: %d %q", code, body)
	}

	ended := e.share(t, "101", now.Add(-2*time.Hour), now.Add(time.Second))
	e.c.shares.mu.Lock()
	e.c.shares.byToken[ended.Token].EndsAt = now.Add(-time.Minute)
	e.c.shares.mu.Unlock()
	if code, _, _ := getBody(t, e.front.URL+"/share/"+ended.Token+"/live.ts"); code != http.StatusGone {
		t.Fatalf("ended stream: %d", code)
	}

	revoked := e.share(t, "101", now, now.Add(time.Hour))
	e.c.revokeLiveShare(revoked.Token)
	if code, _, _ := getBody(t, e.front.URL+"/share/"+revoked.Token+".m3u"); code != http.StatusGone {
		t.Fatalf("revoked playlist: %d", code)
	}
	if code, _, _ := getBody(t, e.front.URL+"/share/doesnotexist/live.ts"); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
}

// Creation refuses bad windows, unknown channels and channels the catalogue
// filter hides from players.
func TestShareCreationValidation(t *testing.T) {
	rules := filter.Config{Live: filter.Patterns{Group: `^A\|`}}
	e := shareTestServer(t, 1, &rules)
	now := time.Now()
	cases := []struct {
		name       string
		id         string
		start, end time.Time
		code       int
	}{
		{"end before start", "101", now.Add(time.Hour), now, http.StatusBadRequest},
		{"too long", "101", now, now.Add(25 * time.Hour), http.StatusBadRequest},
		{"already over", "101", now.Add(-3 * time.Hour), now.Add(-time.Hour), http.StatusBadRequest},
		{"unknown channel", "999", now, now.Add(time.Hour), http.StatusNotFound},
		{"filtered channel", "103", now, now.Add(time.Hour), http.StatusForbidden},
		{"bad id", "../1", now, now.Add(time.Hour), http.StatusBadRequest},
	}
	for _, tc := range cases {
		if _, code, err := e.c.createLiveShare("lucas", "", tc.id, tc.start, tc.end, now); err == nil || code != tc.code {
			t.Errorf("%s: code=%d err=%v, want %d", tc.name, code, err, tc.code)
		}
	}
	sh, _, err := e.c.createLiveShare("lucas", "", "101.ts", now, now.Add(time.Hour), now)
	if err != nil || sh.StreamID != "101" || sh.Title != "A| Channel One" || len(sh.Token) != 22 {
		t.Fatalf("valid share: %+v %v", sh, err)
	}
}

// A share link plays its channel, at most three devices at once.
func TestShareStreamsAndCapsDevices(t *testing.T) {
	e := shareTestServer(t, 0, nil)
	sh := e.share(t, "101", time.Now(), time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var counters []*int64
	for i := 0; i < shareMaxDevices; i++ {
		code, n, _ := watch(t, ctx, e.front.URL+"/share/"+sh.Token+"/live.ts")
		if code != http.StatusOK {
			t.Fatalf("device %d: status %d", i+1, code)
		}
		counters = append(counters, n)
	}
	if !eventually(3*time.Second, func() bool {
		for _, n := range counters {
			if atomic.LoadInt64(n) == 0 {
				return false
			}
		}
		return true
	}) {
		t.Fatal("every device should receive the stream")
	}
	if code, _, body := getBody(t, e.front.URL+"/share/"+sh.Token+"/live.ts"); code != http.StatusTooManyRequests || !strings.Contains(body, "max 3") {
		t.Fatalf("4th device: %d %q", code, body)
	}
	if got := atomic.LoadInt32(&e.p.maxOpen); got != 1 {
		t.Fatalf("provider saw %d connections at once, want 1 (devices share the stream)", got)
	}
	if name := displayNameFor(shareIdentity(sh.Token), nil); name != "lucas (share)" {
		t.Fatalf("display name = %q", name)
	}
}

// A link may name its guest: viewers show up under the creator who invited
// them, with the guest's name.
func TestShareGuestName(t *testing.T) {
	e := shareTestServer(t, 0, nil)
	sh, _, err := e.c.createLiveShare("lucas", "  Paul  ", "101", time.Now(), time.Now().Add(time.Hour), time.Now())
	if err != nil || sh.Guest != "Paul" {
		t.Fatalf("guest = %q, %v", sh.Guest, err)
	}
	if name := displayNameFor(shareIdentity(sh.Token), nil); name != "lucas (share: Paul)" {
		t.Fatalf("display name = %q", name)
	}
}

// A guest on the channel the household is watching joins the household's
// stream: one provider connection for both.
func TestShareJoinsHouseholdStream(t *testing.T) {
	e := shareTestServer(t, 1, nil)
	sh := e.share(t, "101", time.Now(), time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	code, mine, _ := watch(t, ctx, e.front.URL+"/household/101")
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return atomic.LoadInt64(mine) > 0 }) {
		t.Fatalf("household stream: %d", code)
	}
	code, guest, _ := watch(t, ctx, e.front.URL+"/share/"+sh.Token+"/live.ts")
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return atomic.LoadInt64(guest) > 0 }) {
		t.Fatalf("share stream: %d", code)
	}
	if got := atomic.LoadInt32(&e.p.total); got != 1 {
		t.Fatalf("provider saw %d stream connections, want 1", got)
	}
}

// With one connection held by the household on another channel, a share is
// told the channel is busy at once instead of waiting or taking the slot.
func TestShareBusyWhileHouseholdHoldsTheConnection(t *testing.T) {
	e := shareTestServer(t, 1, nil)
	sh := e.share(t, "102", time.Now(), time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	code, mine, _ := watch(t, ctx, e.front.URL+"/household/101")
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return atomic.LoadInt64(mine) > 0 }) {
		t.Fatalf("household stream: %d", code)
	}
	start := time.Now()
	code, _, body := getBody(t, e.front.URL+"/share/"+sh.Token+"/live.ts")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "busy") || time.Since(start) > time.Second {
		t.Fatalf("share while household holds the slot: %d %q after %s", code, body, time.Since(start))
	}
	before := atomic.LoadInt64(mine)
	if !eventually(time.Second, func() bool { return atomic.LoadInt64(mine) > before }) {
		t.Fatal("the household stream must keep playing")
	}
}

// The household takes the only connection back from a share watching another
// channel: the share stream stops and the household plays.
func TestHouseholdTakesTheConnectionFromAShare(t *testing.T) {
	e := shareTestServer(t, 1, nil)
	sh := e.share(t, "102", time.Now(), time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	code, guest, guestDone := watch(t, ctx, e.front.URL+"/share/"+sh.Token+"/live.ts")
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return atomic.LoadInt64(guest) > 0 }) {
		t.Fatalf("share stream: %d", code)
	}
	code, mine, _ := watch(t, ctx, e.front.URL+"/household/101")
	if code != http.StatusOK || !eventually(5*time.Second, func() bool { return atomic.LoadInt64(mine) > 0 }) {
		t.Fatalf("household stream: %d", code)
	}
	select {
	case <-guestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the share stream should end when the household takes the connection")
	}
}

// When the window ends, the sweeper cuts the share's viewers.
func TestShareCutWhenWindowEnds(t *testing.T) {
	old := shareSweepInterval
	shareSweepInterval = 50 * time.Millisecond
	t.Cleanup(func() { shareSweepInterval = old })

	e := shareTestServer(t, 0, nil)
	sh := e.share(t, "101", time.Now(), time.Now().Add(800*time.Millisecond))
	stop := make(chan struct{})
	defer close(stop)
	e.c.startLiveShareSweeper(stop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, n, done := watch(t, ctx, e.front.URL+"/share/"+sh.Token+"/live.ts")
	if code != http.StatusOK || !eventually(time.Second, func() bool { return atomic.LoadInt64(n) > 0 }) {
		t.Fatalf("share stream: %d", code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the share stream should be cut when its window ends")
	}
	if code, _, _ := getBody(t, e.front.URL+"/share/"+sh.Token+"/live.ts"); code != http.StatusGone {
		t.Fatalf("after the end: %d", code)
	}
}

// Revoking a link cuts its viewers at once.
func TestShareRevocationCutsViewers(t *testing.T) {
	e := shareTestServer(t, 0, nil)
	sh := e.share(t, "101", time.Now(), time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, n, done := watch(t, ctx, e.front.URL+"/share/"+sh.Token+"/live.ts")
	if code != http.StatusOK || !eventually(time.Second, func() bool { return atomic.LoadInt64(n) > 0 }) {
		t.Fatalf("share stream: %d", code)
	}
	if !e.c.revokeLiveShare(sh.Token) {
		t.Fatal("revoke failed")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("revoking should cut the viewers")
	}
}

// The internal API creates, lists, and revokes links; only the creator (or a
// call without username, i.e. an admin) may revoke.
func TestShareInternalAPI(t *testing.T) {
	e := shareTestServer(t, 1, nil)
	r := gin.New()
	r.POST("/shares", e.c.createShareAPI)
	r.GET("/shares", e.c.listSharesAPI)
	r.GET("/shares/channels", e.c.shareChannelsAPI)
	r.DELETE("/shares/:token", e.c.revokeShareAPI)

	do := func(method, target, body string) (int, map[string]interface{}) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		var out map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	end := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	code, out := do(http.MethodPost, "/shares", `{"username":"lucas","guest":"Paul","stream_id":"101","ends_at":"`+end+`"}`)
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	data := out["data"].(map[string]interface{})
	token := data["token"].(string)
	urls := data["urls"].(map[string]interface{})
	if data["state"] != types.ShareActive || data["guest"] != "Paul" || urls["playlist"] != "https://tv.example/share/"+token+".m3u" {
		t.Fatalf("create data: %v", data)
	}
	if code, out := do(http.MethodPost, "/shares", `{"username":"lucas","stream_id":"101","ends_at":"tomorrow"}`); code != http.StatusBadRequest {
		t.Fatalf("bad ends_at: %d %v", code, out)
	}

	code, out = do(http.MethodGet, "/shares?username=bob", "")
	if code != http.StatusOK || len(out["data"].([]interface{})) != 0 {
		t.Fatalf("list for another user: %d %v", code, out)
	}
	code, out = do(http.MethodGet, "/shares?username=lucas", "")
	if code != http.StatusOK || len(out["data"].([]interface{})) != 1 {
		t.Fatalf("list own: %d %v", code, out)
	}

	code, out = do(http.MethodGet, "/shares/channels?q=channel%20two", "")
	if code != http.StatusOK {
		t.Fatalf("channels: %d %v", code, out)
	}
	if chs := out["data"].([]interface{}); len(chs) != 1 || chs[0].(map[string]interface{})["num"] != "2" {
		t.Fatalf("channels = %v", chs)
	}

	if code, _ := do(http.MethodDelete, "/shares/"+token+"?username=bob", ""); code != http.StatusForbidden {
		t.Fatalf("revoke by another user: %d", code)
	}
	code, out = do(http.MethodDelete, "/shares/"+token+"?username=lucas", "")
	if code != http.StatusOK || out["data"].(map[string]interface{})["state"] != types.ShareRevoked {
		t.Fatalf("revoke by creator: %d %v", code, out)
	}
}
