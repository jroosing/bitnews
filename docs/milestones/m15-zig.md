# M15 — Zig rewrite

## Goal

A Zig implementation of bitnews, on the `zig` branch, that can replace the Go one without the user noticing: same data, same configuration, same API, same adapters, same UI. Decision: [ADR-0014](../adr/0014-zig-memory-safe-where-practical.md).

## Scope

**In:** everything the Go version does. **Out:** new features (they go to `main` in Go first), published images and releases until the merge.

## Design

- **Branch.** `zig` lives next to `main`. It is merged into `main` once the parity gate below is met; until then nothing from it is published. Images are built locally only.
- **Layout.** `zig/` holds the Zig code with its own `build.zig` and `build.zig.zon`. The Go code stays as the reference and the parity harness. Files both need (SQL migrations, web assets, golden files, fuzz seeds) are copied into `zig/` by a sync step, and a check fails when the copies differ from the originals.
- **Toolchain.** Zig 0.17.0, pinned with a SHA-256 by `scripts/get-zig.sh`; upgrades are deliberate commits, never master. Dependencies in `build.zig.zon` are pinned by hash.
- **Platform.** Debian trixie (glibc), linux/amd64 and linux/arm64; macOS for development (libtorrent from Homebrew). Dynamic linking against the distro's libtorrent-rasterbar 2.0.x, OpenSSL 3 and libcurl. `7zz` and par2cmdline-turbo stay pinned downloads (`scripts/get-postproc-tools.sh`).
- **Torrent engine.** libtorrent-rasterbar in-process behind `libbitnews_lt`, a C wrapper compiled with the system g++ (about 15 functions: add, remove, pause, resume, stats, relocate, limits, port and interfaces, alerts as events). Zig never sees C++.
- **Concurrency.** Code is written against `std.Io` and runs on its thread-based implementation; cancellation and timeouts go through Io. Z0/Z1 check that a blocked socket read can be cancelled; where it cannot, plain threads are used there.
- **Memory.** An arena per request, Article and post-processing step; one documented owner for long-lived state; `smp_allocator` in release, `DebugAllocator` (leaks, use-after-free) in tests and fuzzing; hard caps on every hostile input; no pointers into growing buffers.
- **HTTP.** `std.http.Server` with our own router, middleware (auth, gate, limits, timeouts), server-sent events and multipart reader.
- **TLS and outgoing HTTP.** OpenSSL for NNTPS, libcurl (on the same OpenSSL) for indexer fetches and the prober; strict verification, TLS 1.2 or newer, no skip-verify.
- **SQLite.** The pinned amalgamation, compiled by Zig, with our own thin binding (WAL, busy timeout, one writer and readers).
- **Crypto.** `std.crypto` (XChaCha20-Poly1305 in the `v1:` sealed format, argon2id PHC, SHA-256, MD5, constant-time compares). Golden files written by Go are read by Zig and the other way round.
- **Post-processing tools.** The ADR-0007 runner contract on `std.process.Child`: argument list, scrubbed environment, working directory, timeout, process group killed on cancel, capped output, passwords on stdin; table tests with a fake tool that stalls, floods or forks.
- **Web UI.** Typed component functions on our own `html` module (context-aware escaping, safe URLs, conditional classes, out-of-band swaps), with tests and a fuzz target. The `.templ` files are the specification: same ids, classes and `hx-` attributes. CSS, JS, htmx and Alpine are the shared assets.
- **Configuration and output.** The same environment variables, flags, defaults and messages; settings and API JSON with the same content; logs with the same levels and field names in text and JSON; a `Secret` type that prints `[redacted]` and refuses JSON.
- **Fuzzing.** Zig's built-in fuzzer, seeded with the Go corpora; `zig build fuzz-smoke` runs every target for a fixed time, locally.

## Parity gate (merge to main)

1. **Black-box parity.** The Go e2e harness gets `BITNEWS_E2E_BINARY=path`, which starts an external binary with a temporary config dir and ports. Every e2e test that only uses HTTP, NNTP, the local swarm and the database file passes against the Zig binary; tests that inject in-process fakes are skipped there and covered by Zig unit tests.
2. **Data compatibility both ways.** A config dir and database written by Go open in Zig with servers, sessions, the app key and history intact, and the other way round.
3. **Zig tests and fuzzing.** `zig build check` is green; every hostile parser has a fuzz target.
4. **Soak.** Two weeks on the owner's instance with Sonarr and Radarr, without import regressions.

## Issues

### M15-01: Z0 — Build spike
**Goal:** Prove the toolchain split. On Debian trixie and macOS, the distro libtorrent 2.0.x, OpenSSL and libcurl are installed; `libbitnews_lt` is built with the system C++ compiler; a Zig `main` links it with the SQLite amalgamation, adds a magnet and prints its peers; a `std.Io` socket read is cancelled.
- [x] `scripts/get-zig.sh` pins Zig 0.17.0 (SHA-256 per platform); `zig/build.zig` builds the spike on linux/arm64 (Debian trixie, in `zig/Dockerfile.dev`) and macOS arm64. **linux/amd64 is not verified**: see the findings.
- [x] Findings (what works, what does not, `std.Io` cancellation, fuzzer maturity) are written into this spec.

