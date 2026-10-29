# Deploying bitnews

bitnews is one static binary that listens on one port (default `9494`) for the UI, its own API and the Sonarr/Radarr adapters. It speaks plain HTTP; put a reverse proxy in front of it for TLS. Docker is the supported way to run it: an Alpine-based image and `deploy/docker-compose.example.yml`, aimed at a homelab. [Running without Docker](#without-docker) is possible but not packaged.

Then read the [hardening checklist](#hardening-checklist).

## Bootstrap settings

Only these settings live outside the database. A flag wins over its environment variable, and an empty variable counts as unset. Everything else is configured in the UI or API.

| Flag | Env | Default | Notes |
|---|---|---|---|
| `--config-dir` | `BITNEWS_CONFIG_DIR` | `$XDG_DATA_HOME/bitnews` | Holds `bitnews.db` and the master key `bitnews.key`. Image: `/config`. Absolute path. |
| `--key-file` | `BITNEWS_KEY_FILE` | `bitnews.key` inside the config dir | Master key that encrypts the stored Server and archive passwords. Generated on first start. Set it only to keep the key apart from the database (see [The master key](#the-master-key)). Absolute path. |
| `--listen` | `BITNEWS_LISTEN` | `0.0.0.0:9494` | `host:port`. |
| `--log-level` | `BITNEWS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `--log-format` | `BITNEWS_LOG_FORMAT` | `text` | `text` or `json`. |
| – | `UMASK` | `002` | Octal file creation mask, applied process-wide at startup before any file is created. See [Identity and UMASK](#identity-and-umask). |
| – | `BITNEWS_ADMIN_PASSWORD_FILE` | unset | Absolute path of a file holding the initial admin password; skips the setup screen. |
| – | `BITNEWS_TORRENT_PORT` | unset (the setting, `51413`) | TCP and uTP port the torrent engine listens on, 1 to 65535. When set it overrides the port in Settings, Torrent network. See [Torrent port](#torrent-port). |

`bitnews --help` prints the same list and `bitnews --version` prints the version and commit.

On first start (no admin yet), bitnews prints a one-time **setup token** in its log. The setup screen asks for it, so nobody else on the LAN can claim a fresh instance. Read it with `docker compose logs bitnews`.

## Docker

The image holds bitnews and the two programs it runs for post-processing ([ADR-0007](adr/0007-external-postproc-tools.md)): 7-Zip (`7zz`) and par2cmdline-turbo (`par2`), static binaries pinned by version and checksum. Archives and PAR2 sets are untrusted input and these are native programs, so keep the container hardening from the compose example (bitnews running as PUID, read-only root with a `tmpfs` at `/tmp`, `cap_drop: ALL` with only the four capabilities the entrypoint needs, `no-new-privileges`) and rebuild the image when either tool publishes a security fix. Consider a memory limit (`mem_limit`) too: 7-Zip can use a lot of memory on a large archive.

### Build the image

```sh
docker build -t bitnews \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg COMMIT="$(git rev-parse --short HEAD)" .
```

The build stage compiles with `CGO_ENABLED=0`. The supported image is linux/amd64, and it is what CI publishes (a plain `docker build`); there is no arm64 image. The final stage is `alpine`: the binary, the two tools, CA certificates, tzdata (so `TZ` works) and `su-exec`, which the entrypoint uses to drop from root to PUID:PGID ([ADR-0010](adr/0010-puid-pgid-entrypoint.md)).

Pinning is a hardening step. Tags are mutable; pin both base images to a digest for reproducible builds:

```sh
docker buildx imagetools inspect golang:1.27.3                  # note "Digest: sha256:..."
docker buildx imagetools inspect alpine:3.21
```

Then write `FROM golang:1.27.3@sha256:<digest>` and `FROM alpine:3.21@sha256:<digest>` in the `Dockerfile`. Renovate or Dependabot can keep the digests current.

### Publishing the image by hand

CI builds and pushes the image only for a release (a tag push), so `latest` is the newest release. You can do the same from your own machine, for example to publish a build of `main` or when CI is unavailable. The image is linux/amd64 only; building it on an Apple Silicon Mac works without emulation, because the Go compile cross-compiles and the two tools are static downloads.

1. Create a GitHub personal access token (classic) with the `write:packages` scope.
2. `docker login ghcr.io -u <your GitHub user>` and paste the token as the password.
3. `make image-push` builds `ghcr.io/jroosing/bitnews` and pushes it as `latest` and as the version (`git describe`). Set `REGISTRY_IMAGE=...` for another repository.

The compose example pulls `ghcr.io/jroosing/bitnews:latest`; a new ghcr.io package is private until you make it public (package settings), so until then the Docker host needs `docker login ghcr.io` with a token that has the `read:packages` scope.

### The master key

The master key encrypts the secrets bitnews has to send in plaintext later: the Usenet Server passwords and the passwords of encrypted archives. (Login passwords and session tokens are only hashed and need no key. The API key is stored in plain text in the database, like the key of Sonarr and Radarr, so that Settings can show it.)

You do nothing: on first start bitnews generates the key and stores it as `bitnews.key` in the config directory (`/config`, mode `0600`), and logs a warning with its location. It holds 32 random bytes, base64-encoded, plus a newline.

- **Back up the whole config directory.** The key and the database belong together. If the key is lost, the stored Server passwords cannot be decrypted and must be entered again (the UI shows which Servers need it); nothing else is lost. Never replace the key of an existing installation with a new one.
- **What it protects.** A copy of `bitnews.db` alone, such as a database dump attached to a support request, does not expose your provider logins. It does not protect against someone who can read the whole config directory. If that matters to you, keep the key elsewhere (below).

**Optional: keep the key apart from the database.** Point `BITNEWS_KEY_FILE` at a file outside `/config`, for example a Docker secret:

```sh
mkdir -p secrets
head -c 32 /dev/urandom | base64 > secrets/bitnews_key
chmod 600 secrets/bitnews_key
sudo chown 1000:1000 secrets/bitnews_key      # your PUID:PGID
```

```yaml
services:
  bitnews:
    environment:
      BITNEWS_KEY_FILE: /run/secrets/bitnews_key
    secrets: [bitnews_key]
secrets:
  bitnews_key:
    file: ./secrets/bitnews_key
```

bitnews reads the key as the user it runs as (PUID:PGID, or `user:`). Only `/config` is re-owned on start, never a key mounted elsewhere. Under plain `docker compose` (no Swarm) a `file:` secret is a bind mount of the host file, so the host file's owner and mode apply inside the container; a root-owned `0600` file fails with "permission denied". A key file readable by other users (such as `0444` in Swarm) only produces a warning. If the file named by `BITNEWS_KEY_FILE` is missing and its directory is not writable (the normal case for `/run/secrets`), startup fails with instructions rather than silently using a different key. Back the key up separately from the database.

### Compose

Copy `deploy/docker-compose.example.yml` next to your other stacks, adjust `PUID`, `PGID`, `TZ` and the `/data` path, then:

```sh
docker compose up -d
docker compose logs bitnews        # setup token
```

Open `http://<host>:9494`. The healthcheck is inherited from the image.

**Another port.** The example reads `BITNEWS_PORT` (default `9494`) for both the published port and `BITNEWS_LISTEN`, so one value moves the UI, the API and the Sonarr/Radarr adapters. Put it in a `.env` file next to the compose file:

```sh
echo BITNEWS_PORT=8080 > .env
docker compose up -d
```

Point Sonarr, Radarr and Prowlarr at the new port. The healthcheck follows `BITNEWS_LISTEN` by itself. To keep 9494 inside the container and only change the host side, leave `BITNEWS_PORT` alone and edit the mapping to `"8080:9494"`.

### Torrent port

The torrent engine listens on the port of Settings, Torrent network (default `51413`) or, when you set `BITNEWS_TORRENT_PORT`, on that port: the settings page then shows it read-only. A port changed in the settings or through `POST /api/v1/torrent/port` is used right away, without a restart. It listens for TCP and uTP (UDP) and, with DHT on, uses the same UDP port. bitnews does **no UPnP or NAT-PMP port mapping**, ever. Peers can only reach you if you forward that port yourself (router: TCP and UDP to the host running bitnews). Without a forwarded port torrents still download and seed over the connections bitnews opens itself, only more slowly and to fewer peers.

With Docker publish both protocols (the example compose file does):

```yaml
    ports:
      - "9494:9494"
      - "51413:51413"       # torrent peers, TCP
      - "51413:51413/udp"   # torrent peers, uTP and DHT
```

If you change `BITNEWS_TORRENT_PORT`, change the published port with it. Behind a VPN container the forwarded port is the one your VPN provider gives you, and it can change on every reconnect: let the VPN container set it (next section) and leave `BITNEWS_TORRENT_PORT` unset.

The torrent queue and Settings, Torrent network show whether the port is **open**: it shows as open once a peer has connected in on the current port, and as *not confirmed* until then. Port forwarding is only needed for seeding; downloading works without it.

### Port forwarding through a VPN (gluetun)

Providers such as ProtonVPN hand out a forwarded port that changes when the VPN reconnects. gluetun can run a command each time it gets a port, and bitnews has an API route for exactly that: `POST /api/v1/torrent/port` with the API key (Settings, Security) moves the listen port live and remembers it. In the gluetun service of the compose example:

```yaml
  gluetun:
    environment:
      VPN_SERVICE_PROVIDER: protonvpn
      VPN_PORT_FORWARDING: "on"
      # bitnews shares gluetun's network namespace, so 127.0.0.1 is bitnews.
      # The loop retries while bitnews is still starting.
      VPN_PORT_FORWARDING_UP_COMMAND: >-
        /bin/sh -c 'for i in 1 2 3 4 5 6 7 8 9 10; do
        wget -qO- --header="X-Api-Key: YOUR_API_KEY" --header="Content-Type: application/json"
        --post-data="{\"port\":{{PORTS}}}" http://127.0.0.1:${BITNEWS_PORT:-9494}/api/v1/torrent/port && exit 0;
        sleep 3; done; exit 1'
```

Instead of the hook, a small sidecar can poll gluetun's port file: `deploy/bitnews-port-sync.sh` runs in an `alpine` container that shares gluetun's network and mounts gluetun's data directory read-only. Every `SYNC_INTERVAL_SECONDS` it compares the port in `forwarded_port` with the one bitnews reports and sets it when they differ. Configure it with `BITNEWS_URL` (default `http://127.0.0.1:9494`), `BITNEWS_API_KEY` (required), `FORWARDED_PORT_FILE` (default `/gluetun/forwarded_port`) and `SYNC_INTERVAL_SECONDS` (default `30`).

Replace `YOUR_API_KEY` with the key (rotating the key on the Security page means updating it here). Do not publish the torrent port on the gluetun service and do not set `BITNEWS_TORRENT_PORT`: gluetun opens the forwarded port in its own firewall, and a fixed port makes the route answer `409 port_fixed`. `GET /api/v1/torrent/port` shows the port in use and whether a peer has connected in yet.

### Identity and UMASK

Set `PUID` and `PGID` to the owner of your media library (`id <user>`, same group as Sonarr and Radarr); the image default is `65532:65532`. Like the linuxserver.io images, the container starts as root only for its entrypoint ([ADR-0010](adr/0010-puid-pgid-entrypoint.md)): it creates `/config` if needed, gives it and everything in it to `PUID:PGID` when it is not theirs yet (symlinks are re-owned, never followed), and runs bitnews as that user with `su-exec`. bitnews itself never runs as root. `PUID=0` runs bitnews as root, with a warning.

- `/config` needs nothing from you: a missing bind-mount directory (which Docker creates as root) is handed over on the first start, and after changing `PUID` the next start re-owns it.
- `/data` is never re-owned (it may hold a whole media library): it must be writable by `PUID` or by its group.

`user: "uid:gid"` (or `--user`) still works and overrides all of this: the entrypoint then starts bitnews directly as that user, `PUID`/`PGID` are ignored, nothing is re-owned, and `/config` must already be writable by that user (bitnews stops at start with the `chown` command if it is not).

`UMASK` (default `002`) is applied process-wide before bitnews creates any file: files become `664` and directories `775`. It stays because Sonarr and Radarr usually run as another user in the same group, and they can only manage what bitnews wrote if the group may write it. Use `027` or `077` if nothing else needs access.

The hardened compose example uses `cap_drop: [ALL]` and adds back only what the entrypoint needs as root: `CHOWN` (hand over `/config`), `SETUID` and `SETGID` (switch to PUID:PGID) and `DAC_READ_SEARCH` (read a `/config` that an earlier PUID owns with mode `0700`). A process that switches from root to a normal user keeps no capabilities, so bitnews runs with none. `no-new-privileges` blocks gaining privilege through `execve`. With `user:` instead of PUID/PGID, `cap_add` can go.

### The shared `/data` layout

Mount **one** `/data` into bitnews, Sonarr, Radarr and anything else that handles the files, at the same path in every container, following the [TRaSH guides](https://trash-guides.info/):

```
/data
├── usenet
│   ├── incomplete/<download>/              bitnews works here
│   └── complete/<category>/<download>/     finished Downloads (Ready)
├── torrents/<category>/<download>/         torrent Downloads (M5)
└── media
    ├── tv/                                  Sonarr library
    └── movies/                              Radarr library
```

Why it matters:

- **Hardlinks.** Sonarr and Radarr make the hardlinks, from the `complete` directory into the library; bitnews never hardlinks. A hardlink only works inside one filesystem, and Docker treats each mount as separate, so keep the `complete` directory and the library on one filesystem (one mount). If `/data/usenet` and `/data/media` are separate mounts (or separate host filesystems), Sonarr must copy instead of hardlink: twice the disk use, slower imports, and torrents stop seeding from the same file. bitnews does not check this, and it cannot know where the library is.
- **Moves.** bitnews moves a finished Download from `incomplete` to `complete` with a `rename`, which is instant on one filesystem, and copies then removes the source when the two are on different filesystems (for example `incomplete` on an SSD and `complete` on a share). That keeps working; it is just slower. bitnews does not probe paths when you save them: a folder it cannot write fails the Download with the folder and the user id it runs as.
- **No remote path mapping.** Sonarr is told "the download is at `/data/usenet/complete/tv/X`", and that path exists for Sonarr too. If container paths differed you would need Remote Path Mappings, and they are an easy source of "import failed" errors.

Files are never modified after Ready, because hardlinks share the data with the library.

Ownership tips:

- Run bitnews and the *arr containers with the **same group**, ideally the same user too, and keep `UMASK=002` everywhere so group members can read and write each other's files.
- Make the tree group-writable and let new directories inherit the group:
  `sudo chgrp -R 1000 /data && sudo chmod -R g+rwX /data && sudo find /data -type d -exec chmod g+s {} +`
- Do not mix `UMASK=022` containers with `002` ones on the same tree.

### Reverse proxy and TLS

bitnews serves HTTP only. Terminate TLS in a reverse proxy and do not publish port 9494 to the internet. If the proxy runs on the same host, publish on loopback only (`"127.0.0.1:9494:9494"`); if it shares a Docker network with bitnews, drop `ports:` and reach it as `bitnews:9494`.

Caddy:

```caddyfile
bitnews.example.com {
    reverse_proxy bitnews:9494
}
```

Traefik (labels on the `bitnews` service; adjust entrypoint and resolver names):

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.bitnews.rule=Host(`bitnews.example.com`)
  - traefik.http.routers.bitnews.entrypoints=websecure
  - traefik.http.routers.bitnews.tls.certresolver=letsencrypt
  - traefik.http.services.bitnews.loadbalancer.server.port=9494
```

nginx needs the original `Host` header passed on (`proxy_set_header Host $host;`) plus `X-Forwarded-For` and `X-Forwarded-Proto`, and a long `proxy_read_timeout` with `proxy_buffering off` for the SSE stream.

What bitnews checks behind a proxy:

- State-changing browser requests (such as login) must come from bitnews's own page: the browser's `Sec-Fetch-Site`, or an `Origin` that matches the request's `Host` or an entry of `allowed_hosts` (ADR-0013). If the proxy rewrites `Host` to an internal name, login fails with `403 csrf_origin_invalid`. Pass the original `Host` through (Caddy and Traefik do by default), or list the public name in **Settings → Security → allowed_hosts**.
- The session cookie is `Secure` when the request arrived over HTTPS, either directly or through a **trusted proxy** that sets `X-Forwarded-Proto`. `X-Forwarded-For` is honoured only when the direct peer is in **`trusted_proxies`**. Add the proxy's address or Docker network CIDR there, and nothing else.
- `allowed_hosts` is mandatory before you can enable *disable auth* or *LAN bypass*; a request with any other `Host` then gets `421`. Both options are off by default and show a permanent warning in the UI. Prefer neither. These settings live in the database and are edited through the UI or by a logged-in session over the API (`PUT /api/v1/settings`; an API key may not change the `auth` section), not through environment variables.

### VPN

bitnews does not set up a VPN. If you want its Usenet or torrent traffic to leave through one, run it in the network namespace of a VPN container such as gluetun: the commented variant in the compose example uses `network_mode: "service:gluetun"` and publishes the API port on the `gluetun` service. Provider, credentials, DNS and kill switch are the VPN container's configuration, and testing that they work is yours. Torrent peer traffic can additionally be bound to one interface (M5), which acts as a kill switch.

Interface and address binding for Usenet and torrents, what it covers and what it does not (DNS), and the namespace test: [network-binding.md](network-binding.md).

### The `bitnews healthcheck` subcommand

The image's `HEALTHCHECK` runs `["/entrypoint.sh", "healthcheck"]`, which starts `bitnews healthcheck` without re-owning anything. Contract of the subcommand:

- It sends `GET http://127.0.0.1:<port>/api/v1/health`, with `<port>` taken from `BITNEWS_LISTEN` (default `9494`).
- It exits `0` when the answer is `200` within **3 seconds**, and `1` for anything else (connection refused, timeout, other status).
- The endpoint needs no credentials and answers even while first-run setup is open.

The `HEALTHCHECK` uses `--interval=30s --timeout=5s --start-period=20s --retries=3`. Inspect it with `docker inspect --format '{{.State.Health.Status}}' bitnews`. It always queries the loopback address, so keep `BITNEWS_LISTEN` on `0.0.0.0` or `127.0.0.1` inside the container.

## Proxmox LXC

`deploy/proxmox/` holds an installer for a Proxmox VE host, built on the [community-scripts](https://community-scripts.org) framework. Run this on the Proxmox shell:

```sh
export COMMUNITY_SCRIPTS_URL=https://raw.githubusercontent.com/jroosing/bitnews/main/deploy/proxmox
bash -c "$(curl -fsSL $COMMUNITY_SCRIPTS_URL/ct/bitnews.sh)"
```

`COMMUNITY_SCRIPTS_URL` is the framework's documented way to run its scripts from another repository ([Script Origin](https://community-scripts.org/docs/guides/source_origin)); the framework itself is still downloaded from community-scripts. The script creates an unprivileged Debian 13 LXC (2 cores, 2 GiB RAM, 8 GiB disk by default, all changeable in the advanced settings) and installs the release tarball from GitHub: `bitnews` plus the pinned `7zz` and `par2`, in `/opt/bitnews`. Run it again to update: its update function installs the newest release and keeps the database and master key.

- **Login.** The script creates the admin account itself, so there is no setup screen. The username is `admin` and the password is in `/root/bitnews.creds` inside the container. Change it in the UI.
- **Service.** `systemctl status bitnews`, logs with `journalctl -u bitnews`. It runs as the user `bitnews` (uid and gid 1000, so `101000` on the Proxmox host of an unprivileged container) with the settings of `/etc/bitnews/bitnews.env` ([bootstrap settings](#bootstrap-settings)). Config, database and master key are in `/var/lib/bitnews`: back that directory up as a whole ([The master key](#the-master-key)).
- **Storage.** Downloads go to `/data` inside the container, in the [shared layout](#the-shared-data-layout). Mount your media storage over it (Proxmox: container, Resources, Add, Mount Point, path `/data`), and give `bitnews` write access: on the host `chown -R 101000:101000 <directory>` for an unprivileged container (or add the group, with `lxc.idmap`, that Sonarr and Radarr share). Mount the same storage at the same path in the Sonarr and Radarr containers so they can hardlink and need no remote path mapping.
- **Torrent port.** Forward `51413` (TCP and UDP) from your router to the container's address. There is no UPnP.
- **TLS.** Plain HTTP on port 9494: put a reverse proxy in front of it ([Reverse proxy and TLS](#reverse-proxy-and-tls)).

The release tarball is built by `scripts/package-release.sh` and published by `.github/workflows/release.yml` when a version tag (`0.6.0`) is pushed; the installer needs at least one release.

## Without Docker

The binary is static and has no other files to install, so it also runs directly (`make build-all` builds `dist/bitnews-linux-amd64`). It needs two programs on `PATH`: `7zz` (7-Zip 23 or newer, for unpacking) and `par2` (par2cmdline or par2cmdline-turbo, for repair), and refuses to start without them. `scripts/get-postproc-tools.sh` downloads the pinned, checksum-verified static versions for Linux and macOS into `bin/tools/`. Set the [bootstrap settings](#bootstrap-settings) as flags or environment variables, run it as the user that owns your media (see [Identity and UMASK](#identity-and-umask)), and keep it behind a reverse proxy as described above. `bitnews healthcheck` works outside Docker too, with the same contract. Docker is the supported way to run it; for a Proxmox host there is the [LXC installer](#proxmox-lxc), whose systemd unit is a good starting point (`deploy/proxmox/install/bitnews-install.sh`).

## Hardening checklist

- [ ] bitnews runs as a dedicated non-root user (`PUID`/`PGID`, or `user: "1000:1000"`).
- [ ] The config directory, which holds the database and the master key, is backed up as a whole and readable only by the service. If you want the key apart from the database: a Docker secret via `BITNEWS_KEY_FILE`.
- [ ] TLS is terminated by a reverse proxy; port 9494 is not reachable from the internet (loopback or internal Docker network only).
- [ ] `allowed_hosts` lists the public hostname, `trusted_proxies` lists only the proxy, and *disable auth* and *LAN bypass* are off.
- [ ] Compose: `read_only: true`, `tmpfs: /tmp`, `cap_drop: [ALL]` with nothing added back, `no-new-privileges:true`.
- [ ] Base images are pinned to digests and rebuilt regularly, and `govulncheck` (CI) is clean for the release you run.
- [ ] One `/data` mount shared with the *arr containers, same path everywhere, same group, `UMASK=002`.
- [ ] A VPN, if wanted, is configured and tested in its own container (bitnews does not manage it).
- [ ] The API key (Settings → Security) is rotated if it leaked, and then updated in Sonarr, Radarr and your scripts. There is one key for all of them, with full access. No admin password in the environment or shell history (`BITNEWS_ADMIN_PASSWORD_FILE` points at a file only the service can read).
- [ ] The database (`/config`) is backed up; the key is not stored with it.
