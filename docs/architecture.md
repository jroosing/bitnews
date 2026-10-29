# bitnews architecture

bitnews is a self-hosted download daemon for homelabs. It fetches content from Usenet (like NZBGet/SABnzbd) and BitTorrent (like qBittorrent), post-processes it, and hands Ready output to media managers (Sonarr, Radarr, Prowlarr). A web UI (htmx) comes later and is built on the daemon's own API.

Domain terms (**Download**, **Source**, **Fetched**, **Ready**, …) are defined in [CONTEXT.md](../CONTEXT.md). Key decisions are recorded in [docs/adr/](adr/).

## Guiding constraints

- **Security without concessions.** The daemon parses hostile input constantly, so we use a memory-safe language and mature TLS. The only native code it runs is 7-Zip and par2cmdline-turbo for post-processing, in a hardened container, with our own guards around them. See [ADR-0001](adr/0001-go-over-zig.md) and [ADR-0007](adr/0007-external-postproc-tools.md) (which replaced ADR-0002).
- **Media managers must just work.** bitnews imitates the client APIs they already support. "Complete" means Ready, never merely Fetched. See [ADR-0003](adr/0003-emulate-qbittorrent-and-sabnzbd-apis.md).
- **Hardlinks are the user's layout, not our code.** Sonarr and Radarr make the hardlinks, so the user keeps the complete folders and the library on one filesystem (the TRaSH-guides layout). bitnews never hardlinks, moves with rename and copies when the folders differ, and never modifies files after Ready. See [ADR-0006](adr/0006-simplify-filesystem-permissions-and-api-key.md).
- **Runs with or without Docker.** It ships as one static binary (`CGO_ENABLED=0`) that is packaged for Docker (the primary target, a homelab) and also runs on bare metal.

## Technology

| Concern | Choice |
|---|---|
| Language | Go (ADR-0001) |
| BitTorrent | `anacrolix/torrent`, wrapped behind our own port |
| Usenet (NNTP, yEnc, NZB) | Our own implementation |
| PAR2 verify and repair | `par2` (par2cmdline-turbo), run as a subprocess (ADR-0007) |
| RAR / 7z unpack | `7zz` (7-Zip), run as a subprocess (ADR-0007) |
| Persistence | SQLite via `modernc.org/sqlite` (no cgo), one `bitnews.db` |
| TLS | Go `crypto/tls` |
| Password hashing | argon2id for human passwords. Session tokens are stored as SHA-256 hashes. The single app key is stored in plain text so Settings can show it. |
| Secret encryption | XChaCha20-Poly1305 / AES-GCM, with the key in a separate file |
| UI (later) | htmx with server-rendered HTML handlers on the same application services as the own API, plus SSE |

## Components

```
                    ┌──────────── Media managers ─────────────┐     Browser (htmx, later)
                    │  Sonarr / Radarr / Prowlarr             │            │
                    └────┬──────────────────────┬─────────────┘            │
                 SAB API │ apikey     qBit API v2│ user + key        API v1 + SSE
                ┌────────▼──────┐   ┌────────────▼──┐   ┌───────────────────▼──┐
                │ adapter/      │   │ adapter/qbit  │   │ api (own) + sse      │
                │ sabnzbd       │   │               │   │                      │
                └────────┬──────┘   └──────┬────────┘   └──────────┬───────────┘
                         └───────────┬─────┴───────────────────────┘
                                     │      auth (sessions, CSRF, app key, LAN bypass)
                         ┌───────────▼──────────────────────────────────────┐
                         │ core                                             │
                         │  Download · Source · Category · Queue            │
                         │  lifecycle state machine · scheduler · event bus │
                         │  ports: UsenetFetcher, TorrentEngine,            │
                         │         Repairer, Unpacker, Store, FS            │
                         └──┬──────────────┬──────────────┬──────────────┬──┘
                ┌───────────▼───┐ ┌────────▼──────┐ ┌─────▼───────┐ ┌────▼─────┐
                │ usenet        │ │ torrent       │ │ postproc    │ │ store    │
                │ nzb·nntp·yenc │ │ (anacrolix)   │ │ par2·unpack │ │ sqlite   │
                └───────┬───────┘ └───────┬───────┘ └─────┬───────┘ └──────────┘
                  TLS/NNTP          BT peers/DHT        fsutil (safe paths, same-fs, umask)
                  Servers           (optionally bound
                                     to an interface)
```

