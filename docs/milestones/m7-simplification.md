# M7 — Simplification

## Goal

Remove what the operating system already does and what the user is responsible for, and replace per-client credentials with one API key. The decisions and reasoning are in [ADR-0006](../adr/0006-simplify-filesystem-permissions-and-api-key.md).

## Scope

**In:** running 7-Zip and par2cmdline-turbo instead of the pure-Go post-processing (M7-06, [ADR-0007](../adr/0007-external-postproc-tools.md)), shape-only path validation, removing the same-filesystem warning, one free-space helper, removing the privilege drop, a single API key for SABnzbd, qBittorrent and the own API, and removing dead code.

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

### M7-06: Post-processing with 7-Zip and par2cmdline-turbo
- **Labels:** area:postproc, area:deploy, ready-for-agent
- **Depends on:** M3-06, M3-08

**Goal:** Replace the pure-Go PAR2 and RAR/7z code with the reference tools, as decided in [ADR-0007](../adr/0007-external-postproc-tools.md) (reverses ADR-0002).

**Work**
- `internal/postproc/tool`: the only place that starts a process (no shell, scrubbed environment, capped output, timeout, process-group kill). `scripts/get-postproc-tools.sh` pins both tools by version and SHA-256 (`make tools`).
- Unpack runs `7zz`: list first and enforce the entry, size and ratio caps, extract to staging, then walk the result and refuse symlinks, special files and escaping paths. Passwords go through stdin. Delete the pure-Go RAR/7z code, the 7z header guard (M3-12) and the `rardecode`/`sevenzip` dependencies.
- PAR2 verify and repair run `par2`; parse its output into the existing `VerifyOutcome`. A small Go pre-check refuses hostile file names inside a set before par2 runs. Delete the pure-Go PAR2 and GF(2^16) code.
- The daemon refuses to start when `7zz` or `par2` is missing from `PATH`.
- Docker image: static `7zz` and `par2` for amd64 on distroless. CI installs them for the tests.

**Acceptance criteria**
- [ ] The orchestrator and the core ports are unchanged, and the post-processing end-to-end tests pass with the real tools.
- [ ] Crafted archives and PAR2 sets (traversal names, symlinks, bombs, wrong password, truncation, missing volume) end in the right core error and write nothing outside the destination.
- [ ] No pure-Go PAR2 or RAR/7z code or dependency remains, and `os/exec` is imported only by `internal/postproc/tool`.
- [ ] The image runs both tools on amd64.

### M7-07: PUID/PGID in the container entrypoint
- **Labels:** area:deploy, ready-for-agent

**Goal:** A first `docker compose up` works without preparing `/config` by hand ([ADR-0010](../adr/0010-puid-pgid-entrypoint.md), which reverses the container part of M7-03).

**Work**
- `scripts/docker-entrypoint.sh`: as root, check that `PUID`/`PGID` (default 65532) are numeric, create `/config`, re-own it to `PUID:PGID` when it is not theirs yet (`chown -R -h`), and `exec su-exec PUID:PGID /bitnews`. As anyone else, `exec /bitnews`. The healthcheck goes straight to `bitnews healthcheck`.
- Dockerfile: Alpine final stage with `ca-certificates`, `tzdata` and `su-exec`; `PUID`/`PGID` env defaults; entrypoint and healthcheck through `/entrypoint.sh`; starts as root.
- Compose example: `PUID`/`PGID` instead of `user:`, `cap_add` CHOWN, SETUID, SETGID, DAC_READ_SEARCH. Docs follow.

**Acceptance criteria**
- [x] A missing bind-mount `/config` (created by Docker as root) works on the first start with `PUID=988`; bitnews runs as 988 and its files are 988's.
- [x] Changing `PUID` re-owns `/config` on the next start; `user:` skips the hand-over; a non-numeric `PUID` stops with a message.
- [x] The healthcheck reports healthy with the hardened compose options.

