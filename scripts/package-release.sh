#!/usr/bin/env bash
# Builds the release tarball for linux/amd64: the bitnews binary plus the two
# pinned post-processing tools (7zz and par2, ADR-0007), flat in one archive.
#
#   scripts/package-release.sh VERSION [OUTDIR]     OUTDIR defaults to dist/
#
# Writes OUTDIR/bitnews_VERSION_linux_amd64.tar.gz and its .sha256. The
# Proxmox LXC scripts (deploy/proxmox/) and any bare-metal install unpack it
# as is, so the tools and the binary always come from the same release.
set -euo pipefail

version="${1:?usage: package-release.sh VERSION [OUTDIR]}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${2:-$root/dist}"
name="bitnews_${version}_linux_amd64"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
	-ldflags "-s -w -X main.version=$version -X main.commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
	-o "$stage/bitnews" ./cmd/bitnews
scripts/get-postproc-tools.sh linux amd64 "$stage" >/dev/null
rm -f "$stage"/.postproc-tools-*
cp LICENSE "$stage/LICENSE"

mkdir -p "$out"
# Owned by root in the archive, whoever builds it (GNU tar and bsdtar spell it differently).
if tar --version 2>/dev/null | grep -q GNU; then
	owner=(--owner=0 --group=0)
else
	owner=(--uid 0 --gid 0)
fi
tar -C "$stage" "${owner[@]}" -czf "$out/$name.tar.gz" bitnews 7zz par2 LICENSE
(cd "$out" && { sha256sum "$name.tar.gz" 2>/dev/null || shasum -a 256 "$name.tar.gz"; } >"$name.tar.gz.sha256")
echo "$out/$name.tar.gz"
