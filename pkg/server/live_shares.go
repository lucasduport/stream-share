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

// Live share links.
//
// A household user can share one live channel for a scheduled window: anyone
// holding the link watches that channel between its start and end, on behalf
// of the creator. The token in the URL is the credential (no LDAP login).
//
// Viewers of a share get their own internal identity ("share:<token prefix>",
// see session.ShareIdentityPrefix), never the creator's username: the session
// manager treats all connections of one username as one person's devices, and
// a guest zapping must not stop the creator's channel or the reverse. They are
// displayed under the creator who invited them: "<creator> (share: <guest>)"
// in /status and history, or "<creator> (share)" when the link names no guest.
//
// Priority: share streams hold provider slots in the provider.Share class,
// above background cache downloads and below the household. With a
// one-connection account the household takes the connection back from a share
// stream at once, and a share never takes it from the household.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/filter"
	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	// shareMaxDevices is how many connections one share link may hold at once.
	shareMaxDevices = 3
	// shareMaxWindow caps how long a share link may be valid.
	shareMaxWindow = 24 * time.Hour
	// shareRetention is how long ended links stay listed before being purged.
	shareRetention = 7 * 24 * time.Hour
	// shareIdentityLen is how many token characters name a share's identity
	// (and appear in logs); the rest of the token never leaves the link.
	shareIdentityLen = 6
)

// shareSweepInterval is how often active share connections are checked
// against their window, so a link that ends or is revoked is cut within it.
var shareSweepInterval = 5 * time.Second

// shareStore holds the share links (backed by the database when there is one;
// in memory only otherwise, lost on restart) and how many connections each has
// open.
type shareStore struct {
	mu      sync.RWMutex
	byToken map[string]*types.LiveShare
	conns   map[string]int
}

func newShareStore() *shareStore {
	return &shareStore{byToken: map[string]*types.LiveShare{}, conns: map[string]int{}}
}

// shareNames maps a share identity to its display name: always the creator
// (who invited), with the guest's name when the link has one. It is
// package-level because displayNameFor is called without a Config.
var shareNames sync.Map // identity -> display name

// shareIdentity is the internal username of a share link's viewers.
func shareIdentity(token string) string {
	if len(token) > shareIdentityLen {
		token = token[:shareIdentityLen]
	}
	return session.ShareIdentityPrefix + token
}

// shareDisplayName returns "<creator> (share: <guest>)", or "<creator> (share)"
// when the link names no guest, for a share identity.
func shareDisplayName(identity string) (string, bool) {
	v, ok := shareNames.Load(identity)
	if !ok {
		return "", false
	}
	return v.(string), true
}

func shareDisplayFor(sh *types.LiveShare) string {
	if sh.Guest != "" {
		return sh.CreatedBy + " (share: " + sh.Guest + ")"
	}
	return sh.CreatedBy + " (share)"
}

// tokenLabel is the part of a token that may appear in logs.
func tokenLabel(token string) string {
	if len(token) > shareIdentityLen {
		return token[:shareIdentityLen] + "…"
	}
	return token
}

func (s *shareStore) put(sh *types.LiveShare) {
	s.mu.Lock()
	s.byToken[sh.Token] = sh
	s.mu.Unlock()
	shareNames.Store(shareIdentity(sh.Token), shareDisplayFor(sh))
}

func (s *shareStore) get(token string) (*types.LiveShare, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byToken[token]
	if !ok {
		return nil, false
	}
	cp := *sh
	return &cp, true
}

// identityTaken reports whether a share already uses identity.
func (s *shareStore) identityTaken(identity string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for t := range s.byToken {
		if shareIdentity(t) == identity {
			return true
		}
	}
	return false
}

