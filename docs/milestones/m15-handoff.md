# M15 Zig rewrite: handoff

Written after Z4. Branch `zig` (next to `main`), last commit `74d4b49`. Spec and findings: [m15-zig.md](m15-zig.md), decision: [ADR-0014](../adr/0014-zig-memory-safe-where-practical.md).

## Where we are

Z0 to Z4 are done and their issues (#9 to #13) are closed. The Zig daemon in `zig/` can:

- start, serve the own API (`/api/v1`): Downloads, Categories, Servers, Settings, the SSE event stream;
- add an NZB (upload or URL), fetch it from NNTP servers over verified TLS, resume after a restart;
- test a server connection;
- post-process: `par2` repair, `7zz` unpack, finalize into the complete tree, ending `ready` or `failed` with the Go daemon's reasons and texts.

Go is the observable contract and the parity harness, not a source to transcribe. Prefer `zig/src` and the 0.17 stdlib; see [m15-zig.md](m15-zig.md) Design. Nothing from `zig` is published; Docker images are built locally only.

## What still needs to be done

| Issue | Milestone | Work |
|---|---|---|
| #14 | Z5 | SABnzbd adapter. Done: `/sabnzbd/api` on the Zig daemon, `TestSABnzbd*` green against that binary. Soak stays in Z8. |
| #15 | Z6 | Web UI (history, queue, settings pages), the admin account, setup token, `BITNEWS_ADMIN_PASSWORD_FILE`. The black-box tests for these are the only ones still skipped (`ZIG_BLACKBOX_SKIP` in the Makefile). |
| #16 | Z7 | Torrents: libtorrent in-process through a C wrapper (only a spike exists), qBittorrent adapter, the torrent runner, seeding, `listen_port_changed` event, torrent port routes. Hooks already left: `add_service.Torrents` (answers `add_unavailable`) and `postprocess_adapters.WorkDirs.torrent`. |
| #17 | Z8 | Merge to `main`: verify linux/amd64 (never run), the parity gate below, two weeks of soak on the real instance. |

Parity gate to merge (from m15-zig.md): the black-box suite green against the Zig binary with nothing skipped, databases readable both ways, Zig tests and fuzz green, soak done.

Smaller open items:

- A longer, one-target-at-a-time `--fuzz` pass over the NZB tokenizer, NNTP and add parsers (the build only runs the first fuzz test of a group).
- The server connection test is not cancelled when the HTTP client disconnects (`Exchange` only exposes that for streaming responses).
- Zig 0.17 std bug: a failed `exec` on Linux can leak three descriptors and a zombie. The tool runner checks first, so only rare cases remain; consider reporting upstream.
- The PAR2 name guard (`zig/src/par2_names.zig`) is the only code reading PAR2 data. It exists because par2 versions differ in which hostile names they neutralise. It can be dropped if you prefer to trust par2 alone.

## How to work on it

- Gate: `make zig-check` (Zig format, tests, sync check, then the Go black-box suite against the Zig binary) and `make check` for the Go side.
- macOS builds need `PKG_CONFIG_PATH=/opt/homebrew/opt/openssl@3/lib/pkgconfig:/opt/homebrew/lib/pkgconfig` (the Makefile sets it).
- Linux check: `zig/Dockerfile.dev` (image `bitnews-zig-dev`) with `zig/dev-run.sh build check`. The host's `bin/tools/7zz` and `par2` are macOS binaries, so mount Linux ones over them (`scripts/get-postproc-tools.sh linux arm64 <dir>`) or the real-tool tests skip. For the black-box suite on Linux, cross-build the Go test binary (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/blackbox/`) and run it in the container with `BITNEWS_E2E_BINARY` pointing at the Zig build.
- Go on macOS does not honour `SSL_CERT_FILE`, so the TLS black-box scenarios skip against the Go daemon there; against the Zig binary they run.
- Commit straight onto `zig` and push it. GitHub does not close issues from a non-default branch: close them with `gh issue close`.
- Method that worked: write a shared foundation first (types and seams), then give each agent its own files, tell them not to edit `build.zig`, `main.zig`, `services.zig`, the Makefile or docs, wire everything in `services.zig` yourself, then run the full gate. Search `zig/src` before Go. Do not clone a Go helper when Zig already has it. Use the `worker` agent (`.cursor/agents/worker.md`) and pass a stronger model only for security-sensitive slices; subagents were 94% of usage before that. Never leave a `--fuzz` run going. Never thinking above high on `architect`. Never fast.

## Things that surprised us (details in m15-zig.md)

- Zig tests must run on the LLVM backend or fuzz seeds never execute (`use_llvm` is set in `build.zig`).
- `Future.cancel` right after `io.concurrent` can be lost in 0.17; code that cancels must also use a stop flag.
- The daemon refuses to start without `7zz` and `par2` on PATH, as the Go one does.
- Go behaviours worth knowing, pinned in the black-box tests: a Download with missing articles and no PAR2 set fails in post-processing (not in the fetch); zip files are not unpacked, only RAR and 7z; deleting a Download that is `fetched` or `post_processing` returns `409 download_busy`.
