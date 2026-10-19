# M2 — Usenet fetch

## Goal

By the end of M2, a Download added from an NZB, whether uploaded or fetched by URL, gets all its Articles fetched over strict TLS from the configured Servers. Server Priority fallback and retention apply. Articles are yEnc-decoded and written directly into pre-allocated files, so the Download reaches **Fetched**, and it resumes correctly after a restart. Post-processing comes in M3, so for now Fetched is where a Download stops.

## Scope

**In:** `fsutil`, NZB parser, adding Downloads from NZBs, Servers (model, encrypted credentials, API), NNTP client, fake NNTP server, connection pool, yEnc decoder, Article scheduler, direct file writer, Usenet Queue runner (pause, progress, speed limit, disk guard), resume after restart.

**Out:** PAR2 and deferred recovery volumes (M3), the SAB adapter (M4), interface binding (M5-07, which retrofits the dialer).

## Design

### `internal/fsutil`

All hostile names (NZB subjects, yEnc `name=`, and later archive entries and torrent file names) go through this package.

- `SafeName(s) (string, error)`:
  - Normalizes Unicode (NFC) and strips control characters and path separators (`/`, `\`).
  - Rejects `.`, `..` and empty results.
  - Caps the name at 255 bytes, preserving the extension.
  - On Windows-reserved names, prefixes `_`, because shares may be SMB.
- `SafeJoin(root, rel) (string, error)` cleans the path and ensures the result stays under `root`, after resolving symlinks in the existing parent dirs.
- `SameFilesystem(a, b) (bool, error)` compares `stat.Dev`.
- `MoveDir(src, dst) error`:
  - Uses `rename` when both are on the same filesystem.
  - Otherwise it copies, fsyncs, then deletes, and returns a `CrossDevice` marker so the caller can emit a warning.
- `Unique(path)` appends `.1`, `.2`, … when a name collides.

### NZB parsing (`internal/usenet/nzb`)

- Uses `encoding/xml` with a streaming decoder. That package does not expand external entities, which avoids XXE. The input is capped at 64 MiB, 10 000 files and 1 000 000 segments.
- Model: `NZB{Meta{Name, Password, Category}, Files[]{Subject, Poster, Date, Groups[], Segments[]{Number, Bytes, MessageID}}}`.
- Normalization:
  - Sort segments by number.
  - Drop duplicate numbers, keeping the first.
  - Trim `<>` from message IDs and validate them (printable ASCII, max 250 chars).
- The file name guess comes from the quoted subject name if present. It is only a provisional name; the authoritative name comes from the yEnc `name=` (see the file writer), run through `fsutil.SafeName`.
- PAR2 files are classified by name (`.par2`, `.volNN+MM.par2`) so that M3 can defer them.

### Adding a Download from an NZB

- `POST /api/v1/downloads` accepts:
  - `multipart/form-data` with an `nzb` file, or
  - JSON `{"url": "...", "category": "...", "priority": "...", "name": "...", "password": "..."}`.
- URL fetching:
  - Runs immediately when the Download is added, with a 30 s timeout and a 64 MiB cap.
  - Allows `http` and `https`, because Prowlarr on the LAN is often `http://prowlarr:9696`.
  - Follows at most 5 redirects.
  - The URL itself is never persisted. Only the redacted form is logged.
- The NZB is stored as `incomplete/<download>/<name>.nzb` (mode `0600`), so resume and history detail can use it.
- The Category comes from the request first, then the NZB meta, then none. Unknown Categories are rejected with `400`; they are not auto-created here.
- The password comes from the request first, then the NZB meta, then the `{{password}}` suffix of the NZB file name. It is stored encrypted (M1-09).

### Servers

- Fields:
  - `ID`, `Name`, `Host`, `Port` (default 563)
  - `Username`, `Password` (encrypted)
  - `MaxConnections` (1–100), `RetentionDays` (0 = unlimited), `Priority` (Server Priority, int ≥ 0)
  - `Enabled`
- TLS is always on.
  - `tls.Config{MinVersion: TLS12, ServerName: Host}` with system roots.
  - There is no option to skip verification and no plaintext. The data model has no field for either.
- API:
  - `GET/POST /api/v1/servers`, `PATCH/DELETE /api/v1/servers/{id}`. Responses never include the password.
  - `POST /api/v1/servers/{id}/test` connects, authenticates and runs `DATE`, then reports the latency or the exact error class.

- Implementation notes (M2-03, M2-04):
  - Passwords are stored as `TEXT`, not `BLOB`: `Seal` returns the text form `v1:<base64>` and `STRICT` tables reject text in a `BLOB` column. A Server with no password stores `''`; an NZB Download without one stores `NULL` in `download_sources.password`.
  - A password that cannot be decrypted (wrong or lost key) does not fail `List`: the Server comes back with `PasswordUnavailable` (API: `password_unavailable: true`, `has_password: true`) so the daemon starts and the UI can ask for it again. A PATCH without a password leaves the stored value alone, a PATCH with one repairs it, and testing such a Server is `409 password_unavailable`.
  - New event `servers_changed` (no payload) is published after every create, update and delete, so the runner can rebuild its pools.
  - The connection test classes are `ok`, `dns`, `tcp`, `tls`, `auth` and `protocol`, each with a `detail`. A certificate problem is `tls` with `certificate_untrusted`, `certificate_hostname`, `certificate_expired` or `certificate_invalid`; a wrong password is `auth`/`rejected`; a plain-text port is `tls`/`not_tls`. See `docs/api-v1.md`.
  - Adding is two-phase. `AddService.Stage` receives the NZB (an upload may send it before its form fields), writes the raw bytes to `<incomplete>/<download id>/` while parsing, and leaves nothing behind on failure. `Add` then validates the request, names the file and stores the Download. The `.nzb` file is named from the display name with `fsutil.SafeName`.
  - The Download is created `Added` (with its Queue position at the end of the NZB Queue), its `download_sources` row is stored, and only then does it move to `Queued`; `DownloadAdded` is published after that. The runner only handles `Queued` and `Fetching`, so it never sees a Download without its source. On a failure the row is deleted (the source cascades) and the directory is removed.
  - URL targets on loopback, private and link-local addresses are allowed on purpose (Prowlarr is usually on the LAN); only http and https are spoken, also across redirects (at most 5). The client sends no cookies and no `Authorization`, and does not use `HTTP_PROXY`, like the NNTP connections.
  - The NZB's own category is matched to a Category by name (case-insensitive); one that matches none is ignored instead of refused. A category in the request must exist (`400 unknown_category`).
  - `DownloadRepo.UpdateProgress` writes only `done_bytes`, `total_bytes` (when positive) and `updated_at`, so the runner's progress writes cannot overwrite a concurrent edit of the Priority, Category or Paused flag.

### NNTP client (`internal/usenet/nntp`)

- Connects with TLS, reads the greeting (`200`/`201`), then authenticates with `AUTHINFO USER`/`PASS`:
  - `281` means OK.
  - `381` means a password is required.
  - `481`/`482` means auth failed, which is terminal for that Server until its config changes.
- `BODY <message-id>`:
  - `222` is followed by a dot-terminated multiline body. Dot-unstuffing is applied, and the body is streamed to a callback rather than buffered whole.
  - `430` means the Article is absent on this Server.
  - `411`/`412`/`420`/`423` are treated as absent.
  - `400`/`502`/`5xx` and I/O errors are connection errors.
- Limits: 8 KiB max line length, 16 MiB max body. Per-command deadlines are 60 s idle-read and 30 s connect.
- `DATE` is used for health checks. `QUIT` is sent on close.

### Connection pool

- One pool per Server, up to `MaxConnections`, connecting lazily and closing after 60 s idle.
- Reconnects use exponential backoff from 1 s up to 60 s.
- An auth failure disables the Server at runtime and emits a `ServerError` event.
- A pool exposes a `Body(ctx, messageID, sink)` primitive that is used only by the Article scheduler.
- The pool dials one connection at a time until the first successful dial, and again after any failure streak or a new factory. A wrong password is therefore tried once and not once per connection, which avoids provider lockouts.

- Implementation notes (M2-11, package `internal/usenet/runner`):
  - The runner reacts to bus events with a 10 s poll as a fallback, applies `core.Select` with the Usenet limit, and builds one shared pool set per Server. A change to the Server set restarts the scheduler for the unresolved Articles.
  - If every Server rejects the login, the Download goes back to Queued and backs off for 30 s instead of being marked Fetched with every Article missing. Zero enabled Servers leaves Downloads Queued.
  - `.volNN+MM.par2` files are never requested until M3-07 un-defers them. A file whose Articles are all missing is never created and is reported in `FetchDetail.MissingFiles`; the Download still reaches Fetched so that Verify decides.
  - The disk guard pauses the whole queue with reason `disk_full` below `min_free` (a runner option defaulting to 5 GiB, because `core.Settings` has no such field yet) and resumes by itself.
  - Resume works behind the `ProgressStore` interface (writer snapshot plus a record of every resolved Article, versioned `GNRP` v1 and CRC-protected). Without a store, resume state is kept in memory, and a daemon restart re-fetches everything.
  - M2-12 (migration `0003_runner_state.sql`, `internal/store/repo_usenet_state.go`): `usenet_progress(download_id, blob, updated_at)` holds the opaque blob (capped at 16 MiB by `store.UsenetProgressRepo`, `Load` of a missing row wraps `core.ErrNotFound`), `usenet_reports(download_id, missing_articles, missing_bytes, damaged_articles, damaged_bytes, missing_files, updated_at)` holds the `FetchDetail`, and `usenet_undeferred(download_id, file_index, fetched)` holds the recovery volumes M3-07 asked for. All three cascade on Download delete. The spec's per-file bitmaps are replaced by the runner's own blob, which also records the damaged state per Article. The composition root (`internal/daemon`) wires them.
  - A Download's working directory is the parent of its stored NZB path (created at add time), cached by the daemon until the Download is removed, so changing the incomplete root in the settings affects only Downloads added afterwards; the current settings decide only when no source row exists.
  - `Runner.Hold(ctx, id)` and `Release(id)` stop a Download's fetch and keep it from restarting while its data is deleted (DELETE with `delete_data=true`); the Removed event also ends the hold.
  - Wiring must call `Recover` before `Run`, and must use `UpdateProgress` (a narrow repo method) for `DoneBytes`, so that a concurrent API edit of paused or priority is never overwritten.

### Article scheduler

- **Work unit:** an Article, identified by Download, file and segment number.
- **State per Article:** `tried` (set of Server IDs), `attempts` per Server, and status (`pending` | `done` | `missing`).
- **Dispatch:**
  - Each connection worker belongs to one Server with priority *P*.
  - A worker takes the next pending Article in file/segment order that it hasn't tried yet and whose current level is ≤ *P*.
  - An Article starts at the lowest configured priority level.
- **Fallback:**
  - `430`/absent from a Server marks it as tried.
  - Once every enabled Server at the Article's current level has tried it, the Article moves up to the next level.
  - When no level is left, it becomes a **Missing Article**.
- **Retention:** a Server whose `RetentionDays` is shorter than the Article's age (from the NZB file `date`) counts as already tried.
- **Transient errors** (timeouts, connection resets) are retried on the same Server up to 3 times, then the Server counts as tried.
- **CRC mismatch** after decoding is treated like absent for that Server. If no Server gives a valid copy, the best data received so far is written and the Article is flagged damaged, so that Repair can fix it later.
- Workers are not tied to a single Download. With a Usenet Queue limit of 1 this is moot, but the scheduler must still support more than one active Download.
- Implementation notes (M2-09, package `internal/usenet/scheduler`):
  - A part is damaged only when a CRC is announced and does not match; a part with no announced CRC counts as good.
  - The scheduler has no pause. The runner cancels the context and creates a new scheduler for the Articles not yet done.
  - `Stats().Servers[].Bytes` counts wire bytes. The runner sums decoded `Part.Data` lengths for progress.
  - A scheduler handles at most 64 Servers, and an auth failure marks a Server dead for the run (`ServerDown` fires once).

### yEnc decoding (`internal/usenet/yenc`)

- Parses `=ybegin` (`part`, `total`, `line`, `size`, `name`), `=ypart` (`begin`, `end`) and `=yend` (`size`, `part`, `pcrc32`, `crc32`).
- Decodes escapes (`=x` → `x-64-42`) and ignores CR/LF. Dot-unstuffing happens only in the NNTP client; the decoder takes lines literally, so a line starting with `..` decodes as two bytes. Unknown `key=value` header tokens are ignored.
- Computes CRC32 (IEEE) over the decoded part and compares it with `pcrc32` (or `crc32` for single-part files).
- Rejects these as malformed parts: `end < begin`, `end - begin + 1` different from the decoded length, or `size` larger than 1 TiB.
- Streaming API: `Decoder.Write(encodedLine)` and `Decoder.Finish() (Part, error)`.

### Direct file writer

- On the first decoded part of a file:
  - Take the name from yEnc `name=`, run it through `SafeName`, and make it unique within the Download dir.
  - Create the file under `incomplete/<download>/` and pre-allocate it with `ftruncate(size)`, falling back to `fallocate` where it is supported.
- Every part is written with `WriteAt(data, begin-1)`.
- Per-file bookkeeping records `Done`, `Damaged` (CRC mismatch) and `Lost` (declared missing with `MarkMissing`) ranges as half-open, zero-based ranges. `Missing` is the size minus Done and Damaged, and `Pending` is Missing minus Lost, so the runner treats a file as Fetched when `Pending` is empty. A damaged copy never overwrites good bytes, and a later good copy heals the range. M3 uses these.
- A file whose Articles are all missing is never created, because the size comes from the first yEnc part. The runner (M2-11) must handle that case from the NZB data.
- `Snapshot()` and `Restore()` serialize the per-file state compactly for resume (M2-12). `Restore` forgets missing files and resets files of the wrong size.
- `fsync` runs when a file completes and before a state is persisted.

### Usenet Queue runner

- The scheduling policy (M1-06) decides which Downloads are active. The runner starts and stops their Article work.
- Pause stops dispatching for that Download; in-flight Articles finish. Global pause stops the whole Queue.
- Progress: `DoneBytes` (decoded bytes written) and the rate over a 5 s window, emitted through the throttled bus.
- Speed limit: `golang.org/x/time/rate` token buckets, global and per protocol, applied to bytes read from the network.
- Disk guard: before activating a Download and every 30 s, check `free(incomplete root) ≥ remaining + min_free` (setting, default 5 GiB). If there is too little space, pause the Queue with reason `disk_full` and emit an event.
- When every Article is either `done` or `missing`, the Download is **Fetched**. The Missing Article and damaged counts are kept for M3.

### Resume after restart

- Per-Download Article state is persisted as a compact bitmap per file (`done`, `missing`, `damaged`) in a new table `usenet_progress`. It is written every 10 s and on shutdown.
- On startup, the Downloads that were `Fetching` go back to `Queued` with their bitmaps intact. The files are reopened and `done` Articles are skipped.
- If a file on disk is missing or shorter than expected, its bitmap is reset.

## Exit criteria

- Against the fake NNTP server and a real provider (manual check), an NZB reaches Fetched with byte-exact files.
- Server Priority fallback is shown working, as are retention skips and Missing Articles.
- Killing the daemon mid-download and restarting it resumes without re-fetching completed Articles.
- The NZB, NNTP response and yEnc parsers have fuzz targets in CI.

## Issues

### M2-01: fsutil — safe names, safe join, same-filesystem check, directory move
- **Labels:** area:core, ready-for-agent
- **Depends on:** M1-01

**Goal:** Implement `internal/fsutil` as specified. Every hostile file name in the project goes through it.

**Work**
- `SafeName`, `SafeJoin` (including the symlink check on existing parents), `SameFilesystem`, `MoveDir` (rename, or copy+fsync+delete with a `CrossDevice` marker) and `Unique`.
- Add fuzz targets for `SafeName` and `SafeJoin`. The invariant: a successful result never escapes `root` and never contains a separator or `..`.

**Acceptance criteria**
- [ ] Table tests cover `..`, absolute paths, separators, NUL/control characters, overlong names, Unicode normalization, Windows-reserved names and symlinked parents.
- [ ] Fuzz targets run in CI.
- [ ] The cross-device path is tested using two tmpfs mounts in Linux CI, or skipped when that isn't possible.

### M2-02: NZB parser
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M1-01

**Goal:** Parse NZBs safely into the normalized model from the design.

**Work**
- Streaming `encoding/xml` decoding with the size, file and segment caps.
- Parse meta (name, password, category), files, groups and segments.
- Normalize: sort, dedupe, validate message IDs.
- Guess provisional names from subjects and classify PAR2 files (index vs `.volNN+MM`).

**Acceptance criteria**
- [ ] Tests use real-world NZB shapes: newznab with meta, without meta, obfuscated subjects, duplicate segments, and missing `bytes`.
- [ ] A billion-laughs or external-entity payload does not expand.
- [ ] A fuzz target runs in CI.

### M2-03: Add Download from NZB (upload and URL)
- **Labels:** area:api, area:usenet, ready-for-agent
- **Depends on:** M1-13, M2-01, M2-02

**Goal:** Implement `POST /api/v1/downloads` for NZB Sources, following the "Adding a Download" design.

**Work**
- Accept a multipart upload or a JSON URL.
- Fetch URLs immediately with the timeout, size cap and redirect limit. Never persist the URL, and redact it in logs.
- Store the NZB in the Download dir with mode `0600`.
- Resolve the Category and password in the specified order, and store the password encrypted.
- Create the Download in `Queued` and emit `DownloadAdded`.

**Acceptance criteria**
- [ ] API tests cover both input forms, an unknown Category (400), an oversized NZB (413), a URL timeout, and redaction of `apikey` in the logs.

### M2-04: Usenet Servers — model, encrypted credentials, API and connection test
- **Labels:** area:usenet, area:api, ready-for-agent
- **Depends on:** M1-09, M1-13

**Goal:** Manage Servers as designed, with no way to weaken TLS.

**Work**
- Migration for a `servers` table with the password encrypted (AAD `servers/password/<id>`).
- Add the CRUD routes. Responses never return the password; a PATCH without a password keeps the existing one.
- `POST /api/v1/servers/{id}/test` connects, authenticates and runs `DATE` (it uses the M2-05 client, so stub it until that lands).
- Validate fields: port range, connection limit, priority ≥ 0, and a host that is not empty.

**Acceptance criteria**
- [ ] The password never appears in responses or logs.
- [ ] The data model has no skip-verify or plaintext field, and a test asserts the JSON schema.
- [ ] The test endpoint reports distinct error classes: DNS, TCP, TLS (including certificate errors), auth, and protocol.

### M2-05: NNTP client
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M1-01

**Goal:** A minimal, strict NNTP client for fetching bodies over TLS.

**Work**
- Strict TLS dialing (TLS ≥1.2, system roots, `ServerName`) with a pluggable dialer, so M5-07 can add interface binding.
- Greeting, `AUTHINFO USER/PASS`, `BODY`, `DATE`, `QUIT`.
- Stream multiline responses with dot-unstuffing to a sink.
- Map response codes to typed errors as in the design: absent, auth, connection, protocol.
- Enforce line and body limits and per-command deadlines.

**Acceptance criteria**
- [ ] Unit tests run against an in-memory server, covering every code path.
- [ ] A fuzz target parses server responses and never panics, with bounded memory.
- [ ] A TLS certificate error can never be bypassed. A test with a self-signed server fails unless its CA is injected through a test-only hook.

### M2-06: Fake NNTP server test harness
- **Labels:** area:usenet, area:ci, ready-for-agent
- **Depends on:** M2-05

**Goal:** An in-process, scriptable NNTP server for integration tests.

**Work**
- Serve TLS with a test CA for client-level tests. The CA can be injected only inside package `nntp`'s own tests, because `rootCAs` is deliberately unexported and there is no public way to weaken TLS.
- Tests above the client (M2-07 pool, M2-09 scheduler, M2-11/12 runner) must not need TLS. M2-07 defines a small connection-factory interface (dial and return a `Conn`-like value with `Body`, `Date`, `Close`, `Dead`), and the harness provides an in-memory implementation of it that serves the same scripted articles.
- Configure articles per server, `430` for chosen message IDs, latency, mid-body disconnects, auth failures, connection limits (`502` when exceeded) and corrupted bodies (bad CRC).
- Generate fixture NZBs plus their articles from arbitrary payload files, using a yEnc encoder in test code.

**Acceptance criteria**
- [ ] The harness is used by the M2-07, M2-09, M2-11 and M2-12 tests.
- [ ] Usage is documented in the package doc.

### M2-07: Per-Server connection pool
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M2-05, M2-06

**Goal:** Pool NNTP connections per Server as designed.

**Work**
- Lazy connect up to `MaxConnections`, an idle timeout, and reconnect backoff.
- Disable the Server at runtime when auth fails, and emit a `ServerError` event.
- Expose the `Body(ctx, messageID, sink)` primitive, and dial one connection at a time until the first success.
- Apply config changes, such as connection count or credentials, without a restart.

**Acceptance criteria**
- [ ] Tests confirm the connection limit is never exceeded, dead connections are replaced, auth failure stops all attempts, and there are no goroutine leaks.

### M2-08: yEnc decoder
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M1-01

**Goal:** A streaming yEnc decoder with CRC32 verification.

**Work**
- Parse the headers and trailers (`=ybegin`, `=ypart`, `=yend`) and decode escapes. The input lines are already dot-unstuffed by the NNTP client (M2-05).
- Verify CRC32 per part, and for whole single-part files.
- Validate the ranges and sizes listed in the design.

**Acceptance criteria**
- [ ] Round-trip tests run against the test encoder from M2-06.
- [ ] Known-answer tests use real-world-style multipart posts.
- [ ] A fuzz target runs in CI with bounded memory.
- [ ] A benchmark is included, with a target of at least 500 MB/s on amd64.

### M2-09: Article scheduler with Server Priority fallback
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M2-07, M2-08

**Goal:** Dispatch Articles to connection workers following the "Article scheduler" design.

**Work**
- Track per-Article state (`tried`, `attempts`, status) and dispatch by level and order.
- Move to the next level after `430`/absent on all Servers at the current level, and mark an Article Missing when no level is left.
- Treat Servers whose retention is shorter than the Article's age as tried.
- Retry transient errors up to 3 times per Server. A CRC mismatch counts as absent, with the best data kept and flagged damaged.

**Acceptance criteria**
- [ ] Integration tests with the fake server cover these scenarios:
  - the main Server has everything
  - the main Server is missing 5% and the fill Server has them
  - an Article is absent everywhere, so it is Missing
  - retention skip
  - a flaky connection
  - a CRC-corrupt main Server with a clean fill Server
- [ ] A test shows the fill Server is never asked for Articles the main Server delivered.

### M2-10: Direct file writer
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M2-01, M2-08

**Goal:** Write decoded parts directly into pre-allocated files with range bookkeeping.

**Work**
- On the first part, name the file from yEnc `name=` via `SafeName`/`Unique` and pre-allocate it.
- Write each part at its offset with `WriteAt`, and track written, damaged and missing ranges per file.
- `fsync` when a file completes and before state is persisted.

**Acceptance criteria**
- [ ] Out-of-order parts produce byte-exact files.
- [ ] A hostile yEnc name (`../../etc/passwd`, an absolute path, NUL) is sanitized and stays inside the Download dir.
- [ ] Range bookkeeping is exposed for M3.

### M2-11: Usenet Queue runner
- **Labels:** area:usenet, area:core, ready-for-agent
- **Depends on:** M1-05, M1-06, M2-03, M2-09, M2-10

**Goal:** Run the Usenet Queue end to end: activate per the policy, fetch, emit progress, and reach Fetched.

**Work**
- Wire the scheduling policy to the Article scheduler, so Downloads are activated and deactivated when settings or priorities change.
- Pause and resume per Download and globally.
- Throttled progress events (bytes, rate).
- Token-bucket speed limits, global and Usenet-specific, changeable at runtime.
- The disk guard with the `min_free` setting.
- Transition to **Fetched** with Missing and damaged counts. Removing a Download stops its work and deletes its data when requested.

**Acceptance criteria**
- [ ] An integration test goes from adding an NZB to Fetched, with the expected events in order.
- [ ] A speed-limit test passes within ±10%.
- [ ] The disk guard pauses the Queue when there is too little space (simulated free-space provider).

### M2-12: Resume Usenet Downloads after restart
- **Labels:** area:usenet, area:store, ready-for-agent
- **Depends on:** M2-11

**Goal:** Persist Article progress so a restart resumes without re-fetching.

**Work**
- Migration for a `usenet_progress` table (per-file bitmaps: done, missing, damaged).
- Flush every 10 s and on graceful shutdown.
- On startup, requeue `Fetching` Downloads, reopen their files and skip `done` Articles. If a file is missing or truncated, reset its bitmap.

**Acceptance criteria**
- [ ] An integration test kills the runner mid-download (cancelling the context without a graceful flush, after one periodic flush), restarts it, and checks that fewer than 10 s of Articles are re-fetched and the final files are byte-exact.
