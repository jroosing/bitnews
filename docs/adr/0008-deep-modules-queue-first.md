# Deepen the shared modules, starting with one Queue module

An architecture review (2026-10-23) found the same domain logic re-implemented per protocol and per transport. We deepen it into a few modules with small interfaces, in this order:

1. **Queue** (`internal/queue`): one module owns a Queue's policy for both protocols: admission through `core.Select`, pause reasons, the disk guard, holds, orphan pickup, recovery after a restart, and the "change state, then publish" step. Each runner plugs in through a small protocol interface (start, stop, abandon a fetch). The Usenet and torrent runners keep only what is protocol-specific (Articles and Servers; the torrent engine, metadata and Seeding).
2. **Download view**: one module merges a Download with its live Queue facts into a protocol-neutral view (state class, queue order, progress, rate, ETA, stage, ratio, Share Limit, readable failure). The SABnzbd and qBittorrent adapters, the own API and the UI only translate it to their wire formats.
3. **Add**: one request-shaped Add call replaces the staged add protocol that four callers drive by hand, including the Category auto-create.
4. **Settings change**: Settings are changed through one mutate-under-lock call, and every settings section uses the existing card engine.
5. **Post-processing steps**: `par2` and `unpack` implement the core ports directly, deferred-volume accounting lives in one place, and `VerifyOutcome.Opaque` goes.
6. **Torrent facts**: the torrent runner answers ratio, seeding time and Share Limit in one call; callers stop adding engine counters to stored bases.

## Why the Queue first

It is the largest duplication (about 1,000 lines across the two runners) and the copies have already drifted into bugs: the Usenet kill switch pretends to be a user pause, the Usenet disk guard announces a resume while the user still has the Queue paused, and a full disk pauses Usenet but fails a torrent. The seam has two real adapters (Usenet, torrent) plus a test fake, and the Queue becomes testable with a fake clock instead of sleeps.

## Consequences

- One pause vocabulary for both Queues: `user`, `disk_full`, `bind_unavailable`, each held separately, and one pair of events (`core.QueuePaused`, `core.QueueResumed`) carrying the Source kind. The per-runner event types go.
- A torrent that hits "no space left" pauses its Queue like Usenet, instead of failing the Download.
- Candidates 2 to 6 are planned (milestone M8) and each gets its own design pass before it is built; this record does not fix their interfaces.
