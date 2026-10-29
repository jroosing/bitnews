---
status: accepted on the zig branch; supersedes ADR-0001 once the branch is merged
---

# Zig, memory-safe where practical and contained where not

bitnews is rewritten in Zig on the `zig` branch, which lives next to `main` and replaces it only once it reaches parity. Zig was always the preferred language (ADR-0001); Go was chosen because "security without concessions" was a hard requirement. That requirement becomes **memory-safe where practical, contained where not**, and with it Zig is acceptable.

Of ADR-0001's reasons, one no longer applies and one is mitigated:

- **TLS server.** bitnews never serves TLS: it speaks plain HTTP behind a reverse proxy. Outgoing TLS (NNTPS, indexer fetches) uses OpenSSL and libcurl, mature and audited, updated by the distro.
- **Memory safety.** Release builds are `ReleaseSafe` (bounds, overflow and unreachable checks stay on). Use-after-free and double free, which it does not catch, are kept out by allocator discipline: an arena per request, Article and post-processing step, one documented owner for long-lived state, no pointers into growing buffers, hard caps on every hostile input. Tests and fuzzing run on the debug allocator, which reports leaks and use-after-free. Every hostile parser has a fuzz target from its first commit. The container hardening of ADR-0007 contains what slips through.

## Considered options

- **Stay on Go** (ADR-0001). Safer by construction; rejected because Zig is the owner's preference and the remaining risk is judged acceptable with the rules above.
- **libtorrent in a helper process.** In Go that would keep C++ out of a memory-safe process; in Zig the process is not memory-safe either, so libtorrent is linked in-process.

## Consequences

- The torrent engine is libtorrent-rasterbar 2.0.x (the engine of qBittorrent) instead of anacrolix/torrent, behind a small C wrapper of our own. The wrapper is compiled with the system g++ against the distro's libtorrent, because `zig c++` uses its own libc++ and the distro libtorrent is built against libstdc++; Zig only sees C.
- Dependencies come from the OS (Debian trixie: libtorrent-rasterbar, OpenSSL, libcurl); the binary is not static. SQLite is the pinned amalgamation, compiled by Zig, so the database file does not depend on the distro.
- Zig is pinned at 0.17.0 and upgraded deliberately, never to master.
- The rewrite is drop-in: same database, settings, secrets, configuration, API and adapters. An install switches binaries and keeps its data; torrents get their piece completion converted to fast-resume data on the first start.
