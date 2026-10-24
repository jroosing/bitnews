# M5 — Torrents

## Goal

By the end of M5, bitnews downloads and seeds torrents (from .torrent files and magnet links) through `anacrolix/torrent` behind the `TorrentEngine` port. It honours Share Limits, and it Unpacks archived releases into `_unpack/` without touching the originals. Sonarr and Radarr use it as a **qBittorrent** client with no changes. Peer traffic can optionally be bound to one network interface, which acts as a kill switch.

## Scope

**In:**
- the `TorrentEngine` port and the anacrolix adapter
- piece completion in our SQLite
- adding torrent Downloads
- the torrent Queue runner
- moving to complete without a rehash
- Seeding and Share Limits
- interface/address binding for both protocols
- torrent Unpack
- qBittorrent fixtures, adapter and contract tests
- a local swarm e2e test

**Out:**
- UPnP/NAT-PMP (permanently out, per the architecture)
- RSS, search, torrent creation
- web seeds configuration beyond the anacrolix defaults
- per-torrent file selection, which is a later candidate

## Design

### `TorrentEngine` port (`internal/core`)

```go
type TorrentEngine interface {
    Add(ctx, spec TorrentSpec) (InfoHash, error) // spec: metainfo bytes | magnet URI, storage dir, paused
    Remove(ctx, h InfoHash, deleteData bool) error
    Pause(h InfoHash) error
    Resume(h InfoHash) error
    Relocate(ctx, h InfoHash, newDir string) error // see "Move to complete"
    SetShareLimits(h InfoHash, l ShareLimit) error
    SetRateLimits(down, up Rate) error
    Stats(h InfoHash) (TorrentStats, error)        // bytes, rates, peers, ratio, seeding time
    Events() <-chan TorrentEvent                    // MetadataReceived, Fetched, Error
}
```

- `internal/torrent` is the only package that imports anacrolix.
- Domain types never contain anacrolix types.
- A `fake` engine for tests lives in `internal/core/coretest`.

### anacrolix configuration

- **Listen:** a configurable TCP+uTP port (default 51413). With binding active it listens only on the bound address.
- **Disabled:** UPnP / port mapping (`NoDefaultPortForwarding = true`, or the equivalent in the pinned version).
- **DHT, PEX, LSD:**
  - On globally for public torrents. Each has a setting.
  - Torrents flagged private (BEP 27) never use them. anacrolix v1.61.0 does **not** enforce this, so the engine does (see the implementation notes).
  - A test must assert it.
- **MSE:** the `prefer` / `require` setting maps onto anacrolix's header obfuscation policy (`Preferred`/`RequirePreferred`). The default is `prefer`.
- **Rate limits:** `rate.Limiter`s for download and upload, shared with the global limits from M2. They can be changed at runtime.
- **Storage:** file storage with a per-Download dir, and our own `PieceCompletion` implementation (see below).
- **Versions:** the anacrolix version is pinned. Its CVE and advisory status is checked with `govulncheck` in CI.

### Piece completion

- Implement the anacrolix `storage.PieceCompletion` interface (`Get`/`Set`/`Close`, keyed by infohash and piece index) over our SQLite. Use a new table `torrent_pieces(info_hash, piece, complete)`, stored as a bitmap per torrent in one row and updated in batches.
- This avoids a second SQLite driver or bolt file and keeps the single-file backup property.

### Adding torrent Downloads

- `POST /api/v1/downloads` also accepts:
  - multipart `torrent` (.torrent, ≤ 10 MiB)
  - JSON `{"magnet": "magnet:?..."}`
  - JSON `{"url": ...}` that points to a .torrent. That fetch uses the same limits and redaction as M2-03; a redirect to `magnet:` is accepted.
- Parsing:
  - Metainfo is parsed by anacrolix, wrapped in our size limits.
  - Torrent file names pass through `fsutil.SafeName`. Anacrolix storage paths are built only through our path maker using `SafeJoin`.
- **Magnet links:** start in `FetchingMetadata`. On `MetadataReceived` they move to `Queued`. A metadata timeout setting (default none) can fail them with `source_invalid`.
- **Duplicates:** an infohash that already exists returns `409` with the existing Download ID. Merging the new request's trackers into the existing torrent is not done (M5-03 decided to leave it out; see the notes below).

### Torrent Queue runner

- Uses the same scheduling policy as Usenet, with a default limit of 5 active Downloads (fetching). Seeding torrents don't count toward the limit.
- Pause and resume, per Download and per Queue.
- Progress events: bytes, rates, peers.
- The disk guard applies as in M2.
- Once all pieces are verified, the Download is **Fetched**. Seeding starts immediately, then "Move to complete" runs, and then torrent Post-processing.

### Move to complete

- Torrents fetch into `<torrent incomplete root>/<download>/`, and at Fetched they move into `complete/<category>/<download>/`.
- With the TRaSH defaults both roots can be the same path (`/data/torrents`). The settings allow that, and then the move is a no-op.
- `Relocate`:
  1. Pause the torrent and close its storage.
  2. `fsutil.MoveDir`.
  3. Re-add it with storage at the new dir. Piece completion carries over because it is keyed by infohash, so there is no rehash.
  4. Resume.
- If anything fails partway, it rolls back to the old location.
- This is a known risk point, which is why the issue requires a stress test.

### Seeding and Share Limits

- **Share Limit** is a ratio and/or a seeding time. It is set per Download (it can come through the qBit adapter), with global defaults in settings. `-1` means unlimited.
- When the limit is reached, the action is to `pause` (the default, so media managers then remove the torrent) or `remove` (with or without data, per setting).
- The ratio is uploaded bytes divided by the torrent size. Seeding time accrues only while seeding is active. Both values are persisted so they survive restarts.

