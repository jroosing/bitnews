#!/usr/bin/env bash
# Downloads the pinned Zig toolchain into bin/tools/ and verifies its SHA-256
# against the checksums below, which come from https://ziglang.org/download/index.json.
# Prints the path of the zig binary.
#
# Zig is pre-1.0 and its standard library breaks between releases (ADR-0014):
# upgrade on purpose, one release at a time, never to master. To bump: change
# ZIG_VERSION, replace the checksums from the index.json entry of that release,
# and set minimum_zig_version in zig/build.zig.zon.
set -euo pipefail

ZIG_VERSION=0.17.0

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dir="$root/bin/tools"

case "$(uname -s)-$(uname -m)" in
Darwin-arm64) target=aarch64-macos want=b607e9b9234790a008116ae5bdb71c6243b84b9fb42a53a9e70fde41c06c536a ;;
Darwin-x86_64) target=x86_64-macos want=4f9a1c5269aa17ebda5e6d3c2b89d6cbf36f7d2b22a0306e9ab98f25f95529c6 ;;
Linux-x86_64) target=x86_64-linux want=1cbe9df9f27e6b78d14ccbca43b6703a404ef79ef1c463de901d7f088d4e2026 ;;
Linux-aarch64 | Linux-arm64) target=aarch64-linux want=9e8d11661d4ae3bd57702a3832781e23ad151dde5798e16a5ccd503f65234ff8 ;;
*)
	echo "get-zig: unsupported platform $(uname -s)-$(uname -m)" >&2
	exit 1
	;;
esac

home="$dir/zig-$target-$ZIG_VERSION"
bin="$home/zig"

if [ -x "$bin" ] && [ "$("$bin" version 2>/dev/null)" = "$ZIG_VERSION" ]; then
	echo "$bin"
	exit 0
fi

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

mkdir -p "$dir"
tmp="$(mktemp "$dir/.download.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
curl -fsSL --proto '=https' --tlsv1.2 \
	"https://ziglang.org/download/$ZIG_VERSION/zig-$target-$ZIG_VERSION.tar.xz" -o "$tmp"
got="$(sha256 "$tmp")"
if [ "$got" != "$want" ]; then
	echo "get-zig: checksum mismatch for zig-$target-$ZIG_VERSION.tar.xz" >&2
	echo "  want $want" >&2
	echo "  got  $got" >&2
	exit 1
fi

rm -rf "$home"
mkdir -p "$home"
tar -xJf "$tmp" -C "$home" --strip-components=1
echo "$bin"
