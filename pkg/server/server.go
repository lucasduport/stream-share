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

package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/discord"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
	uuid "github.com/satori/go.uuid"

	"github.com/gin-gonic/gin"
)

var defaultProxyfiedM3UPath = filepath.Join(os.TempDir(), uuid.NewV4().String()+".stream-share.m3u")
var endpointAntiColision = strings.Split(uuid.NewV4().String(), "-")[0]

// Config represents all server dependencies and runtime configuration.
type Config struct {
	*config.ProxyConfig

	// M3U service part
	playlist *m3u.Playlist
	// this variable is set only for m3u proxy endpoints
	track *m3u.Track
	// path to the proxyfied m3u file
	proxyfiedM3UPath string

	endpointAntiColision string

	// New components
	sessionManager *session.SessionManager
	catchupManager *catchup.Manager
	db             *database.DBManager
	discordBot     *discord.Bot

	// inProgressDownloads guards against concurrent duplicate fetchToFile goroutines
	inProgressDownloads sync.Map
}

// NewServer initializes a new server configuration with all necessary components.
func NewServer(config *config.ProxyConfig) (*Config, error) {
	var p m3u.Playlist

	// Parse the M3U playlist from the remote URL if provided
	if config.RemoteURL.String() != "" {
		var err error
		p, err = m3u.Parse(config.RemoteURL.String())
		if err != nil {
			return nil, utils.PrintErrorAndReturn(err)
		}
		utils.InfoLog("Successfully parsed M3U playlist from %s", config.RemoteURL.String())
	}

	// Use custom ID for endpoint if provided, otherwise use a generated one
	customID := endpointAntiColision
	if trimmedCustomId := strings.Trim(config.CustomId, "/"); trimmedCustomId != "" {
		customID = trimmedCustomId
		utils.InfoLog("Using custom endpoint ID: %s", customID)
	}

	// Initialize debug logging from environment variable
	utils.Config.DebugLoggingEnabled = os.Getenv("LOG_DEBUG_ENABLED") == "true"

	// Pin the internal API key from configuration if provided
	SetAPIKey(config.InternalAPIKey)

	// Create server configuration
	serverConfig := &Config{
		ProxyConfig:          config,
		playlist:             &p,
		track:                nil,
		proxyfiedM3UPath:     defaultProxyfiedM3UPath,
		endpointAntiColision: customID,
		sessionManager:       nil,
		db:                   nil,
		discordBot:           nil,
	}

	// Force PostgreSQL initialization (sqlite removed)
	utils.InfoLog("Bootstrap: Forcing PostgreSQL database initialization")
	db, err := database.NewDBManager("") // path unused for postgres
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}
	serverConfig.db = db
	serverConfig.sessionManager = session.NewSessionManager(db)
	serverConfig.sessionManager.SetNameResolver(serverConfig.resolveStreamName)
	utils.InfoLog("Session manager initialized with database connection")

	// Seed in-memory name indices from DB so names are available before the first API call.
	serverConfig.warmChannelIndexFromDB()

	// After session manager init
	if serverConfig.sessionManager == nil {
		utils.ErrorLog("Bootstrap: sessionManager is NIL - multiplexing will NOT be used")
	} else {
		utils.InfoLog("Bootstrap: sessionManager initialized OK")
	}

	// Initialize local catchup buffering from configuration
	catchupEnabled := config.CatchupEnabled
	catchupDir := utils.CatchupBufferDir()
	catchupDur := 4
	if config.CatchupDurationHours > 0 {
		catchupDur = config.CatchupDurationHours
	}
	serverConfig.catchupManager = catchup.New(catchupEnabled, catchupDir, catchupDur)
	if catchupEnabled {
		serverConfig.catchupManager.CleanupOldFiles()
		tz := os.Getenv("TZ")
		if tz == "" {
			utils.WarnLog("Bootstrap: catchup is ENABLED but TZ env var is not set — timeshift timestamps from clients will be parsed as UTC and rewinds will land at the wrong position. Set TZ to your clients' timezone (e.g. TZ=Europe/Amsterdam).")
		} else {
			utils.InfoLog("Bootstrap: local catchup buffering ENABLED (dir=%s, duration=%dh, TZ=%s)", catchupDir, catchupDur, tz)
		}
	} else {
		utils.InfoLog("Bootstrap: local catchup buffering DISABLED (set CATCHUP_ENABLED=true to enable)")
	}
	if serverConfig.sessionManager != nil {
		serverConfig.sessionManager.SetCatchupManager(serverConfig.catchupManager)
	}

	// Pause grace: how long a catchup-enabled live stream stays alive (upstream
	// connection open, disk recording continuing) after its last viewer
	// disconnects, so a TiviMate "pause" followed by a timeshift-based resume
	// has continuous buffered content with no gap. Channel switches are detected
	// separately and bypass this grace period (see SessionManager.RequestStream).
	catchupPauseGrace := 5
	if config.CatchupPauseGraceMinutes >= 0 {
		catchupPauseGrace = config.CatchupPauseGraceMinutes
	}
	if serverConfig.sessionManager != nil {
		serverConfig.sessionManager.SetPauseGrace(time.Duration(catchupPauseGrace) * time.Minute)
		if catchupEnabled && catchupPauseGrace > 0 {
			utils.InfoLog("Bootstrap: catchup pause grace set to %d minute(s) — paused live streams keep recording for seamless resume", catchupPauseGrace)
		}
	}

	// Configure session parameters from configuration. Each setter is only
	// applied when the value is > 0, leaving the manager defaults otherwise.
	if serverConfig.sessionManager != nil {
		if mins := config.SessionTimeoutMinutes; mins > 0 {
			serverConfig.sessionManager.SetSessionTimeout(time.Duration(mins) * time.Minute)
			utils.InfoLog("Session timeout set to %d minutes", mins)
		}
		if mins := config.StreamTimeoutMinutes; mins > 0 {
			serverConfig.sessionManager.SetStreamTimeout(time.Duration(mins) * time.Minute)
			utils.InfoLog("Stream timeout set to %d minutes", mins)
		}
		if hours := config.TempLinkHours; hours > 0 {
			serverConfig.sessionManager.SetTempLinkTimeout(time.Duration(hours) * time.Hour)
			utils.InfoLog("Temporary link timeout set to %d hours", hours)
		}
		if hours := config.VODCacheStaleHours; hours > 0 {
			serverConfig.sessionManager.SetVODCacheStaleAge(time.Duration(hours) * time.Hour)
			utils.InfoLog("VOD cache stale age set to %d hours", hours)
		}
		if secs := config.MultiplexStallTimeoutSeconds; secs > 0 {
			serverConfig.sessionManager.SetClientStallTimeout(time.Duration(secs) * time.Second)
			utils.InfoLog("Multiplex client stall timeout set to %d seconds", secs)
		}
	}

	// Initialize Discord bot if token is provided
	discordToken := config.DiscordBotToken
	if discordToken != "" {
		utils.InfoLog("Initializing Discord bot")
		discordAdminRole := config.DiscordAdminRoleID

		// Get API URL from config, defaulting to host/port, but honor reverse proxy
		apiURL := config.DiscordAPIURL
		if apiURL == "" {
			protocol := "http"
			if config.HTTPS { protocol = "https" }
			hostPart := fmt.Sprintf("%s:%d", config.HostConfig.Hostname, config.HostConfig.Port)
			if config.ReverseProxyEnabled {
				// Behind reverse proxy: use hostname without port by default
				hostPart = config.HostConfig.Hostname
			}
			apiURL = fmt.Sprintf("%s://%s", protocol, hostPart)
		}
		utils.InfoLog("Discord API URL used by bot: %s", apiURL)
		utils.InfoLog("Reminder: Ensure 'MESSAGE CONTENT INTENT' is enabled in Discord Developer Portal for this bot.")

		bot, err := discord.NewBot(discordToken, discordAdminRole, apiURL, GetAPIKey())
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Discord bot: %w", err)
		}

		serverConfig.discordBot = bot
	} else {
		utils.InfoLog("Bootstrap: DISCORD_BOT_TOKEN not set - Discord bot is DISABLED")
	}

	// Remove debug API JSON dumps left over from previous runs when debug logging
	// is off. These files accumulate whenever CACHE_FOLDER is set and are only
	// useful in debug mode.
	if !utils.IsDebugLogEnabled() {
		if cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER")); cacheDir != "" {
			cleanDebugAPIFiles(cacheDir)
		}
	}

	return serverConfig, nil
}

