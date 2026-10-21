#!/usr/bin/env bash
# Vendors the browser assets of the web UI from the npm registry (no npm or
# node needed). Every tarball is checked against the sha512 integrity string
# that npm publishes for it, pinned below. Re-run it after bumping a version;
# fill in the new integrity from `curl -s https://registry.npmjs.org/<pkg>/<ver>`
# (the dist.integrity field) and record the change in
# internal/web/assets/static/VENDOR.md.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
static="$root/internal/web/assets/static"

HTMX_VERSION=2.0.11
HTMX_INTEGRITY=sha512-Thx/WtpeOQqSrqBCw/A1cwGJGg4UrVa3+sW0GmrM3p4gJgO89ecH4qtbnyzDDWFvBTqjnIMCgELTNt636dtamA==
SSE_VERSION=2.2.4
SSE_INTEGRITY=sha512-LJmxVhykyflBWgh5PvbRidcyuqMHlgfajmmzumvKctv9puvsufeH6OaejSMZTNTnEI8O2wXXn4ZZtBdpEMqmEQ==
ALPINE_VERSION=3.17.4
ALPINE_INTEGRITY=sha512-SlRXmqO6kYhnxlg+99etmuzJtE9Lk4QbKjBHqerXzaMflJqoJXdz/SI3IvHJGZ/vRVyC3bR0SSBz40oY7goBeg==
INTER_VERSION=5.3.0
INTER_INTEGRITY=sha512-OupL48va4JNofb97w6NYeF9S7W/kHNKM0Er8Dem5nqi4jeOLrVJDoE8tZEpnMJmtkvNbB1EIPPwHcdkF6b1oUA==

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# fetch <url> <integrity> <dir>: download, verify, extract into <dir>.
fetch() {
	local url="$1" want="$2" dir="$3"
	mkdir -p "$dir"
	curl -fsSL --proto '=https' --tlsv1.2 "$url" -o "$dir.tgz"
	local got
	got="sha512-$(openssl dgst -sha512 -binary "$dir.tgz" | openssl base64 -A)"
	if [ "$got" != "$want" ]; then
		echo "integrity mismatch for $url" >&2
		echo "  want $want" >&2
		echo "  got  $got" >&2
		exit 1
	fi
	tar -xzf "$dir.tgz" -C "$dir" --strip-components=1
}

fetch "https://registry.npmjs.org/htmx.org/-/htmx.org-$HTMX_VERSION.tgz" "$HTMX_INTEGRITY" "$work/htmx"
fetch "https://registry.npmjs.org/htmx-ext-sse/-/htmx-ext-sse-$SSE_VERSION.tgz" "$SSE_INTEGRITY" "$work/sse"
fetch "https://registry.npmjs.org/@alpinejs/csp/-/csp-$ALPINE_VERSION.tgz" "$ALPINE_INTEGRITY" "$work/alpine"
fetch "https://registry.npmjs.org/@fontsource-variable/inter/-/inter-$INTER_VERSION.tgz" "$INTER_INTEGRITY" "$work/inter"

mkdir -p "$static/vendor" "$static/fonts"
rm -f "$static"/vendor/*.js
cp "$work/htmx/dist/htmx.min.js" "$static/vendor/htmx-$HTMX_VERSION.min.js"
cp "$work/sse/dist/sse.min.js" "$static/vendor/htmx-ext-sse-$SSE_VERSION.min.js"
cp "$work/alpine/dist/cdn.min.js" "$static/vendor/alpine-csp-$ALPINE_VERSION.min.js"
cp "$work/htmx/LICENSE" "$static/vendor/LICENSE-htmx.txt"
cp "$work/inter/files/inter-latin-wght-normal.woff2" "$static/fonts/inter-latin-wght-normal.woff2"
cp "$work/inter/files/inter-latin-ext-wght-normal.woff2" "$static/fonts/inter-latin-ext-wght-normal.woff2"
cp "$work/inter/LICENSE" "$static/fonts/LICENSE-Inter.txt"
echo "vendored htmx $HTMX_VERSION, htmx-ext-sse $SSE_VERSION, alpine csp $ALPINE_VERSION, Inter $INTER_VERSION"
