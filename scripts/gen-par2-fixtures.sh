#!/usr/bin/env bash
#
# gen-par2-fixtures.sh - regenerate the PAR2 golden fixtures (issue M3-06).
#
# Usage:   scripts/gen-par2-fixtures.sh [case ...]     (default: every case)
#
# DEV ONLY. This is the one place that runs par2cmdline, and only offline, by
# hand, to produce committed test data. The product never runs it (ADR-0002).
# Needs `par2` (par2cmdline) on PATH and a Go toolchain. The output depends on
# the par2cmdline version, which is recorded in every manifest.json; commit
# the result and say which version made it in
# internal/postproc/par2/testdata/README.md.
#
# For every case this writes
#   internal/postproc/par2/testdata/golden/<case>/data/       payload files
#   internal/postproc/par2/testdata/golden/<case>/par2/       what par2 c wrote
#   internal/postproc/par2/testdata/golden/<case>/manifest.json
# Payload bytes come from `fixturegen payload` with a fixed per-file seed
# (math/rand/v2 PCG), never from /dev/urandom, so reruns give identical
# payloads, and par2cmdline is deterministic, so identical .par2 files (check
# with `git status` after a rerun). Keep the total under 5 MiB.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
pkg=./internal/postproc/par2/internal/fixturegen
out="$root/internal/postproc/par2/testdata/golden"

command -v par2 >/dev/null || { echo "par2 (par2cmdline) not found on PATH" >&2; exit 1; }
version=$(par2 --version | head -n1 | sed 's/^par2cmdline version //')
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
gen="$tmp/fixturegen"
(cd "$root" && go build -o "$gen" "$pkg")

# gen_case NAME "PAR2 ARGS" "file name:size:seed" ...
gen_case() {
  local name=$1 args=$2
  shift 2
  if [ -n "${ONLY:-}" ] && [[ " $ONLY " != *" $name "* ]]; then return; fi
  local dest="$out/${name:?}" work spec fname rest size seed
  work=$(mktemp -d "$tmp/case.XXXXXX")
  local files=()
  for spec in "$@"; do
    fname=${spec%%:*}; rest=${spec#*:}; size=${rest%%:*}; seed=${rest#*:}
    "$gen" payload -seed "$seed" -size "$size" -out "$work/$fname"
    files+=("$fname")
  done
  # $args is a list of flags on purpose (unquoted); par2 progress uses \r.
  local info
  # shellcheck disable=SC2086
  info=$(cd "$work" && par2 c $args set.par2 "${files[@]}" 2>&1 | tr '\r' '\n') ||
    { echo "$name: par2 failed" >&2; exit 1; }
  printf '%s\n' "$info" | grep -q '^Done$' || { echo "$info" >&2; echo "$name: par2 failed" >&2; exit 1; }
  local slice recov
  slice=$(printf '%s\n' "$info" | sed -n 's/^Block size: \([0-9]*\)$/\1/p')
  recov=$(printf '%s\n' "$info" | sed -n 's/^Recovery block count: \([0-9]*\)$/\1/p')
  [ -n "$slice" ] && [ -n "$recov" ] || { echo "$name: cannot read block size / count" >&2; exit 1; }

  rm -rf "$dest"
  mkdir -p "$dest/data" "$dest/par2"
  for fname in "${files[@]}"; do mv "$work/$fname" "$dest/data/$fname"; done
  mv "$work"/set*.par2 "$dest/par2/"
  "$gen" manifest -case "$name" -dir "$dest" -args "$args" -version "$version" \
    -slice-size "$slice" -recovery-blocks "$recov"
  echo "$name: slice size $slice, $recov recovery blocks, $(ls "$dest/par2" | wc -l | tr -d ' ') par2 files"
}

ONLY="$*"
gen_case single_small      "-s4096 -r50"              "single.bin:10000:1"
gen_case multi_files       "-s4096 -r10 -n2"          "alpha.bin:100000:2" "beta.dat:33333:3" "tiny.txt:700:4" "exact.bin:16384:5" "empty.bin:0:6"
gen_case slice_16k         "-s16384 -r40 -n3"         "movie.mkv:200000:7" "sub.srt:50001:8" "one.byte:1:9"
gen_case block_count       "-b64 -r15"                "big.dat:300000:10" "small.dat:77777:11"
gen_case explicit_uniform  "-s4096 -c20 -u -n4"       "u1.bin:150000:12" "u2.bin:9999:13"
gen_case variable_volumes  "-s2048 -r20 -n5"          "v1.bin:120000:14" "v2.bin:3000:15"
gen_case limited_volumes   "-s4096 -c20 -l"           "l1.bin:100000:16" "l2.bin:50000:17"
gen_case first_block       "-s4096 -c10 -f7 -n2"      "f1.bin:60000:18" "f2.bin:20000:19"
gen_case high_exponents    "-s4096 -c10 -f32764 -n2"  "h1.bin:60000:20" "h2.bin:20000:21"
gen_case unicode_names     "-s4096 -c4"               "my movie ß 日本語 (2024).mkv:50000:22" "a  b  c.bin:8000:23"
gen_case many_slices       "-s32 -c8"                 "ms1.bin:20000:24" "ms2.bin:777:25"
gen_case large             "-s16384 -r10 -n3"         "large1.bin:1048576:26" "large2.bin:400000:27"
