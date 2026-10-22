// Package scheduler dispatches the Articles of one Download to per-Server
// connection workers and falls back along Server Priority.
//
// A Scheduler handles one set of Articles. It has no global state, so the
// Usenet Queue runner can run several Schedulers at once, one per active
// Download.
//
// # Dispatch
//
// Every Server contributes Workers goroutines. The distinct Server Priority
// values form levels, lowest first. An Article starts at the lowest level;
// only workers of Servers at the Article's current level take it, in
// (FileKey, SegmentNumber) order. Servers at the same level share load, and
// each tries an Article at most once. When every enabled Server of the level
// has tried the Article (absent, bad CRC, retries exhausted, retention too
// short, or Server dead) it moves to the next level. When no level is left it
// is Missing, or Done with Damaged set if some Server delivered a copy that
// failed its CRC check.
//
// Taking the next Article is O(1) amortized: the lowest level walks the sorted
// Article list with a cursor, and Articles promoted to a higher level or
// handed to a sibling Server go through a small per-Server min-heap.
//
// # Errors
//
//   - nntp.ErrArticleNotFound: the Server has tried the Article.
//   - nntp.ErrConnection, nntp.ErrProtocol and other unclassified errors are
//     transient: the same Server is retried up to MaxAttempts times in all,
//     waiting Config.Backoff between attempts, and then counts as tried.
//   - nntp.ErrServerDisabled, nntp.ErrAuth and nntp.ErrPoolClosed mark the
//     Server dead for the rest of the run: its workers exit, it counts as
//     tried for every remaining Article and Config.OnServerDown fires once.
//   - A part that fails yEnc decoding counts as absent for that Server.
//   - A part that decodes but fails an announced CRC check counts as absent for that
//     Server, but the largest such copy is kept as the best damaged copy. A
//     part whose trailer announces no CRC is good.
//
// # Results and back-pressure
//
// Config.OnResult is called once per Article from the worker goroutines, so
// it must be safe for concurrent use; the runner serializes. A slow callback
// blocks its worker, which is the back-pressure. The Part handed over is owned
// by the callee.
//
// Run blocks until every Article is resolved or the context ends. After a
// cancel all goroutines exit before Run returns, Run returns the context
// error and results already delivered stay delivered.
package scheduler
