# Emulate the qBittorrent and SABnzbd APIs for media managers

Media managers (Sonarr, Radarr, Prowlarr) have no generic download-client API; they integrate with specific clients only. So bitnews exposes thin adapters that imitate qBittorrent WebUI API v2 (torrents) and the SABnzbd API (Usenet) on one port, under separate paths. Each adapter covers only the calls media managers actually make. Alongside them sits bitnews's own API, which the future UI uses. All three translate onto the same core model, and the core knows nothing about the adapters.

## Considered Options

- **NZBGet API for Usenet.** This would match the "nzbget-like" goal, but its JSON-RPC surface is larger and media managers also read its config. SABnzbd's queue/history states map directly onto Fetched → Ready/Failed.
- **Own API only.** Rejected, because media managers couldn't use bitnews.

## Consequences

- The adapters must stay compatible with media-manager releases. Contract tests should replay recorded media-manager requests. (Update: no recordings were made, M4-01 and M5-09 were dropped, and the hand-written replay harness was removed; the adapters are covered by per-call unit tests and fixed from real Sonarr/Radarr reports.)
- The "completed" status in both adapters means Ready, not Fetched.
