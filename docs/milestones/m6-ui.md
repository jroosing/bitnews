# M6 — Web UI

## Goal

By the end of M6, the daemon itself serves a browser UI built with htmx and server-rendered HTML. Users can watch the Queues live, inspect the history, and configure everything that has so far only been reachable through the API: paths, Categories, Servers, torrent network settings, speed and Share Limits, auth options, API keys and media-manager credentials. There is no JS build step and no inline script, and a strict CSP applies.

## Scope

**In:** the web foundation (templates, assets, CSP, CSRF wiring), login and setup, a live queue, history and Download detail, and all settings pages.

**Out:** multi-user, theming beyond light/dark, mobile apps, i18n (English only in v1), charts and statistics.

## Design

### Architecture

- The UI handlers live in `internal/web`. They call the **same application services** as the own API v1 (the M1-13 service layer) and render HTML fragments. They don't go through the JSON API, because htmx works with HTML responses.
- Business logic stays in core and services, never in templates or handlers.
- Templates use `html/template` (contextual auto-escaping) and are embedded with `embed.FS`. Assets are embedded the same way and served with `Cache-Control: immutable` under a content hash path.
- **htmx and its SSE extension are vendored** as pinned files with their versions recorded in `web/static/VENDOR.md`. They are served from the daemon, never a CDN, so nothing loads from a third-party origin.

### Security

- **CSP:**
  `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`
- No inline scripts or styles, and no `hx-on` attributes.
- htmx config: `htmx.config.allowEval = false`, `htmx.config.allowScriptTags = false` and `htmx.config.selfRequestsOnly = true`.
- **CSRF:** the token from the session goes into a `<meta>` tag. A small vendored `app.js` (the only first-party JS) adds `X-CSRF-Token` to every htmx request through `htmx:configRequest`. Plain form posts carry a hidden field.
- **Auth banner:** when disable auth or LAN bypass is active (from M1-11), every page shows a persistent warning banner that can't be dismissed.
- All dynamic content is escaped by `html/template`. Download names are hostile input (they come from NZBs and torrents), so a test renders malicious names.

### Live updates

- The queue page opens an SSE connection to `/api/v1/events`. This uses the session, so the read scope is implied.
- Rows are swapped through out-of-band fragments. The server renders a row fragment per affected Download, through a UI-specific SSE route `/ui/events` that converts bus events into named HTML-fragment events.
- On `resync`, or when the page reconnects, the whole queue table is refetched.

### Pages

| Page | Content |
|---|---|
| Setup / Login | Setup with the setup token (M1-10), and login with rate-limit feedback |
| Queue | Both Queues, with live progress, rate, ETA, state and stage. Actions: pause/resume, Download Priority, move top/bottom, change Category, remove (with a data-deletion option). Global pause per Queue and the current speed limits. |
| History | Ready and Failed Downloads with filters (Category, kind, state) and paging. Failed rows show a readable failure reason. |
| Download detail | Source info, files, Missing/damaged counts, Post-processing stage log, output path. Torrents also show peers, ratio and Share Limit. |
| Settings → Paths & Categories | Per-protocol incomplete/complete roots, Category CRUD with dir overrides, and same-filesystem warnings checked when saving |
| Settings → Usenet Servers | CRUD, Server Priority ordering, retention, connections, and a "Test" button with the specific error class. The password field is write-only. |
| Settings → Torrent network | Listen port, DHT/PEX/LSD, MSE policy, bind interface/address per protocol (with live status), global Share Limit defaults |
| Settings → Speed & queue | Global and per-protocol limits, Queue concurrency, `postproc_concurrency`, `min_free` |
| Settings → Security | Disable auth / LAN bypass (CIDRs), `allowed_hosts`, `trusted_proxies` (each with a warning explanation), password change, API keys, media-manager credentials with `last_used_at`. Plaintext secrets are shown once, at creation. |

### Accessibility and responsiveness

Semantic HTML, labels on every input, and keyboard-operable actions. The layout works at 360 px width; tables collapse into cards on narrow screens. It follows `prefers-color-scheme` for light/dark.

## Exit criteria

- Every setting from M1 through M5 can be configured from the UI, so none needs the API.
- A CSP violation report check finds no violations in any page (run headless with a CSP report endpoint in the e2e test).
- Malicious Download names (`<script>`, `"><img onerror>`, RTL override characters) render inert.
- The queue updates live without polling, and it resyncs after a server restart.

## Issues

### M6-01: Web foundation — templates, assets, CSP and CSRF wiring
- **Labels:** area:ui, ready-for-agent
- **Depends on:** M1-13, M1-14

**Goal:** The `internal/web` skeleton, following the M6 "Architecture" and "Security" design.

**Work**
- A layout template, embedded templates and assets with content-hash paths and immutable caching.
- Vendored, pinned htmx and the SSE extension (with `VENDOR.md`), plus a minimal `app.js` that sets the htmx config and attaches the CSRF header.
- The strict CSP from the design on all UI routes.
- The auth-mode warning banner, wired to `/api/v1/auth/session` data.
- Base CSS covering light/dark, responsive tables and accessible focus styles.

