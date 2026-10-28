# A Ready torrent can be paused, which stops its Seeding

`Download.Paused` now also applies to a Ready torrent: pausing one stops its Seeding, resuming it seeds again. Until now a Download could only be paused before it was Fetched, and for a seeding torrent `pause` answered `409 not_pausable` (own API) or did nothing (qBittorrent adapter). The glossary calls the new state **Seeding paused**, and keeps it apart from the Seeding that stops by itself when a Share Limit is reached.

## Why

The History page shows seeding torrents (Ready, still uploading) and users want to stop one without removing it and losing the seed. A stop is the same act as a pause from the user's side, so it should be the same control in the UI, the own API and the qBittorrent adapter (`torrents/pause`, `stop`, `resume`, `start`), not a second set of routes.

## Decisions

- **One flag, wider meaning.** `Paused` is persisted already, so Seeding paused survives a restart. `pausable` allows a Ready torrent; a Ready NZB, a Failed Download and a Download in Post-processing stay `not_pausable`.
- **The runner acts on it.** For a Fetched torrent the runner stops the engine when `Paused` is set and starts it again when it is cleared, on `DownloadUpdated` and at startup (a paused torrent does not upload after a restart). Seeding time does not accrue while the torrent is paused.
- **A Share Limit stop is not a pause.** When the Share Limit is reached the runner stops the torrent as before (`limitPaused`, runtime only). Resume has nothing to override there: History shows `Share Limit reached` with no control, and the user raises or clears the Share Limit on the detail page, which resumes the torrent at once.
- **What the media managers see.** A paused seeding torrent reports `pausedUP` (`stoppedUP` for API 2.11+), the same as one stopped by a Share Limit. Sonarr and Radarr already import on those states and remove a torrent only when their own seed goal is met, so a user's pause does not remove anything.

## Considered options

- **A separate stored flag** (`TorrentRecord.SeedingStopped`) with its own routes. Keeps the old meaning of `Paused` clean, but needs a second set of routes in the UI, the own API and the adapter, and the adapter's `pause` would have to be rerouted, or Sonarr's and Radarr's `pause` stays a no-op. Rejected.
- **Let Resume override a reached Share Limit.** Needs new stored state ("ignore the limit until raised") or silently clears the limit a media manager set. Rejected.

## Consequences

- `Paused` no longer means "not Fetched yet" in the docs and in `view.Phase`; `PhasePaused` stays the Phase before Fetched and a Ready paused torrent is `PhaseReady` with `Torrent.Paused` set.
- `docs/api-v1.md` and `docs/adapters-qbit.md` drop the "pause does nothing for a seeding torrent" note.