// list returns the links created by username (all when empty), newest first.
func (s *shareStore) list(username string) []*types.LiveShare {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.LiveShare, 0, len(s.byToken))
	for _, sh := range s.byToken {
		if username == "" || sh.CreatedBy == username {
			cp := *sh
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// revoke marks a link revoked; false when it is unknown.
func (s *shareStore) revoke(token string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byToken[token]
	if !ok {
		return false
	}
	if sh.RevokedAt == nil {
		t := at
		sh.RevokedAt = &t
	}
	return true
}

// acquireConn counts a new connection for token; false when it already has
// shareMaxDevices open.
func (s *shareStore) acquireConn(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[token] >= shareMaxDevices {
		return false
	}
	s.conns[token]++
	return true
}

func (s *shareStore) releaseConn(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[token] <= 1 {
		delete(s.conns, token)
		return
	}
	s.conns[token]--
}

// connected returns the links with at least one open connection.
func (s *shareStore) connected() []*types.LiveShare {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*types.LiveShare
	for token := range s.conns {
		if sh, ok := s.byToken[token]; ok {
			cp := *sh
			out = append(out, &cp)
		}
	}
	return out
}

// purge drops links that ended before cutoff from memory.
func (s *shareStore) purge(cutoff time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, sh := range s.byToken {
		if sh.EndsAt.Before(cutoff) && s.conns[token] == 0 {
			delete(s.byToken, token)
			shareNames.Delete(shareIdentity(token))
		}
	}
}

// loadLiveShares fills the store from the database at startup.
func (c *Config) loadLiveShares() {
	if c.db == nil || !c.db.IsInitialized() {
		return
	}
	shares, err := c.db.ListLiveShares(time.Now().Add(-shareRetention))
	if err != nil {
		utils.WarnLog("Share links: cannot load from database: %v", err)
		return
	}
	for _, sh := range shares {
		c.shares.put(sh)
	}
	if len(shares) > 0 {
		utils.InfoLog("Share links: loaded %d link(s)", len(shares))
	}
}

// newShareToken returns a random 128-bit URL-safe token whose identity prefix
// is not used by another link.
func (c *Config) newShareToken() (string, error) {
	for i := 0; i < 8; i++ {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		token := base64.RawURLEncoding.EncodeToString(b)
		if !c.shares.identityTaken(shareIdentity(token)) {
			return token, nil
		}
	}
	return "", fmt.Errorf("could not generate a unique share token")
}

// shareURLs are the public addresses of a link.
func (c *Config) shareURLs(token string) map[string]string {
	return map[string]string{
		"playlist": c.PublicURL("/share/" + token + ".m3u"),
		"stream":   c.PublicURL("/share/" + token + "/live.ts"),
	}
}

// liveChannel returns the cached live catalogue entry for streamID.
func (c *Config) liveChannel(streamID string, allowFetch bool) (map[string]interface{}, bool) {
	var resp interface{}
	if allowFetch {
		r, err := c.getCachedLiveStreamsCatalog()
		if err != nil {
			return nil, false
		}
		resp = r
	} else {
		r, ok := peekCatalog(c.catalogKey("get_live_streams", ""))
		if !ok {
			return nil, false
		}
		resp = r
	}
	items, _ := resp.([]interface{})
	id := normalizeStreamID(streamID)
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if ok && catalogString(m["stream_id"]) == id {
			return m, true
		}
	}
	return nil, false
}

// createLiveShare validates and stores a new link.
func (c *Config) createLiveShare(username, guest, streamID string, start, end, now time.Time) (*types.LiveShare, int, error) {
	username = strings.TrimSpace(username)
	guest = strings.TrimSpace(guest)
	if len([]rune(guest)) > 40 {
		guest = string([]rune(guest)[:40])
	}
	streamID = normalizeStreamID(strings.TrimSpace(streamID))
	switch {
	case username == "":
		return nil, http.StatusBadRequest, fmt.Errorf("username is required")
	case streamID == "" || !isValidStreamID(streamID):
		return nil, http.StatusBadRequest, fmt.Errorf("invalid stream_id")
	case !end.After(start):
		return nil, http.StatusBadRequest, fmt.Errorf("the end must be after the start")
	case end.Sub(start) > shareMaxWindow:
		return nil, http.StatusBadRequest, fmt.Errorf("a share link may last at most %s", shareMaxWindow)
	case !end.After(now):
		return nil, http.StatusBadRequest, fmt.Errorf("the window is already over")
	}
	ch, ok := c.liveChannel(streamID, true)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("unknown live channel %s", streamID)
	}
	// Judged like the players' list: with the category name, fetched if need
	// be (this runs from the API, not on a stream path).
	if c.catalogFilter.ActiveFor(filter.Live) {
		group := c.categoryNames("get_live_categories")[catalogString(ch["category_id"])]
		if !c.catalogFilter.Keep(filter.Live, group, catalogString(ch["name"]), catalogFlag(ch["is_adult"])) {
			return nil, http.StatusForbidden, fmt.Errorf("this channel is not offered to players")
		}
	}
	token, err := c.newShareToken()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	sh := &types.LiveShare{
		Token: token, StreamID: streamID, Title: strings.TrimSpace(catalogString(ch["name"])),
		CreatedBy: username, Guest: guest, StartsAt: start, EndsAt: end, CreatedAt: now,
	}
	if c.db != nil && c.db.IsInitialized() {
		if err := c.db.CreateLiveShare(sh); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("store share link: %w", err)
		}
	}
	c.shares.put(sh)
	forWhom := ""
	if guest != "" {
		forWhom = " (guest " + guest + ")"
	}
	utils.InfoLog("Share link %s created by %s%s for %s, %s → %s", tokenLabel(token), username, forWhom,
		c.streamLabel(streamID), start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"))
	return sh, http.StatusOK, nil
}

