// Package daemon is the composition root: it wires the stores, the services,
// the API, the Usenet Queue runner and the Post-processing pipeline into one
// Daemon. cmd/bitnews stays thin: it parses the bootstrap configuration,
// opens the database and calls Build, then Serve.
//
// # Wiring
//
//   - The runner reads Servers through serverSource (core.ServerRepo to
//     runner.ServerSpec; a Server whose password cannot be decrypted is
//     skipped with one warning), NZBs through nzbSource
//     (core.DownloadSourceRepo), and keeps its resume blob, fetch reports and
//     un-deferred volumes in SQLite (store.UsenetProgressRepo,
//     store.UsenetReportRepo, store.UsenetUndeferredRepo). The working
//     directory of a Download is <Settings usenet incomplete root>/<id>,
//     resolved from the current Settings each time it is asked for.
//   - The core.PostProcessor runs on adapt.Verifier (with the runner's
//     DeferredBlocks), adapt.Repairer, adapt.Unpacker (passwords from the
//     Download's source) and finalize.Finisher. Its FetchReporter is the
//     runner. Deferred recovery volumes go through adapt.DeferredVolumes with
//     the runner as the controller; the requeue moves the Download from
//     PostProcessing back to Fetching, which the runner picks up.
//   - DELETE ?delete_data=true calls the data remover: it holds the
//     Download's fetch off, then deletes the working directory and the
//     output, each only if it lies inside a configured root (never the root
//     itself, never through a symlink).
//
// # Torrents
//
// Unless Deps.DisableTorrent is set, Build creates a torrent engine (or takes
// Deps.TorrentEngine) and the torrent Queue runner of internal/torrent/runner
// on top of it. The runner moves a Fetched torrent to its complete location
// itself (the destination comes from finalize.Finisher.Destination), so the
// Finisher of the torrent side, torrentFinisher, leaves a torrent the engine
// has alone and returns its directory. DirFor for a torrent is the runner's
// ContentDir, FetchReport answers with a zero report, and the data remover
// holds the torrent runner and deletes the whole Download directory inside
// the torrent roots. The Share Limit actions remove Downloads through the
// DownloadService. DHT, PEX and the encryption policy come from
// settings.torrent, read when the engine starts; the listen port comes from
// there too but moves live (torrentPort, port.go), also through
// POST /api/v1/torrent/port (BITNEWS_TORRENT_PORT, Deps.Torrent.Port, fixes
// the port instead); the sockets are bound as settings.network says.
// A netbind.Killswitch per bound protocol pauses its Queue while the binding
// is unavailable and resumes only what it paused. Sockets are not rebound
// live: a change of the network settings logs a warning and needs a restart.
//
// # Speed, queue and disk settings
//
// speedApplier gives the runners the limits of settings.speed at start and on
// every change of them (the overall limit caps each protocol, so each runner
// gets the tighter of it and its own). Queue sizes, min_free and the Share
// Limit defaults are read from the Settings by the runners on each pass.
// settings.torrent and queues.postproc_concurrency are read once, at start:
// restartWatcher logs a warning when they change, and the web UI says that a
// restart is pending (webConfig).
//
// # Lifecycle
//
// Run recovers interrupted work (Downloads left Fetching go back to Queued,
// those left PostProcessing start again from the first stage), runs the
// runner, the Post-processor and the session cleanup, and returns when ctx
// ends and all of them have stopped; the torrent engine, when Build made it, is
// closed after them. Serve orders a graceful shutdown: it
// stops the HTTP server first, then cancels the workers and waits for them.
// The caller closes the database afterwards.
package daemon
