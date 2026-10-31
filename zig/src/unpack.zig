//! Extracts RAR, 7z and zip archives by running 7-Zip (7zz) with path and
//! size guards (internal/postproc/unpack). 7zz does the extraction (ADR-0007);
//! nothing of the archive formats is implemented here, and there is no
//! fallback.
//!
//! `detect` (unpack_detect.zig) finds the archive sets in a directory by
//! magic bytes; `unpack` extracts one set into a staging directory and
//! renames it into place.
//!
//! # Hostile archives
//!
//! Every archive is hostile, and so is the output of 7zz about it. Unpack
//! therefore wraps the tool in its own checks:
//!
//!  1. List. `7zz l -slt -ba` prints one block of fields per entry. The
//!     parser (unpack_listing.zig) is strict about the shape, because names
//!     and comments are printed into the output.
//!  2. Guard. Before a byte is written, every entry name goes through
//!     `cleanName` (`fsutil.safeName` per component; no "..", absolute paths,
//!     drive letters or NUL bytes; no names that collide once case is
//!     ignored), only files and directories are accepted (a symlink,
//!     hardlink, device, FIFO or socket fails with PathViolation), and the
//!     entry count, the declared total size, the compression ratio and the
//!     free disk space are checked. An encrypted archive without a password
//!     fails with Password.
//!  3. Extract. `7zz x -y -bd -aos -o<staging>` runs with the staging
//!     directory as its working directory. Multi-volume sets are given by
//!     their first volume only; 7zz finds the others.
//!  4. Walk. The staging tree is checked again, by what is on disk (see
//!     unpack_walk.zig).
//!  5. Publish. The staging directory is renamed to the destination.
//!
//! # Passwords and errors
//!
//! A password is written to the stdin of 7zz. It is never an argument, so it
//! does not show in the process list, and it is not part of any error. Exit
//! status and message text are mapped to the error classes by
//! `unpack_sevenzip.classify`. A cancelled run kills the process group of
//! 7zz and removes the staging directory.
//!
//! What is not guarded: memory. 7zz allocates what an archive's headers and
//! dictionaries ask for. The container's memory limit contains that
//! (ADR-0007).
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Dir = Io.Dir;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const detect_mod = @import("unpack_detect.zig");
const guard_mod = @import("unpack_guard.zig");
const sevenzip = @import("unpack_sevenzip.zig");
const walk_mod = @import("unpack_walk.zig");
const nested_mod = @import("unpack_nested.zig");
const name_mod = @import("unpack_name.zig");
const ufs = @import("unpack_fs.zig");
const fsutil = @import("fsutil.zig");
const log = @import("log.zig");

pub const ArchiveSet = detect_mod.ArchiveSet;
pub const Format = detect_mod.Format;
pub const detect = detect_mod.detect;
pub const Config = env_mod.Config;

/// Describes a finished Unpack. The strings live in `env.arena`.
pub const Result = struct {
    /// The extracted files, relative to the destination with '/'
    /// separators, sorted.
    files: []const []const u8,
    /// The total size of the extracted files.
    bytes: i64,
    /// Archives found in the output were unpacked too.
    nested: bool = false,
};

/// Extracts the archive set into `dst` with 7zz. The archive is listed first
/// and checked against the limits, then extracted into a staging directory
/// next to `dst` (`dst` + ".tmp", cleared first, so an interrupted run is
/// simply repeated), the result is checked again, and the directory is
/// renamed to `dst` once everything passed. Any failure removes the staging
/// directory and leaves `dst` as it was. It fails with `Exists` when `dst`
/// exists, unless `env.cfg.replace` is set.
pub fn unpack(env: *Env, set: ArchiveSet, dst: []const u8) Error!Result {
    if (set.err) |m| {
        env.diag.message = m;
        return error.MissingVolume;
    }
    if (set.volumes.len == 0) {
        return errors.fail(env.arena, env.diag, error.Corrupt, "archive set {f} has no volumes", .{name_mod.quoted(set.name)});
    }
    return unpackLevel(env, set, dst, true);
}

