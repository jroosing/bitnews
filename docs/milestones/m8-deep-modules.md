# M8 — Deep modules

## Goal

Concentrate logic that is duplicated per protocol and per transport into deep modules, as decided in [ADR-0008](../adr/0008-deep-modules-queue-first.md). The Queue comes first; the other modules follow, each after its own design pass.

## Scope

**In:** the Queue module and its adoption by both runners and their callers (M8-01 to M8-04). The Download view, Add, Settings change, post-processing steps and torrent facts are listed as M8-05 to M8-09 and are specified when they are started.

**Out:** behaviour changes beyond the drift fixes named below.

## Design: the Queue module

`internal/queue` holds one Queue per Source kind. Its interface:

- `New(Config)`: the Source kind, the Download repo, the bus, the Settings, min_free and the free-space probe, a clock, a logger, the protocol adapter and a `Wake` hook the runner's control loop listens to.
- `Reconcile(ctx)`: one pass. Lists the Queue's Queued and Fetching Downloads, drops held ones and the ones the protocol excludes, applies `core.Select` with the Queue's limit, releases the disk guard when the next Download fits, honours pause reasons, stops what is deselected, restarts orphaned Fetching Downloads (re-reading them first), checks the disk guard per start, moves Queued to Fetching and asks the protocol to start.
- `Pause(reason, detail)`, `Resume(reason)`, `Paused() (paused bool, reason, detail string)`: pause reasons are held separately (`user`, `disk_full`, `bind_unavailable`, or any caller reason); the Queue runs when none is held. `QueuePaused` is published when a reason is added, `QueueResumed` only when the last one is released.
- `Fits(ctx, remaining)`: the disk guard for a running fetch; it pauses the Queue with `disk_full` when the space is short. `DiskFull(detail)` pauses for a "no space left" error.
- `Hold(ctx, id)`, `Release(id)`, `Held(id)`: keep a Download from being started (two-minute TTL) while its files are removed; `Hold` asks the protocol to abandon the fetch and waits.
- `SetState(ctx, id, change)`: apply a state change and publish `DownloadUpdated`.
- `Recover(ctx)`: at start-up, move the kind's Fetching Downloads back to Queued.

The protocol adapter (the seam) is: `Prepare(ctx) (canStart bool)`, `Exclude(id) bool`, `Running(id) bool`, `Start(ctx, d) error` (`ErrNotNow` to skip for this pass), `Stop(ctx, id)`, `Abandon(ctx, id) error`.

## Implementation notes

- **Protocol interface as built:** `Prepare`, `Exclude`, `Running`, `Start`, `Stop`, `Abandon`. The Usenet runner uses `Exclude` for Downloads in a failure cooldown, so they stay Queued instead of being moved to Fetching with nothing running. The torrent runner's `Running` means "registered in the engine", so `Hold` also reaches torrents paused for the Queue or by a Share Limit.
- **Events:** `core.QueuePaused{Source, Reason, Detail}` is published when a reason is added and `core.QueueResumed{Source}` when the last is released. The own API streams them as `usenet_queue_paused|resumed` and `torrent_queue_paused|resumed` (the torrent pair is new).
- **Drift fixes:** the Usenet kill switch pauses with `bind_unavailable` instead of faking a user pause; a resume is announced only when no reason is left; a torrent that hits "no space left" pauses its Queue and goes back to Queued instead of failing.

- **Download view as built (M8-05, M8-09):** `internal/view` (`Of`, `Queue`, `Phase`, `CleanDetail`, `PriorityLabel`) over one live source in the daemon (`live.go`: the event tracker plus `Runner.Facts`). The SABnzbd and qBittorrent adapters and the UI translate the View; they no longer compute the in-queue set, queue order, progress, ETA, ratio, Share Limit, stage words or failure cleaning. Deliberate changes: a slot's time left in SABnzbd uses the Download's own rate; the UI says "Unpacking" and "Finishing" instead of "Extracting" and "Moving"; the UI's detail page shows the ETA. The qBittorrent log masker keeps its own regex because it must also mask `magnet:?` links.
- **Disk guard fix found on the way:** a Queue paused for `disk_full` by a failed fetch no longer releases itself while the next Download still needs more than is free.

## Issues

### M8-01: The Queue module
- **Labels:** area:core, ready-for-agent

**Goal:** `internal/queue` as designed above, tested through its interface with a fake protocol and a fake clock (no sleeps).

**Acceptance criteria**
- [ ] Admission, pause reasons, the disk guard, holds, orphan pickup and recovery are covered by tests on the fake protocol.
- [ ] `core.QueuePaused{Kind, Reason, Detail}` and `core.QueueResumed{Kind}` replace the per-runner event types.

