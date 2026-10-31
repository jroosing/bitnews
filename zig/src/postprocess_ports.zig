//! The ports of the post-processing pipeline (internal/core/postprocess_ports.go).
//! The orchestrator (postprocess.zig) owns these interfaces; par2, unpack,
//! finalize and the fetch side implement them and `services.zig` wires them.
//!
//! Idempotency contract, as in Go: progress is not persisted mid-stage. After
//! a restart a Download in post_processing is processed again from the first
//! stage, so every port must be safe to call again on a directory that an
//! interrupted call left half done. Every method that does I/O returns
//! `error.Canceled` promptly when its task is cancelled; the orchestrator
//! treats that as "stop now, change nothing".
//!
//! Errors come back as a `PortError` class plus text in a `Diag`: the class
//! maps to a FailureReason, the text is what Go's `err.Error()` says and ends
//! up (redacted) in the Download's failure detail. Strings in a `Diag` and in
//! results live in the arena the caller passes.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const dl = @import("download.zig");

pub const PortError = error{
    /// The recovery data on disk does not cover the damage; `Diag.needed` and
    /// `Diag.available` hold the block counts when they are known (Go's
    /// *RecoveryShortfall).
    InsufficientRecovery,
    /// Repair ran and could not produce correct files.
    RepairFailed,
    /// Articles are missing or damaged and there is no PAR2 set.
    MissingArticles,
    ArchiveUnsupported,
    ArchivePassword,
    ArchiveCorrupt,
    /// An entry or path would escape its root, or a PAR2 set has unsafe names.
    PathViolation,
    DiskFull,
    Canceled,
    OutOfMemory,
    /// Anything else (Go: FailureInternal).
    Failed,
};

/// What a failing port says besides its error class.
pub const Diag = struct {
    /// Go's err.Error() text, without the class prefix the orchestrator adds.
    message: []const u8 = "",
    /// Set with InsufficientRecovery; zero when unknown.
    needed: i64 = 0,
    available: i64 = 0,
    /// `message` is shown as it is (only cut to length), not redacted: Go's
    /// FailureDetail() of the permission error, whose text the daemon wrote.
    vouched: bool = false,
};

pub const RepairOutcome = struct {
    /// A usable PAR2 set is in the directory. False means nothing was
    /// checked and the orchestrator asks the FetchReport instead.
    has_par2: bool = false,
    /// Slices rebuilt; zero when the files were intact.
    repaired_slices: i64 = 0,
};

/// Checks a Download's files against its PAR2 set and repairs them in one
/// pass (par2 does the work: ADR-0007).
pub const Repairer = struct {
    ctx: *anyopaque,
    repair_fn: *const fn (ctx: *anyopaque, arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError!RepairOutcome,

    pub fn repair(r: Repairer, arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError!RepairOutcome {
        return r.repair_fn(r.ctx, arena, io, dir, diag);
    }
};

pub const UnpackResult = struct {
    files: i64 = 0,
    bytes: i64 = 0,
};

/// Finds and extracts archives (7zz does the work). Unpack never modifies or
/// deletes the archives: torrents keep seeding them.
pub const Unpacker = struct {
    ctx: *anyopaque,
    /// Whether `dir` holds archives to unpack. A set with a volume missing is
    /// ArchiveCorrupt.
    detect_fn: *const fn (ctx: *anyopaque, arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError!bool,
    /// Extracts every archive set of `dir` into `dst`, which does not exist
    /// yet. `download_id` lets the adapter find the archive password.
    unpack_fn: *const fn (ctx: *anyopaque, arena: Allocator, io: Io, dir: []const u8, dst: []const u8, download_id: []const u8, diag: *Diag) PortError!UnpackResult,

    pub fn detect(u: Unpacker, arena: Allocator, io: Io, dir: []const u8, diag: *Diag) PortError!bool {
        return u.detect_fn(u.ctx, arena, io, dir, diag);
    }

    pub fn unpack(u: Unpacker, arena: Allocator, io: Io, dir: []const u8, dst: []const u8, download_id: []const u8, diag: *Diag) PortError!UnpackResult {
        return u.unpack_fn(u.ctx, arena, io, dir, dst, download_id, diag);
    }
};

/// The last step: cleanup and move to the complete location. The behaviour
/// differs by `d.source_kind` and is the adapter's job (see Go's Finisher).
/// `unpacked_dir` is empty when nothing was unpacked. Returns the path where
/// the importable files are. If `dir` no longer exists (a previous run moved
/// it) the orchestrator calls with an empty `unpacked_dir` and the Finisher
/// returns the already moved result or fails.
pub const Finisher = struct {
    ctx: *anyopaque,
    finish_fn: *const fn (ctx: *anyopaque, arena: Allocator, io: Io, d: dl.Download, dir: []const u8, unpacked_dir: []const u8, diag: *Diag) PortError![]const u8,

    pub fn finish(f: Finisher, arena: Allocator, io: Io, d: dl.Download, dir: []const u8, unpacked_dir: []const u8, diag: *Diag) PortError![]const u8 {
        return f.finish_fn(f.ctx, arena, io, d, dir, unpacked_dir, diag);
    }
};

/// What the fetch side knows about a Download's articles.
pub const FetchReport = struct {
    missing_articles: i64 = 0,
    damaged_articles: i64 = 0,

    pub fn damaged(r: FetchReport) bool {
        return r.missing_articles > 0 or r.damaged_articles > 0;
    }
};

/// The FetchReport, used when a Download has no PAR2 set to verify against.
pub const FetchReporter = struct {
    ctx: *anyopaque,
    report_fn: *const fn (ctx: *anyopaque, arena: Allocator, io: Io, d: dl.Download) PortError!FetchReport,

    pub fn report(r: FetchReporter, arena: Allocator, io: Io, d: dl.Download) PortError!FetchReport {
        return r.report_fn(r.ctx, arena, io, d);
    }
};

/// The contract with the fetch side for recovery volumes that were not
/// downloaded up front: asks it to un-defer volumes that cover at least
/// `min_blocks` more blocks and to move the Download back to fetching itself.
/// False, with nothing changed, when all deferred volumes together cannot.
pub const DeferredVolumes = struct {
    ctx: *anyopaque,
    fetch_fn: *const fn (ctx: *anyopaque, io: Io, d: dl.Download, min_blocks: i64) PortError!bool,

    pub fn fetchDeferred(v: DeferredVolumes, io: Io, d: dl.Download, min_blocks: i64) PortError!bool {
        return v.fetch_fn(v.ctx, io, d, min_blocks);
    }
};