// revokeLiveShare revokes a link and cuts its open connections.
func (c *Config) revokeLiveShare(token string) bool {
	now := time.Now()
	if !c.shares.revoke(token, now) {
		return false
	}
	if c.db != nil && c.db.IsInitialized() {
		if err := c.db.RevokeLiveShare(token, now); err != nil {
			utils.WarnLog("Share link %s: cannot record revocation: %v", tokenLabel(token), err)
		}
	}
	c.cutShare(token, "revoked")
	return true
}

// cutShare drops every connection a link's viewers hold.
func (c *Config) cutShare(token, reason string) {
	if c.sessionManager == nil {
		return
	}
	utils.InfoLog("Share link %s %s: cutting its viewers", tokenLabel(token), reason)
	c.sessionManager.DisconnectUser(shareIdentity(token))
}

// sweepLiveShares cuts connections of links that ended or were revoked, and
// purges links ended long ago.
func (c *Config) sweepLiveShares(now time.Time) {
	for _, sh := range c.shares.connected() {
		if st := sh.State(now); st != types.ShareActive {
			c.cutShare(sh.Token, st)
		}
	}
}

// startLiveShareSweeper runs sweepLiveShares until stop closes.
func (c *Config) startLiveShareSweeper(stop <-chan struct{}) {
	go func() {
		tick := time.NewTicker(shareSweepInterval)
		defer tick.Stop()
		lastPurge := time.Now()
		for {
			select {
			case <-stop:
				return
			case now := <-tick.C:
				c.sweepLiveShares(now)
				if now.Sub(lastPurge) > time.Hour {
					lastPurge = now
					cutoff := now.Add(-shareRetention)
					c.shares.purge(cutoff)
					if c.db != nil && c.db.IsInitialized() {
						if _, err := c.db.DeleteLiveSharesEndedBefore(cutoff); err != nil {
							utils.WarnLog("Share links: purge failed: %v", err)
						}
					}
				}
			}
		}
	}()
}

// shareFromRequest resolves the link named by the :token route parameter
// (with an optional ".m3u" suffix) and answers 404/410 for unknown, ended or
// revoked links. scheduled links are returned with ok=true.
func (c *Config) shareFromRequest(ctx *gin.Context) (*types.LiveShare, bool) {
	ctx.Header("Cache-Control", "no-store")
	if c.shares == nil {
		ctx.String(http.StatusNotFound, "Unknown share link.\n")
		return nil, false
	}
	token := strings.TrimSuffix(ctx.Param("token"), ".m3u")
	sh, ok := c.shares.get(token)
	if !ok {
		ctx.String(http.StatusNotFound, "Unknown share link.\n")
		return nil, false
	}
	switch sh.State(time.Now()) {
	case types.ShareRevoked:
		ctx.String(http.StatusGone, "This share link was revoked.\n")
		return nil, false
	case types.ShareEnded:
		ctx.String(http.StatusGone, "This share link has ended.\n")
		return nil, false
	}
	return sh, true
}