**Dependency rule:** `core` imports nothing else in the project. It defines the ports (interfaces), and every other package depends on `core`, never the other way round. Only `internal/torrent` imports anacrolix.

## Package layout

```
github.com/jroosing/bitnews   (Go module and GitHub repository)
cmd/bitnews/            bootstrap flags/env, applies the umask, opens the DB
internal/daemon/        composition root: wires stores, services, API, runner, post-processing
internal/core/          domain model, lifecycle, scheduler, event bus, ports
internal/usenet/nzb/    NZB parser
internal/usenet/nntp/   NNTP client, per-Server connection pool, Server Priority fallback
internal/usenet/yenc/   yEnc decoder + CRC32
internal/torrent/       TorrentEngine adapter over anacrolix
internal/postproc/par2/ runs par2 (Verify, Repair), hostile-name check
internal/postproc/unpack/ runs 7zz: listing guards, path guard, size caps
internal/store/         SQLite repositories, migrations, secret encryption
internal/auth/          sessions, CSRF, argon2id, app key, LAN bypass
internal/api/           own API v1 + SSE
internal/adapter/qbit/  qBittorrent WebUI API v2 subset
internal/adapter/sabnzbd/ SABnzbd API subset
internal/fsutil/        safe join, free space, umask, move (rename, else copy)
internal/web/           htmx + Tailwind + Alpine UI, templ views and assets embedded (milestone 6, ADR-0005, ADR-0011)
```

## Download lifecycle

```
Added ─► Queued ─► Fetching ─► Fetched ─► Post-processing ─► Ready ─► Removed
  │         ▲  │       │                        │                ▲
  │      paused│       └───────────┬────────────┘                │
  │            ▼                   ▼                             │
  │         Paused               Failed ─────────────────────────┘
  └─ (magnet) Fetching metadata ─► Queued

Torrent only: Seeding runs in parallel from Fetched until the Share Limit is reached or the Download is removed.
```

- **Fetched** means the protocol-level work is done: all Articles are fetched, or all pieces are hash-verified.
- **Post-processing** for Usenet is Verify → Repair (if needed) → Unpack → cleanup → move. For a torrent it is Unpack only, and only when archives are present. The output goes to `_unpack/` and the originals are never touched.
- **Ready** is the only state media managers see as complete.
- **Failed** covers Missing Articles beyond repair, unsupported archives and wrong passwords. Media managers see it as failed, which lets them blocklist the release and pick another.
- **Removed** happens when a media manager or the user deletes the Download. Its data is deleted, including `_unpack/`.

## Usenet engine

- Each **Server** has a host, a port, credentials (encrypted), max connections, retention and a **Server Priority**.
- TLS is strict: TLS only, certificates are always verified, and there is no plaintext NNTP and no skip-verify option.
- Articles are fetched from the lowest Server Priority first, with load shared among Servers of equal priority. A `430` on all Servers at one priority falls through to the next. An Article that is missing everywhere is recorded as a Missing Article and left to Repair.
- Servers are skipped for Articles older than their retention.
- Decoded yEnc data is written straight to the right offset in pre-allocated files. There are no per-Article temp files. Each part is checked against its CRC32.
- The NZB is fetched immediately when the Download is added. Indexer `apikey` parameters are stripped before anything is persisted or logged.

## Torrent engine

- `anacrolix/torrent` sits behind the `TorrentEngine` port, so domain types never leak anacrolix types.
- DHT, PEX and LSD are on for public torrents and automatically off for private ones (BEP 27). MSE is set to `prefer` by default, and `require` is available.
- There is no UPnP or NAT-PMP. The listen port is configurable.
- An optional bind interface/address can be set per protocol. If the interface goes away, traffic stops; it never falls back to another route. Setting up the VPN (e.g. gluetun) is the user's job.
- Share Limits (ratio/time) are honoured per Download, with global defaults. Media managers set them through the qBit adapter.

## Post-processing

