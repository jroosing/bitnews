//! The adapter that implements `postprocess_ports.Unpacker` on top of
//! `detect` and `unpack` (internal/postproc/unpack ports.go). 7zz does the
//! extraction through the `Runner` seam; this file decides which sets to
//! extract, finds the archive password and merges several sets into one
//! directory.
//!
//! Ownership: an `Adapter` is built once by the daemon and outlives every
//! call; it holds no per-request state, so concurrent calls are fine. The
//! Runner, the PasswordSource and the free-space probe are borrowed.
//! Everything a call returns (messages, results) lives in the arena the
//! caller passes; scratch memory comes from `gpa` and is returned before the
//! call does.
//!
//! Password: the PasswordSource (the daemon plugs the sealed store in)
//! returns the plain text in the arena of the call; a copy goes into `gpa`
//! and is wiped when the call ends, and the text is never logged or put into
//! an error.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Dir = Io.Dir;
const ports = @import("postprocess_ports.zig");
const tool = @import("tool_types.zig");
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const unpack = @import("unpack.zig");
const detect_mod = @import("unpack_detect.zig");
const name_mod = @import("unpack_name.zig");
const ufs = @import("unpack_fs.zig");
const fsutil = @import("fsutil.zig");
const log = @import("log.zig");

/// Finds the archive password of a Download: the text, or null for none (an
/// empty text means none too). An error is returned to the orchestrator as it
/// is (class Failed).
pub const PasswordSource = struct {
    ctx: ?*anyopaque = null,
    get_fn: *const fn (ctx: ?*anyopaque, arena: Allocator, download_id: []const u8) anyerror!?[]const u8,
};

/// The disk space to keep free, read at each unpack so it can change while
/// the daemon runs.
pub const MinFreeSource = struct {
    ctx: ?*anyopaque = null,
    get_fn: *const fn (ctx: ?*anyopaque) u64,
};

pub const Adapter = struct {
    gpa: Allocator,
    runner: tool.Runner,
    /// The 7zz program, as `tool.locate` found it.
    seven_zip: []const u8,
    passwords: ?PasswordSource = null,
    min_free: u64 = 0,
    min_free_source: ?MinFreeSource = null,
    free_space: env_mod.FreeSpace = .{},
    max_entries: usize = env_mod.default_max_entries,
    max_bytes: i64 = 0,
    bomb_min: i64 = env_mod.bomb_min_bytes,
    bomb_ratio: i64 = env_mod.bomb_ratio,
    logger: ?log.Logger = null,

    pub fn unpacker(self: *Adapter) ports.Unpacker {
        return .{ .ctx = self, .detect_fn = detectFn, .unpack_fn = unpackFn };
    }

    fn envFor(self: *Adapter, arena: Allocator, io: Io, diag: *ports.Diag) Env {
        const min_free = if (self.min_free_source) |m| m.get_fn(m.ctx) else self.min_free;
        return .{
            .io = io,
            .gpa = self.gpa,
            .arena = arena,
            .runner = self.runner,
            .logger = self.logger,
            .diag = diag,
            .cfg = .{
                .seven_zip = self.seven_zip,
                .min_free = min_free,
                .max_bytes = self.max_bytes,
                .max_entries = self.max_entries,
                .free_space = self.free_space,
                .bomb_min = self.bomb_min,
                .bomb_ratio = self.bomb_ratio,
            },
        };
    }

    fn detectFn(ctx: *anyopaque, arena: Allocator, io: Io, dir: []const u8, diag: *ports.Diag) ports.PortError!bool {
        const self: *Adapter = @ptrCast(@alignCast(ctx));
        var env = self.envFor(arena, io, diag);
        const sets = detectComplete(&env, dir) catch |err| return errors.toPort(err);
        return sets.len > 0;
    }

    fn unpackFn(ctx: *anyopaque, arena: Allocator, io: Io, dir: []const u8, dst: []const u8, download_id: []const u8, diag: *ports.Diag) ports.PortError!ports.UnpackResult {
        const self: *Adapter = @ptrCast(@alignCast(ctx));
        var env = self.envFor(arena, io, diag);
        const pw = self.password(&env, download_id) catch |err| return errors.toPort(err);
        defer if (pw.len > 0) {
            std.crypto.secureZero(u8, pw);
            self.gpa.free(pw);
        };
        env.cfg.password = pw;
        const res = unpackAll(&env, dir, dst) catch |err| return errors.toPort(err);
        return res;
    }

    /// The password of the Download in a buffer of `gpa` (empty: none).
    fn password(self: *Adapter, env: *Env, id: []const u8) Error![]u8 {
        const src = self.passwords orelse return &.{};
        const got = src.get_fn(src.ctx, env.arena, id) catch |err| switch (err) {
            error.Canceled => return error.Canceled,
            error.OutOfMemory => return error.OutOfMemory,
            else => return errors.fail(env.arena, env.diag, error.Internal, "archive password: {s}", .{@errorName(err)}),
        };
        const text = got orelse return &.{};
        if (text.len == 0) return &.{};
        return env.gpa.dupe(u8, text);
    }
};

