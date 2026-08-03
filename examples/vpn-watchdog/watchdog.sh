#!/bin/sh
# stream-share VPN watchdog (EXAMPLE)
#
# stream-share only *reports* provider health. This script is the external half
# that acts on it: on a schedule it asks stream-share whether the IPTV provider
# is blocking our current VPN egress IP, and if so it reconnects the VPN onto a
# new server and re-checks — repeating until the new IP is accepted or the
# attempt budget runs out.
#
# It lives OUTSIDE stream-share on purpose, so that app has no dependency on any
# VPN. Everything VPN-specific is here. This example targets the gluetun control
# server, but gluetun is just one option — swap cycle_vpn()/provider-independent
# bits for whatever VPN you run.
#
# The gluetun control sequence is: stop, confirm stopped, start, confirm running.
# Once gluetun reports "running" we assume the tunnel is back and re-run the
# health check to decide whether the new server is unblocked — rather than
# polling gluetun's public-IP endpoint.
#
# Requirements: a POSIX shell and `curl` (both in the alpine/curl image used by
# docker-compose.snippet.yml). It must be able to reach the stream-share internal
# API (STREAM_SHARE_URL) and the VPN control server (GLUETUN_URL).
#
# NOTE: this file must use LF line endings. CRLF (Windows) endings make a POSIX
# shell fail with errors like `: not found` on blank lines and `elif unexpected`.
# The repo enforces LF via .gitattributes; if you edited it on Windows, run
# `sed -i 's/\r$//' watchdog.sh` (or `dos2unix watchdog.sh`).

# --- stream-share ------------------------------------------------------------
STREAM_SHARE_URL="${STREAM_SHARE_URL:-http://localhost:8080}"
INTERNAL_API_KEY="${INTERNAL_API_KEY:?set INTERNAL_API_KEY to the stream-share internal API key}"

# --- VPN control (gluetun example) -------------------------------------------
GLUETUN_URL="${GLUETUN_URL:-http://localhost:8000}"
# Auth: the gluetun control server supports HTTP Basic Auth or an API key,
# depending on its roles config. Set whichever matches; if both are set, Basic
# Auth wins (a client maps to exactly one auth method).
GLUETUN_USER="${GLUETUN_USER:-}"
GLUETUN_PASSWORD="${GLUETUN_PASSWORD:-}"
GLUETUN_API_KEY="${GLUETUN_API_KEY:-}"
# /v1/vpn/status is the current unified gluetun status/start/stop endpoint (both
# OpenVPN and WireGuard). Override to /v1/openvpn/status for older gluetun.
GLUETUN_STATUS_PATH="${GLUETUN_STATUS_PATH:-/v1/vpn/status}"

# --- behaviour ---------------------------------------------------------------
# CHECK_TIMES is the single schedule for hitting the provider: each run forces a
# fresh probe (which also refreshes the stream-share /healthz). When this watchdog
# is running, leave the stream-share HEALTHCHECK_TIMES empty so the provider is
# not probed twice on two schedules for the same information.
CHECK_TIMES="${CHECK_TIMES:-04:00,16:00}"       # local times to run, comma-separated HH:MM
MAX_RECONNECTS="${MAX_RECONNECTS:-5}"           # give up after this many server switches
RECONNECT_TIMEOUT="${RECONNECT_TIMEOUT:-45}"    # per-cycle budget (seconds) to confirm stopped then running

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }

# gluetun_curl applies whichever auth is configured (Basic wins over API key).
gluetun_curl() {
  if [ -n "$GLUETUN_USER" ] && [ -n "$GLUETUN_PASSWORD" ]; then
    curl -fsS -u "$GLUETUN_USER:$GLUETUN_PASSWORD" "$@"
  elif [ -n "$GLUETUN_API_KEY" ]; then
    curl -fsS -H "X-Api-Key: $GLUETUN_API_KEY" "$@"
  else
    curl -fsS "$@"
  fi
}

# json_str extracts a string field value from a small JSON body.
json_str() { sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" <<EOF
$1
EOF
}

# fetch_health forces a fresh probe and sets HEALTH_STATUS / HEALTH_DETAIL from
# the JSON body. Verdicts: healthy | blocked | error | unknown | disabled.
#
# IMPORTANT: /api/internal/health returns HTTP 503 when the provider is blocked
# or errored — that 503 is the intended "unhealthy" signal for Docker's
# HEALTHCHECK, and the real verdict is in the body. So read the body regardless
# of status code: do NOT pass curl -f here, or it discards the body exactly when
# it says "blocked". (A genuine connection failure yields an empty body, which we
# treat as unreachable.)
fetch_health() {
  body="$(curl -sS -H "X-API-Key: $INTERNAL_API_KEY" "$STREAM_SHARE_URL/api/internal/health" 2>/dev/null)"
  HEALTH_STATUS="$(json_str "$body" status)"
  HEALTH_DETAIL="$(json_str "$body" detail)"
}