- **Verify and repair** run one `par2 r`, which also renames obfuscated files, after checking the file names in the PAR2 set. PAR2 files are found by content, so hash-named ones from fully obfuscated posts count. When no PAR2 set renamed the movie, Finish names the largest file after the Download if its content is a video (SABnzbd's deobfuscation, M14).
- **Repair** runs `par2 r`. It runs only when Verify finds damage.
- **Unpack** runs `7zz` (RAR 2–5, 7z, zip, multi-volume, passwords on stdin). It lists the archive first and enforces the entry, size and ratio caps, extracts to a staging dir, then walks the result and rejects `..`, absolute, symlink, hardlink and special entries (ADR-0007).
- One Download is post-processed at a time by default (configurable). Fetching continues meanwhile: Usenet Download B fetches while A is post-processing.
- 7-Zip and par2cmdline-turbo are the only subprocesses, started through `internal/postproc/tool` (ADR-0007).

## Scheduling

- There are two Queues, one per protocol.
  - The Usenet Queue runs 1 active Download by default, which gets all connections.
  - The torrent Queue runs 5 active Downloads by default, with unlimited Seeding.
- **Download Priority** is `force` / `high` / `normal` / `low`. `force` ignores the concurrency limit. The adapters map media-manager priorities onto it.
- Pause/resume works per Download and globally per Queue.
- Speed limits are global and per protocol, for download and upload.

## Media manager adapters

Both adapters and the own API serve on one port under separate paths: `/api/v2/*` (qBit), `/sabnzbd/api` (SAB) and `/api/v1/*` (own). Each adapter implements only the calls media managers make: version, auth, add (file/URL/magnet), queue/list, history, delete, categories, priority and share limits.

| bitnews state | SAB adapter | qBit adapter |
|---|---|---|
| Queued / Paused | queue: `Queued` / `Paused` | `queuedDL` / paused state |
| Fetching | queue: `Downloading` | `downloading` / `stalledDL` / `metaDL` |
| Fetched → Post-processing | history: `Verifying` / `Repairing` / `Extracting` | non-complete state, progress < 1 |
| Ready | history: `Completed` | `uploading` / `stalledUP` / paused-up |
| Failed | history: `Failed` + reason | `error` |

The exact strings media managers rely on are pinned down by contract tests that replay recorded Sonarr/Radarr traffic.

## Storage and filesystem

The defaults follow the TRaSH-guides layout:

```
/data/usenet/incomplete/<download>/
/data/usenet/complete/<category>/<download>/
/data/torrents/<category>/<download>/          (+ _unpack/ when archives are present)
/data/media/...                                 (media manager library, same filesystem)
```

- The `incomplete` and `complete` roots are set per protocol in the UI. Each **Category** can optionally override its dir, which otherwise defaults to `<complete root>/<category>`.
- Path changes affect only new Downloads.
- Moving into `complete` is a `rename`, or a copy then remove when the roots are on different filesystems. bitnews does not check or warn about filesystems: keeping `complete` and the library on one filesystem, so that Sonarr and Radarr can hardlink, is the user's job. Saved paths are checked for shape only (absolute, clean, not nested); missing directories are created at first use.
- Files are never modified after Ready, because hardlinks share their data with the library.
- The umask is set with the `UMASK` bootstrap env (default `002`), because it has to apply before any file is created. bitnews and the media managers should share a group.

## Persistence and secrets

- One SQLite file, `bitnews.db`, in the config dir, with mode `0600` and WAL. It holds Downloads, Categories, Servers, settings, users, the app key and torrent piece completion.
- The UI password is stored as an argon2id hash. Session tokens are 256-bit random values stored as SHA-256 hashes; the app key is a 256-bit random value kept in plain text and compared against a copy in memory. Both are compared in constant time. argon2id isn't needed for high-entropy tokens and would cost 64 MiB per request.
- Server and archive passwords are encrypted with a master key. It is generated on first start as `bitnews.key` (mode `0600`) next to the database, and its location is logged ([ADR-0004](adr/0004-master-key-in-config-dir.md)). `--key-file` can place it elsewhere, such as a Docker secret. Losing the key means re-entering Server passwords; nothing else is lost.
- Secrets never appear in logs, and URLs are redacted.

## Authentication

- **UI:** session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` behind HTTPS), the standard library's cross-origin check on state-changing requests (ADR-0013), login rate limiting with backoff. On first start a setup screen creates the admin; `BITNEWS_ADMIN_PASSWORD_FILE` can bootstrap it instead.
- **Optional, UI only, off by default:** *disable auth* and *LAN bypass*. LAN bypass takes an explicit CIDR list; `X-Forwarded-For` is honoured only from configured trusted proxies. While LAN bypass is active, the UI shows a permanent warning banner; Settings → Security warns about both options. CSRF and Host-header checks stay on regardless, to block drive-by requests and DNS rebinding.
- **Adapters and own API:** always authenticated, never bypassed.
  - One app key serves all three ([ADR-0006](adr/0006-simplify-filesystem-permissions-and-api-key.md)): the SAB `apikey` parameter, the qBit password (fixed username `bitnews`) and `Authorization: Bearer` on the own API. Full access, no scopes, rotated from Settings → Security.
- v1 has a single admin. Multi-user and forward-auth SSO come later.

## Events

- `core` emits domain events: `DownloadAdded`, `Fetched`, `Ready`, `Failed`, `Removed`, plus progress ticks throttled to about 1/s per Download.
- An in-process bus delivers them to `/api/v1/events` (SSE) for the UI.
- Events are notifications only. State lives in the DB.
- Later webhooks and notifications subscribe to the same bus without core changes.

## Deployment

- **Binary:** a static `bitnews` for linux amd64 (darwin for development).
- **Bootstrap config (flags/env only):** `--config-dir`, optional `--key-file`, listen address (default `:9494`), `UMASK`. Everything else is configured through the UI/API and stored in the DB. The full table is in [M1](milestones/m1-foundation.md#bootstrap-configuration).
- **Privileges:** bitnews runs as whoever starts it. The container's entrypoint starts as root, gives `/config` to `PUID:PGID` and runs bitnews as that user ([ADR-0010](adr/0010-puid-pgid-entrypoint.md)); `user:` still works. bitnews applies `UMASK` (default `002`) before creating any file.
- **Docker:** Alpine image with a PUID/PGID entrypoint, `/config` volume (database and key), `/data` mounted at the same path as in the media-manager containers. HTTPS is terminated by a reverse proxy (Caddy/Traefik/nginx).

## Testing and CI

- **Fuzzing** (`go test -fuzz`) covers every hostile parser: NZB, NNTP responses, yEnc, PAR2 file names and archive entry paths. The corpus is committed. Fuzzing runs locally (`make fuzz-smoke`), not in CI.
- **PAR2 tests:** the tests build real sets with `par2 c` (the pinned `par2`, from `make tools`), damage them and check that Repair restores the data byte-exactly.
- **End-to-end tests** (`internal/daemon`, build tag `e2e`, `make e2e`) run a whole daemon over HTTP against fake NNTP servers and a local torrent swarm. They run locally only, not in CI, like fuzzing.
- **Fake NNTP server:** an in-process server simulates `430`s, slow and dropped connections, and Server Priority fallback.
- **Torrent:** a local anacrolix swarm (seeder plus bitnews as leecher).
- **Adapter contract tests:** replay recorded Sonarr/Radarr requests.
- **GitHub Actions:** `go test -race`, `govulncheck`, `staticcheck`, and pinned dependencies. Fuzzing is not part of CI; it runs locally.

## Milestones

Each milestone has a design doc with full issue specs in [docs/milestones/](milestones/README.md).

1. **Foundation:** core model and lifecycle, SQLite store and migrations, secret encryption, auth, own API v1 + SSE, bootstrap and privilege drop.
2. **Usenet fetch:** NZB parsing, NNTP pool with strict TLS, Server Priority fallback, retention, yEnc and direct write. Ends at Fetched.
3. **Post-processing:** PAR2 Verify and Repair, Unpack, path guard, cleanup and move. Ends at Ready/Failed.
4. **SAB adapter:** Sonarr/Radarr work end-to-end on Usenet. This is the first point of daily use.
5. **Torrents:** anacrolix adapter, Queue, Seeding and Share Limits, torrent Unpack, bind interface, qBit adapter.
6. **UI:** htmx on the own API + SSE, covering queue, history and settings.

Until milestone 6, configuration goes through the API.

## Out of scope for v1

Scheduled speed limits, webhooks and notifications, multi-user/roles, SSO, moving existing Downloads, RSS, UPnP/NAT-PMP, and in-app VPN management.
