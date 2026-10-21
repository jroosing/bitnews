#!/usr/bin/env bash
# Downloads the pinned Tailwind CSS standalone CLI (no node or npm) into
# bin/tools/ and verifies its SHA-256 against the checksums below, which come
# from the release's sha256sums.txt. Prints the path of the binary.
#
# To bump the version: change TAILWIND_VERSION, replace the checksums from
# https://github.com/tailwindlabs/tailwindcss/releases/download/vX.Y.Z/sha256sums.txt
# and update docs/adr/0005-web-ui-stack.md.
set -euo pipefail

TAILWIND_VERSION=4.3.3

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dir="$root/bin/tools"

case "$(uname -s)-$(uname -m)" in
Darwin-arm64) asset=tailwindcss-macos-arm64 want=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d ;;
Darwin-x86_64) asset=tailwindcss-macos-x64 want=7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1 ;;
Linux-x86_64) asset=tailwindcss-linux-x64 want=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a ;;
Linux-aarch64 | Linux-arm64) asset=tailwindcss-linux-arm64 want=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195 ;;
*)
	echo "get-tailwind: unsupported platform $(uname -s)-$(uname -m)" >&2
	exit 1
	;;
esac

bin="$dir/tailwindcss-$TAILWIND_VERSION"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

if [ -x "$bin" ] && [ "$(sha256 "$bin")" = "$want" ]; then
	echo "$bin"
	exit 0
fi

mkdir -p "$dir"
tmp="$(mktemp "$dir/.download.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
curl -fsSL --proto '=https' --tlsv1.2 \
	"https://github.com/tailwindlabs/tailwindcss/releases/download/v$TAILWIND_VERSION/$asset" -o "$tmp"
got="$(sha256 "$tmp")"
if [ "$got" != "$want" ]; then
	echo "get-tailwind: checksum mismatch for $asset" >&2
	echo "  want $want" >&2
	echo "  got  $got" >&2
	exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$bin"
trap - EXIT
echo "$bin"
