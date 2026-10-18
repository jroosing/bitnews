// Package runner is the Usenet Queue runner. It decides, with core.Select,
// which Usenet Downloads are active, and for every active Download it parses
// the stored NZB, runs the Article scheduler over the configured Servers and
// writes the decoded parts straight into the Download's working directory
// until the Download is Fetched.
//
// # Control loop
//
// Run reacts to the Bus events DownloadAdded, DownloadUpdated, Removed,
// SettingsChanged, Fetched and Failed and, as a fallback, polls every
// Config.PollInterval (10 s). Each pass lists the Queued and Fetching NZB
// Downloads and applies core.Select with the Usenet limit. A chosen Queued
// Download becomes Fetching and starts a fetch (fetchOne); a Fetching
// Download that is no longer chosen (a lower priority, a new limit, paused)
// is cancelled and moved back to Queued. A Download that vanishes from the
// repository, or whose Removed event arrives, is cancelled at once; its data
// is left to whoever removes it.
//
// Servers come from ServerSource on every pass. There is one nntp.Pool per
// Server for the whole Runner, shared by all Downloads and reconfigured in
// place when a Server changes. Running fetches pick up a changed Server set
// without losing work: the scheduler is restarted for the Articles that are
// not resolved yet.
//
// # What a fetch does
//
//   - All files except PAR2 recovery volumes (.volNN+MM.par2) are fetched.
//     Recovery volumes are deferred until M3-07 un-defers them; the small
//     index .par2 is fetched like any data file. The file key of an Article
//     is the index of its file in NZB.Files.
//   - Results are written with filewriter. Missing Articles are counted; the
//     byte range of a Missing Article is not known before decoding (NZB
//     sizes are encoded sizes), so it is not marked in the writer. A file
//     whose Articles are all missing is never created by the writer; it is
//     counted in FetchDetail.MissingFiles and the Download still reaches
//     Fetched, so PAR2 can decide what to do.
//   - Fetched carries a FetchReport (Runner.Report, Runner.FetchReport),
//     which implements the core.FetchReporter port.
//   - A Download with no fetchable Article, or an NZB that does not parse, is
//     Failed with source_invalid.
//   - When no Server answered a single request (wrong password, provider
//     down) the Download is not marked Fetched with everything Missing. It
//     goes back to Queued and waits Config.FailureBackoff (30 s).
//
// # Pause, speed limit and disk guard
//
// core.Download.Paused is honoured through core.Select. PauseQueue and
// ResumeQueue pause the whole Queue at run time. SetSpeedLimit applies a
// token bucket to the bytes received from the network. Before a Download is
// activated, and every Config.DiskCheckInterval while it runs, the free space
// of the incomplete root must be at least the remaining bytes plus min_free
// (default 5 GiB). Otherwise the Queue is paused with reason disk_full, the
// event QueuePaused is published and the Queue resumes by itself when the
// space is back. QueuePaused reports the state for the API.
//
// # Resume and restart
//
// A fetch keeps a resume blob (see ProgressStore): the filewriter snapshot
// plus the Articles already resolved. It is saved every Config.SaveInterval,
// when a Download is paused or pre-empted, and on shutdown, and loaded the
// next time the Download starts, so finished Articles are not fetched again.
// With Config.Progress nil the blob is only kept in memory: pause and resume
// continue where they left off, but a restart cannot.
//
// Limitation until the persistent store (M2-12) is wired: after a restart
// there is no blob, so the whole Download is fetched again. To keep the
// writer from creating duplicates such as movie.mkv.1 next to the files of
// the interrupted run, a fetch that starts without a usable blob first
// deletes every file directly inside the working directory except the stored
// .nzb. A blob that no longer matches the disk (files missing or truncated)
// only resets the affected files.
//
// Recover moves Downloads found Fetching back to Queued; call it before Run
// after a restart.
package runner
