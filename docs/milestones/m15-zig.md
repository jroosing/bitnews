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

1. **Black-box parity.** `internal/blackbox` starts a daemon binary as a process (`BITNEWS_E2E_BINARY=path`, else the Go build) with a temporary config dir and ports and talks to it over HTTP, NNTP, the local swarm and the files on disk. It passes against the Zig binary with nothing skipped (`ZIG_BLACKBOX_SKIP` is empty); behaviour that today only the in-process e2e tests check (injected fakes) is moved into it or covered by Zig unit tests.
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
**Goal:** Bootstrap (environment variables, flags, umask), logging, SQLite with the shared migrations, master key and sealed secrets read from a Go database, the HTTP server with health and app-key auth, the sync step and `zig build check`, and a black-box suite that runs against either binary.
- [x] A Go-written config dir opens (database, key file, app key, sealed secrets: golden files in both directions); the black-box tests for health, auth, routing, start-up failures, fresh and existing config dirs, logging and shutdown pass against the Zig binary on macOS and on Debian trixie (`make blackbox-zig`).
- [x] `zig build check` (format, tests, shared files) and `make zig-check` (that plus the parity suite) are the gate of the branch.

#### Z1 findings

**Parity suite as a separate package.** The black-box mode did not go into the existing e2e harness (which builds the daemon in-process with injected fakes). `internal/blackbox` is a new test package that starts a binary as a process: the Go one (built in `TestMain`) by default, or the one in `BITNEWS_E2E_BINARY`. It is always green against Go first; `make blackbox-zig` runs it against the Zig binary and skips what later milestones add (`ZIG_BLACKBOX_SKIP` in the Makefile, which shrinks): the data routes and the events stream (Z2), the admin account, the setup token and the web pages (Z6). Later milestones add their scenarios to the same package.

**Where Zig differs from Go, and what it cost:**
- **Port sharing.** std's `reuse_address` sets `SO_REUSEPORT` as well, so a second daemon bound the same port without an error. Clearing it after the bind works on macOS but not on Linux (found by running the tests in Debian), so `http_listen.zig` creates the socket itself: `SO_REUSEADDR` only, as Go does, with tests for a taken port and for an immediate rebind after a restart.
- **`Io.File.Writer.init` writes at an offset** (`pwrite`) when stdout or stderr is redirected to a regular file, so each process overwrote the file from byte 0. stdout and stderr use `initStreaming`.
- **No connect timeout** in the threaded `Io` (`@panic("TODO ...")`): the health probe connects directly and reads through the server's deadline-aware reader.
- **Errors carry no message.** Go's messages need data (the migration that failed, the length of a wrong key), so the Zig code uses a `Diagnostic` next to the error (`migrate`), or recomputes it on the error path (`keyfile.decodedLength`). Paths in messages are quoted with Go's `%q` rules (`log_text.writeQuoted`).
- **The setup gate** (503 `setup_required` for everything but `GET /api/v1/health` while no user exists) is a hook in the server, evaluated before routing like `auth.Gate`; the state is one atomic read from the users table. Creating the first user comes with the web UI.
- **Redirects** of unclean paths are 307, like Go's mux; a body the handler never reads no longer fails with 413 (only a read does, as with `http.MaxBytesReader`), and the connection is closed afterwards.
- **`Secret`** refuses JSON at compile time; Go writes `"[redacted]"`. Text log timestamps are Go's (`Z` for UTC, milliseconds). `isPrintRune` approximates `unicode.IsPrint`, so a rare unassigned code point may be printed bare where Go quotes it.
- **Fuzzing.** `zig build test-<group> --fuzz` runs one target per module and fuzz runs must not overlap (they share `.zig-cache`).

**Deliberately not in Z1:** `BITNEWS_ADMIN_PASSWORD_FILE` (logs a warning), the setup token, the web pages, the events stream, and `go_version` in `/api/v1/version` (Zig returns `zig_version`).

### M15-03: Z2 — Core domain and own API
**Goal:** Downloads, Categories, Servers, Settings, the event bus and `/api/v1`.
- [x] The API part of the black-box suite passes against the Zig binary on macOS and on Debian trixie (125 `TestAPI*` scenarios, green against Go first). What still fails is listed in `ZIG_BLACKBOX_SKIP` by milestone: adding downloads and the server connection tests (Z3), the admin account and the web pages (Z6).
- [x] Databases written by Go load and what Zig writes loads in Go: categories, servers with sealed passwords, settings, downloads.

