# Milestones

Each milestone file is the design doc for that milestone and holds the full issue specs. These files are the source of truth. GitLab milestones, issues and wiki pages mirror them. If GitLab data is ever lost, everything needed to recreate it is here.

Issue IDs (`M1-01`, …) are stable: don't renumber them, and append new IDs instead. When you change a spec here, update the matching GitLab issue too.

## Status

The Status column is maintained by hand when issues close. Run `glab issue list` for live state.

## Index

### [M1 — Foundation](m1-foundation.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/1) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m1-foundation)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M1-01 | [#1](https://gitlab.com/kyura4k/bitnews/-/issues/1) | Repository bootstrap and CI pipeline | area:ci, ready-for-agent | done |
| M1-02 | [#2](https://gitlab.com/kyura4k/bitnews/-/issues/2) | Bootstrap configuration and logging | area:core, ready-for-agent | done |
| M1-03 | [#3](https://gitlab.com/kyura4k/bitnews/-/issues/3) | Privilege drop, umask and root warning | area:deploy, ready-for-agent | done |
| M1-04 | [#4](https://gitlab.com/kyura4k/bitnews/-/issues/4) | Core domain model and lifecycle state machine | area:core, ready-for-agent | done |
| M1-05 | [#5](https://gitlab.com/kyura4k/bitnews/-/issues/5) | In-process event bus | area:core, ready-for-agent | done |
| M1-06 | [#6](https://gitlab.com/kyura4k/bitnews/-/issues/6) | Queue scheduling policy | area:core, ready-for-agent | done |
| M1-07 | [#7](https://gitlab.com/kyura4k/bitnews/-/issues/7) | SQLite store, migrations and file permissions | area:store, ready-for-agent | done |
| M1-08 | [#8](https://gitlab.com/kyura4k/bitnews/-/issues/8) | Repositories for downloads, categories and settings | area:store, ready-for-agent | done |
| M1-09 | [#9](https://gitlab.com/kyura4k/bitnews/-/issues/9) | Secret encryption and key file | area:store, ready-for-agent | done |
| M1-10 | [#10](https://gitlab.com/kyura4k/bitnews/-/issues/10) | Users, password hashing and first-run setup | area:auth, ready-for-agent | done |
| M1-11 | [#11](https://gitlab.com/kyura4k/bitnews/-/issues/11) | Sessions, CSRF, rate limiting and UI auth options | area:auth, ready-for-agent | done |
| M1-12 | [#12](https://gitlab.com/kyura4k/bitnews/-/issues/12) | API keys with scopes | area:auth, ready-for-agent | done |
| M1-13 | [#13](https://gitlab.com/kyura4k/bitnews/-/issues/13) | HTTP server and own API v1 | area:api, ready-for-agent | done |
| M1-14 | [#14](https://gitlab.com/kyura4k/bitnews/-/issues/14) | SSE event stream | area:api, ready-for-agent | done |
| M1-15 | [#15](https://gitlab.com/kyura4k/bitnews/-/issues/15) | Docker image and compose example | area:deploy, ready-for-agent | done (verified with Docker; the CI image job first runs on `main`) |

### [M2 — Usenet fetch](m2-usenet-fetch.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/2) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m2-usenet-fetch)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M2-01 | [#16](https://gitlab.com/kyura4k/bitnews/-/issues/16) | fsutil — safe names, safe join, same-filesystem check, directory move | area:core, ready-for-agent | done |
| M2-02 | [#17](https://gitlab.com/kyura4k/bitnews/-/issues/17) | NZB parser | area:usenet, ready-for-agent | done |
| M2-03 | [#18](https://gitlab.com/kyura4k/bitnews/-/issues/18) | Add Download from NZB (upload and URL) | area:api, area:usenet, ready-for-agent | done |
| M2-04 | [#19](https://gitlab.com/kyura4k/bitnews/-/issues/19) | Usenet Servers — model, encrypted credentials, API and connection test | area:usenet, area:api, ready-for-agent | done |
| M2-05 | [#20](https://gitlab.com/kyura4k/bitnews/-/issues/20) | NNTP client | area:usenet, ready-for-agent | done |
| M2-06 | [#21](https://gitlab.com/kyura4k/bitnews/-/issues/21) | Fake NNTP server test harness | area:usenet, area:ci, ready-for-agent | done |
| M2-07 | [#22](https://gitlab.com/kyura4k/bitnews/-/issues/22) | Per-Server connection pool | area:usenet, ready-for-agent | done |
| M2-08 | [#23](https://gitlab.com/kyura4k/bitnews/-/issues/23) | yEnc decoder | area:usenet, ready-for-agent | done |
| M2-09 | [#24](https://gitlab.com/kyura4k/bitnews/-/issues/24) | Article scheduler with Server Priority fallback | area:usenet, ready-for-agent | done |
| M2-10 | [#25](https://gitlab.com/kyura4k/bitnews/-/issues/25) | Direct file writer | area:usenet, ready-for-agent | done |
| M2-11 | [#26](https://gitlab.com/kyura4k/bitnews/-/issues/26) | Usenet Queue runner | area:usenet, area:core, ready-for-agent | done |
| M2-12 | [#27](https://gitlab.com/kyura4k/bitnews/-/issues/27) | Resume Usenet Downloads after restart | area:usenet, area:store, ready-for-agent | done (SQLite stores and refetch wired in the daemon) |

### [M3 — Post-processing](m3-post-processing.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/3) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m3-post-processing)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M3-01 | [#28](https://gitlab.com/kyura4k/bitnews/-/issues/28) | Post-processing pipeline orchestrator | area:core, area:postproc, ready-for-agent | done |
| M3-02 | [#29](https://gitlab.com/kyura4k/bitnews/-/issues/29) | PAR2 packet parser | area:postproc, ready-for-agent | done |
| M3-03 | [#30](https://gitlab.com/kyura4k/bitnews/-/issues/30) | PAR2 Verify including obfuscated-file renaming | area:postproc, ready-for-agent | done |
| M3-04 | [#31](https://gitlab.com/kyura4k/bitnews/-/issues/31) | GF(2^16) arithmetic and Reed-Solomon solver | area:postproc, ready-for-agent | done |
| M3-05 | [#32](https://gitlab.com/kyura4k/bitnews/-/issues/32) | PAR2 Repair | area:postproc, ready-for-agent | done |
| M3-06 | [#33](https://gitlab.com/kyura4k/bitnews/-/issues/33) | PAR2 golden fixtures and benchmarks | area:postproc, area:ci, ready-for-human | done |
| M3-07 | [#34](https://gitlab.com/kyura4k/bitnews/-/issues/34) | Deferred recovery volumes | area:usenet, area:postproc, ready-for-agent | done |
| M3-08 | [#35](https://gitlab.com/kyura4k/bitnews/-/issues/35) | Archive detection and Unpack (RAR and 7z) | area:postproc, ready-for-agent | done |
| M3-09 | [#36](https://gitlab.com/kyura4k/bitnews/-/issues/36) | Unpack guards — path traversal, entry types and size caps | area:postproc, ready-for-agent | done |
| M3-10 | [#37](https://gitlab.com/kyura4k/bitnews/-/issues/37) | Cleanup and move to complete | area:postproc, ready-for-agent | done |
| M3-11 | [#38](https://gitlab.com/kyura4k/bitnews/-/issues/38) | Compatibility spike — pure-Go unpack against real releases | area:postproc, ready-for-human | open |
| M3-12 | [#66](https://gitlab.com/kyura4k/bitnews/-/issues/66) | 7z header memory guard | area:postproc, ready-for-agent | done |

### [M4 — SABnzbd adapter](m4-sabnzbd-adapter.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/4) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m4-sabnzbd-adapter)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M4-01 | [#39](https://gitlab.com/kyura4k/bitnews/-/issues/39) | Record media manager ↔ SABnzbd traffic fixtures | area:adapter, ready-for-human | dropped (real use reports what to fix) |
| M4-02 | [#40](https://gitlab.com/kyura4k/bitnews/-/issues/40) | Adapter credentials per media manager | area:auth, area:adapter, ready-for-agent | done |
| M4-03 | [#41](https://gitlab.com/kyura4k/bitnews/-/issues/41) | SAB adapter — routing, auth, version and config modes | area:adapter, ready-for-agent | done |
| M4-04 | [#42](https://gitlab.com/kyura4k/bitnews/-/issues/42) | SAB adapter — addfile and addurl | area:adapter, ready-for-agent | done |
| M4-05 | [#43](https://gitlab.com/kyura4k/bitnews/-/issues/43) | SAB adapter — queue, history, delete and the status mapping | area:adapter, ready-for-agent | done |
| M4-06 | [#44](https://gitlab.com/kyura4k/bitnews/-/issues/44) | SAB adapter contract test suite | area:adapter, area:ci, ready-for-agent | done (hand-written fixtures, not recorded; M4-01 dropped) |
| M4-07 | [#45](https://gitlab.com/kyura4k/bitnews/-/issues/45) | End-to-end check with Sonarr and Radarr | area:adapter, area:deploy, ready-for-human | open |

### [M5 — Torrents](m5-torrents.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/5) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m5-torrents)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M5-01 | [#46](https://gitlab.com/kyura4k/bitnews/-/issues/46) | TorrentEngine port and anacrolix adapter skeleton | area:torrent, ready-for-agent | done |
| M5-02 | [#47](https://gitlab.com/kyura4k/bitnews/-/issues/47) | Piece completion in SQLite | area:torrent, area:store, ready-for-agent | done |
| M5-03 | [#48](https://gitlab.com/kyura4k/bitnews/-/issues/48) | Add Download from Torrent (.torrent, magnet, URL) | area:torrent, area:api, ready-for-agent | done |
| M5-04 | [#49](https://gitlab.com/kyura4k/bitnews/-/issues/49) | Torrent Queue runner | area:torrent, area:core, ready-for-agent | done |
| M5-05 | [#50](https://gitlab.com/kyura4k/bitnews/-/issues/50) | Move torrent to complete without rehash | area:torrent, ready-for-agent | done |
| M5-06 | [#51](https://gitlab.com/kyura4k/bitnews/-/issues/51) | Seeding and Share Limits | area:torrent, ready-for-agent | done |
| M5-07 | [#52](https://gitlab.com/kyura4k/bitnews/-/issues/52) | Interface and address binding for both protocols | area:torrent, area:usenet, ready-for-agent | done |
| M5-08 | [#53](https://gitlab.com/kyura4k/bitnews/-/issues/53) | Torrent Unpack into `_unpack/` | area:torrent, area:postproc, ready-for-agent | done |
| M5-09 | [#54](https://gitlab.com/kyura4k/bitnews/-/issues/54) | Record media manager ↔ qBittorrent traffic fixtures | area:adapter, ready-for-human | dropped (real use reports what to fix) |
| M5-10 | [#55](https://gitlab.com/kyura4k/bitnews/-/issues/55) | qBit adapter — auth and app endpoints | area:adapter, ready-for-agent | done |
| M5-11 | [#56](https://gitlab.com/kyura4k/bitnews/-/issues/56) | qBit adapter — torrents endpoints and state mapping | area:adapter, ready-for-agent | done |
| M5-12 | [#57](https://gitlab.com/kyura4k/bitnews/-/issues/57) | qBit contract tests and local swarm end-to-end | area:adapter, area:torrent, area:ci, ready-for-agent | done (hand-written fixtures; the daemon end-to-end test is in; the real Sonarr/Radarr checklist is open) |

### [M6 — Web UI](m6-ui.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/6) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m6-ui)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M6-01 | [#58](https://gitlab.com/kyura4k/bitnews/-/issues/58) | Web foundation — templates, assets, CSP and CSRF wiring | area:ui, ready-for-agent | done |
| M6-02 | [#59](https://gitlab.com/kyura4k/bitnews/-/issues/59) | Login and setup pages | area:ui, area:auth, ready-for-agent | done |
| M6-03 | [#60](https://gitlab.com/kyura4k/bitnews/-/issues/60) | Queue page with live SSE updates and actions | area:ui, ready-for-agent | done |
| M6-04 | [#61](https://gitlab.com/kyura4k/bitnews/-/issues/61) | History page and Download detail | area:ui, ready-for-agent | done |
| M6-05 | [#62](https://gitlab.com/kyura4k/bitnews/-/issues/62) | Settings — paths and Categories | area:ui, ready-for-agent | done |
| M6-06 | [#63](https://gitlab.com/kyura4k/bitnews/-/issues/63) | Settings — Usenet Servers | area:ui, area:usenet, ready-for-agent | done |
| M6-07 | [#64](https://gitlab.com/kyura4k/bitnews/-/issues/64) | Settings — torrent network, speed, queue and Share Limits | area:ui, area:torrent, ready-for-agent | done |
| M6-08 | [#65](https://gitlab.com/kyura4k/bitnews/-/issues/65) | Settings — security, API keys and media-manager credentials | area:ui, area:auth, ready-for-agent | done |

### [M7 — Simplification](m7-simplification.md)

GitLab: [milestone](https://gitlab.com/kyura4k/bitnews/-/milestones/7) · [wiki](https://gitlab.com/kyura4k/bitnews/-/wikis/milestones/m7-simplification)

| ID | Issue | Title | Labels | Status |
|---|---|---|---|---|
| M7-01 | [#67](https://gitlab.com/kyura4k/bitnews/-/issues/67) | Paths are the user's responsibility | area:ui, area:core, ready-for-agent | done |
| M7-02 | [#68](https://gitlab.com/kyura4k/bitnews/-/issues/68) | One free-space helper | area:core, ready-for-agent | done |
| M7-03 | [#69](https://gitlab.com/kyura4k/bitnews/-/issues/69) | Remove the privilege drop | area:deploy, ready-for-agent | done |
| M7-04 | [#70](https://gitlab.com/kyura4k/bitnews/-/issues/70) | A single API key | area:auth, area:adapter, ready-for-agent | done |
| M7-05 | [#71](https://gitlab.com/kyura4k/bitnews/-/issues/71) | Security page key card | area:ui, area:auth, ready-for-agent | done |
