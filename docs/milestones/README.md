# Milestones

Each milestone file is the design doc for that milestone and holds the full issue specs. These files are the source of truth. Open work also has a GitHub issue that mirrors its spec; finished work lives only here.

Issue IDs (`M1-01`, …) are stable: don't renumber them, and append new IDs instead. When you change a spec here, update the matching GitHub issue too.

## Status

The Status column is maintained by hand when issues close. Run `gh issue list` for the open ones.

## Index

### [M1 — Foundation](m1-foundation.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M1-01 |  | Repository bootstrap and CI pipeline | area:ci, ready-for-agent | done |
| M1-02 |  | Bootstrap configuration and logging | area:core, ready-for-agent | done |
| M1-03 |  | Privilege drop, umask and root warning | area:deploy, ready-for-agent | done |
| M1-04 |  | Core domain model and lifecycle state machine | area:core, ready-for-agent | done |
| M1-05 |  | In-process event bus | area:core, ready-for-agent | done |
| M1-06 |  | Queue scheduling policy | area:core, ready-for-agent | done |
| M1-07 |  | SQLite store, migrations and file permissions | area:store, ready-for-agent | done |
| M1-08 |  | Repositories for downloads, categories and settings | area:store, ready-for-agent | done |
| M1-09 |  | Secret encryption and key file | area:store, ready-for-agent | done |
| M1-10 |  | Users, password hashing and first-run setup | area:auth, ready-for-agent | done |
| M1-11 |  | Sessions, CSRF, rate limiting and UI auth options | area:auth, ready-for-agent | done |
| M1-12 |  | API keys with scopes | area:auth, ready-for-agent | done |
| M1-13 |  | HTTP server and own API v1 | area:api, ready-for-agent | done |
| M1-14 |  | SSE event stream | area:api, ready-for-agent | done |
| M1-15 |  | Docker image and compose example | area:deploy, ready-for-agent | done (verified with Docker; the CI image job first runs on `main`) |

### [M2 — Usenet fetch](m2-usenet-fetch.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M2-01 |  | fsutil — safe names, safe join, same-filesystem check, directory move | area:core, ready-for-agent | done |
| M2-02 |  | NZB parser | area:usenet, ready-for-agent | done |
| M2-03 |  | Add Download from NZB (upload and URL) | area:api, area:usenet, ready-for-agent | done |
| M2-04 |  | Usenet Servers — model, encrypted credentials, API and connection test | area:usenet, area:api, ready-for-agent | done |
| M2-05 |  | NNTP client | area:usenet, ready-for-agent | done |
| M2-06 |  | Fake NNTP server test harness | area:usenet, area:ci, ready-for-agent | done |
| M2-07 |  | Per-Server connection pool | area:usenet, ready-for-agent | done |
| M2-08 |  | yEnc decoder | area:usenet, ready-for-agent | done |
| M2-09 |  | Article scheduler with Server Priority fallback | area:usenet, ready-for-agent | done |
| M2-10 |  | Direct file writer | area:usenet, ready-for-agent | done |
| M2-11 |  | Usenet Queue runner | area:usenet, area:core, ready-for-agent | done |
| M2-12 |  | Resume Usenet Downloads after restart | area:usenet, area:store, ready-for-agent | done (SQLite stores and refetch wired in the daemon) |

### [M3 — Post-processing](m3-post-processing.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M3-01 |  | Post-processing pipeline orchestrator | area:core, area:postproc, ready-for-agent | done |
| M3-02 |  | PAR2 packet parser | area:postproc, ready-for-agent | done, superseded by ADR-0007 (par2 subprocess) |
| M3-03 |  | PAR2 Verify including obfuscated-file renaming | area:postproc, ready-for-agent | done, superseded by ADR-0007 (par2 subprocess) |
| M3-04 |  | GF(2^16) arithmetic and Reed-Solomon solver | area:postproc, ready-for-agent | done, superseded by ADR-0007 (par2 subprocess) |
| M3-05 |  | PAR2 Repair | area:postproc, ready-for-agent | done, superseded by ADR-0007 (par2 subprocess) |
| M3-06 |  | PAR2 golden fixtures and benchmarks | area:postproc, area:ci, ready-for-human | done, superseded by ADR-0007 (par2 subprocess) |
| M3-07 |  | Deferred recovery volumes | area:usenet, area:postproc, ready-for-agent | done |
| M3-08 |  | Archive detection and Unpack (RAR and 7z) | area:postproc, ready-for-agent | done |
| M3-09 |  | Unpack guards — path traversal, entry types and size caps | area:postproc, ready-for-agent | done |
| M3-10 |  | Cleanup and move to complete | area:postproc, ready-for-agent | done |
| M3-11 |  | Compatibility spike — pure-Go unpack against real releases | area:postproc, ready-for-human | superseded (ADR-0007) |
| M3-12 |  | 7z header memory guard | area:postproc, ready-for-agent | superseded (ADR-0007) |