// cleanDebugAPIFiles removes timestamped JSON debug dumps written by the
// player_api handler from the cache directory. VOD media files are unaffected.
func cleanDebugAPIFiles(cacheDir string) {
	patterns := []string{
		"login_????????_??????.json",
		"get_*_????????_??????.json",
	}
	removed := 0
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(cacheDir, pattern))
		for _, f := range matches {
			if err := os.Remove(f); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		utils.InfoLog("Cleaned %d debug API JSON file(s) from cache folder", removed)
	}
}

// Serve the stream-share api
// Serve boots the HTTP server, internal API, routes, and optional Discord bot.
func (c *Config) Serve() error {
	utils.InfoLog("[stream-share] Server is starting...")

	if c.db != nil && c.db.IsInitialized() {
		utils.InfoLog("Bootstrap: Database is initialized and connected")
	} else if c.db != nil {
		utils.WarnLog("Bootstrap: Database manager present but not initialized")
	} else {
		utils.WarnLog("Bootstrap: Database is DISABLED (no persistence)")
	}

	if c.sessionManager == nil {
		utils.ErrorLog("Bootstrap: sessionManager is NIL inside Serve()")
	} else {
		utils.InfoLog("Bootstrap: sessionManager ready (timeouts: session=%v, stream=%v, tempLink=%v)",
			// not exported; we just acknowledge presence
			time.Minute, time.Minute, time.Hour)
	}

	if err := c.playlistInitialization(); err != nil {
		utils.ErrorLog("Playlist initialization failed: %v", err)
		return err
	}

	// Warm the channel-name index from get_live_streams so /status and logs can
	// resolve names immediately, without waiting for a player to request the list.
	go c.warmChannelNameIndex()

	// Start background goroutine to keep apiChannelIndex fresh.
	nameRefreshStop := make(chan struct{})
	c.startNameIndexRefresher(nameRefreshStop)
	defer close(nameRefreshStop)

	if c.sessionManager != nil {
		defer c.sessionManager.Stop()
	}
	if c.catchupManager != nil {
		defer c.catchupManager.Cleanup()
	}

	// Start Discord bot if configured
	if c.discordBot != nil {
		utils.InfoLog("Starting Discord bot...")
		if err := c.discordBot.Start(); err != nil {
			return fmt.Errorf("failed to start Discord bot: %w", err)
		}
		defer c.discordBot.Stop()
	}

	router := gin.Default()
	router.Use(cors.Default())
	utils.InfoLog("Setting up routes and internal API...")

	// Setup API routes for Discord bot and other internal tools
	c.setupInternalAPI(router)

	// Setup regular routes
	group := router.Group("/")
	c.routes(group)

	// Add direct streaming routes with proxy credentials
	c.addProxyCredentialRoutes(router)

	// Add temporary link download route
	router.GET("/download/:token", c.handleTemporaryLink)

	// Add a message to indicate the server is ready
	utils.InfoLog("[stream-share] Server is ready and listening on :%d", c.HostConfig.Port)
	return router.Run(fmt.Sprintf(":%d", c.HostConfig.Port))
}

