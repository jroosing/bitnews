//! Small directory helpers shared by tidy and finalize: sorted listings and
//! opens that never follow a symlink. Everything here works on directory
//! handles, so a symlink swapped in after a check cannot redirect what comes
//! next.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;

/// The most entries of one directory a listing returns; more is an error
/// (hard cap on hostile trees).
pub const max_entries = 1 << 20;

pub const Entry = struct {
    name: []const u8,
    kind: Io.File.Kind,
};

pub const ListError = Allocator.Error || Io.Dir.Iterator.Error || Io.Dir.StatFileError || error{TooManyEntries};

/// The entries of `d` (opened with `iterate`), sorted by name like Go's
/// os.ReadDir. The kind is what the entry is itself: a symlink is
/// `.sym_link`. Names live in `a`.
pub fn list(a: Allocator, io: Io, d: Io.Dir) ListError![]Entry {
    var out: std.ArrayList(Entry) = .empty;
    var it = d.iterate();
    while (try it.next(io)) |e| {
        if (out.items.len >= max_entries) return error.TooManyEntries;
        var kind = e.kind;
        if (kind == .unknown) kind = (try d.statFile(io, e.name, .{ .follow_symlinks = false })).kind;
        try out.append(a, .{ .name = try a.dupe(u8, e.name), .kind = kind });
    }
    std.mem.sort(Entry, out.items, {}, lessThan);
    return out.items;
}

fn lessThan(_: void, x: Entry, y: Entry) bool {
    return std.mem.lessThan(u8, x.name, y.name);
}

/// A directory handle that may be borrowed (the root it was found from).
pub const Opened = struct {
    dir: Io.Dir,
    owned: bool,

    pub fn close(o: Opened, io: Io) void {
        if (o.owned) o.dir.close(io);
    }
};

/// Opens the directory `rel` below `root` one component at a time without
/// following a symlink at any step. `rel` is "" or "." for `root` itself,
/// which is then borrowed. Components must be plain names ("..", empty and
/// NUL are refused with `error.BadPath`).
pub fn openBelow(io: Io, root: Io.Dir, rel: []const u8, iterate: bool) (Io.Dir.OpenError || error{BadPath})!Opened {
    var cur: Opened = .{ .dir = root, .owned = false };
    var it = std.mem.splitScalar(u8, rel, '/');
    while (it.next()) |part| {
        if (part.len == 0 or std.mem.eql(u8, part, ".")) continue;
        if (std.mem.eql(u8, part, "..") or std.mem.indexOfScalar(u8, part, 0) != null) {
            cur.close(io);
            return error.BadPath;
        }
        const next = cur.dir.openDir(io, part, .{ .iterate = iterate, .follow_symlinks = false }) catch |err| {
            cur.close(io);
            return err;
        };
        cur.close(io);
        cur = .{ .dir = next, .owned = true };
    }
    return cur;
}

/// Renames `old` to `new` inside `d` without replacing anything that exists
/// (a name taken by a racing writer is `error.PathAlreadyExists`). File
/// systems without a non-replacing rename get a check and a plain rename.
pub fn renameNoClobber(io: Io, d: Io.Dir, old: []const u8, new: []const u8) (Io.Dir.RenamePreserveError)!void {
    d.renamePreserve(old, d, new, io) catch |err| switch (err) {
        error.OperationUnsupported => {
            if (d.statFile(io, new, .{ .follow_symlinks = false })) |_| {
                return error.PathAlreadyExists;
            } else |e| switch (e) {
                error.FileNotFound => {},
                else => return error.PathAlreadyExists,
            }
            return d.rename(old, d, new, io);
        },
        else => |e| return e,
    };
}

/// Whether `name` exists in `d`, without following a symlink; any error but
/// "not found" counts as existing, as Go's Lstat check does.
pub fn exists(io: Io, d: Io.Dir, name: []const u8) bool {
    _ = d.statFile(io, name, .{ .follow_symlinks = false }) catch |err| switch (err) {
        error.FileNotFound => return false,
        else => return true,
    };
    return true;
}

/// How deep `removeAllWritable` looks for directories to open up.
const max_chmod_depth = 256;

/// Removes `name` in `parent` with everything below it, like Go's
/// removeAllWritable: every directory is made owner-accessible first, so a
/// tree with read-only directories (an archive can hold them) goes too.
/// Symlinks are never followed (they are unlinked). A missing `name` is fine.
pub fn removeAllWritable(gpa: Allocator, io: Io, parent: Io.Dir, name: []const u8) !void {
    openUp(gpa, io, parent, name, 0);
    try parent.deleteTree(io, name);
}

/// Best effort: whatever cannot be opened up is left for deleteTree to report.
fn openUp(gpa: Allocator, io: Io, parent: Io.Dir, name: []const u8, depth: usize) void {
    if (depth > max_chmod_depth) return;
    const st = parent.statFile(io, name, .{ .follow_symlinks = false }) catch return;
    if (st.kind != .directory) return;
    parent.setFilePermissions(io, name, .fromMode(0o700), .{ .follow_symlinks = false }) catch {};
    var sub = parent.openDir(io, name, .{ .iterate = true, .follow_symlinks = false }) catch return;
    defer sub.close(io);
    var arena: std.heap.ArenaAllocator = .init(gpa);
    defer arena.deinit();
    const entries = list(arena.allocator(), io, sub) catch return;
    for (entries) |e| {
        if (e.kind == .directory) openUp(gpa, io, sub, e.name, depth + 1);
    }
}
