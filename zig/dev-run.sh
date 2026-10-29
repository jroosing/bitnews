#!/bin/sh
# Runs `zig <args>` with the pinned toolchain inside the dev container. The
# prefix goes to /tmp so the mounted tree keeps no Linux build output.
set -eu
zig=$(/src/scripts/get-zig.sh)
exec "$zig" "$@" --prefix /tmp/zig-out
