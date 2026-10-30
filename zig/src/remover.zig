//! Deleting the data of a Download (internal/daemon/remover.go): its working
//! directory and its output, but only strictly inside the directories the
//! daemon is configured to write in.
//!
//! This is the most dangerous thing the daemon does, so the rules are strict:
//!
//! - A target is deleted only if it lies strictly inside one of the roots: the
//!   protocol's incomplete and complete roots from the Settings, the folder
//!   overrides of every Category (of both protocols: output that landed before
//!   the overrides were split per protocol may sit in the other one), and the
//!   parent of the working directory the Download was created with (it stays
//!   removable after the incomplete root was changed). Outside every root, a
//!   root itself, a relative path, a path with ".." that leaves a root: left
//!   alone, and the call still succeeds (the setting it came from may have
//!   changed since).
//! - Symlinks are never followed. A symlink target is unlinked; deleting a
//!   directory unlinks the symlinks in it without entering them. A parent
//!   chain that climbs out of its root through a symlink is an error
//!   (`error.UnsafePath`), and nothing is deleted.
//! - The parent of the target is opened first and its real path checked
//!   against the real root through the handle, so a symlink swapped in after
//!   the check cannot redirect the delete; the target itself is then removed
//!   relative to that handle.
//! - A target that is gone already is fine; one that cannot be removed (no
//!   permission, read-only file system) is an error and the Download stays.
//!   Nothing is chmod-ed: like Go's os.RemoveAll, a directory the daemon may
//!   not write to cannot be emptied.
//!
//! `Remover` also holds a Download (`hold`): it asks the runner of the
//! Download's protocol to stop and keep stopped, so nothing writes into a
//! directory that is being deleted.
//!
//! Memory: one arena per call for roots, paths and the lists the seams
//! return. Owner: whoever calls `init` (the daemon). The only long-lived
//! state is the table of holds in flight (see `Remover.hold`).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const fsutil = @import("fsutil.zig");
const id_mod = @import("id.zig");
const log = @import("log.zig");
const settings_types = @import("settings_types.zig");
const categories_types = @import("categories_types.zig");
const downloads_service = @import("downloads_service.zig");

pub const Paths = settings_types.Paths;
pub const Category = categories_types.Category;

// ---- seams ----

/// The runner of one protocol, as the remover needs it (Go: fetchHolder): it
/// stops whatever works on the files of Download `id` and keeps it stopped
/// until `release`, or until the Download is gone. `hold_fn` fails when the
/// fetch cannot be stopped; the removal then does not go ahead. The Usenet
/// runner (Z3) and the torrent engine (Z5) implement it.
pub const FetchHolder = struct {
    ctx: ?*anyopaque = null,
    hold_fn: *const fn (ctx: ?*anyopaque, id: []const u8) anyerror!void,
    release_fn: *const fn (ctx: ?*anyopaque, id: []const u8) void,
};

/// The paths of the Settings in effect now (Go: the `settings` func). The
/// strings are allocated in `a`, so the Settings snapshot is not held across
/// the removal. `fromSettings` adapts the Settings service.
pub const PathsSource = struct {
    ctx: ?*anyopaque = null,
    paths_fn: *const fn (ctx: ?*anyopaque, a: Allocator) anyerror!Paths,
};

/// Every Category (Go: CategoryRepo.List). The list and its strings are
/// allocated in `a`. `fromRepo` adapts the Categories repository.
pub const CategorySource = struct {
    ctx: ?*anyopaque = null,
    list_fn: *const fn (ctx: ?*anyopaque, a: Allocator) anyerror![]const Category,
};

/// Where a Download keeps its working directory (Go: dirFor / torrentDir),
/// for a protocol whose directory is not simply <incomplete root>/<id>: a
/// Usenet Download records the directory it was created with, a torrent
/// reports its storage directory. The result is allocated in `a`; empty means
/// "no working directory". Without one the remover uses
/// <incomplete root of the protocol>/<id>.
pub const WorkDirs = struct {
    ctx: ?*anyopaque = null,
    dir_fn: *const fn (ctx: ?*anyopaque, a: Allocator, d: dl.Download) anyerror![]const u8,
};

pub const Deps = struct {
    paths: PathsSource,
    categories: CategorySource,
    /// Holds Usenet Downloads; null means there is nothing to hold.
    usenet_holder: ?FetchHolder = null,
    /// Holds torrents; null (the torrent engine is off) means nothing to hold.
    torrent_holder: ?FetchHolder = null,
    usenet_dirs: ?WorkDirs = null,
    torrent_dirs: ?WorkDirs = null,
    /// Warnings about targets that are left alone; null logs nothing.
    log: ?log.Logger = null,
};