#### Z2 findings

**How it was built.** A shared foundation first (ids, Go-compatible timestamps, the Download lifecycle, events and a `Publisher`, the `Failure` convention, request decoding), then six slices in parallel (settings, servers, categories, downloads, events, and the Go scenarios) and a seventh for the data remover, joined in `services.zig`. Each slice defines the seams it needs as small interface structs (`DownloadsLookup`, `CategoryLookup`, `DataRemover`, `FetchHolder`, ...), and `services.zig` is the only place that knows all of them.

**Where Zig differs from Go, and what it cost:**
- **Request bodies.** `std.json` reads an array of small integers into a string, accepts `"563"`, `563.0` and `5e2` for integers and matches keys case-sensitively. `strict_json.zig` replaces it for requests: keys match case-insensitively, `null` into a plain field is a no-op (and a top-level `null` an empty request), strings, integers and bools are strict, the last duplicate key wins, nothing may follow the value. Every request field needs a default.
- **Response bodies.** Go escapes `<`, `>`, `&` and U+2028/9 in JSON; `Response.json` and the SSE frames do too (a byte-level replacement, which is safe because those bytes only occur inside strings).
- **Events.** Go also has `servers_changed` (added), `postprocess_stage` and `listen_port_changed` (Z4 and Z7 add them; the SSE mapping is an exhaustive switch, so the compiler will ask). The queue events are named `usenet_queue_paused`, `torrent_queue_resumed` and so on on the wire.
- **Settings row.** Go stores the Settings under the key `settings`; the `main` row in the golden database is never read.
- **Streaming.** The HTTP server gained a streaming response mode (chunked, a per-write timeout, a probe for a client that left, cancellation on shutdown) for the SSE stream. Blocking writes ignore the deadline in the threaded `Io`, so stream writes go out in 2 KiB pieces.
- **Data remover.** Go's `os.RemoveAll` does not chmod, so a directory without write permission is an error and the Download stays; the delete is bound to the parent's handle after the symlink checks, so a link swapped in afterwards cannot redirect it. Stricter than Go only on odd input (a NUL byte, a `/` parent, an id that is not a plain name).
- **Tests run on the LLVM backend.** On aarch64 the default Debug backend (self-hosted) makes `testing.fuzz` return at once, so no fuzz corpus ever ran in `zig build test`; `build.zig` sets `use_llvm` for tests. `zig build ... --fuzz=N` still fuzzes only the first fuzz test it finds, so fuzz targets one at a time.

**Not in Z2:** adding downloads (Z3 for NZB, Z7 for torrents), the server connection tests (Z3), the Queue and the runners (Z3, Z7), the torrent port routes (Z7), the history page's query (Z6; the repo has it).

### M15-04: Z3 — Usenet fetch
**Goal:** NZB, NNTP client and pool, yEnc, file writer, scheduler and runner, with fuzz targets for NZB, NNTP and yEnc.
- [x] An NZB reaches Fetched against the fake NNTP server, black-box: 52 `TestUsenet*` scenarios (fetch, damage and failover, queue behaviour, the connection test, adding), green against Go on Linux first and then against the Zig binary on macOS and Debian trixie.
- [x] Nothing that `par2` or libtorrent does is reimplemented: the only PAR2 knowledge in the Usenet code is reading the block count from recovery volume file names.

#### Z3 findings

**How it was built.** A shared foundation first (NZB, yEnc and Usenet seam types, OpenSSL and libcurl through pkg-config translate-c modules), then seven slices in parallel (NZB parser, yEnc decoder, TLS, NNTP client and fake server, file writer, queue, Go scenarios), the scheduler once yEnc landed, and then the runner, the add service and the server connection test. `services.zig` joins them; the daemon fetches to `fetched` and stops there until Z4.