// handleSharePlaylist GET /share/:token(.m3u) — a one-channel M3U playlist for
// VLC or an IPTV app. Available before the window starts, so a guest can set it
// up in advance; the stream itself only plays inside the window.
func (c *Config) handleSharePlaylist(ctx *gin.Context) {
	sh, ok := c.shareFromRequest(ctx)
	if !ok {
		return
	}
	title := sh.Title
	if title == "" {
		title = "Live " + sh.StreamID
	}
	attrs := fmt.Sprintf(`tvg-name=%q group-title="Share"`, title)
	if ch, ok := c.liveChannel(sh.StreamID, false); ok {
		if epg := catalogString(ch["epg_channel_id"]); epg != "" {
			attrs = fmt.Sprintf(`tvg-id=%q `, epg) + attrs
		}
		if logo := catalogString(ch["stream_icon"]); logo != "" {
			attrs += fmt.Sprintf(` tvg-logo=%q`, logo)
		}
	}
	body := fmt.Sprintf("#EXTM3U\n#EXTINF:-1 %s,%s\n%s\n", attrs, title, c.shareURLs(sh.Token)["stream"])
	ctx.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s.m3u"`, sanitiseFilename(title)))
	ctx.Data(http.StatusOK, "audio/x-mpegurl; charset=utf-8", []byte(body))
}

// handleShareStream GET /share/:token/live.ts — the shared channel, through
// the same multiplexed path as household viewers (a household viewer already
// on the channel shares its provider connection with the guest).
func (c *Config) handleShareStream(ctx *gin.Context) {
	sh, ok := c.shareFromRequest(ctx)
	if !ok {
		return
	}
	now := time.Now()
	if sh.State(now) == types.ShareScheduled {
		ctx.String(http.StatusForbidden, "This share link is not active yet (starts at %s).\n", sh.StartsAt.Local().Format("2006-01-02 15:04 MST"))
		return
	}
	if c.liveStreamFiltered(sh.StreamID) {
		ctx.String(http.StatusForbidden, "This channel is no longer available.\n")
		return
	}
	if !c.shares.acquireConn(sh.Token) {
		ctx.String(http.StatusTooManyRequests, "Too many devices on this share link (max %d).\n", shareMaxDevices)
		return
	}
	defer c.shares.releaseConn(sh.Token)

	targetURL, joining := c.shareUpstream(sh.StreamID)
	// Not joining a running stream means opening a provider connection: when
	// the household holds the only one, say so at once instead of waiting.
	if !joining && !c.providerSlots.WouldGrant(provider.Share) {
		ctx.String(http.StatusServiceUnavailable, "The channel is busy right now, try again later.\n")
		return
	}

	identity := shareIdentity(sh.Token)
	if c.sessionManager != nil {
		c.sessionManager.RegisterUser(identity, ctx.ClientIP(), ctx.Request.UserAgent())
	}
	ctx.Set("username", identity)
	utils.DebugLog("Share link %s: %s watching %s", tokenLabel(sh.Token), identity, c.streamLabel(sh.StreamID))
	c.multiplexedStream(ctx, targetURL)
}

// shareUpstream returns the upstream URL to request for a shared channel, and
// whether a stream is already running for it. Players address a live channel
// either bare (/<user>/<pass>/<id>) or as /live/.../<id>.ts, and the session
// manager keys streams by that last path element: joining a household viewer's
// stream (one provider connection for both) needs the same key, so an active
// stream under either form is reused; otherwise the bare form is used.
func (c *Config) shareUpstream(streamID string) (*url.URL, bool) {
	id := normalizeStreamID(streamID)
	bare := fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id)
	ts := fmt.Sprintf("%s/live/%s/%s/%s.ts", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id)
	if c.sessionManager != nil {
		for _, raw := range []string{bare, ts} {
			u, err := url.Parse(raw)
			if err != nil {
				continue
			}
			if info, ok := c.sessionManager.GetStreamInfo(path.Base(u.Path)); ok && info.Active {
				return u, true
			}
		}
	}
	u, _ := url.Parse(bare)
	return u, false
}

// shareView is a link as the API returns it.
type shareView struct {
	*types.LiveShare
	State    string            `json:"state"`
	Identity string            `json:"identity"`
	URLs     map[string]string `json:"urls"`
}

func (c *Config) shareViewOf(sh *types.LiveShare, now time.Time) shareView {
	return shareView{LiveShare: sh, State: sh.State(now), Identity: shareIdentity(sh.Token), URLs: c.shareURLs(sh.Token)}
}