pub const Error = error{
    /// The path of a target climbs out of its root through a symlink, or
    /// holds a NUL byte (Go: fsutil.ErrUnsafePath).
    UnsafePath,
    /// `hold` for a Download that is held already.
    AlreadyHeld,
};

// ---- the remover ----

pub const Remover = struct {
    gpa: Allocator,
    io: Io,
    deps: Deps,
    /// Guards `holds`.
    mu: Io.Mutex = .init,
    /// The holds taken by `hold` whose outcome is not known yet, by Download
    /// ID. A node is freed by the `Release` that undoes its hold, or by the
    /// successful `removeData` of its Download, whichever comes first.
    holds: std.StringHashMapUnmanaged(*Held) = .empty,

    const Held = struct {
        owner: *Remover,
        holder: FetchHolder,
        /// Owned (gpa); also the key in `holds`.
        id: []u8,
    };

    pub fn init(gpa: Allocator, io: Io, deps: Deps) Remover {
        return .{ .gpa = gpa, .io = io, .deps = deps };
    }

    /// Frees the table. Holds that are still in it (a caller that neither
    /// released nor removed) are forgotten without being released.
    pub fn deinit(self: *Remover) void {
        var it = self.holds.valueIterator();
        while (it.next()) |h| self.freeHeld(h.*);
        self.holds.deinit(self.gpa);
        self.* = undefined;
    }

    /// The seam `DownloadService` calls. The Remover must not move after this.
    pub fn asDataRemover(self: *Remover) downloads_service.DataRemover {
        return .{ .ctx = self, .hold_fn = holdFn, .remove_fn = removeFn };
    }

    fn holdFn(ctx: ?*anyopaque, d: dl.Download) anyerror!downloads_service.Release {
        const self: *Remover = @ptrCast(@alignCast(ctx.?));
        return self.hold(d);
    }

    fn removeFn(ctx: ?*anyopaque, d: dl.Download) anyerror!void {
        const self: *Remover = @ptrCast(@alignCast(ctx.?));
        return self.removeData(d);
    }

    fn holderFor(self: *Remover, kind: dl.SourceKind) ?FetchHolder {
        return switch (kind) {
            .nzb => self.deps.usenet_holder,
            .torrent => self.deps.torrent_holder,
        };
    }

    /// Stops the running fetch of `d` (or, for a torrent, the engine's hold on
    /// the files) and keeps it stopped, so nothing writes into a directory
    /// that is being deleted. The returned `Release` undoes it when the
    /// removal does not go ahead; call it at most once, and never after a
    /// successful `removeData` of the same Download. With no holder for the
    /// Download's protocol the Release does nothing.
    pub fn hold(self: *Remover, d: dl.Download) anyerror!downloads_service.Release {
        const holder = self.holderFor(d.source_kind) orelse return .{};
        const id_copy = try self.gpa.dupe(u8, d.id);
        const node = self.gpa.create(Held) catch |err| {
            self.gpa.free(id_copy);
            return err;
        };
        node.* = .{ .owner = self, .holder = holder, .id = id_copy };

        // Claim the ID first, so the table never has two holds of one Download.
        self.claim(node) catch |err| {
            self.freeHeld(node);
            return err;
        };
        holder.hold_fn(holder.ctx, d.id) catch |err| {
            self.dropHold(d.id);
            return err;
        };
        return .{ .ctx = node, .release_fn = releaseFn };
    }

    fn claim(self: *Remover, node: *Held) !void {
        try self.mu.lock(self.io);
        defer self.mu.unlock(self.io);
        const slot = try self.holds.getOrPut(self.gpa, node.id);
        if (slot.found_existing) return error.AlreadyHeld;
        slot.value_ptr.* = node;
    }

    fn releaseFn(ctx: ?*anyopaque) void {
        const node: *Held = @ptrCast(@alignCast(ctx.?));
        const owner = node.owner;
        node.holder.release_fn(node.holder.ctx, node.id);
        owner.dropHold(node.id);
    }

    /// Forgets the hold of `id` (not the holder's hold itself) and frees it.
    fn dropHold(self: *Remover, id: []const u8) void {
        self.mu.lockUncancelable(self.io);
        const kv = self.holds.fetchRemove(id);
        self.mu.unlock(self.io);
        if (kv) |e| self.freeHeld(e.value);
    }

    fn freeHeld(self: *Remover, h: *Held) void {
        self.gpa.free(h.id);
        self.gpa.destroy(h);
    }

    /// Deletes the working directory and the output of `d`. The caller holds
    /// `d` first (`hold`).
    ///
    /// A torrent's working directory is its Download directory, which holds
    /// the originals and _unpack/ together, so both go. The roots a torrent
    /// may be deleted from are the torrent roots, never the Usenet ones.
    ///
    /// Both targets are tried even if the first fails; the first error is
    /// returned (and the hold stays, to be released by the caller). On
    /// success the hold's bookkeeping ends here: the runner drops its hold
    /// when the Download is announced as removed.
    pub fn removeData(self: *Remover, d: dl.Download) anyerror!void {
        var arena: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena.deinit();
        const a = arena.allocator();

        const paths = try self.deps.paths.paths_fn(self.deps.paths.ctx, a);
        const cats = try self.deps.categories.list_fn(self.deps.categories.ctx, a);
        var roots = try collectRoots(a, paths, cats, d.source_kind);

        const own = switch (d.source_kind) {
            .nzb => self.deps.usenet_dirs,
            .torrent => self.deps.torrent_dirs,
        };
        const work = if (own) |w|
            try w.dir_fn(w.ctx, a, d)
        else switch (d.source_kind) {
            .nzb => try fsutil.join(a, &.{ paths.usenet.incomplete, d.id }),
            .torrent => try fsutil.join(a, &.{ paths.torrent.incomplete, d.id }),
        };

        // The working directory is trusted when it is <parent>/<id>: it was
        // made for this Download at add time, even if the incomplete root has
        // been changed since. Only a plain ID counts (it is the name of one
        // directory), and a parent of "/" never does.
        if (work.len > 0 and id_mod.isValid(d.id) and std.mem.eql(u8, fsutil.base(work), d.id)) {
            const parent = try fsutil.dir(a, work);
            if (!std.mem.eql(u8, parent, "/")) try roots.append(a, parent);
        }

        var first: ?anyerror = null;
        if (self.removeInside(a, roots.items, work, d.id)) |_| {} else |err| first = err;
        if (self.removeInside(a, roots.items, d.output_path, d.id)) |_| {} else |err| {
            if (first == null) first = err;
        }

        // On failure the caller releases the hold. On success the runner owns
        // it from here (it drops it when the Download is announced as
        // removed), so the bookkeeping ends: `Release` must not run now.
        if (first) |err| return err;
        self.dropHold(d.id);
    }

    /// Deletes `target` if it lies strictly inside one of `roots`. A target
    /// outside every root, or a root itself, is left alone with a warning. A
    /// path that climbs out of its root through a symlink is an error.
    fn removeInside(self: *Remover, a: Allocator, roots: []const []const u8, target_raw: []const u8, id: []const u8) anyerror!void {
        if (target_raw.len == 0) return;
        if (std.mem.indexOfScalar(u8, target_raw, 0) != null) return error.UnsafePath;
        const target = try fsutil.clean(a, target_raw);
        if (!fsutil.isAbs(target)) {
            self.warn("not deleting a relative path", id);
            return;
        }
        for (roots) |root_raw| {
            if (root_raw.len == 0 or !fsutil.isAbs(root_raw)) continue;
            if (std.mem.indexOfScalar(u8, root_raw, 0) != null) continue;
            const root = try fsutil.clean(a, root_raw);
            const rel = (try fsutil.relBelow(a, root, target)) orelse continue;
            if (std.mem.eql(u8, rel, ".")) continue;
            // The first root that holds the target decides, as in Go.
            // Check the parent chain only: the last element may itself be a
            // symlink, which is unlinked, not followed.
            const parent = try fsutil.safeJoin(a, self.io, root, try fsutil.dir(a, rel));
            return self.removeUnder(root, parent, fsutil.base(rel));
        }
        self.warn("not deleting data outside the configured directories", id);
    }

    /// Removes `name` inside `parent`, which `safeJoin` has checked against
    /// `root`. The parent is opened (following symlinks that stay inside the
    /// root) and its real path checked through the handle, so the removal
    /// below acts on exactly the directory that was checked.
    fn removeUnder(self: *Remover, root: []const u8, parent: []const u8, name: []const u8) anyerror!void {
        if (name.len == 0 or std.mem.eql(u8, name, ".") or std.mem.eql(u8, name, "..") or std.mem.indexOfScalar(u8, name, '/') != null) {
            return error.UnsafePath;
        }
        const cwd = Io.Dir.cwd();
        var pd = cwd.openDir(self.io, parent, .{}) catch |err| switch (err) {
            // Nothing there, so nothing to delete.
            error.FileNotFound => return,
            else => |e| return e,
        };
        defer pd.close(self.io);

        var root_buf: [Io.Dir.max_path_bytes]u8 = undefined;
        var real_buf: [Io.Dir.max_path_bytes]u8 = undefined;
        const root_n = cwd.realPathFile(self.io, root, &root_buf) catch |err| switch (err) {
            error.FileNotFound => return,
            else => |e| return e,
        };
        const real_n = try pd.realPath(self.io, &real_buf);
        var scratch_buf: [4 * Io.Dir.max_path_bytes]u8 = undefined;
        var fba: std.heap.FixedBufferAllocator = .init(&scratch_buf);
        if (!try fsutil.within(fba.allocator(), root_buf[0..root_n], real_buf[0..real_n])) return error.UnsafePath;

        const st = pd.statFile(self.io, name, .{ .follow_symlinks = false }) catch |err| switch (err) {
            error.FileNotFound => return,
            else => |e| return e,
        };
        switch (st.kind) {
            // deleteTree opens directories without following symlinks, and
            // unlinks a symlink it meets instead of entering it.
            .directory => try pd.deleteTree(self.io, name),
            // A symlink (never followed), a file, a socket: unlink.
            else => pd.deleteFile(self.io, name) catch |err| switch (err) {
                error.FileNotFound => {},
                else => |e| return e,
            },
        }
    }

    fn warn(self: *Remover, msg: []const u8, id: []const u8) void {
        if (self.deps.log) |l| l.warn(msg, &.{log.Attr.string("id", id)});
    }
};

