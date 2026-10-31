//! File system helpers of the unpack slice. Paths are absolute strings; every
//! operation that has to avoid a symlink says so (lstat, nofollow opens). The
//! names that come back are copies in the caller's allocator, sorted bytewise
//! as Go's os.ReadDir sorts them.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Dir = Io.Dir;
const File = Io.File;
const fsutil = @import("fsutil.zig");
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;

/// lstat: null when nothing is there. Other errors are returned.
pub fn lstat(io: Io, dir: Dir, path: []const u8) !?File.Stat {
    return dir.statFile(io, path, .{ .follow_symlinks = false }) catch |err| switch (err) {
        error.FileNotFound => return null,
        else => return err,
    };
}

/// Whether anything (a dangling symlink included) is at `path`.
pub fn exists(io: Io, path: []const u8) bool {
    const st = lstat(io, Dir.cwd(), path) catch return true;
    return st != null;
}

/// Makes `path` absolute (Go: filepath.Abs): joined to the working directory
/// when relative, then cleaned. The result lives in `a`.
pub fn absPath(a: Allocator, io: Io, path: []const u8) ![]u8 {
    if (fsutil.isAbs(path)) return fsutil.clean(a, path);
    const cwd = try std.process.currentPathAlloc(io, a);
    defer a.free(cwd);
    return fsutil.join(a, &.{ cwd, path });
}

fn lessName(_: void, x: []const u8, y: []const u8) bool {
    return std.mem.order(u8, x, y) == .lt;
}

/// The names in `dir` (an open, iterable directory), sorted.
pub fn names(env: *Env, a: Allocator, dir: Dir) ![][]const u8 {
    var list: std.ArrayList([]const u8) = .empty;
    var it = dir.iterate();
    while (try it.next(env.io)) |e| try list.append(a, try a.dupe(u8, e.name));
    std.mem.sort([]const u8, list.items, {}, lessName);
    return list.items;
}

/// The names of the regular files in `dir`, sorted; symlinks, directories and
/// special files are left out.
pub fn regularNames(env: *Env, a: Allocator, dir: Dir) ![][]const u8 {
    var list: std.ArrayList([]const u8) = .empty;
    var it = dir.iterate();
    while (try it.next(env.io)) |e| {
        var kind = e.kind;
        if (kind == .unknown) {
            const st = (try lstat(env.io, dir, e.name)) orelse continue;
            kind = st.kind;
        }
        if (kind != .file) continue;
        try list.append(a, try a.dupe(u8, e.name));
    }
    std.mem.sort([]const u8, list.items, {}, lessName);
    return list.items;
}

/// Removes `path` and everything below it without following symlinks (a
/// symlink is unlinked, its target is untouched); a path that is gone is
/// fine. It also copes with what an archive can leave behind, directories
/// without write or search permission (Go: removeTree). Cancellation is
/// blocked, so a cleanup after a cancelled run still happens.
pub fn removeTree(io: Io, path: []const u8) !void {
    const prev = io.swapCancelProtection(.blocked);
    defer _ = io.swapCancelProtection(prev);
    const cwd = Dir.cwd();
    cwd.deleteTree(io, path) catch {
        chmodTree(io, cwd, path, 0);
        try cwd.deleteTree(io, path);
    };
}

/// Makes every directory below `name` rwx for the owner, top down, so it can
/// be entered and emptied. Best effort.
fn chmodTree(io: Io, parent: Dir, name: []const u8, depth: usize) void {
    // The recursion holds a directory iterator per level: bound it.
    if (depth > 256) return;
    const st = (lstat(io, parent, name) catch return) orelse return;
    if (st.kind != .directory) return;
    parent.setFilePermissions(io, name, .fromMode(0o700), .{}) catch return;
    var d = parent.openDir(io, name, .{ .iterate = true, .follow_symlinks = false }) catch return;
    defer d.close(io);
    var it = d.iterate();
    var buf: [std.fs.max_name_bytes]u8 = undefined;
    while (it.next(io) catch return) |e| {
        if (e.kind != .directory or e.name.len > buf.len) continue;
        // The name is invalidated by the next read, and the recursion reads
        // another directory with its own iterator: a copy is enough.
        @memcpy(buf[0..e.name.len], e.name);
        chmodTree(io, d, buf[0..e.name.len], depth + 1);
    }
}
