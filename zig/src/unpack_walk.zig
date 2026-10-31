//! The check of the staging tree after 7zz wrote it (internal/postproc/unpack
//! walk.go). It trusts nothing 7zz was told: it looks at what is on disk.
//!
//!   - A symlink, hardlink, device, FIFO or socket anywhere fails the Unpack
//!     with PathViolation (the listing refused them already; this is the
//!     second line of defence, for a listing that lied or a 7zz that did more
//!     than it was asked).
//!   - Every name goes through `fsutil.safeName`; a name it changes ("a:b",
//!     "con.txt", trailing dots) is renamed in place, and two names that end
//!     up equal once case is ignored fail with PathViolation.
//!   - Files get mode 0644 and directories 0755, so an archive cannot plant a
//!     setuid or executable file, or a directory the daemon cannot delete.
//!
//! Every directory is opened relative to its parent without following
//! symlinks, and the modes are changed before the directory is opened, so a
//! directory without permissions does not stop the walk.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Dir = Io.Dir;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const listing = @import("unpack_listing.zig");
const name_mod = @import("unpack_name.zig");
const ufs = @import("unpack_fs.zig");
const fsutil = @import("fsutil.zig");

pub const Walker = struct {
    env: *Env,
    /// Scratch for names; the file list below lives in `env.arena`.
    scratch: Allocator,
    max_entries: usize,
    /// Relative paths of the files, in walk order (sort them).
    files: std.ArrayList([]const u8) = .empty,
    bytes: i64 = 0,
    count: usize = 0,

    pub fn init(env: *Env, scratch: Allocator) Walker {
        return .{ .env = env, .scratch = scratch, .max_entries = env.cfg.max_entries };
    }

    pub fn run(w: *Walker, root: []const u8) Error!void {
        try w.dir(Dir.cwd(), root, "", 0);
    }

    fn violation(w: *Walker, comptime fmt: []const u8, args: anytype) Error {
        return errors.fail(w.env.arena, w.env.diag, error.PathViolation, fmt, args);
    }

    fn q(w: *Walker, s: []const u8) Error!@TypeOf(name_mod.quoted("")) {
        return name_mod.quoted(try name_mod.truncateForError(w.env.arena, s));
    }

    fn dir(w: *Walker, parent: Dir, name: []const u8, rel: []const u8, depth: usize) Error!void {
        const env = w.env;
        const io = env.io;
        try io.checkCancel();
        if (depth > name_mod.max_path_depth) {
            return w.violation("directories nested deeper than {d}", .{name_mod.max_path_depth});
        }
        // By path, before opening: a directory without permissions cannot be
        // opened, but its owner can always change its mode.
        parent.setFilePermissions(io, name, .fromMode(0o755), .{}) catch |err| {
            return errors.io(env.arena, env.diag, err, "normalize directory mode");
        };
        var d = parent.openDir(io, name, .{ .iterate = true, .follow_symlinks = false }) catch |err| {
            return errors.io(env.arena, env.diag, err, "open extracted directory");
        };
        defer d.close(io);

        const ents = ufs.names(env, w.scratch, d) catch |err| return errors.io(env.arena, env.diag, err, "read extracted directory");
        // Decide every final name first, so a rename can never land on a name
        // that another entry of the directory still has.
        const final = try w.scratch.alloc([]const u8, ents.len);
        var seen: std.StringHashMapUnmanaged(void) = .empty;
        for (ents, final) |e, *f| {
            const safe = fsutil.safeName(w.scratch, e) catch |err| switch (err) {
                error.OutOfMemory => return error.OutOfMemory,
                error.InvalidName => return w.violation("unusable name {f} in the extracted tree", .{try w.q(e)}),
            };
            const key = try name_mod.lowerAlloc(w.scratch, safe);
            const gop = try seen.getOrPut(w.scratch, key);
            if (gop.found_existing) {
                const p = try childPath(w.scratch, rel, safe);
                return w.violation("names collide in the extracted tree: {f}", .{try w.q(p)});
            }
            f.* = safe;
        }
        for (ents, final) |e, f| {
            w.count += 1;
            if (w.count > w.max_entries *| 2 +| 1) {
                return errors.fail(env.arena, env.diag, error.TooManyEntries, "more than {d} entries extracted", .{w.max_entries});
            }
            const st = (ufs.lstat(io, d, e) catch |err| return errors.io(env.arena, env.diag, err, "stat extracted entry")) orelse {
                return errors.fail(env.arena, env.diag, error.Internal, "unpack: extracted entry vanished", .{});
            };
            const kind = kindOf(st.kind);
            if (kind != .file and kind != .dir) {
                const p = try childPath(w.scratch, rel, e);
                return w.violation("the extracted tree has a {s} {f}", .{ kind.text(), try w.q(p) });
            }
            var to = e;
            if (!std.mem.eql(u8, f, e)) {
                if (try exists(w, d, f)) {
                    const p = try childPath(w.scratch, rel, f);
                    return w.violation("names collide in the extracted tree: {f}", .{try w.q(p)});
                }
                d.rename(e, d, f, io) catch |err| return errors.io(env.arena, env.diag, err, "rename extracted entry");
                to = f;
            }
            const child_rel = try childPath(env.arena, rel, f);
            if (kind == .dir) {
                try w.dir(d, to, child_rel, depth + 1);
                continue;
            }
            if (st.nlink > 1) {
                return w.violation("the extracted file {f} is a hardlink", .{try w.q(child_rel)});
            }
            d.setFilePermissions(io, to, .fromMode(0o644), .{}) catch |err| {
                return errors.io(env.arena, env.diag, err, "normalize file mode");
            };
            try w.files.append(env.arena, child_rel);
            w.bytes = env_mod.addSat(w.bytes, @intCast(@min(st.size, @as(u64, 1) << 62)));
        }
    }
};

fn exists(w: *Walker, d: Dir, name: []const u8) Error!bool {
    const st = ufs.lstat(w.env.io, d, name) catch return true;
    return st != null;
}

fn childPath(a: Allocator, rel: []const u8, name: []const u8) Allocator.Error![]const u8 {
    if (rel.len == 0) return a.dupe(u8, name);
    return std.mem.concat(a, u8, &.{ rel, "/", name });
}

/// Classifies a file kind from lstat.
pub fn kindOf(k: Io.File.Kind) listing.Kind {
    return switch (k) {
        .file => .file,
        .directory => .dir,
        .sym_link => .symlink,
        .block_device, .character_device => .device,
        .named_pipe => .fifo,
        else => .other,
    };
}
