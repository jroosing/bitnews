# M4 — SABnzbd adapter

## Goal

By the end of M4, Sonarr, Radarr and Prowlarr use bitnews as a **SABnzbd** download client with no changes on their side. Releases are added, tracked through the queue and history, and imported once **Ready**; **Failed** releases get blocklisted. This is the first point where bitnews is usable day to day ([ADR-0003](../adr/0003-emulate-qbittorrent-and-sabnzbd-apis.md)).

## Scope

**In:** recording real media-manager traffic as fixtures, per-media-manager adapter credentials, the SAB API subset media managers call, the state mapping, contract tests, and an end-to-end check with real containers.

**Out:** SAB features media managers don't use (RSS, scheduling, server stats beyond what is needed, the SAB web UI) and the qBittorrent adapter (M5).

## Design

### Principle: the fixtures are the spec

The SAB API is only loosely documented, and media managers depend on specific fields and status strings. M4-01 records real traffic between current Sonarr, Radarr and Prowlarr releases and a real SABnzbd. The request/response pairs from that recording are the authoritative specification. The lists below are the expected starting point, and **the fixtures override them**.

### Routing and auth

- Path: `/sabnzbd/api`, since media managers prepend their configured URL base (`sabnzbd`) to `/api`. `/api` at the root is *not* used, because the own API lives under `/api/v1`.
- Requests use query params or a form body. With `output=json`, responses are JSON.
- Auth uses the `apikey` param, which must be a **SAB adapter credential**:
  - One credential per media manager, so "Sonarr", "Radarr" and "Prowlarr" each get a key and can be revoked separately.
  - Generated and hashed using the M1-12 token mechanism, with kind `adapter_sab`.
  - Valid only on `/sabnzbd/api`.
- A wrong or missing key returns SAB's error shape (`{"status": false, "error": "API Key Incorrect"}` or similar; check against the fixtures).
- Auth is never bypassed. The LAN bypass and disable-auth settings don't apply here.

### Modes (expected subset)

| `mode` | Purpose | Notes |
|---|---|---|
| `version` | Media managers check the minimum version | Report a SAB-compatible version string (e.g. `4.x`) that satisfies current media managers. Pick the value from the fixtures. |
| `get_config` | Categories, `misc.complete_dir`, sorting flags | Return the Categories with their `dir` relative to the Usenet complete root, plus a `*` default. Sorting flags are all off. |
| `get_cats` | Category names | |
| `fullstatus` / `status` | Health or disk checks, if the fixtures show them | Return a minimal valid shape. |
| `addfile` | Upload an NZB (multipart field `name` or `nzbfile`) with `cat`, `priority`, `nzbname`, optional `password` | Maps to the M2-03 service. Returns `{"status": true, "nzo_ids": ["SABnzbd_nzo_<id>"]}`. |
| `addurl` | NZB by URL (`name` = URL) with the same params | The URL is fetched immediately and redacted (M2-03). |
| `queue` | List not-yet-Ready Downloads with `start`/`limit`/`cat` filters | See the mapping below. |
| `queue&name=delete&value=<ids>&del_files=1` | Remove from the queue | The incomplete data is always deleted; `del_files` is irrelevant here. |
| `queue&name=priority` / `switch` | Change priority or order, if the fixtures show it | Maps to Download Priority or queue position. |
| `history` | Ready, Failed and post-processing Downloads, with `start`/`limit`/`category` | See the mapping below. |
| `history&name=delete&value=<ids>&del_files=1` | Remove from history | `del_files=1` deletes the output dir; otherwise it is kept. |
| `retry` | Retry a failed Download, if the fixtures show it | v1 answers "not supported" in the exact way the fixtures show is accepted. |

Any unknown `mode` returns SAB's "not implemented" error shape and is logged at debug level.

### IDs and mapping

- `nzo_id` is `SABnzbd_nzo_` + the Download ID. Unknown IDs are ignored the way SAB ignores them.
- Download Priority maps to SAB priority as follows. SAB's `-100` means "default" and maps to the Category's default, which is `normal`.

  | Download Priority | SAB priority |
  |---|---|
  | `force` | `2` |
  | `high` | `1` |
  | `normal` | `0` |
  | `low` | `-1` |

