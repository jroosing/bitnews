# bitnews architecture

bitnews is a self-hosted download daemon for homelabs. It fetches content from Usenet (like NZBGet/SABnzbd) and BitTorrent (like qBittorrent), post-processes it, and hands Ready output to media managers (Sonarr, Radarr, Prowlarr). A web UI (htmx) comes later and is built on the daemon's own API.

Domain terms (**Download**, **Source**, **Fetched**, **Ready**, …) are defined in [CONTEXT.md](../CONTEXT.md). Key decisions are recorded in [docs/adr/](adr/).

## Guiding constraints

- **Security without concessions.** The daemon parses hostile input constantly, so we use a memory-safe language, mature TLS, and no native subprocesses. See [ADR-0001](adr/0001-go-over-zig.md) and [ADR-0002](adr/0002-pure-go-post-processing.md).
- **Media managers must just work.** bitnews imitates the client APIs they already support. "Complete" means Ready, never merely Fetched. See [ADR-0003](adr/0003-emulate-qbittorrent-and-sabnzbd-apis.md).
- **Hardlinks are mandatory.** We follow the TRaSH-guides layout, never copy across filesystems silently, and never modify files after Ready.
- **Runs with or without Docker.** It ships as one static binary (`CGO_ENABLED=0`) that works in Docker, in LXC/systemd and on bare metal.

## Technology

| Concern | Choice |
|---|---|
| Language | Go (ADR-0001) |
| BitTorrent | `anacrolix/torrent`, wrapped behind our own port |
| Usenet (NNTP, yEnc, NZB) | Our own implementation |
| PAR2 verify and repair | Our own implementation, Reed-Solomon over GF(2^16) (ADR-0002) |
| RAR / 7z unpack | `nwaples/rardecode/v2`, `bodgit/sevenzip` (ADR-0002) |
| Persistence | SQLite via `modernc.org/sqlite` (no cgo), one `bitnews.db` |
| TLS | Go `crypto/tls` |
| Password hashing | argon2id for human passwords. Generated high-entropy tokens (sessions, API keys, adapter credentials) are stored as SHA-256 hashes. |
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
                                     │      auth (sessions, CSRF, API keys, LAN bypass)
                         ┌───────────▼──────────────────────────────────────┐
                         │ core                                             │
                         │  Download · Source · Category · Queue            │
                         │  lifecycle state machine · scheduler · event bus │
                         │  ports: UsenetFetcher, TorrentEngine,            │
                         │         Verifier, Repairer, Unpacker, Store, FS  │
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
gitlab.com/kyura4k/gitnews   (Go module; GitLab project is kyura4k/bitnews)
cmd/bitnews/            bootstrap flags/env, PUID/PGID privilege drop, opens the DB
internal/daemon/        composition root: wires stores, services, API, runner, post-processing
internal/core/          domain model, lifecycle, scheduler, event bus, ports
internal/usenet/nzb/    NZB parser
internal/usenet/nntp/   NNTP client, per-Server connection pool, Server Priority fallback
internal/usenet/yenc/   yEnc decoder + CRC32
internal/torrent/       TorrentEngine adapter over anacrolix
internal/postproc/par2/ packet parser, Verify, GF(2^16) Repair
internal/postproc/unpack/ rardecode + sevenzip, path guard, size caps
internal/store/         SQLite repositories, migrations, secret encryption
internal/auth/          sessions, CSRF, argon2id, API keys, LAN bypass
internal/api/           own API v1 + SSE
internal/adapter/qbit/  qBittorrent WebUI API v2 subset
internal/adapter/sabnzbd/ SABnzbd API subset
internal/fsutil/        safe join, same-filesystem check, umask, atomic move
web/                    htmx UI (milestone 6)
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

- **Verify** uses PAR2 packet parsing plus MD5/CRC32 checks.
- **Repair** is Reed-Solomon over GF(2^16), implemented in Go. It runs only when Verify finds damage.
- **Unpack** uses pure-Go RAR (2–5, multi-volume, passwords) and 7z. It writes to a staging dir, rejects `..`, absolute and symlink entries, and enforces size caps against archive bombs.
- One Download is post-processed at a time by default (configurable). Fetching continues meanwhile: Usenet Download B fetches while A is post-processing.
- There are no subprocesses at all.

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
- Moving into `complete` is a `rename` on the same filesystem. When saving the settings, the UI warns if the roots are on different filesystems.
- Files are never modified after Ready, because hardlinks share their data with the library.
- The umask is set with the `UMASK` bootstrap env (default `002`), because it has to apply before any file is created. bitnews and the media managers should share a group.

