# Own API v1

The own API is served on the daemon's one port under `/api/v1/`. The UI (M6) uses the same routes with a session cookie. Scripts use an API key. This page is the route table; the design is in [M1](milestones/m1-foundation.md#http-server-and-own-api-v1).

## Conventions

- **Bodies** are JSON (`Content-Type: application/json`, else `415 unsupported_media_type`). Decoding is strict: unknown fields, a wrong type and trailing data are `400 invalid_json`. Bodies are capped (16 KiB, settings 1 MiB; over the cap is `413 request_too_large`).
- **Errors** always have this shape, with a stable snake_case `code`:

  ```json
  {"error": {"code": "invalid_priority", "message": "unknown priority \"urgent\": use force, high, normal or low"}}
  ```

  Status codes: `400` invalid input, `401` not authenticated, `403` forbidden (scope, CSRF, session required), `404` not found, `405` wrong method, `409` conflict, `413` too large, `415` wrong media type, `421` Host not allowed, `429` login rate limit (with `Retry-After`), `500` `internal` (generic; the detail is only in the server log), `503` `setup_required`.
- **Request IDs:** every response carries `X-Request-ID`. A client-supplied one is kept only if it is 1 to 64 characters of `[A-Za-z0-9._-]`; otherwise the server makes one. It appears in the server log.
- **Security headers** on every response: `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`. API responses are `Cache-Control: no-store`.
- **IDs** (Downloads, Categories, API keys) are opaque strings. Times are RFC 3339 UTC.

## Authentication

| Auth | Meaning |
|---|---|
| none | No credentials. |
| setup token | The one-time token from the startup log. |
| read / write | An API key of that scope (`Authorization: Bearer bn_...` or `X-Api-Key`), or else a UI session. A `read` key may only use GET; a `write` key may use every method. A request that carries a key is judged as a key request and never falls back to the session. |
| session | A logged-in UI session (cookie `bitnews_session`). State-changing requests also need the `X-CSRF-Token` header from `GET /auth/session` or the login response. API keys are refused. |

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
| GET | `/api/v1/downloads/{id}` | read | | `200 Download` |
| POST | `/api/v1/downloads/{id}/pause` | write | | `200 Download`; `409 not_pausable` once Fetched |
| POST | `/api/v1/downloads/{id}/resume` | write | | `200 Download` |
| PATCH | `/api/v1/downloads/{id}` | write | any of `{"priority":"force\|high\|normal\|low","category_id":"<id>"\|null,"queue_position":{"move":"top\|bottom\|up\|down"}}` | `200 Download`; `409 not_queued` for a move of a Download that is Fetched or later. The state cannot be changed. |
| DELETE | `/api/v1/downloads/{id}?delete_data=true` | write | | `204`. The row is deleted and a `removed` event follows. Data is deleted only with `delete_data=true` (engines hook in later). |
| GET | `/api/v1/categories` | read | | `200 {"items":[Category]}` |
| POST | `/api/v1/categories` | write | `{"name","dir_override"?}` | `201 Category`; `400 invalid_name`, `400 invalid_dir_override`, `409 name_taken` |
| PATCH | `/api/v1/categories/{id}` | write | any of `{"name","dir_override":"/abs/path"\|null}` | `200 Category` |
| DELETE | `/api/v1/categories/{id}` | write | | `204`. Downloads that used it lose it. |
| GET | `/api/v1/settings` | read | | `200 Settings` |
| PUT | `/api/v1/settings` | write | the full `Settings` object | `200 Settings`; `400 invalid_settings` lists every problem. A request with an API key may not change the `auth` section (`403 session_required`). |
| GET | `/api/v1/api-keys` | session only | | `200 {"items":[{"id","name","scope","created_at","last_used_at"}]}` |
| POST | `/api/v1/api-keys` | session only | `{"name","scope":"read\|write"}` | `201` the key's fields plus `"key":"bn_..."`, shown this once |
| DELETE | `/api/v1/api-keys/{id}` | session only | | `204` |
| GET | `/api/v1/events` | read | | `200 text/event-stream`, see below |

Routes that need a session are also refused to the disable-auth and LAN bypass principals (`403 session_required`).

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

**Settings:**

```json
{"queues":{"usenet":1,"torrent":5},
 "auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},
 "paths":{"usenet":{"incomplete":"…","complete":"…"},"torrent":{"incomplete":"…","complete":"…"}}}
```

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

- If the client falls behind (its 256-event buffer fills), the server sends `event: resync` with `data: {}` and ends the stream. Reconnect and refetch.
- **`Last-Event-ID` is not supported.** Events are not persisted, so nothing is replayed: refetch the state after every (re)connect. The stream sends no `id:` field.
- The stream ends when the client disconnects or the server shuts down. The server's write timeout does not apply to it; each single write is limited to 30 seconds, so a client that stops reading is dropped.
- Authentication is the same as for the other `read` routes. A browser `EventSource` sends the session cookie. Scripts send an API key header.