- **Category:** SAB `cat` is a Category name, and `*` or empty means none. Unknown Categories are created automatically **only** through this adapter, because SAB does the same and Sonarr expects it. They get the default dir.

### Status mapping

| bitnews | Listed in | SAB status (expected; the fixtures are the truth) |
|---|---|---|
| Queued | queue | `Queued` |
| Queued + paused | queue | `Paused` |
| Fetching | queue | `Downloading` |
| Fetched, PostProcessing `verifying` | history | `Verifying` |
| PostProcessing `repairing` | history | `Repairing` |
| PostProcessing `unpacking` | history | `Extracting` |
| PostProcessing `cleaning`/`moving` | history | `Moving` |
| Ready | history | `Completed`, with `storage` = `OutputPath` |
| Failed | history | `Failed`, with `fail_message` = a human-readable form of the failure reason (redacted) |

A deferred-volume fetch (M3-07) moves the Download back to Fetching. It then appears in **queue** as `Downloading` again, matching how SAB handles extra par2 downloads; check this in the fixtures.

**Queue slot fields:** `nzo_id`, `filename`, `cat`, `priority`, `status`, `mb`, `mbleft`, `percentage`, `timeleft`, `size`, `sizeleft`, plus queue-level `paused`, `speed`, `kbpersec`, `noofslots`. The exact set comes from the fixtures.

**History slot fields:** `nzo_id`, `name`, `category`, `status`, `fail_message`, `storage`, `bytes`, `download_time`, `completed`. The exact set comes from the fixtures.

### Paths and media managers

`storage` and the `complete_dir`/Category dirs report bitnews's own absolute paths. If a media manager's container mounts `/data` at the same path (the TRaSH layout), no remote path mapping is needed. The adapter does no path translation in v1.

## Exit criteria

- The contract tests replay every recorded request and assert that the responses match semantically: the field presence and types media managers parse, and the status strings.
- In a docker-compose environment with Sonarr and Radarr pointed at bitnews: grab → queue → Ready → import works using hardlinks (the inode count of the imported file is 2), and a forced failure → Failed → the release is blocklisted and another one is grabbed.

## Implementation notes (M4-02 to M4-06)

**Read this first: nothing here was checked against recorded traffic.** M4-01 (the recordings) is not done. The adapter was written from the SABnzbd API documentation and from what Sonarr and Radarr are known to parse, and the fixtures in `internal/adapter/sabnzbd/testdata/contract/` are **hand-written, not recorded**. Each says so in its `provenance` field, and a test insists on it, so nobody mistakes them for the spec. When M4-01 delivers, replace them with the recordings, change their provenance, and fix whatever differs: the recordings override this section.

What was decided, and is therefore a guess until then:

