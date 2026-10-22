// Package runner is the torrent Queue runner. It decides, with core.Select,
// which torrent Downloads are active, drives the TorrentEngine for them, moves
// a Fetched torrent to its complete location without a rehash, keeps Seeding
// and enforces the Share Limits.
//
// # Control loop
//
// Run reacts to the Bus events (every lifecycle event, SettingsChanged,
// Removed) and to the engine's events, and as a fallback polls every
// Config.PollInterval (10 s). Each pass lists the Queued and Fetching torrent
// Downloads and applies core.Select with the torrent limit. A Seeding torrent
// is Fetched or later, so it never counts against the limit. A chosen Queued
// Download is added to the engine (or resumed) and becomes Fetching; a
// Fetching Download that is no longer chosen (lower priority, a new limit,
// paused) is paused in the engine, which keeps its pieces, and goes back to
// Queued. The poll also sweeps: it adds the torrents of Fetched, Post-processing,
// Ready and (after Fetched) Failed Downloads to the engine when they are not
// there, and drops the torrents whose Download is gone.
//
// A torrent's directory is its Download's directory: the one that holds the
// stored .torrent file, <torrent incomplete>/<id> until the move to complete.
// The engine stores the files below it as <dir>/<torrent name>/<file>.
//
// # Magnet links
//
// A magnet Download starts in FetchingMetadata. The runner adds the link to
// the engine and, on TorrentMetadataReceived, writes the .torrent file into the
// Download's directory (mode 0600, named like app.AddService names it),
// records its path in the TorrentSource, stores the real name and size
// (DownloadRepo.UpdateDisplay) and moves the Download to Queued. The torrent
// keeps running if the Queue has room, because a pause would drop the peers
// that delivered the metadata.
//
// # Fetched and the move to complete
//
// On TorrentFetched (or when the stats show all bytes, in case the event was
// dropped) the runner moves the data to <complete>/<category>/<name> with
// Engine.Relocate, which does not rehash, and only then moves the Download to
// Fetched. Post-processing therefore always finds the data where it stays.
// Seeding continues throughout. The destination comes from Config.Destination
// (the Finisher's rule) made unique with fsutil.Unique; when the Download's
// directory already is the destination nothing moves.
//
// The runner writes its intent (the target) to the torrent record before the
// move and confirms it after, so a crash in the middle is settled at the next
// start by looking at which of the two directories exists. A failed move is
// not fatal: the torrent keeps seeding where it is and Post-processing
// continues from there. The .torrent file moves with the directory and its
// recorded path follows.
//
// # Seeding and Share Limits
//
// The engine counts uploaded bytes and seeding time from zero after every
// start. The runner adds the counters of earlier runs (the record), persists
// the totals every Config.PersistInterval, when a torrent is Fetched and on
// shutdown, and evaluates the Share Limit itself every Config.LimitInterval:
//
//	reached = (persisted.uploaded + stats.Uploaded) / size >= ratio
//	       || persisted.seeding  + stats.SeedingTime        >= time
//
// A Download's own limit (SetShareLimit, ClearShareLimit) wins over the
// defaults in core.Settings.Seeding; a negative value is unlimited. The action
// is pause (the torrent stops uploading and stays paused until the limit is
// raised; a restart notices that the limit is reached and adds it paused),
// remove or remove_data (through Config.Remover, retried while it fails).
//
// # Queue pause, speed limit, disk guard
//
// PauseQueue and ResumeQueue pause the Queue as a user; PauseQueueFor takes
// another reason (the daemon uses bind_unavailable) and the Queue runs again
// when every reason is released. SetRateLimits sets the engine's global
// limits. Before a Download is activated, and every Config.DiskCheckInterval
// while torrents run, the free space of the torrent incomplete root must cover
// the remaining bytes plus min_free; otherwise the Queue is paused with reason
// disk_full and resumes by itself. Seeding is never paused by the Queue.
//
// # Restart, removal and the layout helpers
//
// Recover moves Fetching Downloads back to Queued and adds the Seeding
// torrents to the engine; call it before Run. Hold stops and drops a torrent
// (the engine closes its files, nothing is deleted) and keeps it out until
// Release, the Removed event or two minutes pass; the daemon calls it before
// deleting a Download's files. Dir, ContentDir and Managed tell the daemon
// where a Download's files are: ContentDir is the torrent's own root
// directory inside the Download directory (the Unpacker works there, and
// _unpack/ lands there), or the Download directory for a single-file torrent.
package runner
