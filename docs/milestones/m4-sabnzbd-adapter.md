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
| `queue&name=delete&value=<ids>&del_files=1` | Remove from the queue | Data is deleted when `del_files=1`. |
| `queue&name=priority` / `switch` | Change priority or order, if the fixtures show it | Maps to Download Priority or queue position. |
| `history` | Ready, Failed and post-processing Downloads, with `start`/`limit`/`category` | See the mapping below. |
| `history&name=delete&value=<ids>&del_files=1` | Remove from history | `del_files=1` deletes the output dir. |
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
- Delete from queue or history, deleting data when `del_files=1`.
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