- **Package and wiring.** `sabnzbd.New(Deps) http.Handler` (`internal/adapter/sabnzbd`) is mounted by `api.Config.SABnzbd` at `/sabnzbd/api` (any method, also with a trailing slash). `Deps` takes the adapter credential service, the Download, Add and Category services, the Settings, and optional `StageSource` (the post-processing stage of a Download, fed from `core.PostProcessStage` events), `SpeedSource` (rate and global pause), `DiskSpace`, `Version`, `Clock` and `Logger`. The route sits inside the setup gate and the request ID, panic recovery and security header middleware, outside the UI authentication and CSRF. The API access log keeps only the path and the `mode` for it, because its query holds the key and, for `addurl`, an indexer URL with its own key.
- **Auth.** Only the `apikey` parameter (query, urlencoded form, or a text part that comes before the file in an upload) is read, and it must be an `adapter_sab` credential. `mode=version` needs no key; every other mode, including an unknown one, answers `{"status":false,"error":"API Key Required"}` or `"API Key Incorrect"` with HTTP 200. An upload is not read before the caller is authenticated. Username/password auth and `nzbkey` are not supported.
- **Output.** Always JSON, whatever `output` says. Errors are `{"status":false,"error":"..."}` with HTTP 200; an unknown mode is `"not implemented"`, `retry` is `"retry is not supported"`, `addfile` without a file part is `"No file given"`, `addurl` without a URL is `"expect one parameter"`. The app layer's messages (invalid NZB, fetch failures, invalid category) are passed through; they are redacted already.
- **Version.** `4.5.0` (`sabnzbd.DefaultVersion`), configurable.
- **Shapes.** Numbers SAB sends as strings are strings here too: `mb`, `mbleft`, `percentage`, `kbpersec`, `diskspace1`, `pause_int`, `have_warnings`. Sizes in `mb` fields are `"%.2f"` MiB; `size`/`sizeleft` are humanised (`"3.0 GB"`); `speed` is `"10.0 M"`; `timeleft` is `H:MM:SS`, or `D:HH:MM:SS` from one day on, and `0:00:00` when the rate is unknown or the slot is paused. Queue slots carry `priority` as SAB's names (`Force`, `High`, `Normal`, `Low`), `index` is the place in the whole queue, and `password` is always empty: the archive password is never shown. The decisions are in the `shapes.go` structs, the examples in the fixtures.
- **Queue and history.** The queue lists the NZB Downloads in Added, Queued and Fetching (a deferred-volume refetch is Fetching, so it is `Downloading` again) in queue order; `cat` filters (comma-separated names, `*` is uncategorised), `start`/`limit` page (`limit` 0 is everything). History lists Fetched, PostProcessing, Ready and Failed, newest first by when they became Ready or Failed (else when they were Fetched); `category` (or `cat`) filters. `noofslots` counts the filtered list, `noofslots_total` the whole queue. Only NZB Downloads are listed or touched.
- **Status mapping.** As in the table above, with these choices: `Fetched` is shown as `Verifying` (it is waiting for the first step), and a PostProcessing Download whose stage is unknown or new to the adapter is `Verifying` too. `Completed` is only ever produced for Ready; a table test covers every State, every stage and both values of the paused flag, and an end-to-end test checks both lists. A paused Queued or Fetching Download is `Paused`. `fail_message` is SAB-style text per failure reason (for example `Repair failed, not enough repair blocks`) followed by the stored detail, with URLs re-masked, control characters dropped and a 300 byte cap. `storage` is the output path for Ready only.
- **Deletes.** `value` is a comma-separated list of nzo_ids; unknown ones, torrent Downloads and Downloads of the other list are ignored and the answer is still `{"status":true}` (queue deletes add `"nzo_ids":[...]` with the ones removed). A queue delete always removes the Download's working data (its incomplete files), whatever `del_files` says, as SABnzbd does; for history items `del_files=1` (or `true`) deletes the finished output and otherwise only the row goes. Downloads in Fetched or PostProcessing are not deleted from history, because their files are in use (SAB refuses too).
- **Priorities.** `force` 2, `high` 1, `normal` 0, `low` -1, as in the table. `-100` and anything unknown is normal. `-2` ("paused" in SAB) adds the Download as normal priority and paused; `queue&name=priority&value2=-2` pauses it. `name=priority`, `name=pause`, `name=resume` and `name=switch` (to the top, the bottom, or up to 25 places) are implemented; other `name` values are `"not implemented"`.
- **Categories.** `cat` is matched ignoring case; unknown names are created with the default dir, here only (a name that is not a valid Category name is an error and adds nothing). `*` and empty mean none. `get_config` lists `*` first, then the Categories; a Category's `dir` is its name, or its override relative to the Usenet complete root when it lies inside it, else the absolute override. The sorting flags are false and `history_retention_option` is `all`.
- **addfile and addurl.** `addfile` takes the first file part of the multipart body, whatever its field name (`name` or `nzbfile`), streams it to disk through `AddService.Stage` and fills in the rest from `cat`, `priority`, `nzbname`, `password`. `pp` and `script` are ignored. `addurl` checks the cheap fields, fetches `name` through `AddService.FetchNZB` and then adds. The URL is never stored, logged or echoed in clear.
- **Not implemented** (nothing in the docs says media managers call them): global pause and resume, speed limits, `change_cat`, `rename`, `get_files`, `warnings`, `qstatus`, RSS, scheduling, JSONP `callback`.