// Add direct streaming routes with proxy credentials
// addProxyCredentialRoutes registers direct streaming endpoints that accept
// proxy credentials in the path but always use Xtream credentials upstream.
func (c *Config) addProxyCredentialRoutes(router *gin.Engine) {
	utils.InfoLog("[stream-share] Setting up direct stream routes with proxy credentials")

	// Root level (generic)
	router.GET("/:username/:password/:id", c.authWithPathCredentials(), c.xtreamProxyCredentialsStreamHandler)

	// Live
	router.GET("/live/:username/:password/:id", c.authWithPathCredentials(), c.xtreamProxyCredentialsLiveStreamHandler)

	// Movie
	router.GET("/movie/:username/:password/:id", c.authWithPathCredentials(), c.xtreamProxyCredentialsMovieStreamHandler)

	// Series
	router.GET("/series/:username/:password/:id", c.authWithPathCredentials(), c.xtreamProxyCredentialsSeriesStreamHandler)

	// Timeshift — routed through xtreamStreamTimeshift which handles local catchup
	router.GET("/timeshift/:username/:password/:duration/:start/:id", c.authWithPathCredentials(), c.xtreamStreamTimeshift)

	utils.InfoLog("[stream-share] Routes initialized with direct stream URL support")
}

// Authentication middleware that checks credentials from URL path parameters
// and manages user sessions for multiplexing
// authWithPathCredentials authenticates :username/:password path params against
// either LDAP (if enabled) or local credentials, and registers the user session.
func (c *Config) authWithPathCredentials() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		username := ctx.Param("username")
		password := ctx.Param("password")
		ip := ctx.ClientIP()
		userAgent := ctx.Request.UserAgent()

		utils.DebugLog("Path credentials auth check: username=%s, IP=%s", username, ip)

		// If LDAP is enabled, authenticate against LDAP
		if c.LDAPEnabled {
			ok := ldapAuthenticate(
				c.LDAPServer,
				c.LDAPBaseDN,
				c.LDAPBindDN,
				c.LDAPBindPassword,
				c.LDAPUserAttribute,
				c.LDAPGroupAttribute,
				c.LDAPRequiredGroup,
				username,
				password,
			)
			if !ok {
				utils.DebugLog("LDAP authentication failed for user in path: %s", username)
				ctx.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			utils.DebugLog("LDAP authentication succeeded for user in path: %s", username)
		} else if c.User.String() != username || c.Password.String() != password {
			utils.DebugLog("Local authentication failed for user in path: %s", username)
			ctx.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		// Register or update the user session and set username in context for later logs
		if c.sessionManager == nil {
			utils.ErrorLog("authWithPathCredentials: sessionManager is NIL - cannot register user session")
		} else {
			c.sessionManager.RegisterUser(username, ip, userAgent)
			utils.InfoLog("authWithPathCredentials: session registered for user=%s ip=%s", username, ip)
		}
		ctx.Set("username", username)

		ctx.Next()
	}
}