**Acceptance criteria**
- [ ] A headless browser test finds no CSP violations on an empty page.
- [ ] htmx requests carry the CSRF header, and a request without it is rejected.
- [ ] The banner shows when either auth option is on.

### M6-02: Login and setup pages
- **Labels:** area:ui, area:auth, ready-for-agent
- **Depends on:** M6-01, M1-10, M1-11

**Goal:** The browser flows for first-run setup and login.

**Work**
- A setup page that asks for the setup token, a password and its confirmation, and enforces the 12-character minimum.
- A login page with rate-limit feedback ("try again in N s"), plus logout.
- Redirect rules: no user goes to setup, no session goes to login, and a session on login goes to the queue.

**Acceptance criteria**
- [ ] An e2e test covers setup → logout → login → wrong password backoff.

### M6-03: Queue page with live SSE updates and actions
- **Labels:** area:ui, ready-for-agent
- **Depends on:** M6-01, M2-11, M5-04

**Goal:** Live view and control of both Queues.

**Work**
- Render both Queues with state, stage, progress, rate, ETA, Category and Download Priority.
- Add the `/ui/events` SSE route, which turns bus events into row fragments swapped out of band, with a full refetch on `resync` or reconnect.
- Actions: pause/resume, priority, top/bottom, change Category, remove (with a data-deletion confirmation), global pause per Queue.
- Show the current speed limits and the paused reasons (`disk_full`, `bind_unavailable`).

**Acceptance criteria**
- [ ] An e2e test with the fake engines shows progress updating without polling and actions taking effect.
- [ ] A test confirms a malicious Download name renders inert.

### M6-04: History page and Download detail
- **Labels:** area:ui, ready-for-agent
- **Depends on:** M6-01, M3-01

**Goal:** Browse Ready and Failed Downloads and inspect any Download in detail.

**Work**
- History with filters (Category, kind, state) and cursor paging.
- Failure reasons in human-readable form. The redacted detail is available in the detail view.
- A detail page with source info, files, Missing/damaged counts, the stage log and the output path. Torrents also show peers, ratio and Share Limit.

**Acceptance criteria**
- [ ] Tests cover paging, filters, each failure reason's text, and torrent versus Usenet detail.

### M6-05: Settings — paths and Categories
- **Labels:** area:ui, ready-for-agent
- **Depends on:** M6-01, M3-10

**Goal:** Configure the per-protocol roots and Categories from the UI.

**Work**
- Per-protocol incomplete/complete roots with validation (exists, writable), and same-filesystem warnings on save that explain the hardlink impact.
- Category CRUD with an optional dir override. Note in the UI that a change only affects new Downloads.

**Acceptance criteria**
- [ ] Validation errors and cross-filesystem warnings render.
- [ ] Changes persist and apply to the next Download.

### M6-06: Settings — Usenet Servers
- **Labels:** area:ui, area:usenet, ready-for-agent
- **Depends on:** M6-01, M2-04

**Goal:** Manage Servers from the UI.

**Work**
- CRUD with Server Priority ordering, retention, connection count and enable/disable.
- A write-only password field: an empty value keeps the existing password.
- A "Test" button that shows the specific error class (DNS, TCP, TLS/certificate, auth, protocol).
- No plaintext or skip-verify controls, because the model has no such fields.

**Acceptance criteria**
- [ ] A test confirms the password is never rendered in HTML.
- [ ] The test button shows each error class against the fake NNTP server.

### M6-07: Settings — torrent network, speed, queue and Share Limits
- **Labels:** area:ui, area:torrent, ready-for-agent
- **Depends on:** M6-01, M5-06, M5-07

**Goal:** Configure the network and throughput settings.

**Work**
- Listen port, DHT/PEX/LSD and the MSE policy.
- Bind interface or address per protocol, with live status (bound, unavailable).
- Global and per-protocol speed limits, Queue concurrency, `postproc_concurrency` and `min_free`.
- Global Share Limit defaults and the action.

**Acceptance criteria**
- [ ] Every field validates and persists, and runtime-changeable settings apply without a restart (test with the fake engines).

### M6-08: Settings — security, API keys and media-manager credentials
- **Labels:** area:ui, area:auth, ready-for-agent
- **Depends on:** M6-01, M1-12, M4-02

**Goal:** Manage auth options and credentials from the UI.

**Work**
- Disable auth and LAN bypass (CIDRs), each with an explanatory warning. Require `allowed_hosts` before enabling them, and edit `trusted_proxies`.
- Password change, which requires the current password.
- API key CRUD (scope, last used), with the plaintext shown once.
- Media-manager credentials for SAB and qBit: create, revoke and view `last_used_at`. Show copy-ready values for Sonarr/Radarr (host, port, URL base, key or username/password).

**Acceptance criteria**
- [ ] Enabling LAN bypass without `allowed_hosts` is blocked, with an explanation.
- [ ] Secrets appear only once and never in later page loads.
- [ ] Revoking a credential takes effect immediately (the media manager's next request fails).
