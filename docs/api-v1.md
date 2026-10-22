# Own API v1

The own API is served on the daemon's one port under `/api/v1/`. The UI (M6) uses the same routes with a session cookie. Scripts use an API key. This page is the route table; the design is in [M1](milestones/m1-foundation.md#http-server-and-own-api-v1).

## Conventions

- **Bodies** are JSON (`Content-Type: application/json`, else `415 unsupported_media_type`). Decoding is strict: unknown fields, a wrong type and trailing data are `400 invalid_json`. Bodies are capped (16 KiB, settings 1 MiB; over the cap is `413 request_too_large`). The one exception is `POST /downloads`, which also takes a streamed `multipart/form-data` upload of up to 64 MiB, see [Adding a Download](#adding-a-download).
- **Errors** always have this shape, with a stable snake_case `code`:

  ```json
  {"error": {"code": "invalid_priority", "message": "unknown priority \"urgent\": use force, high, normal or low"}}
  ```

  Status codes: `400` invalid input, `401` not authenticated, `403` forbidden (scope, CSRF, session required), `404` not found, `405` wrong method, `409` conflict, `413` too large, `415` wrong media type, `502` `fetch_failed` (a URL the daemon fetched failed), `421` Host not allowed, `429` login rate limit (with `Retry-After`), `500` `internal` (generic; the detail is only in the server log), `503` `setup_required`.
- **Request IDs:** every response carries `X-Request-ID`. A client-supplied one is kept only if it is 1 to 64 characters of `[A-Za-z0-9._-]`; otherwise the server makes one. It appears in the server log.
- **Security headers** on every response: `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`. API responses are `Cache-Control: no-store`.
- **IDs** (Downloads, Categories, Servers, API keys) are opaque strings. Times are RFC 3339 UTC.

## Authentication

| Auth | Meaning |
|---|---|
| none | No credentials. |
| setup token | The one-time token from the startup log. |
| read / write | An API key of that scope (`Authorization: Bearer bn_...` or `X-Api-Key`), or else a UI session. A `read` key may only use GET; a `write` key may use every method. A request that carries a key is judged as a key request and never falls back to the session. |
| session | A logged-in UI session (cookie `bitnews_session`). State-changing requests also need the `X-CSRF-Token` header from `GET /auth/session` or the login response. API keys are refused. |
| UI principal | Like session, but the disable-auth and LAN bypass principals are accepted too (they also need the CSRF token). API keys and adapter credentials are refused with `403 session_required`, even where the bypass would let the request in without them. Used only to manage adapter credentials, so media managers can be configured when the UI runs without a login. |

While first-run setup is open, every route except `POST /setup` and `GET /health` answers `503 setup_required`.

## Routes

| Method | Path | Auth | Request | Response |
|---|---|---|---|---|
| GET | `/api/v1/health` | none | | `200 {"status":"ok"}` |
| POST | `/api/v1/setup` | setup token | `{"token","username","password"}` | `201 {"username"}`; `403 invalid_setup_token`, `409 setup_closed` |
| POST | `/api/v1/auth/login` | none (Origin check, rate limited) | `{"username","password"}` | `200` session info + `Set-Cookie`; `401 invalid_credentials`; `429 rate_limited` |
| POST | `/api/v1/auth/logout` | session | | `204`, cookie cleared |
| GET | `/api/v1/auth/session` | session | | `200 {"auth_mode":"session\|bypass\|disabled","csrf_token","user":{"id","username"}\|null}` |
| GET | `/api/v1/version` | read | | `200 {"version","commit","go_version"}` |
| GET | `/api/v1/downloads` | read | query: `state`, `category_id`, `kind` (`nzb`\|`torrent`), `limit` (1 to 200), `after` | `200 {"items":[Download],"next":"cursor"\|null}` |
| POST | `/api/v1/downloads` | write | multipart upload or `{"url",...}`, see [Adding a Download](#adding-a-download) | `201 Download`; `400 invalid_nzb`, `unknown_category`, `invalid_priority`, `invalid_url`; `413 nzb_too_large`; `415`; `502 fetch_failed` |
| GET | `/api/v1/downloads/{id}` | read | | `200 Download` |
| POST | `/api/v1/downloads/{id}/pause` | write | | `200 Download`; `409 not_pausable` once Fetched |
| POST | `/api/v1/downloads/{id}/resume` | write | | `200 Download` |
| PATCH | `/api/v1/downloads/{id}` | write | any of `{"priority":"force\|high\|normal\|low","category_id":"<id>"\|null,"queue_position":{"move":"top\|bottom\|up\|down"}}` | `200 Download`; `409 not_queued` for a move of a Download that is Fetched or later. The state cannot be changed. |
| DELETE | `/api/v1/downloads/{id}?delete_data=true` | write | | `204`. The row is deleted and a `removed` event follows. Data is deleted only with `delete_data=true` (engines hook in later). |
| GET | `/api/v1/categories` | read | | `200 {"items":[Category]}` |
| POST | `/api/v1/categories` | write | `{"name","dir_override"?}` | `201 Category`; `400 invalid_name`, `400 invalid_dir_override`, `409 name_taken` |
| PATCH | `/api/v1/categories/{id}` | write | any of `{"name","dir_override":"/abs/path"\|null}` | `200 Category` |
| DELETE | `/api/v1/categories/{id}` | write | | `204`. Downloads that used it lose it. |
| GET | `/api/v1/servers` | read | | `200 {"items":[Server]}` ordered by `priority`, then `name` |
| POST | `/api/v1/servers` | write | `{"name","host","port"?,"username"?,"password"?,"max_connections"?,"retention_days"?,"priority"?,"enabled"?}` | `201 Server`; `400 invalid_name`, `invalid_host`, `invalid_port`, `invalid_max_connections`, `invalid_retention_days`, `invalid_priority`, `invalid_credentials`; `409 name_taken` |
| POST | `/api/v1/servers/test` | write | the same body as `POST /servers`; nothing is saved | `200 ProbeResult` |
| GET | `/api/v1/servers/{id}` | read | | `200 Server` |
| PATCH | `/api/v1/servers/{id}` | write | any of the create fields. Without `password` the stored one is kept. | `200 Server`; `400 empty_patch` and the codes above; `409 name_taken` |
| DELETE | `/api/v1/servers/{id}` | write | | `204` |
| POST | `/api/v1/servers/{id}/test` | write | | `200 ProbeResult`; `409 password_unavailable` |
| GET | `/api/v1/settings` | read | | `200 Settings` |
| PUT | `/api/v1/settings` | write | the full `Settings` object | `200 Settings`; `400 invalid_settings` lists every problem. A request with an API key may not change the `auth` section (`403 session_required`). |
| GET | `/api/v1/api-keys` | session only | | `200 {"items":[{"id","name","scope","created_at","last_used_at"}]}` |
| POST | `/api/v1/api-keys` | session only | `{"name","scope":"read\|write"}` | `201` the key's fields plus `"key":"bn_..."`, shown this once |
| DELETE | `/api/v1/api-keys/{id}` | session only | | `204` |
| GET | `/api/v1/adapter-credentials` | UI principal | | `200 {"items":[{"id","kind","name","username"?,"created_at","last_used_at"}]}` (never a key or hash) |
| POST | `/api/v1/adapter-credentials` | UI principal | `{"kind":"adapter_sab\|adapter_qbit","name","username"?}` | `201` the credential's fields plus `"key":"bns_..."` (or `"bnq_..."`), shown this once; `400 invalid_kind`, `invalid_name`, `invalid_username`; `409 username_taken` |
| DELETE | `/api/v1/adapter-credentials/{id}` | UI principal | | `204`; the credential stops working with the next request |
| GET | `/api/v1/events` | read | | `200 text/event-stream`, see below |

Routes that need a session are also refused to the disable-auth and LAN bypass principals (`403 session_required`).

### Adapter credentials

An adapter credential authenticates one media manager (Sonarr, Radarr, Prowlarr) on one adapter, so each can be revoked on its own. The key is 256 random bits shown once, as `bns_` + base32 for `adapter_sab` and `bnq_` + base32 for `adapter_qbit`; only its SHA-256 hash is stored. `username` is set for `adapter_qbit` only: it is `[a-z0-9_-]{1,32}`, unique among qBit credentials, derived from the name when omitted (`"My Radarr"` becomes `my-radarr`), and the key is the password to configure next to it. `last_used_at` is updated at most once a minute, so the UI can show which media manager last connected.

- A credential is valid on its own adapter only: `/sabnzbd/api` takes `adapter_sab`, the qBittorrent adapter takes `adapter_qbit`. Every `/api/v1` route refuses it with `401`, and `/sabnzbd/api` refuses API keys and sessions (`{"status":false,"error":"API Key Incorrect"}`).
- The adapters never honour disable-auth or LAN bypass. The credential routes above do accept those UI principals, so a credential made under them keeps working after the bypass setting is turned off. The UI must say so.

### SABnzbd adapter

`/sabnzbd/api` (any method, also with a trailing slash) is the SABnzbd API emulation of [M4](milestones/m4-sabnzbd-adapter.md). It is outside the UI authentication and CSRF, and behind the setup gate (`503 setup_required` while setup is open). Its access log line holds the path and the `mode` only, because its query carries keys and, for `addurl`, the indexer's URL.

### Objects

**Download:**

```json
{"id":"…","name":"…","source_kind":"nzb","category_id":null,"state":"queued",
 "priority":"normal","queue_position":0,"paused":false,
 "failure_reason":"missing_articles","failure_detail":"…",
 "total_bytes":0,"done_bytes":0,
 "added_at":"…","fetched_at":null,"ready_at":null,"updated_at":"…","output_path":"…"}
```

`failure_reason`, `failure_detail` and `output_path` are left out when empty. `state` is one of `added`, `fetching_metadata`, `queued`, `fetching`, `fetched`, `post_processing`, `ready`, `failed`. Pausing is the `paused` flag, not a state.

**Category:** `{"id","name","dir_override":"/abs/path"|null}`.

**Server:**

```json
{"id":"…","name":"…","host":"news.example.com","port":563,"username":"…",
 "has_password":true,"password_unavailable":false,
 "max_connections":20,"retention_days":0,"priority":0,"enabled":true,"created_at":"…"}
```

The password is never returned: `has_password` says whether one is stored. `password_unavailable` is `true` when it is stored but cannot be decrypted (the key file changed or was lost), so the UI can ask for it again; a `PATCH` with `password` fixes it. Defaults on create: `port` 563, `max_connections` 8, `retention_days` 0 (unlimited), `priority` 0 (the Server Priority: the lowest number is tried first, equal numbers share load), `enabled` true. `max_connections` is 1 to 100. `host` is a bare host name or IP address (no scheme, port or path). **TLS is always on and always verified**: there is no field to turn either off, and sending one is `400 invalid_json`.

**ProbeResult** (the answer of both test routes, with status `200` even when the test failed, because the request worked and the Server did not):

```json
{"ok":false,"class":"tls","detail":"certificate_untrusted","message":"…","connect_ms":0,"latency_ms":0}
```

`class` is `ok`, `dns`, `tcp`, `tls`, `auth` or `protocol`. `detail` refines it: `dns` (`not_found`, `timeout`, `other`), `tcp` (`refused`, `timeout`, `unreachable`, `other`), `tls` (`certificate_untrusted`, `certificate_hostname`, `certificate_expired`, `certificate_invalid`, `not_tls`, `timeout`, `handshake`), `auth` (`rejected`, a wrong user name or password), `protocol` (`greeting`, `auth_response`, `date`, `other`). So the UI can tell "certificate invalid" (`tls` with a `certificate_*` detail) from "wrong password" (`auth`). `connect_ms` covers connecting, TLS, greeting and login; `latency_ms` is the round trip of `DATE`. The message never contains credentials.

**Settings:**

```json
{"queues":{"usenet":1,"torrent":5},
 "auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},
 "paths":{"usenet":{"incomplete":"…","complete":"…"},"torrent":{"incomplete":"…","complete":"…"}}}
```

### Adding a Download

`POST /api/v1/downloads` creates a Download from an NZB, in state `queued` at the end of the Usenet queue, and answers `201` with the Download. It takes one of two bodies; any other `Content-Type` is `415 unsupported_media_type`.

**Upload** (`multipart/form-data`): the file in part `nzb`, plus optional text parts `name`, `category_id`, `priority`, `password` and `paused` (`true`/`false`). The parts may come in any order. An unknown or repeated part, or a text part over 4 KiB, is `400`. The upload is streamed to disk and parsed on the way, never held in memory whole; the NZB may be up to 64 MiB (`413 nzb_too_large`; the whole request is capped a little above that).

**URL** (`application/json`): `{"url":"…","name":"…","category_id":"…","priority":"…","password":"…","paused":false}`. The daemon fetches the NZB at once, within the request:

- `http` and `https` only, also for redirects; at most 5 redirects; 30 seconds in total, body included; at most 64 MiB. A failure is `502 fetch_failed` (a timeout, a refused connection, too many redirects, a redirect to another scheme, an answer other than `200`), an oversize NZB is `413 nzb_too_large`, content that is not an NZB is `400 invalid_nzb`.
- The request carries no cookies, no `Authorization` header and no proxy: only credentials written in the URL itself are used. It sends a `bitnews` `User-Agent`.
- **The URL is never stored.** It is logged and returned in errors only in redacted form (`apikey=` and similar values masked), and the indexer's response body is never echoed.
- **Private, loopback and link-local addresses are allowed on purpose.** bitnews is a LAN tool and Prowlarr or the indexer is usually on a private address (`http://prowlarr:9696`), so there is no SSRF address filter. The protection is that the route needs a write key or a session, and that only http(s) is spoken. Run bitnews on a network where that is acceptable.

How the fields are resolved:

- `category_id` must name an existing Category, else `400 unknown_category`. Without it the NZB's own category is matched by name (case-insensitive); one that matches no Category is ignored.
- `priority` is `force`, `high`, `normal` (default) or `low`. `paused: true` adds the Download paused.
- `name` is a display name (never used as a path). Order: the request, the NZB's own name, the uploaded file's name without `.nzb`, the first file's name, else `nzb-<id>`. Control and format characters are removed and it is cut at 255 bytes. A trailing `{{password}}` is taken out of it.
- The archive password: the request's, else the NZB's, else the `{{password}}` suffix of the name. It is stored encrypted and never returned.
- The NZB is kept as `<usenet incomplete>/<download id>/<name>.nzb` (mode `0600`; the file name goes through `fsutil.SafeName`). `total_bytes` is the size of all files except PAR2 recovery volumes, which are fetched only when repair needs them.
- The Download is stored as `added`, then its source, and only then moved to `queued`, so a Usenet runner never sees a `queued` Download without its NZB. `download_added` is published after that, with the `queued` Download. If any step fails the Download and its files are removed.
- An NZB that does not parse is `400 invalid_nzb`, and nothing is left on disk. The same goes for any failure after the upload started.

### Paging

Downloads are listed oldest first. `next` is an opaque cursor: pass it as `after` to get the following page. It is `null` on the last page. Downloads added between two calls never duplicate or skip an existing one.

## Event stream

`GET /api/v1/events` streams what happens, as server-sent events:

```
event: hello
data: {"instance_id":"…"}

event: download_updated
data: { …Download… }

: ping
```

- `hello` comes first. `instance_id` changes when the daemon restarts.
- A `: ping` comment is sent every 15 seconds.
- Events carry JSON in one `data:` line:

  | Event | Payload |
  |---|---|
  | `download_added`, `download_updated` | the full Download |
  | `download_progress` | `{"id","done_bytes","total_bytes","bytes_per_second"}` (at most about 1/s per Download) |
  | `fetched` | `{"id"}` |
  | `ready` | `{"id","output_path"}` |
  | `failed` | `{"id","reason","detail"}` |
  | `removed` | `{"id"}` |
  | `settings_changed` | `{}` (fetch `/settings`) |
  | `servers_changed` | `{}` (fetch `/servers`) |
  | `postprocess_stage` | `{"id","stage"}`, stage is `verifying`, `repairing`, `unpacking` or `finishing` |
  | `usenet_queue_paused` | `{"reason","detail"}`, reason is `user` or `disk_full` |
  | `usenet_queue_resumed` | `{"reason":"","detail":""}` |

- If the client falls behind (its 256-event buffer fills), the server sends `event: resync` with `data: {}` and ends the stream. Reconnect and refetch.
- **`Last-Event-ID` is not supported.** Events are not persisted, so nothing is replayed: refetch the state after every (re)connect. The stream sends no `id:` field.
- The stream ends when the client disconnects or the server shuts down. The server's write timeout does not apply to it; each single write is limited to 30 seconds, so a client that stops reading is dropped.
- Authentication is the same as for the other `read` routes. A browser `EventSource` sends the session cookie. Scripts send an API key header.