### Interface and address binding (both protocols)

- Settings: `bind_interface` and/or `bind_address`, set **per protocol** (Usenet, torrent). Both are empty by default.
- On Linux with an interface name:
  - `SO_BINDTODEVICE` is set via a `net.Dialer.Control` / `ListenConfig.Control` hook on every socket for that protocol (NNTP dialer, anacrolix dialers and listeners).
  - If it is not permitted, startup fails with a clear error. It never falls back silently.
- With an address only: sockets bind to that local address.
- **Monitor:** every 5 s, check that the interface exists and has the bound address. If it doesn't, pause that protocol's Queue with reason `bind_unavailable` and close its sockets. It resumes when the interface comes back.
- Traffic never leaves through another route. The API/UI listener is never bound to the VPN interface.

### Torrent Post-processing

- At Fetched (after the move), detect archive sets with the M3 `Unpacker.Detect`.
- If none are found, the Download is Ready immediately.
- If archives are found:
  - Unpack into `<download>/_unpack/`, with the M3 guards and staging under `<download>/_unpack.tmp/`.
  - Original files are never modified or deleted, so seeding continues throughout.
  - Then the Download is Ready.
- Unpack failures fail the Download (`archive_*`), but seeding continues until the media manager removes it.
- Removal deletes both the originals and `_unpack/` when data deletion is requested.

### qBittorrent adapter

The fixtures are the spec, as in M4. M5-09 (dropped) was to record real Sonarr/Radarr ↔ qBittorrent traffic. The expected subset of the WebUI API v2, under `/api/v2/`:

| Endpoint | Notes |
|---|---|
| `POST auth/login` | Form `username`, `password` checked against the `adapter_qbit` credential (M4-02). On success, set a `SID` cookie (an adapter session, 1 h idle) and return `Ok.`. On failure return `Fails.`, rate-limited like UI login. |
| `POST auth/logout` | |
| `GET app/version`, `app/webapiVersion` | Report the versions current media managers accept (from the fixtures). |
| `GET app/preferences` | `save_path`, `queueing_enabled`, `max_active_downloads`, `max_ratio_enabled`, `max_ratio`, `max_seeding_time_enabled`, `max_seeding_time`, `max_ratio_act`, `dht`… This is a read-only view of bitnews settings. `setPreferences` is rejected or ignored according to the fixtures. |
| `GET torrents/info` | Filters `category`, `hashes`, `filter`. Fields include `hash`, `name`, `size`, `progress`, `dlspeed`, `upspeed`, `eta`, `state`, `category`, `save_path`, `content_path`, `ratio`, `seeding_time`, `ratio_limit`, `seeding_time_limit`, `added_on`, `completion_on`, `amount_left`. |
| `GET torrents/properties`, `torrents/files` | As the fixtures show. |
| `POST torrents/add` | `urls` (magnets or URLs, newline-separated) or multipart `torrents`. Options: `savepath` (accepted only if it is under the torrent roots, otherwise rejected), `category`, `paused`/`stopped`, `tags` (ignored in v1), `ratioLimit`, `seedingTimeLimit`, `sequentialDownload`/`firstLastPiecePrio` (ignored). |
| `POST torrents/delete` | `hashes`, `deleteFiles`. |
| `POST torrents/pause` / `resume` (or `stop`/`start` in API v2.11+) | Accept both names. |
| `POST torrents/setCategory`, `torrents/createCategory`, `GET torrents/categories` | Categories are shared with Usenet. |
| `POST torrents/setShareLimits` | Maps to Share Limit. |
| `POST torrents/topPrio`, `bottomPrio`, `setForceStart` | Map to queue position and Download Priority `force`. |

- **Hash:** the hex v1 infohash. For v2-only torrents, the truncated v2 hash, matching qBittorrent.
- **`content_path`:** the Download dir (after the move). Media managers scan it recursively, so they find the files under `_unpack/` and ignore the `.rar` files.
- **State mapping** (expected; confirm with the fixtures that media managers do *not* import in the Fetched → Ready window):

  | bitnews | qBit `state` | `progress` |
  |---|---|---|
  | FetchingMetadata | `metaDL` | 0 |
  | Queued | `queuedDL` | real |
  | paused | `pausedDL` / `stoppedDL` (by API version) | real |
  | Fetching | `downloading`, or `stalledDL` with no peers | real |
  | Fetched → Post-processing (move or Unpack) | `checkingUP` or a similar non-complete state | `0.9999` |
  | Ready + Seeding | `uploading`, or `stalledUP` when idle | 1 |
  | Ready, Share Limit reached (paused) | `pausedUP` / `stoppedUP` | 1 |
  | Failed | `error` | real |

## Exit criteria

- A local swarm e2e test (an anacrolix seeder plus bitnews, with no internet) passes for:
  - a .torrent and a magnet link
  - a private torrent with no DHT traffic
  - a move to complete without a rehash
  - archive release → `_unpack/` → Ready while seeding continues
  - Share Limit → pause
- When the bound interface is removed in a Linux network namespace, traffic stops within 5 s and never shows up on the other interface (checked by packet capture).
- The qBit contract tests pass, and a manual Sonarr/Radarr check is recorded in `docs/compat/media-managers.md`.

## Issues

### M5-01: TorrentEngine port and anacrolix adapter skeleton
- **Labels:** area:torrent, ready-for-agent
- **Depends on:** M1-04, M1-05

