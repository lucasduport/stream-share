# StreamShare - Advanced IPTV Access Management Platform

## Overview

StreamShare is a comprehensive IPTV management solution that allows secure sharing of a single IPTV provider account with multiple users. Built upon the foundations of [jtdevops/iptv-proxy](https://github.com/jtdevops/iptv-proxy) and [pierre-emmanuelJ/iptv-proxy](https://github.com/pierre-emmanuelJ/iptv-proxy), this project has evolved far beyond a simple proxy to become a full-featured platform with authentication, stream multiplexing, and media management capabilities.

### Key Features

- **Stream Multiplexing** - Share a single IPTV subscription with multiple users simultaneously
- **Authentication Options**
  - LDAP integration for enterprise user management
  - Basic authentication for simpler setups
- **Content Management**
  - M3U/M3U8 playlist proxying with credential protection
  - Xtream Codes API compatibility (live, VOD, series, EPG)
  - Robust handling of Unicode characters and malformed responses
- **VOD Caching and Local Playback**
  - Cache movies or episodes locally (1–14 days) with progress tracking
  - Automatically stream from cached content for downloads and VOD/series endpoints when available
- **Local Catchup Buffering**
  - Every active live stream is written to disk as it plays, enabling pause and rewind on any channel — even those without provider-side catchup support
  - TiviMate (and other Xtream-compatible players) automatically show the rewind UI on all channels
  - Channels that already have native catchup continue to use the provider's own timeshift endpoint unchanged
  - Configurable buffer duration (default 4 hours) and pause grace period for seamless pause/resume
- **User Experience**
  - Enhanced VOD search, including series episodes (queries like "the office s02e04")
  - Discord bot with prettier embed-based responses, dropdown selection, and pagination
  - Temporary streaming links for content sharing
  - Session management with configurable timeouts
- **Administration**
  - Status API for monitoring active streams and users
  - Stream timeout enforcement and user management
  - PostgreSQL persistence for settings and state
- **Deployment**
  - Docker-ready with comprehensive environment variables
  - Reverse proxy compatibility with HTTPS support

### Upcoming Features
- **Improve priorization between live streaming and download requests**
  - Implement smarter queuing mechanisms for better resource allocation
  - Prioritize live streaming requests over downloads for improved user experience
  - Allow urgent download requests to bypass queues
- **Enhanced /link command**
  - Improved link command being able to link a user that is not me.
- **User Statistics**
  - Detailed user activity reports
  - Streaming quality metrics and analytics
- **Frontend for download link generation**
  - User-friendly interface for creating and managing download links

## How It Works

### Stream Multiplexing Technology

StreamShare's cool feature is its ability to efficiently multiplex streams. When multiple users request the same content:

1. The first user request establishes a single connection to the IPTV provider
2. StreamShare buffers the incoming stream data
3. All subsequent user requests for the same content receive data from this buffer
4. The provider sees only one connection, while multiple users can watch simultaneously
5. When all users disconnect, the upstream connection is gracefully closed

This technology significantly reduces load on the IPTV provider, prevents account limiting/banning for multiple connections, and improves stream start times for subsequent viewers.

### M3U/M3U8 Proxy

StreamShare transforms original IPTV playlist URLs into secure endpoints on your server:

**Original M3U Example:**
```m3u
#EXTM3U
#EXTINF:-1 tvg-ID="examplechanel1.com" tvg-name="chanel1" tvg-logo="http://ch.xyz/logo1.png" group-title="USA HD",CHANEL1-HD
http://iptvexample.net:1234/12/test/1
```

**Proxied Output:**
```m3u
#EXTM3U
#EXTINF:-1 tvg-ID="examplechanel1.com" tvg-name="chanel1" tvg-logo="http://ch.xyz/logo1.png" group-title="USA HD",CHANEL1-HD
http://yourstreamshare.com:8080/12/test/1?username=test&password=passwordtest
```

**Quick Start Example:**
```bash
streamshare --m3u-url http://provider.com/get.php?username=user&password=pass&type=m3u_plus&output=m3u8 \
       --port 8080 \
       --hostname streamshare.example.com \
       --auth-user test \
       --auth-password passwordtest
```
Access your playlist at:  
`http://streamshare.example.com:8080/iptv.m3u?username=test&password=passwordtest`

### Xtream Codes API Compatibility

StreamShare fully supports the Xtream Codes API with enhanced error handling and response sanitization:

```bash
streamshare --m3u-url http://provider.com:1234/get.php?username=user&password=pass&type=m3u_plus&output=m3u8 \
       --port 8080 \
       --hostname streamshare.example.com \
       --xtream-user provider_username \
       --xtream-password provider_password \
       --xtream-base-url http://provider.com:1234 \
       --auth-user your_username \
       --auth-password your_password
```

**Access with Your Credentials:**
```
user: your_username
password: your_password
base-url: http://streamshare.example.com:8080
```

---

## Discord Bot Integration

StreamShare includes a powerful Discord bot for content discovery and streaming. When enabled with the `DISCORD_BOT_TOKEN` environment variable, users can:

### Commands

| Command | Description |
|---------|-------------|
| `/link <ldap_username>` | Link your Discord account with your LDAP username |
| `/vod <query>` | Search movies and series; supports queries like `show s02e04` |
| `/cache <title> <days>` | Cache a movie or episode on the server for 1–14 days |
| `/cached` | List cached items and expiration times |
| `/status` | Show server status (admin only) |
| `/disconnect <ldap_username>` | Disconnect a user from the stream |
| `/timeout <ldap_username> <minutes>` | Temporarily block a user for N minutes |

Tips:
- Link your account first with `/link <ldap_user>`.
- Use specific queries to find episodes, e.g. `game of thrones s02e04` or `S1E1`.

---

## API Documentation (Internal)

StreamShare exposes an internal API (used by the Discord bot and admin tools) under `/api/internal`.

### Endpoints

| Endpoint | Method | Description | Authentication |
|----------|--------|-------------|----------------|
| `/api/internal/status` | GET | Get server status summary | X-API-Key |
| `/api/internal/streams` | GET | List all active streams | X-API-Key |
| `/api/internal/streams/:streamid` | GET | Get details for one active stream | X-API-Key |
| `/api/internal/users` | GET | List all connected users | X-API-Key |
| `/api/internal/users/:username` | GET | Get details for a user | X-API-Key |
| `/api/internal/users/disconnect/:username` | POST | Forcibly disconnect a user | X-API-Key |
| `/api/internal/users/timeout/:username` | POST | Apply a timeout for a user | X-API-Key |
| `/api/internal/discord/link` | POST | Link a Discord account to an LDAP user | X-API-Key |
| `/api/internal/discord/:discordid/ldap` | GET | Resolve LDAP username for a Discord ID | X-API-Key |
| `/api/internal/vod/search` | POST | Enhanced VOD search (movies + series episodes) | X-API-Key |
| `/api/internal/vod/download` | POST | Create a temporary download link for a VOD item | X-API-Key |
| `/api/internal/vod/status/:requestid` | GET | Check VOD request status | X-API-Key |
| `/api/internal/cache/start` | POST | Start caching a movie/episode for N days (1–14) | X-API-Key |
| `/api/internal/cache/by-stream/:streamid` | GET | Get cache entry by stream ID | X-API-Key |
| `/api/internal/cache/progress/:streamid` | GET | Get cache download progress | X-API-Key |
| `/api/internal/cache/list` | GET | List active cache entries | X-API-Key |
| `/api/internal/history?hours=N&limit=N&offset=N` | GET | Chronological watch-history feed across all users, paginated | X-API-Key |
| `/api/internal/history/:username?hours=N&limit=N&offset=N` | GET | Watch history for one user, paginated | X-API-Key |
| `/api/internal/instance` | GET | Identify this deployment (name, uptime, enabled features) — for labeling data in a multi-instance dashboard | X-API-Key |
| `/api/internal/stats?hours=N` | GET | Aggregate dashboard stats: live activity plus historical totals and top titles/users over the window (default 24h, `hours=0` for all-time) | X-API-Key |

### Building a dashboard

Everything above is machine-readable JSON (`{success, data, error}`) and is enough to build an external dashboard, including one that combines several stream-share instances:

- **Active streams**: `/api/internal/streams` or `/api/internal/status` for live sessions and viewers. Each stream item includes a display-resolved `stream_title` (never blank — falls back through the channel/VOD name index to the raw stream ID) and, for live channels, an optional `tech` object with audio/video technical info (see below).
- **Watch history**: `/api/internal/history` (global) or `/api/internal/history/:username`, both paginated with `limit`/`offset` and filterable with `hours`.
- **VOD search**: `/api/internal/vod/search` — the same live provider search used by the `/vod` Discord command.
- **Overview stats**: `/api/internal/stats` for counts and leaderboards to show on a summary page.
- **Multi-instance**: this API has no built-in concept of "instance" or "tenant" — each deployment is independent, with its own database and API key. Call `/api/internal/instance` on each one to fetch a stable display name (set via `INSTANCE_NAME`, see below) and combine results client-side by polling each instance's base URL with its own API key.

#### Technical stream info (`tech`)

When `STREAM_TECH_PROBE_ENABLED=true`, active stream items (from `/api/internal/streams`, `/streams/:streamid`, `/status`, and the Discord `/status` command's text output) include a `tech` object with best-effort audio/video/subtitle characteristics detected by `ffprobe`:

```json
"tech": {
  "container_format": "mpegts",
  "video_codec": "h264", "width": 1920, "height": 1080, "frame_rate": 25, "video_bitrate_kbps": 4500,
  "audio_tracks": [
    {"index": 1, "codec": "aac", "channels": 2, "sample_rate_hz": 48000, "language": "eng", "bitrate_kbps": 128},
    {"index": 2, "codec": "aac", "channels": 2, "sample_rate_hz": 48000, "language": "spa", "bitrate_kbps": 128}
  ],
  "subtitle_tracks": [
    {"index": 3, "codec": "dvb_subtitle", "language": "eng"}
  ],
  "probed_at": "2026-01-01T12:00:00Z"
}
```

Notes:
- **No extra provider connection either way**:
  - **Live channels** are probed from a small sample of bytes already flowing through that channel's existing shared upstream connection (the same one serving its viewers) — never an additional connection to your provider, so it's safe even on strict concurrent-connection limits. A channel is only probed once it has at least one real viewer (there's nothing to sample otherwise).
  - **Cached VOD/series** (once fully downloaded — see [VOD Caching](#vod-caching)) are probed directly off the local file, no network involved at all. Because the whole file is available (unlike a short live sample), this reliably lists every audio and subtitle track the file contains, not just whichever one happened to be in the sample; `duration_sec` is also included. VOD/series that aren't cached (or aren't fully downloaded yet) have no `tech`.
- Requires `ffprobe` in the runtime image (bundled in the official Docker image; if you build your own, install `ffmpeg`).
- Off by default. Results are cached for a few minutes per stream and refreshed lazily on the next request after the cache goes stale — list endpoints never block waiting on a probe, but `/streams/:streamid` (a single-item lookup) will wait briefly for a fresh result if nothing is cached yet.

### Authentication

API requests require an API key provided in the `X-API-Key` header:

```bash
curl -H "X-API-Key: your_api_key" https://streamshare.example.com/api/internal/status
```

The API key is automatically generated on first run and stored in the database.

To override, set `INTERNAL_API_KEY` in the environment so the bot and integrations can authenticate reliably.

Set `INSTANCE_NAME` to give this deployment a stable display name (defaults to the machine hostname), returned by `/api/internal/instance` — useful when a dashboard combines data from multiple stream-share instances.

Set `STREAM_TECH_PROBE_ENABLED=true` to enable the `tech` (audio/video technical info) field on active live streams — see [Technical stream info](#technical-stream-info-tech) above.

---

## Session Management

StreamShare includes sophisticated session management with configurable timeouts:

- **User Sessions** - Track user logins and activity
- **Stream Sessions** - Monitor and manage active streams
- **Temporary Links** - Create expiring download URLs

Configure with environment variables:
```
SESSION_TIMEOUT_MINUTES=120  # User session timeout (default: 60)
STREAM_TIMEOUT_MINUTES=240   # Stream session timeout (default: 120)
TEMP_LINK_HOURS=24           # Temporary link validity (default: 24)
```

### Direct Stream URLs

StreamShare supports direct stream URLs with proxy authentication in the path:

```
https://streamshare.example.com/username/password/12345
https://streamshare.example.com/live/username/password/12345
https://streamshare.example.com/movie/username/password/12345
https://streamshare.example.com/series/username/password/12345
```

These URLs are useful for direct integration with media players and other systems.

When the target movie or series episode is cached and ready, these endpoints serve the local file (with HTTP range support) instead of proxying upstream.

### Temporary Links

Generate temporary download links that expire after a configurable period:

```
https://streamshare.example.com/download/a1b2c3d4e5f6
```

Behavior:
- If the requested VOD is cached and ready, the file is served directly from local storage.
- Otherwise, the request is proxied from the provider.

Temporary links are perfect for sharing VOD content with users who don't have StreamShare accounts. Control lifetime with `TEMP_LINK_HOURS`.

### VOD Caching

Cache movies or episodes to disk for faster start times and to reduce upstream usage:

- Start a cache from Discord with `/cache <title> <days>` (1–14 days).
- Track progress and list items with `/cached`.
- Cached items automatically serve for both downloads and VOD/series streaming endpoints when available.

Configuration:
- `CACHE_FOLDER` — Absolute path to the root cache directory. stream-share creates purpose-specific subfolders underneath it: `<CACHE_FOLDER>/vod` for cached VOD media and `<CACHE_FOLDER>/catchup` for live catchup buffers. Defaults to `$TMPDIR/stream-share` when unset.
- `INTERNAL_API_KEY` — API key used by the internal API (Discord bot and tools).

---

## Local Catchup Buffering

StreamShare can buffer every active live stream to disk as it plays, giving all channels a rewind/catchup capability regardless of whether your IPTV provider supports it.

### How It Works

1. When a viewer starts watching a live channel, a `.ts` file is opened in the configured buffer directory and the stream is written to it in real time.
2. A time-to-offset index is maintained in memory so any point in the stream can be found quickly.
3. `get_live_streams` responses are patched to set `tv_archive=1` on all channels, so TiviMate (and other Xtream-compatible players) show the rewind UI everywhere.
4. When a timeshift request arrives, stream-share checks whether the channel has **native** provider catchup:
   - **Yes** → the request is proxied to the provider's own timeshift endpoint (zero change from before).
   - **No** → the request is served from the local disk buffer at the requested timestamp offset.
5. When all viewers leave, the buffer keeps recording for a configurable grace period (`CATCHUP_PAUSE_GRACE_MINUTES`) so that a TiviMate "pause" (which looks like a disconnect at the protocol level) can resume seamlessly via timeshift. A genuine channel switch is detected separately and stops recording immediately.

### Disk Usage

At 10 Mbps a 4-hour buffer is approximately **18 GB per active channel**. Only channels that are currently being watched are buffered — idle channels use no space. Files are deleted when a stream stops and cleaned up on startup.

### Configuration

| Env var | Default | Description |
|---|---|---|
| `CATCHUP_ENABLED` | `false` | Set to `true` to enable |
| `CATCHUP_DURATION_HOURS` | `4` | Hours of catchup to buffer and advertise to clients |
| `CATCHUP_PAUSE_GRACE_MINUTES` | `5` | Minutes to keep recording after the last viewer leaves (for pause/resume); channel switches bypass this |
| `TZ` | — | **Required.** Must match the timezone of your IPTV clients — TiviMate sends local time in timeshift URLs (e.g. `TZ=Europe/Amsterdam`) |

---

## Database Support

PostgreSQL is required for state persistence. Configure with:
- `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`

---

## Powered By

- [go-ldap/ldap](https://github.com/go-ldap/ldap) - LDAP authentication
- [spf13/cobra](https://github.com/spf13/cobra) - Command-line interface
- [bwmarrin/discordgo](https://github.com/bwmarrin/discordgo) - Discord bot integration
- [tellytv/go.xtream-codes](https://github.com/tellytv/go.xtream-codes) - Xtream Codes client
- [gin-gonic/gin](https://github.com/gin-gonic/gin) - Web framework

---

## Support

If you find StreamShare useful, consider supporting its development:

[![paypal](https://www.paypalobjects.com/en_US/i/btn/btn_donateCC_LG.gif)](https://www.paypal.me/lucasdup135)