// handleTemporaryLink processes temporary link downloads
// handleTemporaryLink serves a previously created temporary link, preferring a
// local cache hit and falling back to proxying the original upstream URL.
func (c *Config) handleTemporaryLink(ctx *gin.Context) {
	token := ctx.Param("token")

	// Get the temporary link from session manager
	tempLink, err := c.sessionManager.GetTemporaryLink(token)
	if err != nil {
		utils.DebugLog("Temporary link not found: %v", err)
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}

	// If cached locally, serve from disk (normalize ID without extension)
	if c.db != nil && tempLink.StreamID != "" {
		idRaw := strings.TrimSuffix(tempLink.StreamID, path.Ext(tempLink.StreamID))
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil && entry.Status == "ready" {
			utils.InfoLog("Download via cache for %s -> %s", c.vodLabel(tempLink.StreamID), entry.FilePath)
			ext := strings.ToLower(path.Ext(entry.FilePath)); if ext == "" { ext = ".mp4" }
			_ = c.db.TouchVODCache(idRaw)
			var ct string
			switch ext { case ".ts": ct = "video/mp2t"; case ".mkv": ct = "video/x-matroska"; case ".mp4": ct = "video/mp4"; default: ct = "application/octet-stream" }
			serveLocalFileRange(ctx, entry.FilePath, ct, sanitiseFilename(tempLink.Title)+ext, true)
			return
		}
	}

	// Fallback: proxy upstream URL
	targetURL, err := url.Parse(tempLink.URL)
	if err != nil { utils.ErrorLog("Invalid URL in temporary link: %v", err); ctx.AbortWithStatus(http.StatusInternalServerError); return }
	ext := strings.ToLower(path.Ext(targetURL.Path)); if ext == "" { ext = ".mp4" }
	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, sanitiseFilename(tempLink.Title), ext))
	c.stream(ctx, targetURL)
}

// resolveRequestUsername derives the viewer identity for session tracking. It
// prefers the authenticated username set by middleware, then path/query params,
// and finally falls back to the client IP. The fallback matters for the direct
// Xtream-credentials routes (e.g. /movie/<user>/<pass>/:id), which have no auth
// middleware and therefore no username in context — without it, VOD views would
// never be registered and /status would miss them.
func (c *Config) resolveRequestUsername(ctx *gin.Context) string {
	username := ctx.GetString("username")
	if username == "" {
		username = ctx.Param("username")
	}
	if username == "" {
		username = ctx.Query("username")
	}
	if username == "" {
		username = ctx.ClientIP()
	}
	return username
}