### M8-02: The Usenet runner uses the Queue
- **Labels:** area:usenet, ready-for-agent
- **Depends on:** M8-01

**Goal:** the Usenet runner keeps Articles, Servers, jobs, cooldown and the rate limiter, and gives the rest to the Queue. Its `PauseQueue`/`QueuePaused`, disk guard, holds and recovery go.

**Acceptance criteria**
- [ ] The runner's existing behaviour tests pass, rewritten where they asserted the old events.
- [ ] The daemon's kill switch pauses the Usenet Queue with `bind_unavailable`, not as a user pause.

### M8-03: The torrent runner uses the Queue
- **Labels:** area:torrent, ready-for-agent
- **Depends on:** M8-01

**Goal:** the torrent runner keeps the engine, metadata, the move to complete and Seeding, and gives the rest to the Queue. "No space left" pauses the Queue instead of failing the Download.

**Acceptance criteria**
- [ ] The runner's existing behaviour tests pass, rewritten where they asserted the old events or the disk-full failure.

### M8-04: Callers use one Queue vocabulary
- **Labels:** area:api, area:ui, ready-for-agent
- **Depends on:** M8-02, M8-03

**Goal:** the daemon, the own API's event stream, the UI and the SABnzbd adapter read pause state and events from the Queue, without per-protocol switches or imports of the runner packages.

**Acceptance criteria**
- [ ] `api` and `web` no longer import `usenet/runner` or `torrent/runner` for events.
- [ ] The daemon's queue glue (`webRuntime` pause and state) asks the Queue of the Download's kind.

### M8-09: Torrent facts from the torrent runner
- **Labels:** area:torrent, ready-for-agent
- **Depends on:** M8-03

**Goal:** the torrent runner answers everything known about one torrent in one call, so nobody outside it adds the engine's counters to the stored bases again.

**Work**
- `Runner.Facts(ctx, id) (Facts, bool, error)`: the info hash, name and size (engine, else the Download's), done bytes, rates, peers, seeds, private flag, uploaded and seeding time (stored base plus the engine's counter, or the stored record when the torrent is not registered), the Download's own Share Limit halves, whether a Share Limit paused it, its content directory and its files. `ok` is false for a Download that has no torrent.
- `qbitTorrents.view`, `webTorrents.Torrent` and `webRuntime.Live` (with its upload-rate scan) use it; their copies of the merge go.

**Acceptance criteria**
- [ ] Unit tests on the runner's fake engine cover registered, unregistered (stored record) and unknown torrents, including the ratio inputs.
- [ ] `engine.Stats` is no longer called outside the torrent runner and the engine package.

### M8-05: Download view module
- **Labels:** area:core, area:adapter, area:ui, ready-for-agent
- **Depends on:** M8-09

**Goal:** one module turns a Download row and what is live about it into a protocol-neutral view, and the SABnzbd and qBittorrent adapters and the UI only translate that view to their wire formats.

**Interface (`internal/view`)**
- `New(Config{Downloads, Live, Settings})`, with `Live` the one source of live facts (stage, rates, torrent facts) that the daemon implements on the tracker and `Runner.Facts`.
- `Of(ctx, d) View` and `Queue(ctx, kind) ([]View, error)` (the in-queue Downloads of a kind, in queue order, all pages).
- `View` holds the Download, its `Phase` (waiting, metadata, paused, downloading, verifying, repairing, unpacking, finishing, ready, failed), whether it is in a Queue, done and total bytes, progress (0 to 1), rates, ETA, the Post-processing stage, the torrent facts (hash, peers, seeds, uploaded, seeding time, ratio, effective Share Limit) and the failure (reason and cleaned detail).
- Helpers: `CleanDetail` (URL redaction, control characters, 300 bytes), `PriorityLabel`, `StageLabel`; `core.State.InQueue()` and the exported queue order.

**Work**
- Build the module test-first. Then move each transport onto it: SABnzbd (`classify`, queue and history slots), qBittorrent (`state`, progress, ETA, ratio, share limit), the UI (queue rows, history, detail), deleting their own copies of the in-queue set, queue order, progress, ETA, stage and priority labels, failure cleaning and URL redaction.
- One stage word: the UI's "Extracting" and "Unpacking" become one.

**Acceptance criteria**
- [ ] A shared test publishes the same Downloads through the view and checks each transport's output for state, progress and ETA.
- [ ] No transport computes the in-queue set, queue order, ETA or ratio itself.
- [ ] The adapters' and the UI's behaviour tests pass (rewritten only where they asserted a deleted helper).

### M8-06: One Add call (planned)
### M8-07: Settings change under lock and one card engine (planned)
### M8-08: Post-processing steps implement the core ports (planned)

See ADR-0008 for what each is about; each gets a design pass and a spec here before it starts.
