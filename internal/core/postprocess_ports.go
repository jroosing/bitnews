package core

import (
	"context"
	"errors"
	"fmt"
)

// The ports of the Post-processing pipeline. core owns the orchestrator
// and these interfaces; the implementations live in other packages (PAR2 in
// internal/postproc/par2, archives in internal/postproc/unpack, deferred
// recovery volumes in the Usenet runner, cleanup and move in the finisher)
// and are wired in the composition root.
//
// Idempotency contract. Post-processing progress is not persisted mid-stage.
// After a restart a Download in StatePostProcessing is processed again from
// the first stage, so every port method must be safe to call again on a
// directory a previous, interrupted call left half-done:
//
//   - Repair renames obfuscated files to their PAR2 names and rewrites
//     damaged ones in a way a second run can redo from the start (a clean
//     set is only read).
//   - Unpack clears its staging directory first and replaces dst atomically.
//   - Finish tolerates a half-done cleanup or move (for example the
//     destination already existing from the interrupted run).
//
// Every method that does I/O takes the context and must return promptly,
// with an error wrapping the context's, when it is cancelled. The
// orchestrator treats a cancelled context as "stop now, change nothing".

// Sentinel errors the ports return, wrapped with detail where useful. The
// orchestrator maps them to a FailureReason with errors.Is; any other error
// is FailureInternal.
var (
	// ErrInsufficientRecovery: the recovery data on disk does not cover the
	// damage. Return a *RecoveryShortfall (which wraps it) when the counts
	// are known.
	ErrInsufficientRecovery = errors.New("not enough recovery data")
	// ErrRepairFailed: Repair ran and could not produce correct files
	// (singular matrix, result fails verification).
	ErrRepairFailed = errors.New("repair failed")
	// ErrMissingArticles: articles are missing or damaged and there is no
	// PAR2 set to recover them with.
	ErrMissingArticles = errors.New("missing articles")

	// ErrArchiveUnsupported: the archive format or feature is not handled.
	ErrArchiveUnsupported = errors.New("archive unsupported")
	// ErrArchivePassword: the archive password is missing or wrong.
	ErrArchivePassword = errors.New("archive password missing or wrong")
	// ErrArchiveCorrupt: the archive is damaged beyond Unpack.
	ErrArchiveCorrupt = errors.New("archive corrupt")
	// ErrPathViolation: an entry or path would escape its root.
	ErrPathViolation = errors.New("path violation")
	// ErrDiskFull: there is not enough space to continue.
	ErrDiskFull = errors.New("disk full")
)

// RecoveryShortfall is the error Repair returns when the recovery data on disk is too small. It wraps
// ErrInsufficientRecovery.
type RecoveryShortfall struct {
	// Needed is the number of recovery blocks the damage requires.
	Needed int
	// Available is the number of recovery blocks on disk.
	Available int
}

// Error implements error.
func (e *RecoveryShortfall) Error() string {
	return fmt.Sprintf("%v: need %d blocks, have %d", ErrInsufficientRecovery, e.Needed, e.Available)
}

// Unwrap lets errors.Is match ErrInsufficientRecovery.
func (e *RecoveryShortfall) Unwrap() error { return ErrInsufficientRecovery }

// RepairOutcome is what Repair found and did.
type RepairOutcome struct {
	// HasPAR2 says the directory holds a usable PAR2 set. When false
	// nothing was checked and the orchestrator asks the FetchReport instead.
	HasPAR2 bool
	// RepairedSlices is the number of slices rebuilt; zero when the files
	// were intact.
	RepairedSlices int
}

// Repairer checks a Download's files against its PAR2 set and repairs them
// in one pass: it renames obfuscated files to their set names and rebuilds
// damaged ones from the recovery data. It returns a *RecoveryShortfall
// (which wraps ErrInsufficientRecovery) when the recovery data on disk does
// not cover the damage, ErrRepairFailed when the repair could not produce
// correct files, and ErrPathViolation for a set with unsafe file names.
type Repairer interface {
	Repair(ctx context.Context, dir string) (RepairOutcome, error)
}

// UnpackOptions carries what Unpack needs beyond the directories.
type UnpackOptions struct {
	// DownloadID is the Download being unpacked, so the adapter can find
	// its password.
	DownloadID ID
}

// UnpackResult is what Unpack produced.
type UnpackResult struct {
	Files int
	Bytes int64
}

// Unpacker finds and extracts archives. Unpack never modifies or deletes
// the archives: torrents keep seeding them.
type Unpacker interface {
	// Detect reports whether dir holds archives to unpack. A set with a
	// volume missing is ErrArchiveCorrupt.
	Detect(ctx context.Context, dir string) (bool, error)
	// Unpack extracts every archive set of dir into dst, which does not
	// exist yet. It returns ErrArchiveUnsupported, ErrArchivePassword,
	// ErrArchiveCorrupt, ErrPathViolation or ErrDiskFull in the cases those
	// say.
	Unpack(ctx context.Context, dir, dst string, opts UnpackOptions) (UnpackResult, error)
}

// Finisher does the last step: cleanup and move to the complete location.
// The behaviour differs by d.SourceKind and is the adapter's job:
//
//   - SourceNZB: delete the PAR2, archive and NZB files, then move the
//     result (unpackedDir when it is not empty, else dir) to the complete
//     location.
//   - SourceTorrent: the originals stay where they are so seeding goes on;
//     unpackedDir, when not empty, is a subdirectory of dir.
//
// If dir no longer exists (a previous run finished and the daemon stopped
// before Ready was recorded) the orchestrator calls Finish with an empty
// unpackedDir and Finish must return the already-moved result or an error.
//
// unpackedDir is empty when nothing was unpacked. Finish returns the path
// where the importable files are.
type Finisher interface {
	Finish(ctx context.Context, d Download, dir, unpackedDir string) (outputPath string, err error)
}

// FS is the little the orchestrator itself needs from the filesystem.
type FS interface {
	// DirExists reports whether dir exists and is a directory.
	DirExists(dir string) (bool, error)
}

// FetchReport is what the fetch side knows about a Download's articles.
type FetchReport struct {
	// MissingArticles is how many articles no server had.
	MissingArticles int
	// DamagedArticles is how many articles failed their yEnc CRC.
	DamagedArticles int
}

// Damaged reports whether any article is missing or damaged.
func (r FetchReport) Damaged() bool { return r.MissingArticles > 0 || r.DamagedArticles > 0 }

// FetchReporter provides the FetchReport, used when a Download has no PAR2
// set to verify against.
type FetchReporter interface {
	FetchReport(ctx context.Context, d Download) (FetchReport, error)
}

// DeferredVolumes is the contract with the fetch side for recovery volumes
// that were not downloaded up front (M3-07).
type DeferredVolumes interface {
	// FetchDeferred asks the fetch side to un-defer recovery volumes that
	// cover at least minBlocks more blocks and to move the Download back
	// to StateFetching itself. It returns more=false, and changes
	// nothing, when all deferred volumes together cannot cover minBlocks.
	FetchDeferred(ctx context.Context, d Download, minBlocks int) (more bool, err error)
}