/// Unpacks one set; `nested` says whether archives found in the output are
/// unpacked too (one level only).
pub fn unpackLevel(env: *Env, set: ArchiveSet, dst_in: []const u8, nested: bool) Error!Result {
    const a = env.arena;
    const io = env.io;
    const cfg = env.cfg;
    const dst = ufs.absPath(a, io, dst_in) catch |err| return errors.io(a, env.diag, err, "resolve destination");
    if (ufs.exists(io, dst) and !cfg.replace) {
        return errors.fail(a, env.diag, error.Exists, "{s}", .{dst});
    }
    try io.checkCancel();
    const packed_size = try volumeSize(env, set.volumes);
    const first = ufs.absPath(a, io, set.volumes[0]) catch |err| return errors.io(a, env.diag, err, "resolve volume");
    const parent = try fsutil.dir(a, dst);
    Dir.cwd().createDirPath(io, parent) catch |err| return errors.io(a, env.diag, err, "create destination parent");

    // Pass 1: list the archive and apply every guard before a byte is written.
    var files: usize = 0;
    var declared: i64 = 0;
    {
        var step: std.heap.ArenaAllocator = .init(env.gpa);
        defer step.deinit();
        const sa = step.allocator();
        const entries = try sevenzip.list(env, sa, try fsutil.dir(a, first), first);
        var g = guard_mod.Guard.init(env, sa);
        for (entries) |e| {
            try g.admit(e);
            if (e.encrypted and cfg.password.len == 0) {
                return errors.fail(a, env.diag, error.Password, "the archive is encrypted and no password is set", .{});
            }
        }
        files = g.files;
        declared = g.declared;
    }
    if (cfg.max_bytes > 0 and declared > cfg.max_bytes) {
        return errors.fail(a, env.diag, error.TooLarge, "archive declares {d} bytes, cap is {d}", .{ declared, cfg.max_bytes });
    }
    if (declared > cfg.bomb_min and @divTrunc(declared, @max(packed_size, 1)) > cfg.bomb_ratio) {
        return errors.fail(a, env.diag, error.Bomb, "archive of {d} bytes declares {d}", .{ packed_size, declared });
    }
    const stage = try std.mem.concat(a, u8, &.{ dst, ".tmp" });
    try checkDisk(env, stage, declared);

    // Pass 2: extract into the staging directory and check what came out.
    // A leftover from a crashed run is stale by definition.
    ufs.removeTree(io, stage) catch |err| return errors.io(a, env.diag, err, "clear staging directory");
    Dir.cwd().createDir(io, stage, .default_dir) catch |err| return errors.io(a, env.diag, err, "create staging directory");
    var res = extractStage(env, first, stage, files, declared) catch |err| {
        ufs.removeTree(io, stage) catch {};
        return err;
    };
    if (nested) nested_mod.unpackNested(env, stage, &res) catch |err| {
        ufs.removeTree(io, stage) catch {};
        return err;
    };
    publish(env, stage, dst) catch |err| {
        ufs.removeTree(io, stage) catch {};
        return err;
    };
    return res;
}

/// Runs 7zz into `stage` and walks the result.
fn extractStage(env: *Env, first: []const u8, stage: []const u8, want_files: usize, want_bytes: i64) Error!Result {
    const io = env.io;
    try sevenzip.extract(env, stage, first);
    try io.checkCancel();
    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    var w = walk_mod.Walker.init(env, step.allocator());
    try w.run(stage);
    if (w.files.items.len > want_files or w.bytes > want_bytes) {
        return errors.fail(env.arena, env.diag, error.Corrupt, "7zz wrote {d} files and {d} bytes, the listing declared {d} and {d}", .{ w.files.items.len, w.bytes, want_files, want_bytes });
    }
    if (env.cfg.max_bytes > 0 and w.bytes > env.cfg.max_bytes) {
        return errors.fail(env.arena, env.diag, error.TooLarge, "more than {d} bytes", .{env.cfg.max_bytes});
    }
    std.mem.sort([]const u8, w.files.items, {}, lessString);
    return .{ .files = w.files.items, .bytes = w.bytes };
}

pub fn lessString(_: void, x: []const u8, y: []const u8) bool {
    return std.mem.order(u8, x, y) == .lt;
}

/// Moves the finished staging directory into place.
fn publish(env: *Env, stage: []const u8, dst: []const u8) Error!void {
    const io = env.io;
    if (ufs.exists(io, dst)) {
        if (!env.cfg.replace) return errors.fail(env.arena, env.diag, error.Exists, "{s}", .{dst});
        ufs.removeTree(io, dst) catch |err| return errors.io(env.arena, env.diag, err, "remove existing destination");
    }
    const cwd = Dir.cwd();
    cwd.rename(stage, cwd, dst, io) catch |err| return errors.io(env.arena, env.diag, err, "publish");
}

fn checkDisk(env: *Env, stage: []const u8, need: i64) Error!void {
    const a = env.arena;
    const parent = try fsutil.dir(a, stage);
    const free = env.cfg.free_space.probe(parent) catch |err| {
        if (env.logger) |l| l.warn("unpack: cannot determine free disk space, skipping the disk check", &.{log.Attr.string("err", @errorName(err))});
        return;
    };
    const min_free = env.cfg.min_free;
    if (free < min_free or @as(u64, @intCast(need)) > free - min_free) {
        return errors.fail(a, env.diag, error.DiskFull, "need {d} bytes, {d} free, {d} reserved", .{ need, free, min_free });
    }
}

/// The total size of the volumes on disk. Detect only lists regular files,
/// but the directory may have changed since: a volume swapped for a symlink
/// or a special file is refused, because 7zz would follow it.
fn volumeSize(env: *Env, paths: []const []const u8) Error!i64 {
    var total: i64 = 0;
    for (paths) |p| {
        const st = (ufs.lstat(env.io, Dir.cwd(), p) catch |err| return errors.io(env.arena, env.diag, err, "lstat volume")) orelse {
            return errors.fail(env.arena, env.diag, error.Internal, "unpack: lstat {s}: no such file or directory", .{p});
        };
        if (st.kind != .file) {
            const t = try name_mod.truncateForError(env.arena, p);
            return errors.fail(env.arena, env.diag, error.Corrupt, "volume {f} is not a regular file", .{name_mod.quoted(t)});
        }
        total = env_mod.addSat(total, @intCast(@min(st.size, @as(u64, 1) << 62)));
    }
    return total;
}