// createShareAPI POST /api/internal/shares
// {username, guest?, stream_id, starts_at?, ends_at} (RFC 3339; starts_at
// defaults to now).
func (c *Config) createShareAPI(ctx *gin.Context) {
	if c.shares == nil {
		ctx.JSON(http.StatusServiceUnavailable, types.APIResponse{Success: false, Error: "share links unavailable"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Guest    string `json:"guest"`
		StreamID string `json:"stream_id"`
		StartsAt string `json:"starts_at"`
		EndsAt   string `json:"ends_at"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "Invalid request: " + err.Error()})
		return
	}
	now := time.Now()
	start := now
	if strings.TrimSpace(req.StartsAt) != "" {
		t, err := time.Parse(time.RFC3339, req.StartsAt)
		if err != nil {
			ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "starts_at must be RFC 3339"})
			return
		}
		start = t
	}
	end, err := time.Parse(time.RFC3339, req.EndsAt)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "ends_at must be RFC 3339"})
		return
	}
	sh, code, err := c.createLiveShare(req.Username, req.Guest, req.StreamID, start, end, now)
	if err != nil {
		ctx.JSON(code, types.APIResponse{Success: false, Error: err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: c.shareViewOf(sh, now)})
}

// listSharesAPI GET /api/internal/shares?username= — links created by username
// (every link when empty), scheduled, active and recently ended.
func (c *Config) listSharesAPI(ctx *gin.Context) {
	if c.shares == nil {
		ctx.JSON(http.StatusServiceUnavailable, types.APIResponse{Success: false, Error: "share links unavailable"})
		return
	}
	now := time.Now()
	shares := c.shares.list(strings.TrimSpace(ctx.Query("username")))
	out := make([]shareView, 0, len(shares))
	for _, sh := range shares {
		out = append(out, c.shareViewOf(sh, now))
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: out})
}

// revokeShareAPI DELETE /api/internal/shares/:token?username= — revoke a link;
// with username, only its creator may.
func (c *Config) revokeShareAPI(ctx *gin.Context) {
	if c.shares == nil {
		ctx.JSON(http.StatusServiceUnavailable, types.APIResponse{Success: false, Error: "share links unavailable"})
		return
	}
	token := ctx.Param("token")
	sh, ok := c.shares.get(token)
	if !ok {
		ctx.JSON(http.StatusNotFound, types.APIResponse{Success: false, Error: "unknown share link"})
		return
	}
	if u := strings.TrimSpace(ctx.Query("username")); u != "" && u != sh.CreatedBy {
		ctx.JSON(http.StatusForbidden, types.APIResponse{Success: false, Error: "only its creator can revoke this link"})
		return
	}
	c.revokeLiveShare(token)
	sh, _ = c.shares.get(token)
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: c.shareViewOf(sh, time.Now())})
}

// shareChannelsAPI GET /api/internal/shares/channels?q= — live channels players
// are offered whose name contains every word of q, for the Discord picker.
func (c *Config) shareChannelsAPI(ctx *gin.Context) {
	words := strings.Fields(strings.ToLower(ctx.Query("q")))
	if len(words) == 0 {
		ctx.JSON(http.StatusBadRequest, types.APIResponse{Success: false, Error: "q is required"})
		return
	}
	resp, err := c.getCachedLiveStreamsCatalog()
	if err != nil {
		ctx.JSON(http.StatusBadGateway, types.APIResponse{Success: false, Error: "live catalogue unavailable: " + err.Error()})
		return
	}
	items, _ := c.filterPlayerAPICatalog("get_live_streams", resp).([]interface{})
	type channel struct {
		StreamID string `json:"stream_id"`
		Num      string `json:"num"`
		Name     string `json:"name"`
	}
	out := []channel{}
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		name := catalogString(m["name"])
		lower := strings.ToLower(name)
		match := true
		for _, w := range words {
			if !strings.Contains(lower, w) {
				match = false
				break
			}
		}
		if match {
			out = append(out, channel{StreamID: catalogString(m["stream_id"]), Num: catalogString(m["num"]), Name: name})
			if len(out) == 25 {
				break
			}
		}
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: out})
}
