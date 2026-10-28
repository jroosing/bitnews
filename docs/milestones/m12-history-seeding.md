# M12 — History: torrent and seeding status

## Goal

In the History page, tell NZBs from Torrents at a glance, show whether a torrent is seeding with its upload speed and ratio against its Share Limit, and let the user pause and resume Seeding. Decision: [ADR-0012](../adr/0012-a-ready-torrent-can-be-paused.md).

## Scope

**In:** a labelled Source pill in the History Status cell; the seeding indicator (`Seeding`, `Seeding paused`, `Share Limit reached`), upload speed and the ratio or time line; pausing and resuming a Ready torrent from History, the own API and the qBittorrent adapter; one batched 5 s poll that keeps the seeding cells current.

**Out:** a "Seeding" filter, seeding time against a ratio limit, the Queue page (it already shows upload speed), changing how Share Limits are set (detail page, `setShareLimits`).

## Design

- **Source pill.** `NZB` or `Torrent` (the glossary's Source names) with its icon, next to the state badge. Replaces the 16 px icon with a tooltip.
- **Seeding cell** for a Ready torrent: `Seeding · ↑ 1.2 MB/s`, and a second line `Ratio 1.4 of 2.0` (`Ratio 1.4 · no limit` without a limit; `3h of 24h` when only a time limit is set; both when both are set). Seeding paused reads `Seeding paused`; a torrent past its Share Limit reads `Share Limit reached · ratio 2.4 of 2.0` with a link to the detail page and no pause control.
- **Pause control.** An inline pause or resume icon button in the History row next to `⋯`, posting to `POST /downloads/{id}/pause|resume`-style History routes that answer with the updated row.
- **Live cells.** The page lists the IDs of its seeding rows; one `hx-get /history/seeding?ids=…` every 5 s returns out-of-band replacements of those rows' seeding cells. A row that stops seeding drops out of the next poll.
- **Pause for a Ready torrent** everywhere `Pause` exists (own API, `torrents/pause|stop`); Ready NZBs stay `not_pausable`.

## Issues

### M12-01: History shows torrent and seeding status and can pause seeding
- **Labels:** area:ui, area:torrent, area:api, ready-for-agent

**Goal:** A History row says whether it is an NZB or a Torrent, shows seeding state, upload speed and the Share Limit progress for a Ready torrent, and lets the user pause and resume its Seeding.

**Acceptance criteria**
- [ ] `DownloadService.Pause` accepts a Ready torrent (Seeding paused) and still refuses a Ready NZB, a Failed Download and one in Post-processing with `not_pausable`; `Resume` clears it.
- [ ] The torrent runner stops the engine for a Fetched torrent with `Paused` and starts it again on resume; a paused torrent does not upload after a restart; seeding time does not accrue while paused.
- [ ] A torrent stopped by its Share Limit is not resumed by `Resume` (it stays `Share Limit reached`); raising or clearing the limit still resumes it at once.
- [ ] The qBittorrent adapter's `pause`, `stop`, `resume`, `start` work on a seeding torrent (`pausedUP` / `stoppedUP` while paused); `view.View.Torrent` exposes `Paused`.
- [ ] History rows show a labelled `NZB` / `Torrent` pill.
- [ ] A Ready torrent's row shows `Seeding · ↑ rate`, `Seeding paused` or `Share Limit reached`, and the ratio / time line as designed.
- [ ] The row has an inline pause or resume button (not for NZBs or a limit-stopped torrent) that answers with the updated row.
- [ ] One batched poll every 5 s refreshes only the seeding rows' cells.
- [ ] `docs/api-v1.md` and `docs/adapters-qbit.md` are updated; `make check` is green.