### [M4 — SABnzbd adapter](m4-sabnzbd-adapter.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M4-01 |  | Record media manager ↔ SABnzbd traffic fixtures | area:adapter, ready-for-human | dropped (real use reports what to fix) |
| M4-02 |  | Adapter credentials per media manager | area:auth, area:adapter, ready-for-agent | done |
| M4-03 |  | SAB adapter — routing, auth, version and config modes | area:adapter, ready-for-agent | done |
| M4-04 |  | SAB adapter — addfile and addurl | area:adapter, ready-for-agent | done |
| M4-05 |  | SAB adapter — queue, history, delete and the status mapping | area:adapter, ready-for-agent | done |
| M4-06 |  | SAB adapter contract test suite | area:adapter, area:ci, ready-for-agent | done (hand-written fixtures, not recorded; M4-01 dropped) |
| M4-07 | [#1](https://github.com/jroosing/bitnews/issues/1) | End-to-end check with Sonarr and Radarr | area:adapter, area:deploy, ready-for-human | open |

### [M5 — Torrents](m5-torrents.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M5-01 |  | TorrentEngine port and anacrolix adapter skeleton | area:torrent, ready-for-agent | done |
| M5-02 |  | Piece completion in SQLite | area:torrent, area:store, ready-for-agent | done |
| M5-03 |  | Add Download from Torrent (.torrent, magnet, URL) | area:torrent, area:api, ready-for-agent | done |
| M5-04 |  | Torrent Queue runner | area:torrent, area:core, ready-for-agent | done |
| M5-05 |  | Move torrent to complete without rehash | area:torrent, ready-for-agent | done |
| M5-06 |  | Seeding and Share Limits | area:torrent, ready-for-agent | done |
| M5-07 |  | Interface and address binding for both protocols | area:torrent, area:usenet, ready-for-agent | done |
| M5-08 |  | Torrent Unpack into `_unpack/` | area:torrent, area:postproc, ready-for-agent | done |
| M5-09 |  | Record media manager ↔ qBittorrent traffic fixtures | area:adapter, ready-for-human | dropped (real use reports what to fix) |
| M5-10 |  | qBit adapter — auth and app endpoints | area:adapter, ready-for-agent | done |
| M5-11 |  | qBit adapter — torrents endpoints and state mapping | area:adapter, ready-for-agent | done |
| M5-12 |  | qBit contract tests and local swarm end-to-end | area:adapter, area:torrent, area:ci, ready-for-agent | done (hand-written fixtures; the daemon end-to-end test is in; the real Sonarr/Radarr checklist is open) |

### [M6 — Web UI](m6-ui.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M6-01 |  | Web foundation — templates, assets, CSP and CSRF wiring | area:ui, ready-for-agent | done |
| M6-02 |  | Login and setup pages | area:ui, area:auth, ready-for-agent | done |
| M6-03 |  | Queue page with live SSE updates and actions | area:ui, ready-for-agent | done |
| M6-04 |  | History page and Download detail | area:ui, ready-for-agent | done |
| M6-05 |  | Settings — paths and Categories | area:ui, ready-for-agent | done |
| M6-06 |  | Settings — Usenet Servers | area:ui, area:usenet, ready-for-agent | done |
| M6-07 |  | Settings — torrent network, speed, queue and Share Limits | area:ui, area:torrent, ready-for-agent | done |
| M6-08 |  | Settings — security, API keys and media-manager credentials | area:ui, area:auth, ready-for-agent | done |

### [M7 — Simplification](m7-simplification.md)


| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M7-01 |  | Paths are the user's responsibility | area:ui, area:core, ready-for-agent | done |
| M7-02 |  | One free-space helper | area:core, ready-for-agent | done |
| M7-03 |  | Remove the privilege drop | area:deploy, ready-for-agent | done |
| M7-04 |  | A single API key | area:auth, area:adapter, ready-for-agent | done |
| M7-05 |  | Security page key card | area:ui, area:auth, ready-for-agent | done |
| M7-06 |  | Post-processing with 7-Zip and par2cmdline-turbo | area:postproc, area:deploy, ready-for-agent | done |

### [M8 — Deep modules](m8-deep-modules.md)


| ID | Issue | Title | Status |
|---|---|---|---|
| M8-01 |  | The Queue module | done |
| M8-02 |  | The Usenet runner uses the Queue | done |
| M8-03 |  | The torrent runner uses the Queue | done |
| M8-04 |  | Callers use one Queue vocabulary | done |
| M8-05 |  | Download view module (planned) | planned |
| M8-06 | [#2](https://github.com/jroosing/bitnews/issues/2) | One Add call (planned) | planned |
| M8-07 | [#3](https://github.com/jroosing/bitnews/issues/3) | Settings change under lock and one card engine | done |
| M8-08 | [#4](https://github.com/jroosing/bitnews/issues/4) | Post-processing steps implement the core ports (planned) | planned |
| M8-09 |  | Torrent facts from the torrent runner (planned) | planned |
| M8-10 | | Download removal | done |

### [M9 — Post-processing options](m9-postprocess-options.md)


| ID | Issue | Title | Status |
|---|---|---|---|
| M9-01 |  | The rename module | done |
| M9-02 |  | Settings and hooks | done |
| M9-03 |  | The Settings page | done |

### [M10 — Port forwarding](m10-port-forwarding.md)

| ID | Issue | Title | Status |
|---|---|---|---|
| M10-01 | | Torrent listen port from a VPN hook | done |