# vpn_status / public_ip read the gluetun control server.
vpn_status() { json_str "$(gluetun_curl "$GLUETUN_URL$GLUETUN_STATUS_PATH" 2>/dev/null)" status; }
public_ip() {
  body="$(gluetun_curl "$GLUETUN_URL/v1/publicip/ip" 2>/dev/null)" || return 1
  ip="$(json_str "$body" public_ip)"; [ -z "$ip" ] && ip="$(json_str "$body" ip)"
  [ -n "$ip" ] || return 1
  echo "$ip"
}

set_vpn() { gluetun_curl -X PUT -H "Content-Type: application/json" -d "{\"status\":\"$1\"}" "$GLUETUN_URL$GLUETUN_STATUS_PATH" >/dev/null; }

# wait_status polls until gluetun reports the desired status or the deadline.
wait_status() { # $1=desired $2=deadline_epoch
  while [ "$(date +%s)" -lt "$2" ]; do
    [ "$(vpn_status)" = "$1" ] && return 0
    sleep 1
  done
  return 1
}

# cycle_vpn reconnects the VPN: stop, confirm stopped, start, confirm running.
# Once gluetun reports "running" we treat the tunnel as back and return — the
# caller re-runs the health check to judge whether the new server is unblocked,
# so we do not poll the public-IP endpoint. Reselecting a *different* server
# depends on the VPN config allowing more than one — e.g. the gluetun
# SERVER_*/VPN_* vars. The public IP is fetched once, best-effort, only to log
# which exit we landed on.
cycle_vpn() {
  deadline=$(( $(date +%s) + RECONNECT_TIMEOUT ))
  log "Cycling VPN..."

  set_vpn stopped || log "warning: stop request failed"
  wait_status stopped "$deadline" || log "warning: never confirmed stopped"

  set_vpn running || log "warning: start request failed"
  if wait_status running "$deadline"; then
    log "VPN reports running (IP: $(public_ip 2>/dev/null || echo '?'))."
  else
    log "warning: never confirmed running within ${RECONNECT_TIMEOUT}s."
  fi
}

# heal probes once and, while blocked, cycles the VPN up to MAX_RECONNECTS times.
heal() {
  fetch_health
  log "Provider status: ${HEALTH_STATUS:-<unreachable>}${HEALTH_DETAIL:+ ($HEALTH_DETAIL)}"

  case "$HEALTH_STATUS" in
    healthy|disabled) return 0 ;;
    blocked) ;;                         # the only case we act on
    *)
      # error / unknown / unreachable: reconnecting will not reliably help
      # (provider outage, bad probe channel, stream-share still starting). The
      # detail above says which — e.g. an empty HEALTHCHECK_STREAM_ID.
      log "Not a block; leaving the VPN alone."
      return 0
      ;;
  esac

  i=1
  while [ "$i" -le "$MAX_RECONNECTS" ]; do
    log "Blocked — reconnect attempt $i/$MAX_RECONNECTS"
    cycle_vpn
    fetch_health
    if [ "$HEALTH_STATUS" = "healthy" ]; then
      log "Recovered after $i reconnect(s)."
      return 0
    fi
    log "Still ${HEALTH_STATUS:-<unreachable>} after attempt $i."
    i=$((i + 1))
  done
  log "Gave up after $MAX_RECONNECTS reconnects; provider still ${HEALTH_STATUS:-<unreachable>}."
  return 1
}

# seconds_until_next returns the seconds to wait until the next HH:MM in CHECK_TIMES.
seconds_until_next() {
  now=$(date +%s)
  best=""
  IFS=','
  for t in $CHECK_TIMES; do
    t=$(echo "$t" | tr -d ' ')
    [ -z "$t" ] && continue
    target=$(date -d "today $t" +%s 2>/dev/null) || continue
    [ "$target" -le "$now" ] && target=$((target + 86400))
    if [ -z "$best" ] || [ "$target" -lt "$best" ]; then best=$target; fi
  done
  unset IFS
  [ -z "$best" ] && best=$((now + 43200))   # fallback: 12h
  echo $((best - now))
}

log "VPN watchdog starting. Checking at: $CHECK_TIMES"
heal || true                                # run once at startup
while true; do
  wait_s="$(seconds_until_next)"
  log "Next check in ${wait_s}s."
  sleep "$wait_s"
  heal || true
done
