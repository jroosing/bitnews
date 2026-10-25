# The user owns the filesystem and the process identity; one API key serves every client

bitnews stops checking and managing things the operating system already handles, and replaces per-client credentials with a single key.

## Filesystem

- The user configures the download paths. Whether a path is a local disk or an NFS share does not matter to bitnews, and bitnews does not special-case either.
- bitnews does not probe a path for existence or writability when it is saved. A path is only checked for its shape: absolute, no `..`, and the four roots must not nest in a way that breaks the move. Missing directories are created at first use. A path bitnews cannot write to fails the download with a message that names the folder and the user id bitnews runs as.
- Hardlinks are made by Sonarr and Radarr, from the complete directory into the library. bitnews never hardlinks, and it cannot know where the library is, so the old warning about incomplete and complete being on different filesystems was misleading and is gone. The documentation tells the user to keep the complete directory and the library on one filesystem. Moving a finished download is "rename, else copy", so incomplete on an SSD and complete on a share keeps working.
- Free space is still protected by `min_free`. One helper, `fsutil.FreeBytes`, replaces the per-component copies.

## Process identity

> **The container part is superseded by [ADR-0010](0010-puid-pgid-entrypoint.md):** the image's entrypoint takes `PUID`/`PGID` again and drops to them. bitnews itself still has no privilege drop and runs as whoever starts it.

- bitnews runs as whoever starts it. `PUID`/`PGID` and the in-process privilege drop are removed. Docker uses `user: "uid:gid"` and systemd uses `User=`/`Group=`, and they take care of permissions. Running as root still works, without a warning.
- `UMASK` stays (default `002`), so the files are group-writable for Sonarr and Radarr running as another user in the same group.

## One API key

- There is one key, generated on first start, shown in Settings → Security with a Copy button and a Rotate button. It authenticates every client:
  - SABnzbd: the `apikey` parameter.
  - qBittorrent: the password, with the fixed username `bitnews`.
  - Own API (`/api/v1`): `Authorization: Bearer <key>`, full access.
- The key is stored as plain text in the database and shown as is on the settings page, the way Sonarr and Radarr keep theirs. There is no encryption and no master-key involvement ([ADR-0004](0004-master-key-in-config-dir.md) only covers Usenet and archive passwords). The daemon loads it at start and compares requests against the in-memory value in constant time.
- Named API keys, read/write scopes and per-media-manager credentials are removed, along with their tables.

## Consequences

- No read-only key for scripts. Rotating the key breaks every client until they are updated; the UI says so.
- A typo in a path shows up at the first download instead of when saving.
- A compose or env file written for `PUID`/`PGID` stops working. No release has shipped, so there is nothing to migrate.
- The key is visible to anyone logged in to the UI, and a copy of `bitnews.db` exposes it. That is the same exposure Sonarr and Radarr have, and the user is responsible for protecting the config directory. Rotate the key after sharing a database copy.