**Goal:** Define the `TorrentEngine` port in core and implement it over a pinned `anacrolix/torrent` with the security-relevant configuration.

**Work**
- Add the port interface, the domain types (`InfoHash`, `TorrentSpec`, `ShareLimit`, `TorrentStats`, `TorrentEvent`) and a fake engine in `coretest`.
- Write the anacrolix client config: listen port, no port forwarding, DHT/PEX/LSD settings, the MSE policy and rate limiters.
- Add a storage path maker that uses `fsutil.SafeName` and `SafeJoin` for every torrent file path.
- Make sure `internal/torrent` is the only package that imports anacrolix. Add an `importas`/depguard-style test.

**Acceptance criteria**
- [ ] A test confirms a torrent with `../` file names cannot write outside its Download dir.
- [ ] A test confirms private torrents never contact DHT or PEX (check with a local DHT node or the anacrolix hooks).
- [ ] UPnP is off, with a test asserting the config.

### M5-02: Piece completion in SQLite
- **Labels:** area:torrent, area:store, ready-for-agent
- **Depends on:** M5-01, M1-07

**Goal:** Implement anacrolix `PieceCompletion` over our DB.

**Work**
- Migration for a `torrent_pieces` table (a bitmap per infohash).
- `Get`/`Set` with batched writes (flush every 2 s and on close), safe for concurrent use.
- Delete the rows when a Download is removed.

**Acceptance criteria**
- [ ] After a restart, a fully downloaded torrent is not rehashed (assert no re-verify via anacrolix stats or the hooks).
- [ ] Tests pass under `-race`.

### M5-03: Add Download from Torrent (.torrent, magnet, URL)
- **Labels:** area:torrent, area:api, ready-for-agent
- **Depends on:** M5-01, M2-03

**Goal:** Extend `POST /api/v1/downloads` to torrent Sources, following the "Adding torrent Downloads" design.

**Work**
- Accept a multipart `torrent` (≤ 10 MiB), JSON `magnet`, or JSON `url` (which may redirect to a magnet), with the same fetch limits and redaction as M2-03.
- Magnet links go through `FetchingMetadata` → `Queued` on `MetadataReceived`, with an optional metadata timeout.
- A duplicate infohash returns `409` with the existing ID (tracker merging was left out, see the implementation notes).
- Category and priority are handled as for NZBs.

**Acceptance criteria**
- [ ] API tests for each input form, an oversized file, an invalid metainfo, a duplicate, and the magnet metadata flow (local swarm).

### M5-04: Torrent Queue runner
- **Labels:** area:torrent, area:core, ready-for-agent
- **Depends on:** M5-02, M5-03, M1-06

**Goal:** Run the torrent Queue: activation, pause/resume, progress, speed limits, the disk guard, and Fetched.

**Work**
- Apply the scheduling policy with the torrent limit, so Seeding doesn't count.
- Pause and resume, per Download and per Queue.
- Throttled progress events (bytes, rates, peers).
- Shared global limits plus torrent-specific limits, changeable at runtime.
- The disk guard as in M2.
- Emit the Fetched transition.

**Acceptance criteria**
- [ ] A local swarm test reaches Fetched with the expected events.
- [ ] The limit of 5 is respected while extra Downloads are Queued.
- [ ] A rate limit test passes within ±15%.

### M5-05: Move torrent to complete without rehash
- **Labels:** area:torrent, ready-for-agent
- **Depends on:** M5-04, M2-01

**Goal:** Implement `Relocate` and the move at Fetched, per the "Move to complete" design.

**Work**
- Pause, close storage, run `MoveDir`, re-add at the new dir, then resume. Roll back on failure.
- The move is a no-op when the incomplete and complete roots resolve to the same dir.
- A cross-device move emits the warning event from M3-10.

**Acceptance criteria**
- [ ] A stress test moves 50 seeding torrents with a peer connected, with no rehash, no data loss and seeding resumed.
- [ ] Injected failures at each step leave the torrent seeding at its old location.

### M5-06: Seeding and Share Limits
- **Labels:** area:torrent, ready-for-agent
- **Depends on:** M5-04

**Goal:** Enforce Share Limits per Download, with global defaults and the configured action.

**Work**
- Persist the ratio and seeding time, so they survive restarts.
- Evaluate limits every 30 s. The action is `pause` (the default) or `remove` (with or without data).
- Add settings for the global default ratio, time and action.
- Expose the limits on the own API (`PATCH /downloads/{id}` `share_limit`).

**Acceptance criteria**
- [ ] Tests cover ratio reached, time reached, unlimited (`-1`), a per-Download override, and a restart keeping the counters.

### M5-07: Interface and address binding for both protocols
- **Labels:** area:torrent, area:usenet, ready-for-agent
- **Depends on:** M5-01, M2-07

**Goal:** Optional per-protocol network binding as a kill switch, per the "Interface and address binding" design.

**Work**
- Add the per-protocol settings `bind_interface` and `bind_address`.
- On Linux, add a `Control` hook that sets `SO_BINDTODEVICE` on the NNTP dialer and on the anacrolix dialers and listeners. If that is not permitted, fail hard, with no silent fallback.
- With an address only, bind sockets to that local address.
- Add a 5 s monitor that pauses the affected Queue with reason `bind_unavailable`, closes its sockets, and resumes automatically.
- Never apply the binding to the API listener.

**Acceptance criteria**
- [ ] A Linux CI test in a network namespace: deleting the bound interface stops traffic within 5 s, a packet capture shows no packets on the other interface, and restoring it resumes the Queue.