## Persistence and secrets

- One SQLite file, `bitnews.db`, in the config dir, with mode `0600` and WAL. It holds Downloads, Categories, Servers, settings, users/API keys and torrent piece completion.
- The UI password is stored as an argon2id hash. Generated tokens (sessions, own API keys, SAB apikeys, qBit adapter passwords) are 256-bit random values stored as SHA-256 hashes. Both are compared in constant time. argon2id isn't needed for high-entropy tokens and would cost 64 MiB per request.
- Server and archive passwords are encrypted with a master key. It is generated on first start as `bitnews.key` (mode `0600`) next to the database, and its location is logged ([ADR-0004](adr/0004-master-key-in-config-dir.md)). `--key-file` can place it elsewhere, such as a Docker secret. Losing the key means re-entering Server passwords; nothing else is lost.
- Secrets never appear in logs, and URLs are redacted.

## Authentication

- **UI:** session cookie (`HttpOnly`, `SameSite=Strict`, `Secure` behind HTTPS), CSRF token on state-changing requests, login rate limiting with backoff. On first start a setup screen creates the admin; `BITNEWS_ADMIN_PASSWORD_FILE` can bootstrap it instead.
- **Optional, UI only, off by default:** *disable auth* and *LAN bypass*. LAN bypass takes an explicit CIDR list; `X-Forwarded-For` is honoured only from configured trusted proxies. While either option is active, the UI shows a permanent warning banner. CSRF and Host-header checks stay on regardless, to block drive-by requests and DNS rebinding.
- **Adapters and own API:** always authenticated, never bypassed.
  - The SAB adapter uses its `apikey`.
  - The qBit adapter uses a generated credential pair for each media manager (username plus generated key).
  - Own-API keys carry `read`/`write` scopes.
  - Adapter credentials work only on adapter endpoints.
- v1 has a single admin. Multi-user and forward-auth SSO come later.

## Events

- `core` emits domain events: `DownloadAdded`, `Fetched`, `Ready`, `Failed`, `Removed`, plus progress ticks throttled to about 1/s per Download.
- An in-process bus delivers them to `/api/v1/events` (SSE) for the UI.
- Events are notifications only. State lives in the DB.
- Later webhooks and notifications subscribe to the same bus without core changes.

## Deployment

- **Binary:** a static `bitnews` for linux amd64/arm64 (darwin for development).
- **Bootstrap config (flags/env only):** `--config-dir`, optional `--key-file`, listen address (default `:9494`), `PUID`/`PGID`, `UMASK`. Everything else is configured through the UI/API and stored in the DB. The full table is in [M1](milestones/m1-foundation.md#bootstrap-configuration).
- **Privileges:** with `PUID`/`PGID` set, the binary drops to them (`setgroups`/`setgid`/`setuid`) before opening the DB, sockets or any input. Compose `user:` also works. Root is allowed and logs a startup warning.
- **Docker:** distroless image, `/config` volume (database and key), `/data` mounted at the same path as in the media-manager containers. HTTPS is terminated by a reverse proxy (Caddy/Traefik/nginx).
- **systemd / LXC:** a `bitnews` user, DB and key in `/var/lib/bitnews`, and hardening (`ProtectSystem=strict`, `NoNewPrivileges`, `ReadWritePaths=` for data dirs only).

## Testing and CI

- **Fuzzing** (`go test -fuzz`) covers every hostile parser: NZB, NNTP responses, yEnc, PAR2 packets and archive entry paths. The corpus is committed.
- **PAR2 golden tests:** fixtures are made offline with `par2cmdline` and committed. Repair must restore damaged data byte-exactly. The tool is never used at runtime.
- **Fake NNTP server:** an in-process server simulates `430`s, slow and dropped connections, and Server Priority fallback.
- **Torrent:** a local anacrolix swarm (seeder plus bitnews as leecher).
- **Adapter contract tests:** replay recorded Sonarr/Radarr requests.
- **GitLab CI:** `go test -race`, `govulncheck`, `staticcheck`, a short fuzz smoke run and pinned dependencies.

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
