//! The cleanup of a Usenet Download's directory (internal/postproc/finalize/
//! cleanup.go): known garbage goes, and the unpacked entries are merged into
//! the root of the directory.
//!
//! Cleanup deletes only regular files directly inside the directory and the
//! _unpack.tmp directory. It never follows a symlink and never leaves the
//! directory: everything is done through the handle of the directory, and a
//! name that is a link is unlinked or left alone, never entered. It is
//! idempotent, which the restart contract needs.
const std = @import("std");
const Io = std.Io;
const fsutil = @import("fsutil.zig");
const fs = @import("tidy_fs.zig");
const ctx_mod = @import("finalize_ctx.zig");
const Ctx = ctx_mod.Ctx;
const PortError = ctx_mod.PortError;
const q = ctx_mod.q;

/// The staging directory Unpack may leave inside the Download's directory.
pub const tmp_dir_name = "_unpack.tmp";

/// `.rar`, old-style `.r00`/`.s00` volumes and 7z (single or split) volumes,
/// ignoring case: Go's `\.(rar|[rs]\d{2,}|7z(\.\d{3,})?)$` with (?i). Besides
/// R and S, the long s (U+017F) counts as an S, as in RE2's case folding.
pub fn isArchive(name: []const u8) bool {
    if (endsWithIgnoreCase(name, ".rar") or endsWithIgnoreCase(name, ".7z")) return true;
    var end = name.len;
    while (end > 0 and std.ascii.isDigit(name[end - 1])) end -= 1;
    const digits = name.len - end;
    // ".7z.001", the volumes of a split 7z.
    if (digits >= 3 and end >= 1 and name[end - 1] == '.' and endsWithIgnoreCase(name[0 .. end - 1], ".7z")) return true;
    // ".r00" and ".s00" and so on.
    if (digits >= 2 and end >= 2 and name[end - 2] == '.' and std.mem.indexOfScalar(u8, "rRsS", name[end - 1]) != null) return true;
    if (digits >= 2 and end >= 3 and name[end - 3] == '.' and std.mem.eql(u8, name[end - 2 .. end], "\xc5\xbf")) return true;
    return false;
}

/// `.par2` and `.nzb`, ignoring case.
pub fn isGarbage(name: []const u8) bool {
    return endsWithIgnoreCase(name, ".par2") or endsWithIgnoreCase(name, ".nzb");
}

fn endsWithIgnoreCase(s: []const u8, suffix: []const u8) bool {
    return s.len >= suffix.len and std.ascii.eqlIgnoreCase(s[s.len - suffix.len ..], suffix);
}

/// Deletes the known garbage from `dir` (an open directory, `dir_path` for
/// messages) and, when archives were unpacked, merges `unpacked_dir` into its
/// root.
pub fn cleanUsenet(c: Ctx, dir: Io.Dir, dir_path: []const u8, unpacked_dir_in: []const u8) PortError!void {
    const unpacked = unpacked_dir_in.len > 0;
    var unpacked_dir: []const u8 = "";
    if (unpacked) {
        unpacked_dir = fsutil.clean(c.arena, unpacked_dir_in) catch return error.OutOfMemory;
        try checkUnpackedDir(c, dir_path, unpacked_dir);
    }
    const entries = fs.list(c.arena, c.io, dir) catch |err| return c.ioFail(err, "read download dir", null);

    // A missing unpacked dir is only fine on a retry, when everything was
    // already merged and the archives are gone. Archives still being there
    // means nothing was unpacked: refuse to delete them.
    var merged = false;
    if (unpacked) {
        if (Io.Dir.cwd().statFile(c.io, unpacked_dir, .{ .follow_symlinks = false })) |_| {} else |err| switch (err) {
            error.FileNotFound => {
                merged = true;
                for (entries) |e| {
                    if (e.kind == .file and isArchive(e.name)) {
                        return c.fail("finalize: unpacked dir {f} is missing but archives remain in {f}", .{ q(unpacked_dir), q(dir_path) });
                    }
                }
            },
            else => {},
        }
    }

    for (entries) |e| {
        if (e.kind == .file and (isGarbage(e.name) or unpacked and isArchive(e.name))) {
            c.io.checkCancel() catch return error.Canceled;
            dir.deleteFile(c.io, e.name) catch |err| switch (err) {
                error.FileNotFound => {},
                else => return c.ioFail(err, "remove", dir_path),
            };
        } else if (unpacked and e.kind == .directory and std.mem.eql(u8, e.name, tmp_dir_name)) {
            c.io.checkCancel() catch return error.Canceled;
            fs.removeAllWritable(c.arena, c.io, dir, e.name) catch |err| return c.ioFail(err, "remove", dir_path);
        }
    }

    if (unpacked and !merged) try mergeUnpacked(c, dir, dir_path, unpacked_dir);
}