// multiplexedStream handles streaming with connection multiplexing
// multiplexedStream proxies a stream while sharing a single upstream connection
// across multiple clients for the same content using the SessionManager.
func (c *Config) multiplexedStream(ctx *gin.Context, targetURL *url.URL) {
	username := c.resolveRequestUsername(ctx)

	// Extract stream ID and type
	streamID := path.Base(targetURL.Path)
	// Normalize stream id for cache lookup (strip extension if present)
	streamIDRaw := strings.TrimSuffix(streamID, path.Ext(streamID))
	streamType := "unknown"
	p := targetURL.Path
	if strings.Contains(p, "/movie/") {
		streamType = "movie"
	} else if strings.Contains(p, "/series/") {
		streamType = "series"
	} else if strings.Contains(p, "/live/") {
		streamType = "live"
	} else if strings.Contains(p, "/timeshift/") {
		streamType = "timeshift"
	}
	// Fallback: check incoming request path for type hints.
	// The generic /:user/:pass/:id route maps to live streams in Xtream protocol.
	if streamType == "unknown" {
		reqPath := ctx.Request.URL.Path
		if strings.Contains(reqPath, "/movie/") {
			streamType = "movie"
		} else if strings.Contains(reqPath, "/series/") {
			streamType = "series"
		} else if strings.Contains(reqPath, "/live/") {
			streamType = "live"
		} else {
			streamType = "live"
		}
	}

	// Title from query parameter, name resolution (live index or lazy VOD
	// get_vod_info), or fallback to stream ID. Resolving here — before
	// RequestStream takes streamLock — both stores the title on the session and
	// warms the VOD cache so later locked log lookups resolve without network I/O.
	streamTitle := targetURL.Query().Get("title")
	if streamTitle == "" {
		if name, ok := c.resolveTitleAtStart(streamIDRaw, streamType); ok && strings.TrimSpace(name) != "" {
			streamTitle = name
		} else {
			streamTitle = streamID
		}
	}

	utils.DebugLog("Multiplexed stream request: user=%s, id=%s, type=%s, title=%s, upstream=%s",
		username, streamID, streamType, streamTitle, targetURL.String())

	// If VOD and cached locally, serve from disk to avoid upstream connection
	if c.db != nil && (streamType == "movie" || streamType == "series") {
		if entry, err := c.db.GetVODCache(streamIDRaw); err == nil && entry != nil && entry.Status == "ready" {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				utils.InfoLog("Multiplex: serving cached %s for %s from %s", streamType, c.streamLabel(streamIDRaw), entry.FilePath)
				// Content-Type based on file extension
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" { ct = "video/mp2t" } else if ext == ".mkv" { ct = "video/x-matroska" } else { ct = "video/mp4" }
				_ = c.db.TouchVODCache(streamIDRaw)
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
			utils.WarnLog("Multiplex: cached %s missing on disk for %s at %s; falling back to upstream", streamType, c.streamLabel(streamIDRaw), entry.FilePath)
		}
	}

	if c.sessionManager == nil {
		utils.ErrorLog("Multiplex: sessionManager is NIL, falling back to direct streaming")
		c.stream(ctx, targetURL)
		return
	}

	// Request the stream through the session manager for multiplexing
	label := c.streamLabel(streamID)
	buffer, err := c.sessionManager.RequestStream(username, streamID, streamType, streamTitle, targetURL)
	if err != nil {
		utils.ErrorLog("Multiplex: RequestStream failed for user=%s %s err=%v", username, label, err)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if buffer == nil {
		utils.WarnLog("Multiplex: buffer returned is NIL for %s (user=%s)", label, username)
	}

	// Get the data channel and termination signal for this client
	dataChan, exists := c.sessionManager.GetClientChannel(streamID, username)
	if !exists {
		utils.ErrorLog("Failed to get client channel for user=%s, %s", username, label)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	doneChan, _ := c.sessionManager.GetClientDone(streamID, username)
	clientGone := ctx.Request.Context().Done()

	// Set content-type and disable intermediary buffering
	setNoBufferingHeaders(ctx, contentTypeForPath(targetURL.Path))

	// Stream data to the client
	utils.DebugLog("Starting multiplexed stream for user %s (%s)", username, label)

	ctx.Stream(func(w io.Writer) bool {
		select {
		case data, ok := <-dataChan:
			if !ok {
				utils.DebugLog("Stream channel closed for user %s (%s)", username, label)
				return false
			}
			if _, err := w.Write(data); err != nil {
				// Client disconnected
				utils.DebugLog("Client write error for user %s (%s): %v", username, label, err)
				return false
			}
			// Force immediate delivery to client to avoid periodic buffering
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return true
		case <-doneChan:
			// Pump dropped this client (slow viewer) or the stream stopped
			utils.DebugLog("Stream done signal for user %s (%s)", username, label)
			return false
		case <-clientGone:
			// Client closed the connection while we were waiting for data
			utils.DebugLog("Client disconnected (idle) for user %s (%s)", username, label)
			return false
		}
	})

	// Clean up after streaming is done
	utils.DebugLog("Stream ended for user %s (%s)", username, label)
	c.sessionManager.RemoveClient(streamID, username)
}

// playlistInitialization writes a proxified M3U file to disk if a playlist was parsed.
func (c *Config) playlistInitialization() error {
	if len(c.playlist.Tracks) == 0 {
		return nil
	}

	f, err := os.Create(c.proxyfiedM3UPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return c.marshallInto(f, false)
}

// MarshallInto a *bufio.Writer a Playlist.
// marshallInto writes the in-memory playlist into an M3U file, rewriting
// credentials and paths depending on xtream mode.
func (c *Config) marshallInto(into *os.File, xtream bool) error {
	filteredTrack := make([]m3u.Track, 0, len(c.playlist.Tracks))

	ret := 0
	into.WriteString("#EXTM3U\n") // nolint: errcheck
	for i, track := range c.playlist.Tracks {
		var buffer bytes.Buffer

		buffer.WriteString("#EXTINF:")                       // nolint: errcheck
		fmt.Fprintf(&buffer, "%d ", track.Length)
		for i := range track.Tags {
			if i == len(track.Tags)-1 {
				fmt.Fprintf(&buffer, "%s=%q", track.Tags[i].Name, track.Tags[i].Value)
				continue
			}
			fmt.Fprintf(&buffer, "%s=%q ", track.Tags[i].Name, track.Tags[i].Value)
		}

		uri, err := c.replaceURL(track.URI, i-ret, xtream)
		if err != nil {
			ret++
			log.Printf("ERROR: track: %s: %s", track.Name, err)
			continue
		}

		_, _ = fmt.Fprintf(into, "%s, %s\n%s\n", buffer.String(), track.Name, uri)

		filteredTrack = append(filteredTrack, track)
	}
	c.playlist.Tracks = filteredTrack

	return into.Sync()
}

// ReplaceURL replace original playlist url by proxy url
// replaceURL rewrites a track URI to point to this proxy with local credentials.
func (c *Config) replaceURL(uri string, trackIndex int, xtream bool) (string, error) {
	oriURL, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	protocol := "http"
	if c.HTTPS {
		protocol = "https"
	}

	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = fmt.Sprintf("/%s", customEnd)
	}

	uriPath := oriURL.EscapedPath()
	if xtream {
		// Xtream get.php mode: replace provider creds with local creds in path
		uriPath = strings.ReplaceAll(uriPath, c.XtreamUser.PathEscape(), c.User.PathEscape())
		uriPath = strings.ReplaceAll(uriPath, c.XtreamPassword.PathEscape(), c.Password.PathEscape())
	} else {
		// M3U proxified path
		uriPath = path.Join(
			"/",
			c.endpointAntiColision,
			c.User.PathEscape(),
			c.Password.PathEscape(),
			fmt.Sprintf("%d", trackIndex),
			path.Base(uriPath),
		)
	}

	basicAuth := oriURL.User.String()
	if basicAuth != "" {
		basicAuth += "@"
	}

	newURI := fmt.Sprintf(
		"%s://%s%s:%d%s%s",
		protocol,
		basicAuth,
		c.HostConfig.Hostname,
		c.AdvertisedPort,
		customEnd,
		uriPath,
	)

	newURL, err := url.Parse(newURI)
	if err != nil {
		return "", err
	}

	return newURL.String(), nil
}

// sanitiseFilename strips characters that are unsafe inside a quoted
// Content-Disposition filename value, preventing header injection.
func sanitiseFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' || r == '\\' {
			return '_'
		}
		return r
	}, name)
}

// startNameIndexRefresher runs a background goroutine that periodically re-fetches
// get_live_streams from the upstream Xtream API to keep apiChannelIndex fresh.
func (c *Config) startNameIndexRefresher(stopCh <-chan struct{}) {
	interval := time.Duration(c.M3UCacheExpiration) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				c.refreshAPIChannelIndex()
			}
		}
	}()
}

// refreshAPIChannelIndex re-fetches get_live_streams from the upstream Xtream API
// and updates the in-memory apiChannelIndex (and persists to DB).
func (c *Config) refreshAPIChannelIndex() {
	if c.XtreamBaseURL == "" {
		return
	}
	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, "")
	if err != nil {
		utils.WarnLog("stream_names refresh: failed to create Xtream client: %v", err)
		return
	}
	resp, _, _, err := client.Action(c.ProxyConfig, "get_live_streams", nil)
	if err != nil {
		utils.WarnLog("stream_names refresh: get_live_streams failed: %v", err)
		return
	}
	if n := c.harvestChannelNames(xtreamapi.ProcessResponse(resp)); n > 0 {
		utils.DebugLog("stream_names: refreshed %d live channel names from upstream", n)
	}
}