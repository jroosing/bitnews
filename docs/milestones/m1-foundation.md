# M1 — Foundation

## Goal

By the end of M1 the daemon starts, drops privileges, opens its encrypted-secret store, authenticates users and API clients, and exposes the own API v1 plus SSE for Downloads, Categories and settings. It cannot fetch anything yet. Every later milestone builds on these pieces.

Read first: [architecture](../architecture.md), [glossary](../../CONTEXT.md), and ADRs [0001](../adr/0001-go-over-zig.md), [0002](../adr/0002-pure-go-post-processing.md) and [0003](../adr/0003-emulate-qbittorrent-and-sabnzbd-apis.md).

## Scope

**In:** repository and CI, bootstrap config, logging, privilege drop, core domain model and lifecycle, event bus, scheduling policy, SQLite store, secret encryption, users, sessions, CSRF, API keys, own API v1 read/manage endpoints, SSE, Docker and systemd packaging.

**Out:** adding Downloads (M2 for NZB, M5 for torrents), any network protocol, Post-processing, media-manager adapters, UI.

## Design

### Module and tooling

- Module path is `gitlab.com/kyura4k/gitnews` (the GitLab project stays `kyura4k/bitnews`), with Go 1.27.2 or later as the `go` directive.
- External dependencies are kept to a minimum and each one is justified in its MR. M1 allows only `modernc.org/sqlite` and `golang.org/x/crypto` (argon2, chacha20poly1305) in production code. Test code may use `github.com/stretchr/testify` (`require`/`assert`) for clearer assertions; test-only dependencies never ship in the binary.
- Routing uses the standard `net/http` `ServeMux` with method and path patterns, so there is no router dependency.
- Logging uses `log/slog`, with text or JSON output.

### Bootstrap configuration