### M5-08: Torrent Unpack into `_unpack/`
- **Labels:** area:torrent, area:postproc, ready-for-agent
- **Depends on:** M5-05, M3-08, M3-09

**Goal:** Torrent Post-processing: Unpack archived releases without touching the originals.

**Work**
- After the move, run `Unpacker.Detect`. With no archives the Download is Ready right away.
- With archives, Unpack into `<download>/_unpack/` with the M3 guards and staging, then the Download is Ready.
- Never modify or delete the originals, so seeding continues.
- A failure gives `archive_*`, with seeding left running.
- Removal deletes both the originals and `_unpack/` when data deletion is requested.

**Acceptance criteria**
- [ ] A local swarm test with a RAR release: `_unpack/` holds the payload, the originals' hashes are unchanged, the Download is Ready, and seeding stays active throughout.

### M5-09: Record media manager ↔ qBittorrent traffic fixtures
- **Status: dropped.** The owner tests against real Sonarr/Radarr and reports what to fix; no recording is planned.
- **Labels:** area:adapter, ready-for-human
- **Depends on:** —

**Goal:** Build the authoritative contract for the qBit adapter, the same way as M4-01.

**Work**
- Set up docker compose with real qBittorrent (current stable, WebUI API v2), Sonarr, Radarr and Prowlarr, behind a recording proxy.
- Exercise these flows:
  - client test
  - grab (magnet and .torrent)
  - queue polling: metaDL, downloading, stalled and paused
  - completion and import
  - setShareLimits and seeding until the limit is reached
  - removal with and without files
  - categories
  - priority (topPrio / forceStart)
- **Key question:** which `state`/`progress` combinations make media managers hold off on importing (the Fetched → Ready window)? Record a qBittorrent torrent while it is `checkingUP` or in a similar state.
- Scrub the fixtures into `internal/adapter/qbit/testdata/fixtures/`.

**Acceptance criteria**
- [ ] The fixtures cover every endpoint media managers call, plus the import-hold-off answer documented in `testdata/README.md` with the versions tested.

### M5-10: qBit adapter — auth and app endpoints
- **Labels:** area:adapter, ready-for-agent
- **Depends on:** M5-09, M4-02

**Goal:** `auth/login` and `logout` with a `SID` cookie, plus `app/version`, `app/webapiVersion` and `app/preferences`.

**Work**
- Check login against the `adapter_qbit` credential, rate-limited like UI login. The `SID` is an adapter session (1 h idle) that is valid only on `/api/v2/*`.
- Return the version strings from the fixtures.
- `preferences` is a read-only view of the bitnews settings. `setPreferences` behaves as the fixtures show.

**Acceptance criteria**
- [ ] Contract tests pass.
- [ ] A `SID` is rejected on `/api/v1/*` and `/sabnzbd/api`.
- [ ] Wrong passwords return `Fails.` and are rate-limited.

### M5-11: qBit adapter — torrents endpoints and state mapping
- **Labels:** area:adapter, ready-for-agent
- **Depends on:** M5-10, M5-06, M5-08

**Goal:** The `torrents/*` subset from the design, mapped onto the core services.

**Work**
- `info` with filters and every field the fixtures require, including `content_path`. Plus `properties` and `files`.
- `add` (urls or multipart; `savepath` validated against the torrent roots; category, paused or stopped, share limits).
- `delete`, `pause`/`resume` and `stop`/`start`, `setCategory`, `createCategory`, `categories`, `setShareLimits`, `topPrio`, `bottomPrio` and `setForceStart`.
- The state mapping table, adjusted to what real use shows about the import hold-off (M5-09 was dropped).

**Acceptance criteria**
- [ ] Contract tests pass.
- [ ] A table test over every state and stage confirms media managers can't see a "complete" state before Ready.

### M5-12: qBit contract tests and local swarm end-to-end
- **Labels:** area:adapter, area:torrent, area:ci, ready-for-agent
- **Depends on:** M5-11, M4-06

**Goal:** Run the qBit fixtures in CI through the M4-06 harness, and add an e2e test without internet.

**Work**
- Hook the qBit fixtures into the shared replay harness.
- Write the e2e test (build tag `e2e`): a local anacrolix seeder, bitnews, and the scenarios from "Exit criteria".
- Add a manual Sonarr/Radarr checklist to `docs/compat/media-managers.md` (hardlink import, Share Limit → removal).

**Acceptance criteria**
- [ ] CI runs the contract tests on every pull request and the e2e job on `main`, and both pass.
- [ ] The manual checklist is recorded with versions.

## Implementation notes (M5-01, M5-02)

Packages: the port and domain types are in `internal/core/torrent_engine.go`, the fake in `internal/core/coretest`, the adapter in `internal/torrent`, and the row-level store in `internal/store/repo_torrent_pieces.go` (migration `0005_torrent_pieces.sql`). `internal/torrent` is the only package that imports anacrolix; `TestOnlyInternalTorrentImportsAnacrolix` walks the module and fails otherwise.