/// The directories data of `kind` may be deleted inside: that protocol's
/// incomplete and complete roots, and the folder overrides of every Category
/// (Ready output of such a Category lands there). Both protocols' overrides
/// count, because output that landed before the overrides were split per
/// protocol may sit in the other one.
fn collectRoots(a: Allocator, paths: Paths, cats: []const Category, kind: dl.SourceKind) Allocator.Error!std.ArrayList([]const u8) {
    var roots: std.ArrayList([]const u8) = .empty;
    const own = switch (kind) {
        .nzb => paths.usenet,
        .torrent => paths.torrent,
    };
    try roots.append(a, own.incomplete);
    try roots.append(a, own.complete);
    for (cats) |c| {
        if (c.usenet_dir.len > 0) try roots.append(a, c.usenet_dir);
        if (c.torrent_dir.len > 0) try roots.append(a, c.torrent_dir);
    }
    return roots;
}

// ---- adapters to the services ----

const settings_service = @import("settings_service.zig");
const categories_repo = @import("categories_repo.zig");

/// The Settings service as a `PathsSource`. The service must outlive the Remover.
pub fn fromSettings(svc: *settings_service.Service) PathsSource {
    const Adapter = struct {
        fn paths(ctx: ?*anyopaque, a: Allocator) anyerror!Paths {
            const s: *settings_service.Service = @ptrCast(@alignCast(ctx.?));
            const snap = s.current();
            defer snap.release();
            const p = snap.settings.paths;
            return .{
                .usenet = .{ .incomplete = try a.dupe(u8, p.usenet.incomplete), .complete = try a.dupe(u8, p.usenet.complete) },
                .torrent = .{ .incomplete = try a.dupe(u8, p.torrent.incomplete), .complete = try a.dupe(u8, p.torrent.complete) },
            };
        }
    };
    return .{ .ctx = svc, .paths_fn = Adapter.paths };
}

/// The Categories repository as a `CategorySource`. The store behind the
/// repository must outlive the Remover.
pub fn fromRepo(repo: *const categories_repo.CategoryRepo) CategorySource {
    const Adapter = struct {
        fn list(ctx: ?*anyopaque, a: Allocator) anyerror![]const Category {
            const r: *const categories_repo.CategoryRepo = @ptrCast(@alignCast(ctx.?));
            return r.list(a);
        }
    };
    return .{ .ctx = @constCast(repo), .list_fn = Adapter.list };
}
