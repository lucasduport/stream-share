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
       --user test \
       --password passwordtest
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
       --user your_username \
       --password your_password
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

### Authentication

API requests require an API key provided in the `X-API-Key` header:

```bash
curl -H "X-API-Key: your_api_key" https://streamshare.example.com/api/internal/status
```

The API key is automatically generated on first run and stored in the database.

To override, set `INTERNAL_API_KEY` in the environment so the bot and integrations can authenticate reliably.

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
- `CACHE_FOLDER` — Absolute path where cached files are stored.
- `INTERNAL_API_KEY` — API key used by the internal API (Discord bot and tools).

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

