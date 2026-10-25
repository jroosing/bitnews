# The container takes PUID/PGID and drops to them in its entrypoint

The image starts as root, its entrypoint gives `/config` to `PUID:PGID` and runs bitnews as that user, the way the linuxserver.io images (Sonarr, Radarr, qBittorrent, NZBGet) do. This brings back what [ADR-0006](0006-simplify-filesystem-permissions-and-api-key.md) removed for the container, and keeps bitnews itself as it is: no privilege drop in the binary, it runs as whoever starts it.

## Why

With `user: "uid:gid"` alone, the first start on a new server failed: Docker creates a missing bind-mount directory on the host as root, and a non-root process can neither write it nor take it over. The user had to `mkdir` and `chown` by hand before the first `docker compose up`, which every other container in a homelab stack spares them. Setting `PUID`/`PGID` and letting the container fix `/config` is what those users expect.

## How

- A shell entrypoint (`scripts/docker-entrypoint.sh`), not Go: the root part is three commands (`mkdir -p`, `chown -R -h`, `su-exec`), outside the binary, and easy to read. The image moves from distroless to Alpine for the shell, `chown` and `su-exec`; CA certificates and tzdata come from Alpine packages. The other homelab images already ship a shell, so this does not change what the stack exposes.
- Started as root: `PUID`/`PGID` (default `65532`) must be numeric. `/config` is created if missing and re-owned, recursively and without following symlinks, only when it is not owned by `PUID:PGID` yet, so a normal restart reads nothing. Then `exec su-exec PUID:PGID /bitnews`. `PUID=0` runs bitnews as root with a warning. The healthcheck runs without re-owning anything.
- Started as anyone else (`user:`, `--user`): bitnews runs directly as that user; `PUID`/`PGID` and the hand-over are skipped. bitnews still stops at start with the `chown` command when `/config` is not writable.
- `/data` is never re-owned: it can be a whole media library, and its permissions are the user's.
- Capabilities: with `cap_drop: ALL`, the entrypoint needs `CHOWN`, `SETUID`, `SETGID` and `DAC_READ_SEARCH` (to read a `/config` that an earlier PUID owns with mode `0700`). A process that switches from root to a normal user keeps no capabilities, so bitnews runs with none.

## Consequences

- The image has a shell and a package database. Pin `alpine` to a digest and rebuild for Alpine security fixes as for the two tools.
- The compose example sets `PUID`/`PGID` and the four capabilities instead of `user:`.
- A compose file with `user:` keeps working unchanged.