These are the only settings that do not live in the DB. Each can be given as a flag or as an env var, and the flag wins.

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--config-dir` | `BITNEWS_CONFIG_DIR` | `$XDG_DATA_HOME/bitnews` (dev) | Holds `bitnews.db`. Docker uses `/config`, systemd uses `/var/lib/bitnews`. |
| `--key-file` | `BITNEWS_KEY_FILE` | `$XDG_CONFIG_HOME/bitnews/bitnews.key` (dev). The image sets `/run/secrets/bitnews_key`; systemd uses `/etc/bitnews/bitnews.key`. | Master key for secret encryption. Must not be inside `config-dir`. If it is missing and its directory is not writable, startup fails with instructions. |
| `--listen` | `BITNEWS_LISTEN` | `0.0.0.0:9494` | One port for the own API, adapters and UI. |
| `--log-level` | `BITNEWS_LOG_LEVEL` | `info` | |
| `--log-format` | `BITNEWS_LOG_FORMAT` | `text` | `text` or `json` |
| — | `PUID` / `PGID` | unset | When running as root, drop to these. |
| — | `UMASK` | `002` | Applied process-wide at startup. This follows the linuxserver convention. |
| — | `BITNEWS_ADMIN_PASSWORD_FILE` | unset | Optional bootstrap of the admin password. |

`UMASK` is a bootstrap setting rather than a UI setting because it has to apply before the DB and any files are created.

### Startup order

1. Parse flags and env, then apply `UMASK`.
2. If the uid is 0 and `PUID`/`PGID` are set, call `setgroups([PGID])`, then `setgid(PGID)`, then `setuid(PUID)`. Verify that `setuid(0)` now fails, and abort if it succeeds. If the uid is 0 and they are not set, log a warning and continue as root.
3. Load or create the key file.
4. Open the DB, which creates it with mode `0600` if missing, and run migrations.
5. Wire up the services and start the HTTP server.
6. On SIGTERM/SIGINT, shut down gracefully: stop accepting requests, stop engines, flush state, close the DB.

No input from the network or disk may be parsed before step 2 finishes.

### Domain model (`internal/core`)

- **Download**
  - `ID`: 128-bit random, base32 lowercase. Stable and opaque.
  - `Name`, `SourceKind` (`nzb` | `torrent`), `CategoryID` (nullable), `State`, `Priority` (Download Priority: `force` | `high` | `normal` | `low`), `QueuePosition`.
  - `Paused` (bool, orthogonal to State while Queued/Fetching).
  - `FailureReason` (enum, empty unless Failed), `FailureDetail` (free text, redacted).
  - `TotalBytes`, `DoneBytes`, `AddedAt`, `FetchedAt`, `ReadyAt`, `UpdatedAt`.
  - `OutputPath`: set once Ready.
- **Category**: `ID`, `Name` (unique, `[a-z0-9._-]{1,64}`), `DirOverride` (nullable absolute path).
- **States**: `Added`, `FetchingMetadata` (magnets, M5), `Queued`, `Fetching`, `Fetched`, `PostProcessing`, `Ready`, `Failed`.
  - Removal deletes the row. A `Removed` event is emitted.
  - `Seeding` is a torrent attribute that runs in parallel. It is not a lifecycle state.
- **Transitions:** the only legal moves are the ones in the architecture lifecycle diagram, enforced by a single `Transition(from, to)` table. Any other move is a bug and returns an error.
- **Failure reasons** (shared enum, extended in later milestones): `missing_articles`, `repair_insufficient`, `archive_unsupported`, `archive_password`, `archive_corrupt`, `disk_full`, `path_violation`, `source_invalid`, `internal`.

### Scheduling policy

A pure function: `Select(queue []Download, limits) (activate, deactivate []ID)`.

- `force` Downloads are always active.
- The remaining Downloads are ordered by Download Priority (high → low), then `QueuePosition`, then `AddedAt`.
- Paused Downloads are never active.
- At most `limit` non-force Downloads are active.
- The policy is the same for both Queues; only the limits differ (Usenet 1, torrent 5).

### Event bus

- Events are typed: `DownloadAdded`, `DownloadUpdated` (state, priority, category or pause changes), `DownloadProgress`, `Fetched`, `Ready`, `Failed`, `Removed`, `SettingsChanged`.
- `Publish` never blocks.
- Each subscriber gets a buffered channel (256 entries).
  - If the buffer is full, `DownloadProgress` events for that subscriber are dropped.
  - If it is full and the event is a lifecycle event, the subscriber is closed with a `resync` reason so that SSE clients reconnect and refetch.
- Progress is throttled at the source to at most 1 event per second per Download.
- Events are never persisted.

### Persistence (`internal/store`)

- SQLite via `modernc.org/sqlite`.
  - Pragmas: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`.
  - One writer connection plus a read pool.
  - Timestamps are stored as INTEGER unix epoch milliseconds (UTC), and the zero time is stored as NULL. Tables are STRICT.
- The DB file is created with mode `0600` before it is first opened. If an existing file has broader permissions, log a warning.
- Migrations are embedded SQL files `NNNN_name.sql`, applied in order inside transactions and tracked in `schema_migrations`. They only move forward; there are no down migrations.
- M1 tables:
  - `downloads`, `categories`
  - `settings`: key → JSON value. Settings are typed in Go and validated on write.
  - `users`, `sessions`, `api_keys`
- Every repository interface is defined in `core`, and `store` implements it.

### Secret encryption

- The key file holds 32 random bytes, base64-encoded (44 chars plus a newline).
  - If the file is missing, it is generated with mode `0600` and its location is logged.
  - If its permissions are broader than `0600`, log a warning instead of refusing, because Docker secrets are often `0444`.
- The cipher is XChaCha20-Poly1305 (`golang.org/x/crypto/chacha20poly1305.NewX`).
  - Stored format: `v1:` + base64(nonce(24) ‖ ciphertext).
  - The AAD is `table/column/rowID`, so ciphertexts can't be swapped between rows.
- A `Secret` string type has `String()` and `MarshalJSON` that return `"[redacted]"`, so secrets can't leak through logs or APIs.

### Authentication (`internal/auth`)

- **Human passwords** are hashed with argon2id (m=64 MiB, t=3, p=2, 16-byte salt, 32-byte key) and stored as a PHC string.
- **Generated tokens** (session tokens, API keys and, later, adapter keys) are 256-bit random values. Only their SHA-256 hash is stored, and lookups compare in constant time. argon2id is not needed for high-entropy tokens and would cost 64 MiB per request.
- **Setup:**
  - While no user exists, every route except `/api/v1/setup`, `/api/v1/health` and static assets returns `503 setup_required`.
  - Setup needs a one-time setup token. It is printed to the log at startup and stored only in memory. This stops someone else on the LAN from claiming the instance first.
  - `BITNEWS_ADMIN_PASSWORD_FILE` creates the admin non-interactively and skips the setup step.