#### Z0 findings

**The toolchain split works.** One Zig binary links the SQLite 3.54.1 amalgamation (a pinned `build.zig.zon` dependency, compiled by Zig as C), libcurl, OpenSSL and `libbitnews_lt`. The wrapper is built by `zig/lt/build-lt.sh` with the system C++ compiler and pkg-config's flags (they carry defines that must match the installed libtorrent) and loaded as a shared library; Zig only sees the translated C header. On Debian trixie arm64 (libtorrent 2.0.11, OpenSSL 3.5.7, libcurl 8.14.1) a magnet's metadata arrived in 3 s from 9 peers; on macOS (Homebrew libtorrent 2.1.2) in 2 s from 3. The `STRICT` tables and `RENAME COLUMN` of the store's migrations work on the pinned SQLite.

**`std.Io` cancellation works.** A task blocked in a socket read (`io.concurrent` on the threaded `Io`) returns as soon as it is cancelled, in 0 ms, on macOS and Linux (`zig/src/io_cancel.zig`). Two things to remember: the `Reader` interface reports every failure as `error.ReadFailed` and the real cause (`error.Canceled`) is in `reader.err`; and a cancellation is signalled once, at the next cancellation point, so a task must not swallow it. Decision 9 (`std.Io` on threads) stands.

**The built-in fuzzer works, with conditions.** `zig build test --fuzz[=limit]` is coverage guided and finds a planted out-of-bounds read within about 10 000 runs, saving the input to `.zig-cache/f/crash`. But it never found a 4-byte magic prefix from scratch in 84 million runs, so good seeds are essential. Seeds are `Smith` streams: a slice is a u32 little-endian length followed by the bytes, so the sync step must wrap the Go corpus files that way (a raw file is not a valid seed). `std.testing.allocator` is a `SafeAllocator` in 0.17. Decision 18 (built-in fuzzer only) stands, with the seed conversion added to the sync step.

**Things the plan did not foresee:**
- **Version skew between development and production.** Homebrew has libtorrent 2.1.2, Debian trixie 2.0.11. The wrapper must compile against both, so it stays on the API they share, and macOS is for development only.
- **macOS libcurl is the system one** (SecureTransport), not OpenSSL. TLS behaviour differs from production there; production TLS is only tested on Debian.
- **linux/amd64 is unverified.** Docker's Rosetta cannot run the amd64 Zig compiler on an arm64 Mac (`rosetta error: bss_size overflow`). It needs an x86_64 machine; the build is native per architecture and Debian ships the same package versions, so the risk is low but not checked.
- **Zig 0.17 specifics:** `main(init: std.process.Init)` brings the `Io`, the allocator arena and the arguments; `addSystemCommand` needs an argv (the wrapper script runs through `sh`); `@cImport` is replaced by `addTranslateC`; fetched packages go to a project-local `zig-pkg/` (ignored); the first fuzz run in a cold cache can panic on a missing `tmp` dir and works when run again.

### M15-02: Z1 — Skeleton
**Goal:** Bootstrap (environment variables, flags, umask), logging, SQLite with the shared migrations, master key and sealed secrets read from a Go database, the HTTP server with health and app-key auth, the sync step and `zig build check`, and the black-box mode of the Go e2e harness.
- [ ] A Go-written config dir opens; the first black-box tests (health, auth) pass against the Zig binary.

### M15-03: Z2 — Core domain and own API
**Goal:** Downloads, Categories, Settings, the Queue, the event bus and `/api/v1`.
- [ ] The API part of the black-box suite passes.

### M15-04: Z3 — Usenet fetch
**Goal:** NZB, NNTP client and pool, yEnc, file writer, scheduler and runner, with fuzz targets for NZB, NNTP and yEnc.
- [ ] An NZB reaches Fetched against the fake NNTP server, black-box.

### M15-05: Z4 — Post-processing
**Goal:** The tool runner, par2, 7zz unpack, finalize and tidy.
- [ ] A Download reaches Ready with the same output tree as the Go version.

### M15-06: Z5 — SABnzbd adapter
**Goal:** `/sabnzbd/api` as in M4.
- [ ] The SABnzbd part of the black-box suite passes; the Usenet soak can start.

### M15-07: Z6 — Web UI
**Goal:** Sign-in, setup, queue, history, detail, settings and the live stream, on the `html` module.
- [ ] The web part of the black-box suite passes.

### M15-08: Z7 — Torrents
**Goal:** The libtorrent engine, the torrent runner, the qBittorrent adapter, and piece completion converted to fast-resume data on the first start.
- [ ] The torrent part of the black-box suite passes; full parity.

### M15-09: Z8 — Merge
**Goal:** The parity gate is met; locally built Docker image and LXC script; `zig` is merged into `main`, the Go code removed, the shared files moved into place.
- [ ] Parity gate items 1–4 hold.
- [ ] The build and the black-box suite are verified on linux/amd64 (the Z0 spike could only run on arm64, see the Z0 findings).
