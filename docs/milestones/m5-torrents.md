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
  - Torrents flagged private (BEP 27) never use them; anacrolix enforces this.
  - A test must assert it anyway.
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
- **Duplicates:** an infohash that already exists returns `409` with the existing Download ID. If the new request has more trackers, they are merged.

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

The fixtures are the spec, as in M4. M5-09 records real Sonarr/Radarr ↔ qBittorrent traffic. The expected subset of the WebUI API v2, under `/api/v2/`:

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
- A duplicate infohash returns `409` with the existing ID and merges trackers.
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
- The state mapping table, adjusted to what M5-09 found about the import hold-off.

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
- [ ] CI runs the contract tests on every MR and the e2e job on `main`, and both pass.
- [ ] The manual checklist is recorded with versions.
