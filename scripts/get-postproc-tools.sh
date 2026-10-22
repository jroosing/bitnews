#!/usr/bin/env bash
# Downloads the pinned post-processing tools, 7-Zip (7zz) and par2cmdline-turbo
# (par2), verifies the SHA-256 of each archive against the checksums below and
# installs the two binaries into a directory (ADR-0007).
#
#   scripts/get-postproc-tools.sh                  host platform -> bin/tools/
#   scripts/get-postproc-tools.sh linux arm64 DIR  another platform -> DIR
#
# OS is linux or darwin, ARCH is amd64 or arm64 (darwin gets one universal
# binary). The Linux binaries are static (the 7-Zip "7zzs" build), so they run
# in a distroless image. Prints the directory.
#
# To bump a version: change the version below, replace the checksums from the
# release assets (https://github.com/ip7z/7zip/releases and
# https://github.com/animetosho/par2cmdline-turbo/releases) and update
# docs/adr/0007-external-postproc-tools.md.
set -euo pipefail

SEVENZIP_VERSION=26.05
PAR2_VERSION=1.5.0

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

os="${1:-}"
arch="${2:-}"
dest="${3:-$root/bin/tools}"

if [ -z "$os" ]; then
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) echo "get-postproc-tools: unsupported OS $(uname -s)" >&2; exit 1 ;;
	esac
fi
if [ -z "$arch" ]; then
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) echo "get-postproc-tools: unsupported architecture $(uname -m)" >&2; exit 1 ;;
	esac
fi

sz_tag="${SEVENZIP_VERSION/./}"
case "$os-$arch" in
linux-amd64)
	sz_asset="7z$sz_tag-linux-x64.tar.xz" sz_sha=22cf82fcc6eb7474f1d5bb4f02c0f8ebd573a718089d6833dc30f8350a3b232e sz_bin=7zzs
	p2_asset="par2cmdline-turbo-$PAR2_VERSION-linux-amd64.zip" p2_sha=5a9f64386813456693c2ea1fb7649436fe7544bbdf97fd73b3483dfcc8aca464
	;;
linux-arm64)
	sz_asset="7z$sz_tag-linux-arm64.tar.xz" sz_sha=0b71f6cfc34669b3bf69f2347efd3429f77020665003a13b9cdc53b784270421 sz_bin=7zzs
	p2_asset="par2cmdline-turbo-$PAR2_VERSION-linux-arm64.zip" p2_sha=3afb2f0b319fc4e6353c6d4261994757cbe3189f968a72420b4ba1d63f9c9a70
	;;
darwin-amd64 | darwin-arm64)
	sz_asset="7z$sz_tag-mac.tar.xz" sz_sha=12ef6830317743cfd3f0ab1724bde43fcc270d8b6d8564f8c12c2a2b7f993570 sz_bin=7zz
	p2_asset="par2cmdline-turbo-$PAR2_VERSION-macos-universal.zip" p2_sha=dcd32f194a4f7144e9a2c58069476cc954c90625b266d57fa012218673b8bd34
	;;
*)
	echo "get-postproc-tools: unsupported platform $os-$arch" >&2
	exit 1
	;;
esac

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

mkdir -p "$dest"
stamp="$dest/.postproc-tools-$SEVENZIP_VERSION-$PAR2_VERSION-$os-$arch"
if [ -x "$dest/7zz" ] && [ -x "$dest/par2" ] && [ -f "$stamp" ]; then
	echo "$dest"
	exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fetch() { # url file sha
	curl -fsSL --retry 3 -o "$2" "$1"
	if [ "$(sha256 "$2")" != "$3" ]; then
		echo "get-postproc-tools: checksum mismatch for $(basename "$2")" >&2
		exit 1
	fi
}

fetch "https://github.com/ip7z/7zip/releases/download/$SEVENZIP_VERSION/$sz_asset" "$work/7z.tar.xz" "$sz_sha"
fetch "https://github.com/animetosho/par2cmdline-turbo/releases/download/v$PAR2_VERSION/$p2_asset" "$work/par2.zip" "$p2_sha"

mkdir "$work/7z" "$work/par2"
tar -xJf "$work/7z.tar.xz" -C "$work/7z"
if command -v unzip >/dev/null 2>&1; then
	unzip -q "$work/par2.zip" -d "$work/par2"
else
	bsdtar -xf "$work/par2.zip" -C "$work/par2"
fi

install -m 0755 "$work/7z/$sz_bin" "$dest/7zz"
install -m 0755 "$work/par2/par2" "$dest/par2"
rm -f "$dest"/.postproc-tools-*
: >"$stamp"
echo "$dest"
