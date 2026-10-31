//! Moving a finished directory (internal/fsutil movedir.go): a rename when the
//! destination is on the same file system, which keeps the inodes (and so the
//! hardlinks a media manager made), and a copy-and-remove when it is not.
//!
//! `dst` must not exist (`error.Exists`) and must not lie inside `src`
//! (`error.Nested`); `src` must be a real directory, not a symlink to one
//! (`error.Symlink`). The existence check is not atomic with the move, but
//! the rename never replaces anything.
//!
//! The copy path (rename failed with CrossDevice):
//!   - symlinks anywhere in `src` are never followed and are refused
//!     (`error.Symlink`); FIFOs, sockets and devices are refused
//!     (`error.SpecialFile`); post-processing has removed them already;
//!   - regular files are copied with their permission bits (setuid, setgid
//!     and sticky are dropped) and modification time; hardlinked files inside
//!     `src` become independent files;
//!   - every file and directory is fsynced before `src` is removed; a
//!     directory fsync that is not supported (some network file systems) is
//!     ignored;
//!   - if the copy fails, the partial `dst` is removed and `src` is left
//!     alone; missing parents of `dst` that were made are kept;
//!   - if the copy succeeds but `src` cannot be removed, `dst` is kept (it is
//!     complete), `src` may be partly deleted, and the result has
//!     `cross_device` set together with the error.
//!
//! Every directory is opened without following a symlink and files are
//! opened relative to their directory handle, and the file that was opened is
//! checked to be the one that was listed, so nothing swapped in during the
//! copy is followed.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const fsutil = @import("fsutil.zig");
const fs = @import("tidy_fs.zig");

/// How a move was done.
pub const MoveResult = struct {
    /// The tree was copied because the destination is on another file
    /// system: hardlinks into or out of it no longer share data.
    cross_device: bool = false,
};

/// The deepest tree the copy path walks (Go has no cap; this keeps a hostile
/// tree from exhausting the stack).
pub const max_depth = 256;

/// Renames `src` to `dst` without replacing; a seam so tests can make it
/// fail with CrossDevice.
pub const Renamer = *const fn (io: Io, src: []const u8, dst: []const u8) anyerror!void;

fn realRename(io: Io, src: []const u8, dst: []const u8) anyerror!void {
    return fs.renameNoClobber(io, Io.Dir.cwd(), src, dst);
}

/// Moves the directory `src` to the new path `dst`, making its missing
/// parents. `res` says how it was done, also when an error comes back. The
/// errors are named in the file comment, plus the ones of the file system.
pub fn moveDir(gpa: Allocator, io: Io, src: []const u8, dst: []const u8, res: *MoveResult, renamer: ?Renamer) anyerror!void {
    res.* = .{};
    const cwd = Io.Dir.cwd();
    const st = try cwd.statFile(io, src, .{ .follow_symlinks = false });
    switch (st.kind) {
        .directory => {},
        .sym_link => return error.Symlink,
        else => return error.NotDirectory,
    }
    if (cwd.statFile(io, dst, .{ .follow_symlinks = false })) |_| {
        return error.Exists;
    } else |err| switch (err) {
        error.FileNotFound, error.NotDir => {},
        else => |e| return e,
    }

    var arena_state: std.heap.ArenaAllocator = .init(gpa);
    defer arena_state.deinit();
    const a = arena_state.allocator();

    if (try isNested(a, io, src, dst)) return error.Nested;
    try cwd.createDirPath(io, try fsutil.dir(a, dst));

    (renamer orelse realRename)(io, src, dst) catch |err| switch (err) {
        error.CrossDevice => {
            res.cross_device = true;
            try copyTree(gpa, io, src, dst, st);
            try fs.removeAllWritable(gpa, io, cwd, src);
            return;
        },
        else => |e| return e,
    };
}

/// Whether `dst` resolves to a place inside `src` (or is it).
fn isNested(a: Allocator, io: Io, src: []const u8, dst: []const u8) anyerror!bool {
    const real_src = try resolveLoosely(a, io, src);
    const real_dst = try resolveLoosely(a, io, dst);
    return fsutil.within(a, real_src, real_dst);
}

/// The absolute path of `path` with the symlinks of its existing part
/// resolved and the part that does not exist yet kept as it is.
fn resolveLoosely(a: Allocator, io: Io, path: []const u8) anyerror![]const u8 {
    const cwd = Io.Dir.cwd();
    const cleaned = try fsutil.clean(a, path);
    var existing: []const u8 = cleaned;
    while (true) {
        if (cwd.statFile(io, existing, .{ .follow_symlinks = false })) |_| {
            break;
        } else |err| switch (err) {
            error.FileNotFound, error.NotDir => {},
            else => |e| return e,
        }
        const i = std.mem.lastIndexOfScalar(u8, existing, '/') orelse {
            existing = ".";
            break;
        };
        existing = if (i == 0) "/" else existing[0..i];
    }
    var buf: [Io.Dir.max_path_bytes]u8 = undefined;
    const n = try cwd.realPathFile(io, existing, &buf);
    const real = buf[0..n];
    const tail = if (std.mem.eql(u8, existing, ".")) cleaned else std.mem.trimStart(u8, cleaned[existing.len..], "/");
    return fsutil.join(a, &.{ real, tail });
}