**Adapter credentials (M4-02).** `core.AdapterCredential`, table `adapter_credentials` (migration 0004), `auth.AdapterCredentialService` (`Create`, `List`, `Revoke`, `Authenticate`, `AuthenticateQBit`), `auth.RequireAdapterCredential(kind)` for adapter routes that take a header or query key, and `auth.UIPrincipalOnly` for the management routes (`GET/POST /api/v1/adapter-credentials`, `DELETE /api/v1/adapter-credentials/{id}`, see [api-v1.md](../api-v1.md#adapter-credentials)). `UIPrincipalOnly` accepts the session, LAN bypass and disable-auth principals and refuses everything else; the routes also use `KeyOr`, so a request that carries an API key is refused with `403 session_required` instead of being let in as the bypass principal. The SAB handler authenticates itself with `Authenticate`, because the key may sit in a multipart body and the answer must be SAB's error shape. Tests: an adapter credential is `401` on `/api/v1/*`, and an API key, a session and the bypass settings never reach `/sabnzbd/api`.

**Contract suite (M4-06).** `internal/adapter/adaptertest` is the generic replay harness (also for the qBit fixtures of M5-12): it loads `*.json` fixtures with `request` (method, target, headers, body or multipart parts, `{{variables}}`), `response` (status, headers, JSON or text) and `capture` (values for later steps), compares with matchers (`$any`, `$absent`, `$type:`, `$re:`, exact; extra response keys are allowed) and reports one line per difference, such as `$.history.slots[0].status: want "Finished", got "Completed"`. The SAB side (`contract_test.go`) seeds a real store (temp dir), real services and a real credential from each fixture's `seed` list and replays it through the handler. A deliberately broken fixture test proves the diff is readable.

## Issues

### M4-01: Record media manager ↔ SABnzbd traffic fixtures
- **Labels:** area:adapter, ready-for-human
- **Depends on:** —

**Goal:** Build the authoritative contract for the SAB adapter by recording real traffic.

**Work**
- Run docker compose with a real SABnzbd, Sonarr, Radarr and Prowlarr (current stable versions) behind a recording proxy such as mitmproxy, or put SAB's API behind a logging reverse proxy.
- Exercise these flows:
  - add the client in each media manager (test button)
  - grab a release (addfile and addurl)
  - queue polling while downloading, paused, and with the queue empty
  - completion through history, including the in-between post-processing statuses
  - failure (an NZB that can't be repaired)
  - removal from queue and from history, with and without files
  - priority changes
  - Prowlarr's sync and test
- Save each request and response pair as `internal/adapter/sabnzbd/testdata/fixtures/*.json`. Scrub the API keys, host names and paths into placeholders.

**Acceptance criteria**
- [ ] The fixtures cover every row of the "Modes" table that media managers actually use. Modes they never call are marked "unused" in `testdata/README.md`.
- [ ] The media-manager versions are recorded in `testdata/README.md`.
- [ ] No real keys or private hostnames are in the repo.

### M4-02: Adapter credentials per media manager
- **Labels:** area:auth, area:adapter, ready-for-agent
- **Depends on:** M1-12

**Goal:** Credentials that are valid only on adapter routes, one per media manager. This is shared with M5.

**Work**
- Add kinds `adapter_sab` and `adapter_qbit` to the token store. `adapter_qbit` has a username plus a generated key that acts as the password.
- Session-only routes `GET/POST/DELETE /api/v1/adapter-credentials`, where the plaintext is shown once.
- Middleware helpers: `RequireAdapterCredential(kind)`. Adapter credentials are rejected on every other route, and every other credential is rejected on adapter routes.
- Managing adapter credentials differs from managing API keys: `auth.SessionOnly` rejects bypass and disabled-auth principals, but media managers must still be able to authenticate when the UI runs with disabled auth or LAN bypass. So the credential-management routes must also accept those UI principals (not API keys), and the UI must say that credentials created this way outlive the bypass setting.
- Track `last_used_at` per credential, so the UI can show which media manager last connected.

**Acceptance criteria**
- [ ] Tests confirm an adapter credential is rejected on `/api/v1/*` and an API key or session is rejected on `/sabnzbd/api`.
- [ ] Revocation takes effect immediately.

### M4-03: SAB adapter — routing, auth, version and config modes
- **Labels:** area:adapter, ready-for-agent
- **Depends on:** M4-01, M4-02, M1-13

**Goal:** Serve `/sabnzbd/api` with `apikey` auth and the read-only modes `version`, `get_config`, `get_cats` and status.

**Work**
- Dispatch on `mode`, with SAB's error shapes for bad auth and unknown modes.
- `version` returns the compatible version string chosen from the fixtures.
- `get_config`/`get_cats` return the Categories with dirs relative to the Usenet complete root and the expected `misc` fields.
- Return status/fullstatus only if the fixtures show them in use.

**Acceptance criteria**
- [ ] Contract tests for these modes pass against the M4-01 fixtures.
- [ ] Each media manager's "Test" button succeeds in the M4-07 environment.

### M4-04: SAB adapter — addfile and addurl
- **Labels:** area:adapter, ready-for-agent
- **Depends on:** M4-03, M2-03

**Goal:** Add Downloads through the SAB API, mapped onto the M2-03 service.

**Work**
- `addfile` (multipart; the field name comes from the fixtures) and `addurl` (`name` is the URL).
- Map `cat` (auto-creating unknown Categories), `priority` (per the mapping table), `nzbname` and `password`.
- Return `nzo_ids`.

**Acceptance criteria**
- [ ] Contract tests pass.
- [ ] The URL isn't persisted, and logs redact it.
- [ ] An unknown Category gets created with the default dir.

### M4-05: SAB adapter — queue, history, delete and the status mapping
- **Labels:** area:adapter, ready-for-agent
- **Depends on:** M4-03, M3-01

**Goal:** Implement queue and history listing, deletion, and the bitnews → SAB status mapping.

**Work**
- `queue` lists not-yet-Ready, non-PostProcessing Downloads, plus deferred-volume refetches. Support paging and the Category filter.
- `history` lists PostProcessing, Ready and Failed Downloads, with stage-specific statuses, `storage` and `fail_message`.
- Delete from queue (always deleting the incomplete data) or history (deleting the output when `del_files=1`).
- Priority and `switch` changes, if the fixtures show them.
- The slot and queue-level fields are those the fixtures require.

**Acceptance criteria**
- [ ] Contract tests pass for every queue and history fixture.
- [ ] A Download is never shown as `Completed` before Ready (table test over every state and stage).

### M4-06: SAB adapter contract test suite
- **Labels:** area:adapter, area:ci, ready-for-agent
- **Depends on:** M4-01

**Goal:** A reusable replay harness that runs in CI.

**Work**
- Load each fixture, substitute the placeholders, seed core state (with a fake engine) to match what the fixture expects, replay the request, and compare the response semantically. That covers JSON field presence and types, the status strings, and the IDs, while ignoring volatile values (times, speeds) listed in each fixture.
- Make the harness generic enough for the qBittorrent fixtures in M5-12.

**Acceptance criteria**
- [ ] `go test ./internal/adapter/...` runs every fixture.
- [ ] A deliberately broken status string makes a test fail with a readable diff.

### M4-07: End-to-end check with Sonarr and Radarr
- **Labels:** area:adapter, area:deploy, ready-for-human
- **Depends on:** M4-04, M4-05, M3-10

**Goal:** Prove the first daily-use point on real containers with the TRaSH layout.

**Work**
- Use `deploy/e2e/docker-compose.yml` with bitnews, Sonarr and Radarr (and optionally Prowlarr), all sharing one `/data` mount, with the same `PUID`/`PGID`/`UMASK`.
- Check these against a real Usenet provider:
  - grab → queue → Ready → import by hardlink (the imported file's link count is 2)
  - a forced failure (a test NZB with too little recovery data) → Failed → blocklisted, and another release is grabbed
  - removal from the media manager deletes the data
- Record the steps and results in `docs/compat/media-managers.md`.

**Acceptance criteria**
- [ ] The checklist is documented with the versions tested, and every step passes.
- [ ] Any deviations get follow-up issues.
