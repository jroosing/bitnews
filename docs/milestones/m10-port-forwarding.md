# M10 — Port forwarding

## Goal

Let a VPN with port forwarding (gluetun, for example with ProtonVPN) set the torrent listen port whenever it changes, and show plainly whether the port is reachable.

## Scope

**In:** moving the engine's listen port live, an API route for it with the app key, the open/not confirmed status on the torrent queue and the Torrent network settings page, the gluetun recipe in the deploy guide.

**Out:** UPnP and NAT-PMP (still never), an outside port check (it would tell a third party our address), reading gluetun's forwarded-port file, the qBittorrent adapter's `setPreferences`.

## Design

- **Live rebind.** anacrolix/torrent cannot remove or move a listener, so the engine always opens its own peer and DHT sockets (also without a network binding) and hands the client stable wrappers. `Engine.SetListenPort` opens every IP family on the new port and swaps the sockets underneath; if any family cannot be opened, the old port stays. Announces read the port from the wrappers, so they carry the new one.
- **Open or not confirmed.** Nothing can prove from the inside that a port is closed. The port is *open* once a peer has connected in on the current port, and *not confirmed* until then; a new port starts unconfirmed.
- **One path.** `POST /api/v1/torrent/port` moves the engine first and only then stores `torrent.listen_port` (through the mutate-under-lock `SettingsService.Change`), so a port that cannot be opened is never stored. A port saved on the settings page is applied by the same code on `SettingsChanged`. `BITNEWS_TORRENT_PORT` fixes the port and wins.

## Issues

### M10-01: Torrent listen port from a VPN hook
- **Labels:** area:torrent, area:api, area:ui, ready-for-agent

**Goal:** The torrent listen port changes without a restart, through the settings page or `POST /api/v1/torrent/port` with the app key, and the UI shows whether peers reach it.

**Acceptance criteria**
- [x] `Engine.SetListenPort` moves TCP, uTP and the DHT, with and without a binding; a taken port keeps the old one; the swarm reaches a seeder on its new port.
- [x] `Engine.ListenPortStatus` reports the port, since when, and the last incoming connection; a new port starts unconfirmed.
- [x] `GET` and `POST /api/v1/torrent/port` with the app key or a session; `400 invalid_port`, `409 port_unavailable`, `409 port_fixed`, `503 torrents_unavailable`.
- [x] The port survives a restart (stored in the Settings); a port-only change on the settings page needs no restart.
- [x] The torrent queue shows a badge "Port N open" or "Port N not confirmed" with an info popover that says the port is only needed for seeding; the Torrent network page shows the same status.
- [x] docs/deploy.md has the gluetun recipe; docs/api-v1.md lists the routes.
