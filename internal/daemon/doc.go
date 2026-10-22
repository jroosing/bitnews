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
// # Lifecycle
//
// Run recovers interrupted work (Downloads left Fetching go back to Queued,
// those left PostProcessing start again from the first stage), runs the
// runner, the Post-processor and the session cleanup, and returns when ctx
// ends and all of them have stopped. Serve orders a graceful shutdown: it
// stops the HTTP server first, then cancels the workers and waits for them.
// The caller closes the database afterwards.
package daemon
