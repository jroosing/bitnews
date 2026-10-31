//! Nested archives (internal/postproc/unpack nested.go): the archive sets
//! found in the extracted output are unpacked one level deep: a RAR inside a
//! RAR is unpacked, a RAR inside that is left alone. Each inner archive is
//! extracted next to where it lay, under the same guards, and its volumes
//! are deleted afterwards. Everything stays inside the staging directory.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Dir = Io.Dir;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const unpack = @import("unpack.zig");
const detect_mod = @import("unpack_detect.zig");
const name_mod = @import("unpack_name.zig");
const ufs = @import("unpack_fs.zig");
const fsutil = @import("fsutil.zig");

pub fn unpackNested(env: *Env, stage: []const u8, res: *unpack.Result) Error!void {
    const a = env.arena;
    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    const sa = step.allocator();

    const rels = try collect(env, sa, stage, .directory);
    std.mem.sort([]const u8, rels.names, {}, unpack.lessString);
    var n: usize = 0;
    for (rels.names) |rel| {
        const dir = try fsutil.join(a, &.{ stage, rel });
        const sets = try detect_mod.detect(env, dir);
        for (sets) |set| {
            try env.io.checkCancel();
            try unpackInner(env, stage, dir, set, n);
            n += 1;
        }
    }
    if (n == 0) return;
    res.nested = true;
    try recount(env, stage, res);
}

/// Extracts one inner set into a scratch directory below `stage` and moves
/// the result into `dir`. A name that is already taken is made unique, so
/// the inner archive can never overwrite what the outer one produced.
fn unpackInner(env: *Env, stage: []const u8, dir: []const u8, set: detect_mod.ArchiveSet, n: usize) Error!void {
    const a = env.arena;
    const io = env.io;
    if (set.err) |m| {
        env.diag.message = m;
        errors.wrap(a, env.diag, "nested archive {f}", .{name_mod.quoted(set.name)});
        return error.MissingVolume;
    }
    var inner = env.*;
    if (env.cfg.max_bytes > 0) {
        // The byte cap covers the outer and the inner archives together.
        var step: std.heap.ArenaAllocator = .init(env.gpa);
        defer step.deinit();
        const used = (try collect(env, step.allocator(), stage, .file)).bytes;
        inner.cfg.max_bytes = @max(env.cfg.max_bytes - used, 1);
    }
    const scratch = try std.fmt.allocPrint(a, "{s}/.nested-{d}", .{ stage, n });
    _ = unpack.unpackLevel(&inner, set, scratch, false) catch |err| {
        errors.wrap(a, env.diag, "nested archive {f}", .{name_mod.quoted(set.name)});
        return err;
    };

    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    const sa = step.allocator();
    var sd = Dir.cwd().openDir(io, scratch, .{ .iterate = true, .follow_symlinks = false }) catch |err| return errors.io(a, env.diag, err, "open nested output");
    const names = ufs.names(env, sa, sd) catch |err| {
        sd.close(io);
        return errors.io(a, env.diag, err, "read nested output");
    };
    sd.close(io);
    const cwd = Dir.cwd();
    for (names) |name| {
        const want = try fsutil.join(a, &.{ dir, name });
        const target = try fsutil.unique(a, io, cwd, want);
        const from = try fsutil.join(a, &.{ scratch, name });
        cwd.rename(from, cwd, target, io) catch |err| return errors.io(a, env.diag, err, "move nested output");
    }
    cwd.deleteDir(io, scratch) catch |err| return errors.io(a, env.diag, err, "remove nested scratch directory");
    for (set.volumes) |v| {
        cwd.deleteFile(io, v) catch |err| return errors.io(a, env.diag, err, "remove nested archive");
    }
}

const Collected = struct { names: [][]const u8, bytes: i64 };

/// The relative paths of the entries of `kind` below `root` ("" is the root
/// itself, in the list of directories), and the total size of the regular
/// files met. Names live in `sa`.
fn collect(env: *Env, sa: Allocator, root: []const u8, kind: Io.File.Kind) Error!Collected {
    const io = env.io;
    var list: std.ArrayList([]const u8) = .empty;
    var bytes: i64 = 0;
    if (kind == .directory) try list.append(sa, "");
    var d = Dir.cwd().openDir(io, root, .{ .iterate = true, .follow_symlinks = false }) catch |err| return errors.io(env.arena, env.diag, err, "open staging directory");
    defer d.close(io);
    var w = d.walk(sa) catch return error.OutOfMemory;
    defer w.deinit();
    while (w.next(io) catch |err| return errors.io(env.arena, env.diag, err, "walk staging directory")) |e| {
        if (e.kind == .file) {
            const st = ufs.lstat(io, e.dir, e.basename) catch |err| return errors.io(env.arena, env.diag, err, "stat nested file");
            if (st) |s| bytes = env_mod.addSat(bytes, @intCast(@min(s.size, @as(u64, 1) << 62)));
        }
        if (e.kind == kind) try list.append(sa, try sa.dupe(u8, e.path));
    }
    return .{ .names = list.items, .bytes = bytes };
}

/// Rebuilds the file list and byte total from what is in `stage`.
fn recount(env: *Env, stage: []const u8, res: *unpack.Result) Error!void {
    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    const c = try collect(env, step.allocator(), stage, .file);
    const files = try env.arena.alloc([]const u8, c.names.len);
    for (c.names, files) |n, *f| f.* = try env.arena.dupe(u8, n);
    std.mem.sort([]const u8, files, {}, unpack.lessString);
    res.files = files;
    res.bytes = c.bytes;
}
