#!/usr/bin/env sh
# Keeps the bitnews torrent listen port equal to the port gluetun forwards.
#
# Meant for a small alpine container that shares gluetun's forwarded_port file
# (a read-only mount of gluetun's /tmp/gluetun) and runs this script in a loop.
# Every SYNC_INTERVAL_SECONDS it reads the file, asks bitnews which port it
# listens on (GET /api/v1/torrent/port) and, when they differ, sets the new one
# (POST /api/v1/torrent/port). bitnews moves the port live and stores it, so no
# restart is needed. Because it compares with bitnews itself, it also repairs a
# bitnews that was reset or restarted with another port.
#
#   BITNEWS_URL            base URL of bitnews            (default http://127.0.0.1:9494)
#   BITNEWS_API_KEY        the app key, Settings > Security   (required)
#   FORWARDED_PORT_FILE    gluetun's port file            (default /gluetun/forwarded_port)
#   SYNC_INTERVAL_SECONDS  seconds between checks         (default 30)
#
# bitnews answers 409 port_fixed when BITNEWS_TORRENT_PORT is set: remove that
# variable to let the port follow gluetun.
set -eu

apk add --no-cache curl >/dev/null

BITNEWS_URL=${BITNEWS_URL:-http://127.0.0.1:9494}
BITNEWS_URL=${BITNEWS_URL%/}
BITNEWS_API_KEY=${BITNEWS_API_KEY:?BITNEWS_API_KEY is required}
FORWARDED_PORT_FILE=${FORWARDED_PORT_FILE:-/gluetun/forwarded_port}
SYNC_INTERVAL_SECONDS=${SYNC_INTERVAL_SECONDS:-30}

ENDPOINT="$BITNEWS_URL/api/v1/torrent/port"

log() {
  printf '%s [bitnews-port-sync] %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$*"
}

valid_port() {
  case "$1" in
    '' | *[!0-9]*) return 1 ;;
  esac
  [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

# current_port prints the port bitnews listens on, or nothing when it cannot
# be read (bitnews down, wrong key, torrents off).
current_port() {
  curl -sS -f --max-time 10 \
    -H "X-Api-Key: $BITNEWS_API_KEY" \
    "$ENDPOINT" 2>/dev/null |
    sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p'
}

# set_port sends the new port and logs bitnews's own error text on failure.
set_port() {
  body=$(mktemp)
  http_code=$(curl -sS -o "$body" -w '%{http_code}' --max-time 10 \
    -X POST \
    -H "X-Api-Key: $BITNEWS_API_KEY" \
    -H 'Content-Type: application/json' \
    --data "{\"port\":$1}" \
    "$ENDPOINT") || http_code=000
  if [ "$http_code" != 200 ]; then
    log "setting port $1 failed: http=$http_code $(tr -d '\n' <"$body" | cut -c1-200)"
    rm -f "$body"
    return 1
  fi
  rm -f "$body"
}

log "starting: url=$BITNEWS_URL file=$FORWARDED_PORT_FILE interval=${SYNC_INTERVAL_SECONDS}s"

seen=
synced=
while true; do
  if [ -f "$FORWARDED_PORT_FILE" ]; then
    port=$(tr -d '[:space:]' <"$FORWARDED_PORT_FILE")
    if valid_port "$port"; then
      # Say what gluetun forwards whenever it is new (at start and after a
      # reconnect), not on every check.
      if [ "$port" != "$seen" ]; then
        log "gluetun forwarded port: $port"
        seen=$port
      fi
      have=$(current_port || true)
      if [ -z "$have" ]; then
        log "cannot read the bitnews port (is it up, and is the API key right?)"
      elif [ "$have" != "$port" ]; then
        if set_port "$port"; then
          log "bitnews torrent port set to $port (was $have)"
          synced=$port
        fi
      elif [ "$port" != "$synced" ]; then
        log "bitnews already listens on $port"
      fi
      [ -n "$have" ] && [ "$have" = "$port" ] && synced=$port
    else
      log "ignoring invalid forwarded port: '$port'"
    fi
  else
    log "waiting for forwarded port file: $FORWARDED_PORT_FILE"
  fi
  sleep "$SYNC_INTERVAL_SECONDS"
done