**Port**
- `InfoHash` is 20 bytes. A v2-only torrent is not supported: `Add` rejects a torrent or magnet without a v1 hash, because anacrolix panics on a zero v1 hash. `ParseInfoHash` accepts 40 hex characters, and 64 (a v2 hash) truncated to 20.
- `SetRateLimits(down, up int64)` is bytes per second; zero or less is unlimited. `ShareLimit` uses `-1` for unlimited, so the zero value means "stop at once"; use `core.UnlimitedShareLimit()`.
- `Add` of a known hash returns an error wrapping `core.ErrConflict` (tracker merging is M5-03's job). Unknown hashes wrap `core.ErrNotFound`.
- The engine only reports `TorrentShareLimitReached` (once per torrent and limit setting). The caller decides pause or remove. Ratio and seeding time live in memory; M5-06 must persist them (there is no way to seed the counters yet).
- `Events()` is buffered (default 256). When it is full the engine drops the event and logs it. `Close` closes the channel.

**anacrolix v1.61.0: decisions and surprises**
- Pinned in `go.mod`; `govulncheck` runs in CI.
- **Port mapping:** `NoDefaultPortForwarding = true` always; there is no setting. A test asserts it.
- **Private torrents (BEP 27) are not handled by anacrolix at all.** The engine enforces them: (1) it starts the DHT announces itself (`PeriodicallyAnnounceTorrentsToDht = false`) and skips private torrents; (2) the DHT server ignores `announce_peer` for private info hashes; (3) a private torrent's connections offer only `ut_metadata` and the peer's `ut_pex` and `ut_holepunch` are removed from its extended handshake, so PEX is never initialised. A magnet starts as public, because its flag is unknown until the metadata arrives; at that point DHT announcing stops and the connections are cycled. Tests: a loopback DHT node observes `get_peers` for a public torrent (the control) and nothing for a private one; two engines negotiate `ut_pex` for a public torrent and not for a private one.
- **LSD:** v1.61.0 has none. `Config.LSD` is accepted and logs a warning.
- **MSE:** `prefer` is `{Preferred}`, `require` is `{Preferred, RequirePreferred}`, `disable` is the zero policy.
- **Web seeds, WebTorrent, magnet `xs`/`as` sources and DHT nodes from metainfo are off or dropped.** A hostile torrent must not make the daemon fetch arbitrary URLs.
- **Storage:** anacrolix's file storage is not used. Its default I/O in v1.61.0 is mmap, which (a) fails with `invalid argument` for any piece spanning a zero-length file, so such torrents never download (reproduced with plain anacrolix storage), (b) turns a full disk or a flaky share into SIGBUS, and (c) can only be changed through an environment variable read at init. `internal/torrent/filestorage.go` is a small positional-I/O storage (open, read or write, close per operation; no part files; empty files are created when the torrent opens; sparse files, no preallocation). Pieces are fsynced before they are marked complete.
- **Paths:** every file path is `fsutil.SafeName` per component and `fsutil.SafeJoin` into the storage dir (`torrentPaths`). Torrents are refused (`ErrInvalidMetainfo`) if a component cannot be made safe (`..`, empty), if two files map to the same path (also case-insensitively) or if a file would also be a directory, or if they exceed 100,000 files, 4 Mi pieces or a 256 MiB piece length. This runs before anything is created on disk. For a magnet the same check runs when the metadata arrives, and failure gives a `TorrentErrored` event. Layout: `<StorageDir>/<torrent name>/<file path>`.
- **Piece completion:** anacrolix treats a piece that storage reports as "unknown" as not requestable, so `Set(false)` must read back as known-incomplete. That state is memory only; the persisted bitmap holds verified pieces. After a restart a piece without a persisted mark is unknown and is hashed, a lost write-behind mark costs a hash, never correctness. Torrent_pieces stores the exact piece count (`Register` is called when the storage opens).
- **Downloads start only when pieces are wanted:** the engine calls `DownloadAll()` once the metadata is known.
- **Pause:** disallow download and upload, and drop to zero connections. Trackers are still announced to.
- **Relocate:** stops DHT, drops the anacrolix torrent (closing its files), `fsutil.MoveDir`, re-adds the same metainfo at the new dir with piece completion intact, restores the peers that had a dialable address, and restores the paused or running state. On any failure it moves the directory back and re-adds at the old place. `Uploaded` carries over. The destination must not exist. The `Fetched` event is not repeated.
- **Bind address:** `Config.BindAddr` makes listeners, tracker HTTP dials and UDP tracker sockets use that address and disables the other IP family. The interface binding and monitor are M5-07.
- **Rate limiters:** one `rate.Limiter` each for download and upload, adjustable at runtime. The burst is the limit (at least 64 KiB, at most 1 MiB).
- **Stall watchdog:** under heavy load a local-swarm transfer sometimes stopped for good: a connection kept an "update requests" flag set (or an unanswered request) and never asked for data again. Once a second the engine checks every downloading torrent that has peers; after 4 s without a received byte it cancels and re-allows the download, and after 10 s it drops the connections and offers the peers again. A torrent choked by every peer is therefore cycled at most every 10 s. There is no direct test for the watchdog; it was introduced because the suite failed about 1 in 10 runs under 5x load before it and passed 66 of 66 runs after.
- **uTP:** `Config.DisableUTP` exists because in v1.61.0 peers dial TCP and uTP at once and the race occasionally stalls a transfer. The local-swarm tests use TCP only. With `CGO_ENABLED=1` anacrolix uses its C++ libutp (and `go-llsqlite`), so the project builds and tests with `CGO_ENABLED=0` to stay pure Go.

**Tests (local swarm, no network, `-race`, about 10 s in total)**
Seeder and leecher engines on 127.0.0.1 with torrents built by anacrolix's metainfo builder (16 KiB pieces): multi-file (with an empty file) and single-file download byte-exact; magnet metadata fetch (BEP 9) with events in order, with and without a peer hint; private torrent downloads and is flagged; DHT silence and PEX absence for private torrents; hostile `.torrent` names (contained or refused, nothing written outside the storage dir, including real downloads); hostile magnet metadata (error event, nothing written); relocate while a leecher is connected (no rehash, seeding resumes, no second `Fetched`); relocate failure leaves the torrent seeding where it was; pause and resume for leecher and seeder; remove with and without data (foreign files survive, completion rows deleted); restart without rehash against a real SQLite DB, with a control that rehashes when completion is lost; download and upload rate limits within 25 percent; share limit by ratio and by seeding time with a fake clock; events never block the engine; fuzz targets for path validation and spec parsing.

**Dependency tree and vulnerabilities**
- `go list -deps ./internal/torrent | wc -l`: 507 packages with cgo and 500 with `CGO_ENABLED=0`, from 78 modules (`go list -deps ./internal/store` is 182 packages from 12). About 70 of the packages are `pion/*` (WebRTC) and 15 are OpenTelemetry. They are linked because anacrolix imports them unconditionally, although the engine turns WebTorrent off. The tree also holds a second SQLite (`go-llsqlite`, cgo builds only). This is the largest supply-chain cost in the project and is accepted for the pure-Go BitTorrent client; the alternative is writing one.
- `go tool govulncheck ./...` (run with the anacrolix tree in `go.mod`) reports three vulnerabilities that static analysis considers reachable, all in transitive modules that only the WebRTC/WebTorrent code uses (the engine disables it, but it stays linked): GO-2026-6278 `gorilla/websocket` v1.5.0 (fixed in v1.5.3), GO-2026-6165 `pion/dtls/v3` v3.0.3 (fixed in v3.1.4) and GO-2026-6163 `pion/stun/v3` v3.0.0 (fixed in v3.1.5). It also lists GO-2026-5506 `go.opentelemetry.io/otel` v1.38.0 (fixed in v1.41.0, not called) and GO-2026-5932 `golang.org/x/crypto` v0.58.0 (no fix yet, not called). **Follow-up:** bump the three reachable modules (and otel) in `go.mod` with `go get`, which needs network access; `make vuln` fails until then.

## Implementation notes (M5-03)

**Shape.** `AddService` gained a torrent side next to the NZB one (`internal/app/add_torrent.go`; the NZB API is untouched). It is enabled by `app.WithTorrentSources(core.TorrentSourceRepo)`; without it every torrent call fails with an `ErrUnavailable` error (`503 torrents_unavailable`).
- `StageTorrent(ctx, io.Reader)` reads at most `core.MaxMetainfoSize` (else `torrent_too_large`), parses with `torrent.ParseMetainfo` and only then creates `<torrent incomplete>/<download id>/` and writes the raw bytes as `.incoming.torrent` (mode 0600). A hostile or broken file (`invalid_torrent`) therefore never touches the disk.
- `StageMagnet(ctx, uri)` checks the link with `torrent.ParseMagnet` (`invalid_magnet`) and writes nothing.
- `Fetch(ctx, url)` is the URL entry for the API: it uses the NZB fetch rules (http/https only, 5 redirects, 30 s, no cookies/proxy, redacted URL) and decides from the first byte: `d` means a torrent (10 MiB cap), anything else is handed to the NZB parser (so garbage is still `invalid_nzb`). It has its own client whose redirect policy returns the redirect response itself when the target is a `magnet:` URI; the `Location` is then staged as a magnet. The link is never requested, so it is "followed once" by construction. `FetchNZB` stays for NZB-only callers. The success log lines are `nzb fetched` and `torrent fetched`; the failure line is `nzb or torrent fetch failed` (the old `nzb fetch failed` test assertion became `fetch failed`).
- `AddTorrent(ctx, staged, AddRequest)` has the NZB ordering: Download in `Added` (position at the end of the **torrent** queue, `queueOf(SourceTorrent)`), source row, then `UpdateState` to `Queued` (file) or `FetchingMetadata` (magnet); `DownloadAdded` last; on any failure the Download is deleted (the source cascades) and the staged directory removed. `core.Transition(Added, to)` is checked first. A magnet starts with `TotalBytes = 0`; the runner sets name and size when the metadata arrives (`DownloadRepo.UpdateDisplay`).
- Duplicates are found by `TorrentSourceRepo.GetByInfoHash` under the add lock, and again when `Create` returns `ErrConflict` (another process). The error is `app.ErrDuplicate{ExistingID, Name}` (it matches `core.ErrConflict`); the API answers `409 duplicate_torrent` with `existing_id` and `existing_name` in the error object. **Tracker merging was left out:** the staged torrent is discarded, and merging needs an engine call (`AddTrackers`) and a decision about private torrents, so it is a follow-up.

**Store.** Migration `0006_torrent_sources.sql`: `torrent_sources(download_id PK -> downloads ON DELETE CASCADE, info_hash UNIQUE with length 40, torrent_path NULL, magnet NULL, created_at)`, `CHECK (torrent_path IS NOT NULL OR magnet IS NOT NULL)`, `STRICT`. The magnet is sealed with AAD `torrent_sources/magnet/<download id>`. A wrong key gives `core.ErrSecretUnavailable` with the rest of the record. `SetTorrentPath` is for the runner after a magnet's metadata is saved.

**Parsing (`internal/torrent/parse.go`).** `ParseMetainfo` reuses `parseMetainfo` and `torrentPaths`, so it refuses exactly what `Engine.Add` refuses, plus one more check (the number of pieces must match the total size; the engine would fail such a torrent later in anacrolix). `Info.Files` carries the same safe paths `Engine.Stats` reports. `ParseMagnet` uses anacrolix's own parser for the final result (so the accepted set cannot be wider than the engine's), but checks the hash shape first: `urn:btih:` needs 40 hex or 32 upper-case base32 characters (lower-case base32 and `URN:BTIH:` are refused, as the engine refuses them), `btmh` is accepted next to `btih`, a v2-only link is refused (the engine needs a v1 hash). Caps: 8 KiB for the whole link (fits the 16 KiB JSON body limit), 128 trackers of at most 2 KiB, a display name of at most 1 KiB. `xs`, `as`, `ws` and `x.pe` are ignored. Fuzz targets: `FuzzParseMetainfo` (accepted torrents are within the caps, paths are safe, unique and equal to the engine's) and `FuzzParseMagnet` (no panic; an accepted link gives the hash anacrolix gives).

**Names on disk.** The stored metainfo is `<display name>.torrent` through `fsutil.SafeName`. A hostile torrent can have a top-level file or directory with that very name (or the user can pick a request name that collides), so the name is checked against the first component of every file path and the fixed name `.bitnews-metainfo.torrent` (then `.bitnews-<id>.torrent`) is used on a collision. The runner must use the same care when it saves a magnet's metainfo (`SetTorrentPath`).

**API.** `POST /downloads` takes the multipart part `torrent` (the NZB cap of 64 MiB applies to the request as a whole, a torrent part over 10 MiB is `413 torrent_too_large`), JSON `{"magnet"}` and JSON `{"url"}`. An `nzb` and a `torrent` part together are `400 multiple_sources`, `url` and `magnet` together `400 ambiguous_source`. Documented in `docs/api-v1.md`.

**Tests.** `internal/store/repo_torrent_sources_test.go`; `internal/torrent/parse*_test.go`; `internal/app/add_torrent_test.go` (staging, hostile input, names, queue positions, duplicates, atomicity at each step, fetch and magnet redirect, redaction); `internal/api/add_torrent_test.go` (every input form, caps, hostile and invalid files, duplicates, auth, scope and CSRF, sealed magnet at rest, atomicity, 503). Tests build torrents with `coretest.BuildTorrent` (hand-written bencode), because only `internal/torrent` may import anacrolix.

## Implementation notes (M5-04..06, M5-08)

Packages: the runner is `internal/torrent/runner` (see its package doc for the control loop); the daemon wiring is `internal/daemon/torrent.go` and `daemon.go`; the store side is `internal/store/repo_torrent_state.go` (migration `0007_torrent_state.sql`); the test swarm helpers are `internal/torrent/torrenttest`.

**Persistence (migration 0007).** `torrent_state(download_id PK -> downloads ON DELETE CASCADE, uploaded, seeding_seconds, ratio_limit REAL NULL, seeding_limit_seconds INTEGER NULL, moved, location NULL, updated_at) STRICT`, behind `core.TorrentRecordRepo` (`Get`/`Upsert`/`Delete`, `store.NewTorrentStateRepo`). A NULL limit means "use the global default", a negative one is unlimited. `moved = 0` with a `location` is the target of a move that was started and not confirmed.

**Core additions.** `core.SeedingSettings` in `settings.seeding` (`ratio_limit`, `seeding_time_minutes`, `action`: `pause` | `remove` | `remove_data`). The defaults are **no limit and pause**, as in qBittorrent: a ratio or time chosen by bitnews could end Seeding before a private tracker's minimum is met, so users and media managers opt in. Zero or negative is "no limit", so the zero value (an old client that PUTs settings without the key) is safe. `DownloadRepo.UpdateDisplay(id, name, totalBytes)` is a narrow write like `UpdateProgress`, used when a magnet's metadata arrives. `TorrentEngine.Metainfo(h)` returns the bytes of the .torrent file once the metadata is known; the engine also gained `Port()`, `AddPeers(h, addrs...)` and `BytesHashed(h)` (diagnostics the e2e tests use). `torrent.ErrInvalidMetainfo` now also matches `core.ErrInvalidTorrentSpec`, which is how the runner tells an unusable torrent (Download fails with `source_invalid`) from a transient engine error (retried). `finalize.Finisher.Destination` exposes the destination rule so the runner and the Finisher agree.

**Queue (M5-04).** One loop per runner: `core.Select` over Queued and Fetching torrents with `Queues.Torrent`; Seeding torrents are Fetched or later and never counted. A Download that is not chosen is paused in the engine (pieces kept) and goes back to Queued; a chosen one is resumed, or added on first use. Progress is read from the engine's stats once a second (`SampleInterval`), `DownloadProgress` goes through `core.ProgressThrottle` and `DoneBytes` is stored at most every 2 s with `UpdateProgress`. If the engine's Fetched event is lost (its channel drops events when full) the sampler sees all bytes and runs the Fetched step anyway. The Queue pauses as a user (`PauseQueue`), for the disk guard (`disk_full`: remaining bytes plus min_free free on the torrent incomplete root, checked before activation and every 30 s) or for another reason (`PauseQueueFor`, used for `bind_unavailable`). `SetRateLimits(down, up)` goes to the engine at start and when changed. Events: `torrent_queue_paused`, `torrent_queue_resumed`, `torrent_share_limit_reached`. A magnet is added to the engine at once (unless paused), `onMetadata` saves `<name>.torrent` in the Download directory (the name rule of `AddService`, with the `.bitnews-metainfo.torrent` fallback when a top-level path of the torrent has that name), stores the real name and size and queues it. It does not pause the torrent: a pause drops the connections, and with it the peer that just delivered the metadata (a magnet with only an `x.pe` hint then never finds it again); the next pass pauses it if the Queue had no room. `Hold`/`Release` work as in the Usenet runner, with one difference: Hold makes the engine let go of the torrent (`Remove(deleteData=false)`), and the data remover deletes the files.

**Move to complete (M5-05).** On Fetched the runner relocates the Download's directory to `Destination` (made unique) and **only then** moves the Download to Fetched. The brief had the order the other way round; the Post-processor starts the moment a Download is Fetched, so it would run in the old directory while the engine moves it, and a crash in between would leave a Fetched Download with half a move. With the move first, a crash leaves the Download Fetching, the next start runs the step again, and a record that already says "moved to X" makes it a no-op. The intent (`location`, `moved = 0`) is written before the move and confirmed after; at the next start, if the original directory exists the move did not happen, if only the target exists it did. The directory is the whole Download directory (`<root>/<id>`: the .torrent file and `<torrent name>/...`), so it lands as `<complete>/<category>/<Download name>/<torrent name>/...`; the path in the TorrentSource follows the file. A failed Relocate is logged and not fatal. A move across filesystems logs a warning with `kind=cross_device` (the Finisher's warning kind); hardlinks will not work for that layout (*reworded by M7-01*: the text now says the download was copied instead of renamed, and that Sonarr/Radarr can only hardlink when the library shares the complete folder's filesystem). With no `Destination` or a directory that already is the destination nothing moves (TRaSH roots that are equal still relocate from `<root>/<id>` to `<root>/<category>/<name>`).

**Seeding and Share Limits (M5-06).** The engine counts from zero at every start, so `torrent_state` keeps the totals of earlier runs. The runner persists `base + engine` every 30 s, when a torrent is Fetched and on shutdown, and every 30 s checks `(uploaded_total / size >= ratio) or (seeding_total >= time)` itself; the engine's own `TorrentShareLimitReached` is not used. The limit is the Download's own (NULL means default, negative unlimited) over `settings.seeding`. `pause` pauses the engine and keeps the Download untouched (Ready or not); raising the limit (`SetShareLimit`, `ClearShareLimit`, or the settings) resumes it. After a restart a torrent whose persisted counters already reach the limit is added **paused**, so there is no upload window. `remove`/`remove_data` go through `Config.Remover` (the daemon uses `DownloadService.Delete`, so the data remover and the Removed event do the rest) and are retried every evaluation if they fail. `SetShareLimit(ctx, id, core.ShareLimit)` and `EffectiveShareLimit` are for the qBit adapter. Seeding starts at Fetched; Ready can come first.

**Post-processing (M5-08).** The orchestrator's directory for a torrent is `Runner.ContentDir`: the torrent's own root directory inside the Download directory (the one entry that is left after ignoring the stored .torrent file, `_unpack`, `_unpack.tmp` and `.bitnews-done`), or the Download directory for a single-file torrent. `Unpacker.Detect` does not recurse, so this is where archives are found, and `_unpack/` is created there, next to the originals. The Finisher of the torrent side is a wrapper (`torrentFinisher`): for a torrent the engine has it returns the directory as it is and never moves or marks it, because moving it would pull the files from under the engine (the real Finisher still does the work for NZBs, and for a torrent that is not in the engine). The output path is the content directory, so the media managers get the originals and `_unpack/`. `FetchReport` answers a zero report for torrents. Removal with data deletes the whole Download directory (originals, `_unpack/`, .torrent file) inside the torrent roots only. A failed Unpack fails the Download (`archive_*`) and seeding goes on: the failed Download keeps its torrent in the engine.

**Daemon.** `Build` creates the engine unless `Deps.TorrentEngine` is set (tests) or `Deps.DisableTorrent` (torrents refused with `503 torrents_unavailable`): DHT and PEX on, MSE prefer, piece completion in the database, the port from `BITNEWS_TORRENT_PORT` (default 51413, 1 to 65535, `Deps.Torrent.Port`), `Binding` from `settings.network.torrent`. A binding that is unavailable at start makes `Build` fail with `torrent.ErrBindUnavailable`. Anacrolix log lines are passed on at warning level and above only, because lower levels contain tracker URLs. Shutdown: the HTTP server, then the workers (the torrent runner persists its counters), then the engine, then the database. The Usenet connection factory gets `Binding.Dialer` when `settings.network.usenet` is set. A `netbind.Killswitch` runs for each protocol that has a binding: on an outage the Usenet Queue is paused (resumed afterwards only if the kill switch paused it) and the torrent Queue is paused with reason `bind_unavailable`. Bound sockets are not rebound live: a change of the network settings logs a warning that a restart is needed, and an engine whose bound interface was deleted and recreated should be restarted too (open question: a supervisor that swaps the engine).

**Tests.** `internal/torrent/runner`: the limit with Seeding not counted, priority and force, lowering the limit, pause and resume, Recover, queue pause, speed limits, disk guard, progress throttle, magnet metadata flow, removal and Hold, the Fetched/move step (success, failure, unique name, destination equal, lost event, restart before and during a move), persistence, ratio and time limits with persisted offsets, per-Download override, the actions, restart with a reached limit, layout helpers, add failures, and a `-race` stress test. `internal/daemon/torrent_e2e_test.go` runs the daemon with the real engine against local seeders and leechers: magnet to Ready with a different complete root and seeding afterwards (the seeder is closed, a new leecher is served by the daemon alone), a .torrent upload, a 7z release unpacked into `_unpack/` with the originals' hashes unchanged and still seeding, a Share Limit pausing the torrent, a restart of a finished torrent without a rehash (`BytesHashed == 0`), and a restart in the middle of a download that does not fetch the received pieces again. Not covered: a private torrent through the daemon (the engine tests cover it), a bound interface in a network namespace (M5-07 notes), a magnet without any peer hint.
