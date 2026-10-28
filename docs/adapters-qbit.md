# qBittorrent adapter (`/api/v2/`)

Implementation notes for M5-10 to M5-12; the design is in [m5-torrents.md](milestones/m5-torrents.md). Package `internal/adapter/qbit`, fixtures in `internal/adapter/qbit/testdata/contract/`.

**Read this first: nothing here was checked against recorded traffic.** M5-09 (the recordings) was dropped. The adapter was written from the qBittorrent WebUI API documentation and from what Sonarr and Radarr are known to parse, and the fixtures are **hand-written, not recorded**. Each says so in its `provenance` field and a test insists on it. When real use shows a difference, fix the adapter and, where useful, replace the fixture with a recording (change its provenance): what real Sonarr and Radarr do overrides this page. The Sonarr-shaped decode structs in `sonarr_shapes_test.go` were written from memory of Sonarr's C# classes.

## Wiring

`qbit.New(Deps) http.Handler` is mounted by `api.Config.QBit` at `/api/v2/` (every method). It sits inside the setup gate and the request ID, panic recovery and security header middleware, and outside the UI authentication and CSRF. The access log of the whole `/api/v2/` tree holds the path only: the query and the form bodies hold hashes, magnet links and URLs with passkeys.

`Deps` requires the app key service (`auth.AppKeyService`), the Download and Category services, the Settings, the Download view module (`view.Module`, M8-05) and three ports the daemon implements (`ports.go`). Everything a media manager reads about a torrent that the view holds comes from `View.Of`: the phase, sizes and done bytes, progress, rates, ETA, info hash, uploaded, ratio, seeding time, peers and seeds, private flag, Share Limit pause and the effective Share Limit. The adapter lists the torrent Downloads through the Download service and only translates the view to qBittorrent's fields. The ports give what the view does not hold:

- `TorrentSource`: `Lookup(hash)` returns the Download of an info hash; `Torrent(d, full)` returns the torrent's name, save path and content path. `Torrent` is called for every torrent on every poll, so without `full` it must be cheap.
- `TorrentAdder` (`AddTorrentFile(data, opts)`, `AddURL(url, opts)`) creates the Download and the torrent and returns the hash. `AddOptions` holds the resolved Category ID, paused, a validated save path and an optional Share Limit. A duplicate must wrap `core.ErrConflict`.
- `ShareLimiter` (`SetShareLimit(id, ratio, seconds, useGlobal)`); `-1` is unlimited.
- `FileSource` (optional, `Files(id)`): the names and sizes of a torrent's files. Each file shows the view's progress of the whole torrent.

