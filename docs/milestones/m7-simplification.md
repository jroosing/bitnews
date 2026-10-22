# M7 — Simplification

## Goal

Remove what the operating system already does and what the user is responsible for, and replace per-client credentials with one API key. The decisions and reasoning are in [ADR-0006](../adr/0006-simplify-filesystem-permissions-and-api-key.md).

## Scope

**In:** shape-only path validation, removing the same-filesystem warning, one free-space helper, removing the privilege drop, a single API key for SABnzbd, qBittorrent and the own API, and removing dead code.

**Out:** the restart-only settings machinery and `MoveDir` (both stay as they are), any new feature.

## Issues

### M7-01: Paths are the user's responsibility
- **Labels:** area:ui, area:core, ready-for-agent
- **Depends on:** M6-05

**Goal:** Saving paths checks their shape only, and nothing in bitnews depends on the kind of filesystem.

**Work**
- Remove `fsutil.SameFilesystem` (both OS files) and the cross-filesystem warning in Settings → Paths and Categories. Say in the UI hint and in `docs/deploy.md` that the user keeps the complete directory and the library on one filesystem for Sonarr and Radarr hardlinks.
- Remove the `pathChecker`/`osPathChecker`, the exists and writable checks and their tests. Validation on save: absolute, clean (no `..`), and the roots do not nest in a way that breaks the move.
- Create missing directories with `MkdirAll` at first use. When a write fails with a permission error, fail the download with a message naming the folder and the user id bitnews runs as.
- Remove the unused settings placeholder: `settings_soon.html`, `SettingsSection.Planned`, `settingsPlaceholderTemplate` and its README text.
- Keep "rename, else copy" in `MoveDir`.

**Acceptance criteria**
- [ ] No code mentions `SameFilesystem`, and no UI text claims a filesystem check.
- [ ] A relative or `..` path is rejected on save with a per-field error; a missing but well-formed path saves.
- [ ] A download into an unwritable folder fails with the folder and uid in the reason.

### M7-02: One free-space helper
- **Labels:** area:core, ready-for-agent
- **Depends on:** M7-01

**Goal:** The `min_free` behaviour stays and the five copies of per-OS code become one.

**Work**
- Add `fsutil.FreeBytes(path)` (unix `statfs`, a fallback that reports "unknown" elsewhere). Replace the copies in `usenet/runner`, `torrent/runner`, `postproc/unpack` and `daemon`.
- Behaviour, settings, the `disk_full` pause and its banner do not change.

**Acceptance criteria**
- [ ] `freespace_*.go` / `diskspace_*.go` copies are gone, and the existing disk-full tests pass unchanged.

### M7-03: Remove the privilege drop
- **Labels:** area:deploy, ready-for-agent

**Goal:** bitnews runs as whoever starts it.

**Work**
- Delete `internal/privdrop`, the `PUID`/`PGID` options in bootstrap, and the "refuse PUID when not root" logic.
- Dockerfile: run as a non-root default user that `user:` can override; drop the CAP_SETUID/CAP_SETGID guidance. Compose example uses `user: "1000:1000"`. Update `deploy/bitnews.service`, `deploy/bitnews.env.example`, `docs/deploy.md` and the bootstrap docs.
- Keep `UMASK` (default `002`) and say why in the docs. Running as root works without a warning.

**Acceptance criteria**
- [ ] No reference to `PUID`, `PGID` or `privdrop` remains outside the ADR.
- [ ] The config reference lists `UMASK` and no identity option.

### M7-04: A single API key
- **Labels:** area:auth, area:adapter, ready-for-agent
- **Depends on:** M1-12, M4-02, M5-10

**Goal:** One plain-text, rotatable key authenticates SABnzbd, qBittorrent and the own API.

**Work**
- A migration with one `app_key` row holding the key as plain text, and dropping the API-key and adapter-credential tables. The key is generated on first start. A service offers `Current`, `Rotate` and `Authenticate` (constant time, against the in-memory copy).
- SABnzbd `apikey`, qBit login (username `bitnews`, password the key) and `/api/v1` Bearer all use it, with full access. Remove scopes, named keys, per-manager credentials and the code and tests around them.
- Rotate takes effect at once for every surface.

**Acceptance criteria**
- [ ] The key authenticates each of the three surfaces; a wrong key or username fails each.
- [ ] After Rotate the old key is rejected on all three, and the new one works.
- [ ] The key is never logged, and it is stored as plain text, as Sonarr and Radarr do.

### M7-05: Security page key card
- **Labels:** area:ui, area:auth, ready-for-agent
- **Depends on:** M7-04, M6-08

**Goal:** Settings → Security shows the one key.

**Work**
- Replace the API keys and media-manager credentials cards with one card: the key in plain sight, Copy and Rotate (with a confirm that says every client stops working), and copy-ready values for Sonarr/Radarr (host, port, URL base `sabnzbd`, qBit username `bitnews`).
- Remove the show-once block and its `no-store` special case.
- Keep the sign-in options, hosts, proxies and password cards as they are.

**Acceptance criteria**
- [ ] The key is shown on the page, and Rotate needs the confirm.
- [ ] A test confirms the other cards are unchanged and the old routes are gone.
