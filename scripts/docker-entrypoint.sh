#!/bin/sh
# Container entrypoint (ADR-0010). Started as root (the image default), it
# gives the config dir to PUID:PGID and runs bitnews as that user, the way
# the linuxserver.io images do. Started as anyone else (`user:` in compose,
# `docker run --user`), it runs bitnews as that user and PUID/PGID do not
# matter. Arguments go to bitnews unchanged ("healthcheck" included).
set -eu

if [ "$(id -u)" != 0 ]; then
	exec /bitnews "$@"
fi

PUID="${PUID:-65532}"
PGID="${PGID:-65532}"
case "$PUID$PGID" in
'' | *[!0-9]*)
	echo "bitnews: PUID and PGID must be numeric ids, got PUID=$PUID PGID=$PGID" >&2
	exit 2
	;;
esac

# The healthcheck only reads; it needs no handover and no drop.
if [ "${1:-}" = healthcheck ]; then
	exec /bitnews "$@"
fi

if [ "$PUID" = 0 ]; then
	echo "bitnews: running as root (PUID=0); set PUID and PGID to run as a normal user" >&2
	exec /bitnews "$@"
fi

config="${BITNEWS_CONFIG_DIR:-/config}"
mkdir -p "$config"
# Hand the config dir over only when it is not ours yet, so a restart reads
# nothing. -h re-owns symlinks themselves and never follows them.
if [ "$(stat -c %u:%g "$config")" != "$PUID:$PGID" ]; then
	if ! chown -R -h "$PUID:$PGID" "$config"; then
		echo "bitnews: cannot give $config to $PUID:$PGID (PUID:PGID). With cap_drop: ALL, add cap_add: CHOWN, SETUID, SETGID and DAC_READ_SEARCH" >&2
		exit 1
	fi
fi

exec su-exec "$PUID:$PGID" /bitnews "$@"