- **Sessions:**
  - The cookie `bitnews_session` is `HttpOnly`, `SameSite=Strict`, `Path=/`, and `Secure` when the request arrived over HTTPS (directly or through a trusted proxy's `X-Forwarded-Proto`).
  - Sessions expire after 7 days idle and 30 days absolute. Logout deletes the row.
- **CSRF:**
  - Each session has a synchronizer token. Every state-changing request authenticated by cookie must send it in `X-CSRF-Token`.
  - The `Origin` header must also match an allowed host (or be absent) on those requests.
  - API-key requests don't need CSRF because they are not cookie-based.
- **Login rate limiting:** in memory, per IP and per username, with exponential backoff starting at 1 s and capped at 15 min after 5 failures. Failures are logged without the password.
- **Disable auth / LAN bypass:**
  - Both are settings that apply to the UI only, and both are off by default.
  - LAN bypass takes an explicit CIDR list.
  - `X-Forwarded-For` is honoured only when the direct peer is in `trusted_proxies`.
  - Enabling either one requires a non-empty `allowed_hosts` list. Requests whose `Host` is not in that list get `421`. This blocks DNS rebinding.
  - CSRF stays enforced.
- **API keys:** the format is `bn_` + base32(32 bytes). Each key has a name, a scope (`read` | `write`), `created_at` and `last_used_at`. The plaintext is shown once, at creation.
- **Adapter credentials** (M4/M5) reuse the token mechanism but are only valid on adapter routes.

### HTTP server and own API v1

- **Server hardening:**
  - Timeouts: `ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s, `IdleTimeout` 120 s. `WriteTimeout` is disabled only on the SSE route.
  - `MaxHeaderBytes` is 64 KiB. Request bodies are capped per route, at 1 MiB by default.
- **Security headers:** `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`.
- **Request IDs:** every request gets an `X-Request-ID`, which is also put into the log context.
- **Errors:** the JSON shape is `{"error": {"code": "snake_case", "message": "..."}}`.

M1 routes (more are added in later milestones):

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/v1/health` | none | Liveness. Returns `{"status":"ok"}`. |
| POST | `/api/v1/setup` | setup token | Create the admin. |
| POST | `/api/v1/auth/login` / `logout` | — / session | Session login and logout. |
| GET | `/api/v1/auth/session` | session | Current user plus CSRF token. |
| GET | `/api/v1/version` | read | Version, commit, Go version. |
| GET | `/api/v1/downloads` | read | Filter by `state`, `category`, `kind`. Cursor paging. |
| GET | `/api/v1/downloads/{id}` | read | Detail. |
| POST | `/api/v1/downloads/{id}/pause` / `resume` | write | |
| PATCH | `/api/v1/downloads/{id}` | write | Change `priority`, `category`, `queue_position`. |
| DELETE | `/api/v1/downloads/{id}?delete_data=true` | write | Remove. |
| GET/POST | `/api/v1/categories` | read/write | |
| PATCH/DELETE | `/api/v1/categories/{id}` | write | |
| GET/PUT | `/api/v1/settings` | read/write | Typed, validated. |
| GET/POST | `/api/v1/api-keys` | session only | |
| DELETE | `/api/v1/api-keys/{id}` | session only | |
| GET | `/api/v1/events` | read | SSE. |

**SSE:**
- Format: `event: <type>` plus `data: <json>`.
- A `hello` event carries the instance ID. A `: ping` comment is sent every 15 s.
- `Last-Event-ID` is not supported, because events are not persisted. Clients refetch state on connect.

### Packaging

- **Docker:**
  - Multi-stage build, ending in `gcr.io/distroless/static-debian12`.
  - The image starts as root so that `PUID`/`PGID` can work; the binary then drops privileges. `user:` in compose also works.
  - Volumes: `/config`, `/data`. The key comes from `/run/secrets/bitnews_key`.
- **compose:** `deploy/docker-compose.example.yml` with `PUID`/`PGID`/`UMASK`, the `/data` layout from TRaSH, and a commented gluetun variant.
- **systemd:** `deploy/bitnews.service` runs as the `bitnews` user with `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `NoNewPrivileges=true`, `ReadWritePaths=/var/lib/bitnews /data` and `LoadCredential=` for the key.

## Exit criteria

- `bitnews` starts in Docker (with and without `PUID`/`PGID`) and under systemd. It creates the DB and key, and requires setup.
- An admin can log in, create API keys and Categories, read and update settings, and see an SSE stream.
- CI is green: `go vet`, `staticcheck`, `govulncheck`, `-race` tests and fuzz smoke runs. `gofmt -s` and `go fix` are run locally (`make check` includes `fmt-check` and `fix-check`).

## Issues

### M1-01: Repository bootstrap and CI pipeline
- **Labels:** area:ci, ready-for-agent
- **Depends on:** —

**Goal:** Create a working Go module skeleton and a GitLab CI pipeline that enforces the quality gates from the architecture.

**Work**
- Keep the existing `go.mod` (`module gitlab.com/kyura4k/gitnews`, `go 1.27.2`). Import paths use `gitlab.com/kyura4k/gitnews/...`.
- Create the package directories from the architecture layout. Each gets a `doc.go` with a one-line package comment.
- Add `cmd/bitnews/main.go`, which prints its version. The version is injected via `-ldflags` (version, commit).
- Add a `Makefile` with targets `build`, `fmt` and `fmt-check` (`gofmt -s`), `fix` and `fix-check` (`go fix`), `test` (`go test -race ./...`), `lint` (`go vet` + `staticcheck`), `vuln` (`govulncheck`) and `fuzz-smoke` (runs each `Fuzz*` for 10 s). The `-check` targets only report and fail, and the others rewrite files.
- Add `.gitlab-ci.yml` with stages `lint` (one job: `make lint` and `make vuln`), `test` (one job: `make test` and `make fuzz-smoke`) and `build`. Formatting (`make fmt`, `make fix`) is done locally before pushing, so CI has no format job. `build` produces linux amd64/arm64 static binaries with `CGO_ENABLED=0`. Cache the Go module and build caches.
- Add `.gitignore` and a short `README.md` that points to `docs/`.

**Acceptance criteria**
- [ ] `make test lint vuln build` passes locally.
- [ ] The pipeline runs on MRs and on `main` and is green.
- [ ] The binaries are static (`file` reports statically linked) and `bitnews --version` prints the version and commit.

### M1-02: Bootstrap configuration and logging
- **Labels:** area:core, ready-for-agent
- **Depends on:** M1-01

**Goal:** Implement the bootstrap flags/env table from the M1 design and the structured logging, including secret redaction.

**Work**
- Parse the flags with env fallback; a flag wins over env. Validate them: paths must be absolute, the listen address must parse, and `UMASK` must be octal.
- Refuse to start when the key file lies inside `config-dir`.
- Configure the `slog` handler (text/JSON, level).
- Add a redaction helper for URLs that masks the query values of `apikey`, `api_key`, `key`, `token`, `password`, `pass` and `nzbkey` (case-insensitive).
- Use the `Secret` type (from M1-09) everywhere a secret is held. Here, add a lint-style test that no struct field named `*password*`, `*key*` or `*token*` in `internal/` has type `string` (allowlist exceptions).

**Acceptance criteria**
- [ ] Table-driven tests cover flag/env precedence and validation errors.
- [ ] A redaction test passes with URLs from real indexers (synthetic keys).
- [ ] `--help` documents every bootstrap option.

### M1-03: Privilege drop, umask and root warning
- **Labels:** area:deploy, ready-for-agent
- **Depends on:** M1-02

**Goal:** Follow the startup order from the M1 design. `PUID`/`PGID` drop root before any file or socket is opened.

**Work**
- Apply `UMASK` via `syscall.Umask`.
- If `os.Geteuid()==0` and `PUID`/`PGID` are set, call `syscall.Setgroups([]int{PGID})`, then `Setgid`, then `Setuid`. These calls apply to all threads on Linux since Go 1.16. Then verify that `Setuid(0)` fails and abort if it succeeds.
- If the uid is 0 with no `PUID`, log the warning: "running as root: a parser bug would run with root privileges".
- Treat `PUID`/`PGID` set while not root as an error, unless they already equal the current ids.
- Restrict the privilege drop to Linux with a build tag. On darwin, setting `PUID` is an error.

**Acceptance criteria**
- [ ] An integration test (Linux CI, in a container started as root) confirms that the process ends up running as `PUID`/`PGID` and cannot regain root.
- [ ] Files created afterwards respect `UMASK`.
- [ ] The root warning is logged exactly once at startup.

### M1-04: Core domain model and lifecycle state machine
- **Labels:** area:core, ready-for-agent
- **Depends on:** M1-01

**Goal:** Implement Download, Category, the Download Priority and state enums, the failure reasons and the transition table in `internal/core`, with no I/O dependencies.

**Work**
- Add the types from the "Domain model" section, along with ID generation (crypto/rand, base32 lowercase).
- Add `Transition(from, to State) error`, backed by one table that mirrors the architecture lifecycle diagram.
- Validate Category names (`[a-z0-9._-]{1,64}`).
- Define the repository interfaces in core: `DownloadRepo`, `CategoryRepo`, `SettingsRepo`, `UserRepo`, `SessionRepo`, `APIKeyRepo`.
- Add a typed `Settings` struct with defaults and `Validate()`. M1 fields: queue limits, auth options (disable auth, LAN bypass CIDRs, trusted proxies, allowed hosts), and the per-protocol `incomplete`/`complete` roots (TRaSH defaults).

**Acceptance criteria**
- [ ] Exhaustive tests check every state pair: legal pairs succeed and illegal ones fail.
- [ ] `go list -deps ./internal/core` shows no project-internal imports and no third-party imports.
- [ ] Each glossary term in `CONTEXT.md` maps to a named type or constant, and the naming matches.

### M1-05: In-process event bus
- **Labels:** area:core, ready-for-agent
- **Depends on:** M1-04

**Goal:** A non-blocking publish/subscribe bus with the delivery semantics from the M1 design.

**Work**
- Add typed events with `Subscribe(ctx) *Subscription`. A `Subscription` has `Events() <-chan Event`, `Close()` and `Err()`, where `Err()` returns `ErrResync` after the bus closed it for a missed lifecycle event.
- Use a buffered channel of 256 per subscriber. When it overflows, drop progress events. When a lifecycle event can't be delivered, close the subscriber with reason `resync`.
- Add a progress throttle helper that allows at most 1 event per second per Download ID and always emits the final value.

**Acceptance criteria**
- [ ] Tests, run under `-race`, show that a slow subscriber never blocks publishers.
- [ ] A test shows that lifecycle event overflow closes the subscriber with `resync`.
- [ ] A throttle test uses a fake clock.

### M1-06: Queue scheduling policy
- **Labels:** area:core, ready-for-agent
- **Depends on:** M1-04

**Goal:** Implement the pure `Select` function from the "Scheduling policy" section.

**Work**
- Add `Select(downloads, limit) (activate, deactivate)` with the ordering: force first; then Download Priority high → low; then `QueuePosition`, then `AddedAt`.
- Never activate paused Downloads. `force` ignores the limit.
- Add helpers to move a Download to the top or bottom of its Queue, or up and down one position. They renumber `QueuePosition` densely.

**Acceptance criteria**
- [ ] Table-driven tests cover limits 0/1/5, mixed priorities, paused items, several force items, and stable ordering.
- [ ] A property test confirms the active set never exceeds `limit + count(force)`.

### M1-07: SQLite store, migrations and file permissions
- **Labels:** area:store, ready-for-agent
- **Depends on:** M1-04

**Goal:** Open the DB safely and apply embedded migrations.

**Work**
- Use `modernc.org/sqlite` with the pragmas from the design, one writer connection and a read pool.
- Create the DB file with mode `0600` before it is first opened. If an existing file has broader permissions, warn.
- Add an embedded forward-only migrator with a `schema_migrations` table. Each migration runs in a transaction.
- Migration `0001` creates `downloads`, `categories`, `settings`, `users`, `sessions` and `api_keys`, with indexes on `downloads(state)`, `downloads(category_id)` and `sessions(token_hash)`.

**Acceptance criteria**
- [ ] A fresh DB migrates. Re-running is a no-op, and a failed migration rolls back.
- [ ] A test checks the file mode is `0600`.
- [ ] `CGO_ENABLED=0 go build` still works.

### M1-08: Repositories for downloads, categories and settings
- **Labels:** area:store, ready-for-agent
- **Depends on:** M1-07

**Goal:** Implement the core repository interfaces for Downloads, Categories and Settings.

**Work**
- `DownloadRepo` needs CRUD, list with filters (`state`, `category`, `kind`) and cursor paging ordered by `added_at, id`, plus a transition-checked `UpdateState`. `id` and `added_at` are immutable on `Update`, so paging stays stable. The default page size is 50 and the maximum is 200.
- `CategoryRepo` needs CRUD with unique names. Deleting a Category that Downloads use sets their `category_id` to NULL.
- `SettingsRepo` loads and saves the typed `Settings` as JSON per key. Validate before writing.

**Acceptance criteria**
- [ ] Repository tests run against a temp-dir DB.
- [ ] Paging is stable under concurrent inserts.
- [ ] Illegal state updates are rejected by core, not by SQL.

### M1-09: Secret encryption and key file
- **Labels:** area:store, ready-for-agent
- **Depends on:** M1-02, M1-07

**Goal:** Encrypt recoverable secrets (Server passwords from M2 onwards) using the key-file scheme from the design.

**Work**
- Load or generate the key file: 32 bytes, base64. A new file gets mode `0600`, and broader permissions trigger a warning.
- `Seal(key, plaintext, aad) (string, error)` and `Unseal(key, stored, aad) (string, error)` (not `Open`, which `store.Open` already uses for the database) use XChaCha20-Poly1305 with the `v1:` format, and the AAD is `table/column/rowID` (`AAD(table, column, rowID)` builds it). Wrong key, wrong AAD or tampering returns `ErrDecrypt`; a malformed or unknown-version string returns `ErrUnsupportedFormat`.
- Add a `Secret` type that redacts itself in `String`, `GoString` and `MarshalJSON`.
- When the key is wrong or missing for existing ciphertexts, fail with a clear error that names the affected Servers. Startup still works.

**Acceptance criteria**
- [ ] Round-trip tests pass. A tampered ciphertext, a wrong AAD and a wrong key all fail.
- [ ] A fuzz test on `Unseal` parsing never panics.
- [ ] A `Secret` printed via `slog`, `fmt` or `json` shows `[redacted]`.

### M1-10: Users, password hashing and first-run setup
- **Labels:** area:auth, ready-for-agent
- **Depends on:** M1-08

**Goal:** Create the single admin user safely on first run.

**Work**
- Hash passwords with argon2id using the parameters from the design, stored as PHC strings, and verify in constant time.
- Add a setup gate: while there is no user, every route except setup, health and static returns `503 setup_required`.
- Generate a one-time setup token at startup, log it at `warn` level and keep it in memory only. `POST /api/v1/setup` requires it.
- `BITNEWS_ADMIN_PASSWORD_FILE` creates the admin at startup if none exists. The file is read once and its content is never logged.
- Enforce a minimum password length of 12.

**Acceptance criteria**
- [ ] Setup without a token or with a wrong token fails, and a second setup after success fails.
- [ ] The bootstrap file path works, and setup is closed afterwards.
- [ ] A test confirms no password appears in the logs.

### M1-11: Sessions, CSRF, rate limiting and UI auth options
- **Labels:** area:auth, ready-for-agent
- **Depends on:** M1-10

**Goal:** Implement cookie sessions plus the protections from the "Authentication" design section, including disable auth and LAN bypass.

**Work**
- Generate session tokens (256-bit) and store their SHA-256 hash, with idle and absolute expiry and the cookie flags from the design.
- Give each session a CSRF synchronizer token. Middleware checks `X-CSRF-Token` and `Origin` on cookie-authenticated, state-changing requests.
- Add an in-memory login rate limiter per IP and per username with exponential backoff.
- Implement the settings `disable_auth` and `lan_bypass_cidrs`, both for UI routes only. Resolve the client IP via `trusted_proxies` and `X-Forwarded-For`. Enabling either setting requires a non-empty `allowed_hosts`; a request whose `Host` isn't listed gets `421`.
- Report the auth mode in `/api/v1/auth/session` so the UI can show a warning banner.
- Never apply the bypass to `/api/v1/*` key-auth, adapter routes or setup.

**Acceptance criteria**
- [ ] Tests cover: missing or wrong CSRF → 403; foreign `Origin` → 403; a spoofed `X-Forwarded-For` from an untrusted peer is ignored; a bypass CIDR match only on UI routes; the `allowed_hosts` requirement.
- [ ] Rate-limit tests use a fake clock.

### M1-12: API keys with scopes
- **Labels:** area:auth, ready-for-agent
- **Depends on:** M1-11

**Goal:** Named API keys with `read`/`write` scope for scripts and the API, managed only by a logged-in admin.

**Work**
- Use the format `bn_` + base32(32 random bytes) and store a SHA-256 hash. Show the plaintext once.
- Middleware accepts `Authorization: Bearer <key>` or `X-Api-Key`, and updates `last_used_at` at most once a minute.
- Enforce scope: `read` gets GET, `write` gets everything.
- Add the session-only CRUD routes from the design.
- Keep the token mechanism reusable for M4/M5 adapter credentials (as a separate kind, valid only on adapter routes).

**Acceptance criteria**
- [ ] Tests cover create → use → revoke, scope enforcement, and API keys being unable to manage API keys.
- [ ] Hash lookups are constant-time and no key is logged.

### M1-13: HTTP server and own API v1
- **Labels:** area:api, ready-for-agent
- **Depends on:** M1-06, M1-08, M1-12

**Goal:** Serve the M1 routes from the design with a hardened `net/http` server.

**Work**
- Configure the server timeouts, header limit, per-route body caps, security headers, request IDs and access logs (with redacted URLs).
- Add the JSON error shape and an input-validation helper.
- Implement the handlers for health, version, downloads (list/detail/pause/resume/patch/delete), categories, settings and auth.
- Write an application service layer between the handlers and core. The M6 UI will reuse it.
- `DELETE /downloads/{id}` deletes data only when `delete_data=true`. The engines hook in later; for now the row is removed and a `Removed` event is emitted.

**Acceptance criteria**
- [ ] Handler tests via `httptest` cover every route, including auth and scope failures.
- [ ] Graceful shutdown finishes in-flight requests.
- [ ] An OpenAPI-ish route table is documented in `docs/api-v1.md`.

### M1-14: SSE event stream
- **Labels:** area:api, ready-for-agent
- **Depends on:** M1-05, M1-13

**Goal:** `GET /api/v1/events` streams bus events to authenticated clients.

**Work**
- Use `text/event-stream`. Send a `hello` event with the instance ID and a `: ping` every 15 s.
- Map the events to JSON payloads.
- On a `resync` close, send `event: resync` and end the stream.
- Disable the write timeout for this route only. Clean up the subscription when the client disconnects.

**Acceptance criteria**
- [ ] A test client receives the events published after connecting.
- [ ] Ping arrives on schedule (fake clock), and a disconnect frees the subscriber with no goroutine leak (goleak or a manual check).

### M1-15: Docker image, compose example and systemd unit
- **Labels:** area:deploy, ready-for-agent
- **Depends on:** M1-03, M1-13

**Goal:** Packaging that matches the deployment design, so bitnews runs both with and without Docker.

**Work**
- Write a multi-stage `Dockerfile` ending in distroless static. It starts as root so `PUID`/`PGID` work. Add a healthcheck via `bitnews healthcheck`, a subcommand that hits `/api/v1/health`.
- Write `deploy/docker-compose.example.yml` with the TRaSH `/data` layout, `PUID`/`PGID`/`UMASK`, the key as a secret, and a commented gluetun variant.
- Write `deploy/bitnews.service` with the hardening from the design and `LoadCredential=` for the key.
- Add CI image builds for amd64/arm64 to the GitLab container registry.
- Write `docs/deploy.md` covering both paths.

**Acceptance criteria**
- [ ] `docker compose up` with the example reaches the setup screen and runs as `PUID`/`PGID`.
- [ ] Under systemd, the service passes `systemd-analyze security` with a score below 3.0.