Optional: `Files` (`FileSource`, for `torrents/files`; empty list without it), `GlobalShare` (the global default, for preferences and to resolve a half-global limit), `Limiter` (a `*auth.LoginLimiter`; the default is the adapter's own), `MaxSessions` (1000), `Version` (`v4.6.7`), `WebAPIVersion` (`2.9.3`), `Clock`, `Logger`.

## Authentication

- `POST auth/login` takes the form fields `username` and `password` (body only) and checks them with `AuthenticateQBit`: the fixed username `bitnews` and the app key as the password (ADR-0006). Success is `Ok.` with a `SID` cookie; a wrong login is `Fails.` with HTTP 200, like qBittorrent.
- The `SID` is an adapter session: a random 128-bit token (32 hex characters) in an in-memory table, bound to the generation of the app key it logged in with, 1 hour after its last use, at most 1000 sessions (expired ones go first, then the least recently used). Cookie: `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure` when the request is TLS or comes through a trusted proxy that says `X-Forwarded-Proto: https`. Nothing else reads it, so it is useless on `/api/v1` and `/sabnzbd/api`, and the key is not a `SID` (tested in both directions in `internal/api/qbit_test.go`).
- Every request re-checks that the session's key generation is still the current one, so rotating the key ends every session with the next request.
- Rate limit: failures are counted **per client IP only** (the client IP honours the trusted proxies). The username is deliberately not a key, because anyone could then lock the media manager out by failing its username. After 5 failures the IP is answered `403` with `Your IP address has been banned after too many failed authentication attempts.` for the backoff of `auth.LoginLimiter` (1 s, doubling, 15 min at most), even with the right password. The adapter's own limiter instance means failures here never lock the UI.
- Without a valid session every call except login is `403 Forbidden`, whatever the path (an unknown path does not give itself away).
- CSRF: a non-safe request whose `Origin` or `Referer` names another host than `Host` (or an entry of the allowed hosts) is `401 Unauthorized`; `Origin: null`, two `Origin` headers and unparsable values count as foreign. Sonarr and Radarr send neither header, which passes. Login is checked too.

## Endpoints

| Call | Notes |
|---|---|
| `POST auth/login`, `POST auth/logout` | as above |
| `GET app/version`, `app/webapiVersion` | bare strings, auth required |
| `GET app/preferences` | read-only view: `save_path` (torrent complete root), `temp_path`, `queueing_enabled` (true), `max_active_downloads` (the torrent queue limit), `max_ratio_enabled`, `max_ratio`, `max_seeding_time_enabled`, `max_seeding_time` (minutes), `max_ratio_act` (0 pause, 1 remove, 3 remove with files), `dht`, `pex`, ... The global Share Limit comes from `Deps.GlobalShare` (off without it). |
| `POST app/setPreferences` | accepted and **ignored** (200): settings are not changed through the adapter |
| `GET torrents/info` | filters `category` (case-insensitive; an empty value is "uncategorised"), `hashes` (`a\|b`, `all`), `filter`, `sort`, `reverse`, `limit`, `offset` (negative counts from the end). Default order: when added, then hash. An unknown `filter` filters nothing. |
| `GET torrents/properties?hash=`, `torrents/files?hash=` | 404 for an unknown or malformed hash |
| `POST torrents/add` | `urls` (newline separated magnets / http(s) URLs) and `torrents` file parts. Options `category` (created if unknown, lowercased), `paused` / `stopped`, `savepath` (only under a torrent root; otherwise ignored, logged), `ratioLimit`, `seedingTimeLimit` (minutes). `tags`, `sequentialDownload`, `firstLastPiecePrio`, `autoTMM`, `rename`, speed limits and the rest are ignored. `Ok.` if at least one torrent was added, else `Fails.`, both HTTP 200 (a duplicate is `Fails.`). |
| `POST torrents/delete` | `hashes`, `deleteFiles` |
| `POST torrents/pause`, `stop`, `resume`, `start` | both name generations |
| `POST torrents/setCategory`, `createCategory`; `GET torrents/categories` | see Categories |
| `POST torrents/setShareLimits` | `hashes`, `ratioLimit`, `seedingTimeLimit` (-2 global, -1 none), `inactiveSeedingTimeLimit` is ignored |
| `POST torrents/topPrio`, `bottomPrio`, `increasePrio`, `decreasePrio`, `setForceStart` | queue position and Download Priority |
| `GET torrents/tags` | always `[]` |
| `GET sync/maindata` | always a full update (`full_update: true`, `rid` + 1): torrents, categories, a minimal `server_state` |

Reads accept GET and POST; calls that change something need POST (`405` with `Allow: POST` otherwise). An unknown endpoint is `404`. Not implemented: `transfer/*`, `torrents/recheck`, `reannounce`, `trackers`, tag calls other than the empty list, `rename*`, `setLocation` and the rest.

## State mapping

`torrentState(view.View)`, on the view's Phase and progress (capped at 0.9999 below Ready); the table test in `status_test.go` builds each view through `view.Module` and walks every State (plus an unknown one) x Download paused x Share Limit paused x priority x up and down rate x done bytes x API generation. `eta` is the view's ETA in seconds, `8640000` (qBittorrent's infinity) when the view has none (not downloading, paused, or no rate).

| bitnews | `state` | `progress` |
|---|---|---|
| Added, Queued | `queuedDL` (`forcedDL` if force, `pausedDL` if paused) | real |
| FetchingMetadata | `metaDL` (`pausedDL` if paused) | 0 |
| Fetching | `downloading`; `stalledDL` when no data arrives; `forcedDL` if force; `pausedDL` if paused | real, at most 0.9999 |
| **Fetched, PostProcessing** (move, Unpack) | **`checkingUP`** | **0.9999** |
| Ready | `uploading`; `stalledUP` when idle; `forcedUP` if force; `pausedUP` when the engine paused it (Share Limit reached) | 1 |
| Failed | `error` | real |

- **The import hold-off.** The states Sonarr and Radarr read as completed (`uploading`, `stalledUP`, `pausedUP`, `stoppedUP`, `forcedUP`, `queuedUP`) and a progress of 1 are produced for **Ready only**. A Download that is Fetched or Post-processing is `checkingUP` at 0.9999 with `amount_left` > 0 and `completion_on` -1. Sonarr maps `checkingUP` to queued, as far as remembered; M5-09 must confirm which states and progress values make the media managers hold off. The `completed` and `seeding` filters leave `checkingUP` out too (real qBittorrent counts it as completed), and `torrents/files` caps file progress at 0.9999 before Ready. The Post-processing stage makes no difference to the state (every stage is `checkingUP`), so the adapter needs no stage source.
- **API generation.** With a `WebAPIVersion` of 2.11 or newer the names are `stoppedDL` and `stoppedUP` (qBittorrent 5); below it `pausedDL` and `pausedUP`. The default is 2.9.3 / `v4.6.7`, so `pause`/`resume` are what Sonarr calls. Both name generations are accepted either way.
- A pause applies to a Download that is not Fetched yet and to a Ready torrent (`DownloadService.Pause`; ADR-0012): for a seeding torrent `pause` / `stop` stops its Seeding and `resume` / `start` seeds again. Any other Download in Post-processing or a Ready NZB is left alone. `pausedUP` (`stoppedUP`) appears when the user paused the seed or the view reports `Torrent.LimitPaused` (a Share Limit reached); `resume` does not override a reached Share Limit.
- `ratio_limit` is -2 (global), -1 (none) or the ratio; `seeding_time_limit` is in minutes with the same codes (rounded up); `seeding_time` is in seconds. `magnet_uri` and `tracker` are always empty: trackers carry passkeys. `content_path` is the Download dir (Ready: `OutputPath`), which the media managers scan; without paths from the engine the torrent roots stand in. `priority` is the queue place + 1 for Downloads in the queue, else 0. `eta` is 8640000 (infinity) unless a download has a rate.

## Semantics worth knowing

- **Delete.** `deleteFiles=false` removes the Download (`DownloadService.Delete(id, false)`) and leaves the data on disk. For a torrent the data is the seed, so nothing seeds it afterwards; `deleteFiles=true` calls the `DataRemover`. A Download that is Fetched or being Post-processed is not deleted (its files are in use, as SAB refuses too). Unknown, malformed and NZB hashes are ignored with HTTP 200.
- **Categories** are shared with Usenet. Names are `[a-z0-9._-]{1,64}`; a name with a slash or space cannot be created (`createCategory` answers 409 `Incorrect category name`, `add` answers `Fails.`). `createCategory` of an existing name (any case) succeeds, so a media manager that spells it differently still works, and its `savePath` is ignored. `setCategory` to an unknown name is 409, an empty name removes the Category. `torrents/categories` reports `savePath` as the Category's dir override or `<torrent complete root>/<name>`.
- **Priority.** `topPrio` etc. move the Download in its queue (`DownloadService.Update` with a move); several hashes keep their relative order; Downloads that left the queue are skipped. `setForceStart true` sets the priority `force` (state `forcedDL`/`forcedUP`), `false` sets `normal`.
- **Share limits.** Both codes -2 means `useGlobal`. The port has one flag, so when only one code is -2 it is resolved now to the current global value (unlimited if the global limit is off). Add does the same with `ratioLimit`/`seedingTimeLimit`.
- **Bounds.** Forms 1 MiB (login 8 KiB), `torrents/add` multipart 32 MiB with at most 64 parts, 16 `.torrent` files of 10 MiB each, text fields 256 KiB, 32 URLs of 16 KiB each, 1000 hashes per call (`400` beyond that). Only `magnet:`, `http:` and `https:` URLs reach the adapter port. The body of an unauthenticated request is never read. Sessions 1000.
- **Never logged:** URLs, magnet links, query strings, passwords, SIDs. Adder errors are logged with every URL masked.

## Tests

`go test ./internal/adapter/qbit` has the auth flows (`auth_test.go`), the state table and import hold-off (`status_test.go`), `torrents/info` with filters, paging and sorting, the 10,000 torrent listing (about 1 s under `-race`) (`info_test.go`), add and the hostile inputs (`add_test.go`), the other calls (`mutate_test.go`), Sonarr-shaped decoding (`sonarr_shapes_test.go`), the contract fixtures through `adaptertest` (`contract_test.go`, with a deliberately broken fixture proving a readable diff) and two fuzz targets (`FuzzParseHashes`, `FuzzRequest`). `internal/api/qbit_test.go` covers the mount, the gate, cross-adapter rejection and log redaction. The in-memory engine fake (the ports plus the `view.LiveSource` of a real `view.Module` over the test store) is in `helpers_test.go`.

`adaptertest` got two backwards-compatible additions: `capture` accepts `header:Name:REGEX` (the SID from `Set-Cookie`), and expected object keys may hold `{{variables}}`.

## Open questions for real use and the daemon

- Which `state`/`progress` pairs Sonarr and Radarr treat as "do not import yet" (above).
- The real `torrents/add` answer for a duplicate and for a failing URL, and the exact error bodies, on current qBittorrent versions.
- Whether any media manager calls `setPreferences`, `transfer/*`, `torrents/recheck` or tag calls.
- The daemon must implement the three ports. `TorrentAdder` creates the Download itself; it should apply `AddOptions.ShareLimit` and `SavePath`.

## Daemon wiring

`daemon.Build` mounts the adapter at `/api/v2/` whenever the torrent side is on (not with `Deps.DisableTorrent`); it shares the app key service with the SABnzbd adapter and the own API. The view module gets the torrent facts from `Runner.Facts` (through the daemon's live source): the persisted counters of earlier runs plus the engine's uploaded bytes and seeding time, so ratio and seeding time survive restarts. The Share Limit is `Global` when the Download has none of its own; with only a ratio or only a time of its own, the other half is the current global default (`view.ShareLimit`). `pausedUP` comes from the view's `LimitPaused`, set only while the Share Limit holds the torrent (a pause before Fetched is the Download's own flag). `added_on` is when the Download was added and `completion_on` when it was Fetched. The three ports are implemented in `internal/daemon/qbit.go` on the Download repository and the torrent runner (`Facts`, `ContentDir`, `SetShareLimit`, `ClearShareLimit`):

- **`TorrentSource`.** `Lookup` finds the Download of an info hash from the TorrentSource once and remembers it. `Torrent` answers the name and paths from `Runner.Facts`, which describes a torrent the engine has from memory and one it has not (Queued, or a magnet waiting for its metadata) from the Download and its `torrent_state` record; it touches no disk. `save_path` is the parent of the Download directory (the category or complete directory). `content_path` is the Download directory in a listing; for one hash (`torrents/properties`, `full`) the runner finds the torrent's own root directory (`ContentDir`, which reads the directory). Once Ready the adapter shows the Download's `OutputPath`, which is that same directory, so nothing the media managers read differs.
- **`TorrentAdder`.** Goes through the `AddService`, so a torrent added here is staged, parsed with the engine's checks, stored, queued and duplicate-checked exactly like one added through the own API. A `.torrent` file is `StageTorrent` + `AddTorrent`, a `magnet:` URL is `StageMagnet` + `AddTorrent`, an http(s) URL is `Fetch` (a `.torrent`, or a redirect to a magnet link). **An NZB behind a URL is refused** (`Fails.`, nothing created): this API is torrent-only. A duplicate returns the existing info hash and an error matching `core.ErrConflict` (`Fails.`). `category` and `paused` are honoured; `ratioLimit` and `seedingTimeLimit` are applied with `SetShareLimit` once the Download exists (a failure there is logged and does not undo the add). **`savepath` is ignored** (logged without any URL): the Download goes to `<torrent complete>/<category>/<name>` like every other.
- **`ShareLimiter`.** `useGlobal` clears the Download's limit; otherwise it stores it (`-1` unlimited) and the runner applies it at once: a torrent that has reached the new limit gets the configured action, and one that was paused by a limit that is no longer reached seeds again.
- **`FileSource`.** From the engine's file list in `Runner.Facts` (paths include the torrent's root directory, like qBittorrent's). The engine reports no per-file progress, so the adapter shows the view's progress of the whole torrent for every file. A Download the engine does not have yet answers an empty list.
- **`GlobalShare`.** From `settings.seeding`: the ratio and the time are "enabled" when they are above zero; the action is `pause`, `remove` or `remove_files`.

Tested through the whole daemon over real HTTP with the real engine and a local seeder (`internal/daemon/qbit_e2e_test.go`): login with the app key (a wrong password, a username other than `bitnews` and an own-API session all fail; the SID opens nothing under `/api/v1`), add by magnet with an `x.pe` hint and by `.torrent` upload, `torrents/info` polled to completion while checking, on every answer, that **progress 1 or an upload state is never shown before the Download is Ready** (`checkingUP` at less than 1 was observed in most runs, and in the others the window was too short to see), files, `setShareLimits` read back through `torrents/info` (a value, `-2`, `-1`), a limit that is already reached pausing the torrent as `pausedUP`, add with `paused` waiting as `pausedDL` and `resume` starting it, a duplicate and an invalid file as `Fails.`, `delete` with and without files, and an NZB URL refused.

Known gaps: `savepath` and `tags` are ignored; speed limits (`dl_limit`, `up_limit`) are not settable and always 0; nothing was checked against recorded Sonarr/Radarr traffic (M5-09 was dropped); the adapter session table is in memory, so every restart of bitnews logs the media managers out (they log in again on the next call).