/// The sets of `dir`, refusing an incomplete one. A set that is not complete
/// (a volume is missing, or a volume number is there twice) is a damaged
/// download, whatever the cause: the alternative, unpacking the sets that are
/// complete and ignoring the broken one, would hand the user a partial result
/// that looks like a success.
pub fn detectComplete(env: *Env, dir: []const u8) Error![]detect_mod.ArchiveSet {
    try env.io.checkCancel();
    const sets = try detect_mod.detect(env, dir);
    for (sets) |s| {
        if (s.err) |m| {
            env.diag.message = try std.fmt.allocPrint(env.arena, "archive corrupt: archive {f}: {s}", .{ name_mod.quoted(s.name), m });
            return error.MissingVolume;
        }
    }
    return sets;
}

/// Extracts every archive set of `dir`, in order, into `dst`. The archives
/// are never modified.
///
/// One set is unpacked straight into `dst`, which is staged next to it and
/// renamed into place. Several sets are each unpacked into their own
/// directory under dst + ".sets" (cleared first), merged into one tree there
/// and renamed to `dst`: where two sets have a directory of the same name the
/// directories are merged, and where anything else collides the later entry
/// gets a free name from `fsutil.unique` ("a.mkv" becomes "a.1.mkv"), so
/// nothing is overwritten. In both cases an existing `dst` is replaced, which
/// is what makes a repeated run after a crash safe (with several sets the
/// replacement is a removal followed by the rename).
pub fn unpackAll(env: *Env, dir: []const u8, dst: []const u8) Error!ports.UnpackResult {
    const sets = try detectComplete(env, dir);
    if (sets.len == 0) {
        env.diag.message = "archive corrupt: no archive is left in the download";
        return error.Corrupt;
    }
    if (sets.len == 1) {
        env.cfg.replace = true;
        const res = try unpack.unpack(env, sets[0], dst);
        return .{ .files = @intCast(res.files.len), .bytes = res.bytes };
    }
    return unpackMany(env, sets, dst);
}

fn unpackMany(env: *Env, sets: []const detect_mod.ArchiveSet, dst_in: []const u8) Error!ports.UnpackResult {
    const a = env.arena;
    const io = env.io;
    const cwd = Dir.cwd();
    const dst = ufs.absPath(a, io, dst_in) catch |err| return errors.io(a, env.diag, err, "resolve destination");
    const work = try std.mem.concat(a, u8, &.{ dst, ".sets" });
    ufs.removeTree(io, work) catch |err| return errors.io(a, env.diag, err, "clear unpack work directory");
    cwd.createDirPath(io, work) catch |err| return errors.io(a, env.diag, err, "create unpack work directory");
    defer ufs.removeTree(io, work) catch {}; // best effort; the next run clears it first

    var total: ports.UnpackResult = .{};
    var parts = try a.alloc([]const u8, sets.len);
    for (sets, 0..) |s, i| {
        parts[i] = try std.fmt.allocPrint(a, "{s}/{d}", .{ work, i });
        var one = env.*;
        one.cfg.replace = false;
        const res = try unpack.unpack(&one, s, parts[i]);
        total.files += @intCast(res.files.len);
        total.bytes += res.bytes;
    }

    const merged = try std.fmt.allocPrint(a, "{s}/merged", .{work});
    cwd.createDir(io, merged, .default_dir) catch |err| return errors.io(a, env.diag, err, "create merge directory");
    for (parts) |p| {
        try io.checkCancel();
        try mergeInto(env, p, merged);
    }
    try io.checkCancel();
    ufs.removeTree(io, dst) catch |err| return errors.io(a, env.diag, err, "replace unpack destination");
    cwd.rename(merged, cwd, dst, io) catch |err| return errors.io(a, env.diag, err, "move merged archives into place");
    return total;
}

/// Moves the entries of the directory `src` into `dst`. Two directories of
/// the same name are merged; any other collision gives the incoming entry a
/// free name, so nothing is overwritten.
pub fn mergeInto(env: *Env, src: []const u8, dst: []const u8) Error!void {
    const a = env.arena;
    const io = env.io;
    const cwd = Dir.cwd();
    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    var d = cwd.openDir(io, src, .{ .iterate = true, .follow_symlinks = false }) catch |err| return errors.io(a, env.diag, err, "merge unpacked archives");
    const names = ufs.names(env, step.allocator(), d) catch |err| {
        d.close(io);
        return errors.io(a, env.diag, err, "merge unpacked archives");
    };
    d.close(io);
    for (names) |name| {
        const from = try fsutil.join(a, &.{ src, name });
        var to = try fsutil.join(a, &.{ dst, name });
        const incoming = (ufs.lstat(io, cwd, from) catch |err| return errors.io(a, env.diag, err, "merge unpacked archives")) orelse continue;
        const existing = ufs.lstat(io, cwd, to) catch |err| return errors.io(a, env.diag, err, "merge unpacked archives");
        if (existing) |ex| {
            if (ex.kind == .directory and incoming.kind == .directory) {
                try mergeInto(env, from, to);
                continue;
            }
            to = fsutil.unique(a, io, cwd, to) catch |err| return errors.io(a, env.diag, err, "merge unpacked archives");
        }
        cwd.rename(from, cwd, to, io) catch |err| return errors.io(a, env.diag, err, "merge unpacked archives");
    }
}