// ---- the copy path ----

/// Copies the directory `src` (described by `st`) to the new path `dst`. On
/// failure it removes whatever it created.
fn copyTree(gpa: Allocator, io: Io, src: []const u8, dst: []const u8, st: Io.File.Stat) anyerror!void {
    const cwd = Io.Dir.cwd();
    cwd.createDir(io, dst, .fromMode(0o700)) catch |err| switch (err) {
        error.PathAlreadyExists => return error.Exists,
        else => |e| return e,
    };
    errdefer fs.removeAllWritable(gpa, io, cwd, dst) catch {};

    var s = try cwd.openDir(io, src, .{ .iterate = true, .follow_symlinks = false });
    defer s.close(io);
    var d = try cwd.openDir(io, dst, .{ .iterate = true, .follow_symlinks = false });
    defer d.close(io);
    try copyContents(gpa, io, s, d, 0);
    try finishDir(io, d, cwd, dst, st);
    try syncDir(io, try cwd.openDir(io, std.fs.path.dirname(dst) orelse ".", .{}));
}

fn copyContents(gpa: Allocator, io: Io, src: Io.Dir, dst: Io.Dir, depth: usize) anyerror!void {
    if (depth > max_depth) return error.TooDeep;
    var arena_state: std.heap.ArenaAllocator = .init(gpa);
    defer arena_state.deinit();
    const entries = try fs.list(arena_state.allocator(), io, src);
    for (entries) |e| {
        try io.checkCancel();
        const st = try src.statFile(io, e.name, .{ .follow_symlinks = false });
        switch (st.kind) {
            .sym_link => return error.Symlink,
            .directory => {
                try dst.createDir(io, e.name, .fromMode(0o700));
                var s = try src.openDir(io, e.name, .{ .iterate = true, .follow_symlinks = false });
                defer s.close(io);
                var d = try dst.openDir(io, e.name, .{ .iterate = true, .follow_symlinks = false });
                defer d.close(io);
                try copyContents(gpa, io, s, d, depth + 1);
                try finishDir(io, d, dst, e.name, st);
            },
            .file => try copyFile(gpa, io, src, dst, e.name, st),
            else => return error.SpecialFile,
        }
    }
}

/// Fsyncs a fully copied directory and then applies the source's permissions
/// and modification time. Permissions come last because a read-only source
/// directory could not be filled otherwise.
fn finishDir(io: Io, d: Io.Dir, parent: Io.Dir, name: []const u8, st: Io.File.Stat) anyerror!void {
    try syncDirHandle(io, d);
    try d.setPermissions(io, .fromMode(st.permissions.toMode() & 0o777));
    try parent.setTimestamps(io, name, .{
        .access_timestamp = .{ .new = st.mtime },
        .modify_timestamp = .{ .new = st.mtime },
    });
}

/// Fsyncs a directory so its entries are durable, and closes it.
fn syncDir(io: Io, d: Io.Dir) anyerror!void {
    defer d.close(io);
    try syncDirHandle(io, d);
}

fn syncDirHandle(io: Io, d: Io.Dir) anyerror!void {
    const f = try d.openFile(io, ".", .{ .mode = .read_only });
    defer f.close(io);
    f.sync(io) catch |err| switch (err) {
        // Not supported here (some network file systems).
        error.Unexpected => {},
        else => |e| return e,
    };
}

/// Copies the regular file `name` (described by `st` from a no-follow stat)
/// to a new file of the same name in `dst`, and fsyncs it.
fn copyFile(gpa: Allocator, io: Io, src: Io.Dir, dst: Io.Dir, name: []const u8, st: Io.File.Stat) anyerror!void {
    const in = try src.openFile(io, name, .{ .follow_symlinks = false, .allow_directory = false });
    defer in.close(io);
    // The entry may have been swapped for something else since the stat.
    const opened = try in.stat(io);
    if (opened.kind != .file or opened.inode != st.inode) return error.Symlink;

    const out = try dst.createFile(io, name, .{ .exclusive = true, .permissions = .fromMode(0o600) });
    defer out.close(io);

    const chunk = 64 * 1024;
    const buf = try gpa.alloc(u8, 2 * chunk);
    defer gpa.free(buf);
    var fr = in.reader(io, buf[0..chunk]);
    var fw = out.writer(io, buf[chunk..]);
    _ = fr.interface.streamRemaining(&fw.interface) catch |err| switch (err) {
        error.ReadFailed => return fr.err.?,
        error.WriteFailed => return fw.err.?,
    };
    fw.interface.flush() catch return fw.err.?;
    try out.sync(io);
    try out.setPermissions(io, .fromMode(st.permissions.toMode() & 0o777));
    try out.setTimestamps(io, .{
        .access_timestamp = .{ .new = st.mtime },
        .modify_timestamp = .{ .new = st.mtime },
    });
}