/// Makes sure the unpacked dir is a real dir that is either inside the
/// Download's dir (but not in the _unpack.tmp leftover) or entirely separate.
fn checkUnpackedDir(c: Ctx, dir: []const u8, unpacked_dir: []const u8) PortError!void {
    const oom = error.OutOfMemory;
    if (std.mem.eql(u8, unpacked_dir, dir) or (fsutil.inside(c.arena, unpacked_dir, dir) catch return oom)) {
        return c.fail("finalize: unpacked dir {f} overlaps download dir {f}", .{ q(unpacked_dir), q(dir) });
    }
    if (fsutil.relBelow(c.arena, dir, unpacked_dir) catch return oom) |rel| {
        const first = if (std.mem.indexOfScalar(u8, rel, '/')) |i| rel[0..i] else rel;
        if (!std.mem.eql(u8, rel, ".") and std.mem.eql(u8, first, tmp_dir_name)) {
            return c.fail("finalize: unpacked dir {f} is inside {s}", .{ q(unpacked_dir), tmp_dir_name });
        }
    }
    const st = Io.Dir.cwd().statFile(c.io, unpacked_dir, .{ .follow_symlinks = false }) catch |err| switch (err) {
        error.FileNotFound => return,
        else => return c.ioFail(err, "stat unpacked dir", null),
    };
    switch (st.kind) {
        .directory => {},
        .sym_link => return c.fail("fsutil: symlink refused: unpacked dir {f}", .{q(unpacked_dir)}),
        else => return c.fail("finalize: unpacked dir {f} is not a directory", .{q(unpacked_dir)}),
    }
}

/// Moves the entries of the unpacked dir to the root of `dir`. An entry whose
/// name is taken keeps the existing file and gets a Unique name. The unpacked
/// dir is removed once it is empty.
fn mergeUnpacked(c: Ctx, dir: Io.Dir, dir_path: []const u8, unpacked_dir: []const u8) PortError!void {
    var src = Io.Dir.cwd().openDir(c.io, unpacked_dir, .{ .iterate = true, .follow_symlinks = false }) catch |err| switch (err) {
        // Emptied and removed since the check: nothing left to merge.
        error.FileNotFound => return,
        else => return c.ioFail(err, "read unpacked dir", null),
    };
    defer src.close(c.io);
    const entries = fs.list(c.arena, c.io, src) catch |err| return c.ioFail(err, "read unpacked dir", null);
    for (entries) |e| {
        c.io.checkCancel() catch return error.Canceled;
        const dst = fsutil.unique(c.arena, c.io, dir, e.name) catch |err| return c.ioFail(err, "merge unpacked output", null);
        src.renamePreserve(e.name, dir, dst, c.io) catch |err| return c.ioFail(err, "merge unpacked output", dir_path);
    }
    Io.Dir.cwd().deleteDir(c.io, unpacked_dir) catch |err| switch (err) {
        error.FileNotFound => {},
        else => return c.ioFail(err, "remove unpacked dir", null),
    };
}