**Where Zig differs from Go, and what it cost:**
- **XML.** Zig has no XML parser; `nzb_xml.zig` is a small bounded tokenizer for the NZB subset. A 150,000-case differential run against Go's `encoding/xml` found no difference in error kind or parsed fields. DOCTYPE is tolerated as Go does (an entity it defines is refused). Segment numbers are `u32`.
- **TLS.** OpenSSL over memory BIOs, fed through `std.Io` streams, so every blocking operation is a cancellable `Io` operation. Verification cannot be switched off: the options only add trust roots. `std.Io.net.Stream.read` does not compile in 0.17.0 (`readWithControl` is used) and `Io.Threaded` has no connect timeout, so the NNTP dialer races the handshake against a timer and a watchdog task enforces idle timeouts.
- **Cancellation.** A `Future.cancel` right after `io.concurrent` can be lost in 0.17 (about 3% in an experiment). The runner therefore also sets a per-job stop flag that makes the fetchers answer `ServerDisabled`, so the scheduler drains by itself.
- **File names.** `SafeName` needed Go's `unicode.In(C, Zl, Zp)` table, generated from Go 1.27.3 into `fsutil_unicode.zig` (7127 golden cases match). The file writer opens files relative to the Download directory's handle, never following symlinks.
- **Compatibility.** The file writer snapshot and the runner's resume blob are byte-identical with Go's in both directions (golden files and throwaway Go programs).
- **Client disconnect.** The connection test is not cancelled when the client leaves, because `Exchange` only exposes that for streaming responses. The 20 s deadline and shutdown still apply.
- **Torrent adds.** The add service has one seam (`Torrents`) that answers `add_unavailable` until Z7.

**Not in Z3:** PAR2 verify and repair, unpacking and the `missing_articles` failure (Go fails a Download with missing articles in post-processing, Z4), name cleaning (Z4), the torrent branch of adding (Z7). Not yet verified on linux/amd64 (M15-09).

### M15-05: Z4 — Post-processing
**Goal:** The tool runner, par2, 7zz unpack, finalize and tidy.
- [x] A Download reaches Ready with the same output tree as the Go version: 108 `TestPostProc*` black-box scenarios, green against Go on Linux first and then against the Zig binary on macOS and Debian trixie (with the Linux `par2` and `7zz`).
- [x] `par2` and `7zz` do the work: nothing of PAR2 repair, hash matching or archive extraction is reimplemented. The Zig side wraps, guards and parses their output as Go does.

#### Z4 findings

**How it was built.** A shared foundation first (the ports of the pipeline and the `Runner` seam to the tools), then five slices in parallel (tool runner, par2, unpack, finalize with tidy, the orchestrator) next to the Go scenarios, joined in `services.zig`. The daemon locates `7zz` and `par2` at start-up and stops with Go's message when one is missing.

**Where Zig differs from Go, and what it cost:**
- **Subprocesses.** `std.process.spawn` is used with a pipe per stream and its own process group; one poll loop reads both streams, feeds stdin and notices cancellation every 25 ms, because `io.concurrent` tasks can fail with `ConcurrencyUnavailable` and an early cancel can be lost. On Linux, 0.17's spawn leaks three descriptors and a zombie when `exec` fails; the runner checks the program and the directory first, so only a rare `exec` failure (bad ELF, noexec mount) can still leak.
- **Error texts.** A port reports a class and the whole of Go's `err.Error()` in `Diag.message`; the orchestrator only prefixes the stage. Wrapped sentinels therefore read exactly as in Go ("unpacking: unpack: unsafe path in archive: path violation: ...").
- **The PAR2 name guard.** `par2_names.zig` reads the File Description packets (magic, length, MD5, type, file name) of every `.par2` file before `par2` runs and refuses a set with an unsafe name, as Go does; `par2` versions differ in what they neutralise. It is the only place that reads PAR2 data and is isolated so it can be dropped (delete the file and its two uses in `par2.zig`).
- **Unicode tables.** Go's `unicode.ToLower` for the archive name checks is a generated table (`unpack_lower.zig`), like the one for `SafeName` in Z3.
- **Free space and CPU count.** `std` has no cgroup quota, so par2's thread count is the CPU count capped at 16 unless set; a bind mount of the same directory is not seen as the same directory by the finalizer.
- **Linux tools in tests.** The tests that run the real tools use the pinned binaries; in the Debian container the host's `bin/tools` are macOS binaries, so those tests skip there unless the Linux ones are mounted over them (`scripts/get-postproc-tools.sh linux arm64 <dir>`).

**Not in Z4:** the SABnzbd adapter (Z5), the web UI (Z6), torrents and their working directory (Z7; `WorkDirs.torrent` is the seam). Not yet verified on linux/amd64 (M15-09).

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
