//! The last step of Post-processing (internal/postproc/finalize): cleanup of
//! known-garbage files, then the move of the finished Download into the
//! complete tree. The Finisher resolves the destination at move time, so
//! later Category and settings changes apply.
//!
//! Usenet. With unpacked archives (`unpacked_dir` is not empty) the Download's
//! dir is cleaned of archive volumes (.rar, .rNN, .sNN, .7z, .7z.NNN), .par2
//! files, the .nzb and any _unpack.tmp leftover. Sample files and .nfo files
//! stay. The entries of `unpacked_dir` are then moved to the root of the dir,
//! and the dir is moved to the destination. The final dir therefore holds what
//! Unpack produced at its root plus the remaining non-archive files. If an
//! unpacked entry has the name of a file that stayed, the unpacked entry gets
//! a Unique name; nothing is overwritten. Without archives only the .par2
//! files and the .nzb are deleted.
//!
//! Cleanup deletes only regular files directly inside the dir (plus the
//! _unpack.tmp dir). It never follows symlinks and never leaves the dir: it
//! works through a directory handle opened without following links. All
//! deletion and merging happens before the move, so nothing inside the final
//! dir changes afterwards; this matters for hardlinks made by media managers.
//! The same step renames underscores to dots (the UnderscoresToDots option)
//! and names an obfuscated video after the Download (tidy.zig).
//!
//! Torrent. Originals are never deleted or modified, because they are being
//! seeded. The dir (with its _unpack/ dir, if any) is moved as a whole to the
//! destination, unless it already is the destination, in which case nothing
//! happens. The returned path is the final dir; _unpack/ is inside it.
//!
//! Restart safety. Before the move a .bitnews-done marker holding the
//! Download ID is written inside the dir, so it travels with it. If `finish`
//! is retried after the dir was already moved, the missing dir is detected and
//! the destination (or its Unique'd variants name.1, name.2, ...) is searched
//! for a marker with this Download's ID; if found, that path is returned
//! without error.
//!
//! The move is a rename on one file system (it keeps inodes, and so
//! hardlinks) and a verified copy across file systems (finalize_move.zig),
//! which raises the `cross_device` warning.
//!
//! Memory: everything lives in the arena the caller passes (the Download's
//! post-processing step); the Finisher itself holds only its seams. The seams
//! are called on the calling task.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const fsutil = @import("fsutil.zig");
const log = @import("log.zig");
const ports = @import("postprocess_ports.zig");
const time_fmt = @import("time_fmt.zig");
const categories_types = @import("categories_types.zig");
const tidy = @import("tidy.zig");
const cleanup = @import("finalize_cleanup.zig");
const move = @import("finalize_move.zig");
const fs = @import("tidy_fs.zig");
const ctx_mod = @import("finalize_ctx.zig");
const Ctx = ctx_mod.Ctx;
const q = ctx_mod.q;
const PortError = ports.PortError;
const Diag = ports.Diag;

pub const Category = categories_types.Category;
pub const MoveResult = move.MoveResult;

/// The file written inside the final dir to make `finish` restart-safe. It
/// holds the Download ID on its first line.
pub const marker_name = ".bitnews-done";

/// A non-fatal condition the caller may turn into a Warning event.
pub const Warning = struct {
    kind: Kind,
    download_id: []const u8,
    message: []const u8,
    /// The path the warning is about, if any.
    path: []const u8 = "",

    /// The stable machine-readable codes, as in Go.
    pub const Kind = enum { cross_device, category_missing, marker_conflict };
};

// ---- seams ----

/// Receives warnings. The strings are valid for the call only.
pub const Warner = struct {
    ctx: ?*anyopaque = null,
    warn_fn: *const fn (ctx: ?*anyopaque, w: Warning) void,
};

/// What the Finisher reads from the Settings in effect now (Go:
/// Deps.Settings). Strings are allocated in `arena`. `settingsFromService`
/// adapts the Settings service.
pub const SettingsSource = struct {
    ctx: ?*anyopaque = null,
    read_fn: *const fn (ctx: ?*anyopaque, arena: Allocator) anyerror!View,

    pub const View = struct {
        /// The complete roots of the two protocols: absolute paths.
        usenet_complete: []const u8,
        torrent_complete: []const u8,
        /// PostProcess.UnderscoresToDots.
        underscores_to_dots: bool = false,
    };
};

/// Looks a Category up by ID (Go: Deps.Categories); null when there is no
/// such Category any more. Strings are allocated in `arena`.
/// `categoriesFromService` adapts the Categories service.
pub const CategorySource = struct {
    ctx: ?*anyopaque = null,
    find_fn: *const fn (ctx: ?*anyopaque, arena: Allocator, id: []const u8) anyerror!?Category,
};

/// Moves the directory `src` to the not yet existing `dst` (Go: Mover). The
/// default is `move.moveDir`.
pub const Mover = struct {
    ctx: ?*anyopaque = null,
    move_fn: *const fn (ctx: ?*anyopaque, gpa: Allocator, io: Io, src: []const u8, dst: []const u8, res: *MoveResult) anyerror!void,
};

/// The current time in unix milliseconds, to stamp the marker.
pub const Clock = *const fn (io: Io) i64;

pub const Deps = struct {
    settings: SettingsSource,
    categories: CategorySource,
    /// Without one, warnings are logged.
    warn: ?Warner = null,
    /// Renames and the like are logged here; null logs nothing.
    log: ?log.Logger = null,
    mover: ?Mover = null,
    clock: ?Clock = null,
};

fn realClock(io: Io) i64 {
    return Io.Clock.now(.real, io).toMilliseconds();
}

// ---- the finisher ----

pub const Finisher = struct {
    /// For the buffers of a cross-device copy.
    gpa: Allocator,
    deps: Deps,

    pub fn init(gpa: Allocator, deps: Deps) Finisher {
        return .{ .gpa = gpa, .deps = deps };
    }

    /// The orchestrator's port. The Finisher must not move afterwards.
    pub fn port(self: *Finisher) ports.Finisher {
        return .{ .ctx = self, .finish_fn = finishFn };
    }

    fn finishFn(ctx: *anyopaque, arena: Allocator, io: Io, d: dl.Download, dir: []const u8, unpacked_dir: []const u8, diag: *Diag) PortError![]const u8 {
        const self: *Finisher = @ptrCast(@alignCast(ctx));
        return self.finish(arena, io, d, dir, unpacked_dir, diag);
    }

    /// Cleans up `dir` and moves it to the destination of `d`, returning the
    /// final dir. `unpacked_dir` is the dir Unpack produced, or "" if there
    /// were no archives; it is ignored for torrents. Every failure is
    /// `error.Failed` with Go's text in `diag`, except cancellation. A
    /// cancelled call changes nothing that a retry cannot redo.
    pub fn finish(self: *Finisher, arena: Allocator, io: Io, d: dl.Download, dir_in: []const u8, unpacked_dir: []const u8, diag: *Diag) PortError![]const u8 {
        const c: Ctx = .{ .arena = arena, .io = io, .diag = diag };
        io.checkCancel() catch return error.Canceled;
        if (dir_in.len == 0) return c.fail("finalize: empty download dir", .{});
        const dir = fsutil.clean(arena, dir_in) catch return error.OutOfMemory;

        const view = try self.readSettings(c);
        const base = try self.destinationOf(c, d, view);
        const cwd = Io.Dir.cwd();

        const st = cwd.statFile(io, dir, .{ .follow_symlinks = false }) catch |err| switch (err) {
            error.FileNotFound => {
                // A retry after a crash: dir may already have been moved.
                if (try findMoved(c, base, d.id)) |found| return found;
                return c.fail("finalize: download dir {f} is gone and no finished copy of {s} was found at {f}", .{ q(dir), d.id, q(base) });
            },
            else => return c.ioFail(err, "stat download dir", null),
        };
        switch (st.kind) {
            .directory => {},
            .sym_link => return c.fail("fsutil: symlink refused: download dir {f}", .{q(dir)}),
            else => return c.fail("finalize: download dir {f} is not a directory", .{q(dir)}),
        }

        if (d.source_kind == .nzb) {
            {
                var dh = cwd.openDir(io, dir, .{ .iterate = true, .follow_symlinks = false }) catch |err| return c.ioFail(err, "open download dir", null);
                defer dh.close(io);
                try cleanup.cleanUsenet(c, dh, dir, unpacked_dir);
            }
            if (view.underscores_to_dots) self.dotUnderscores(c, d, dir);
            self.nameVideo(c, d, dir);
        }

        // Already where it belongs (a torrent the engine relocated, or equal
        // roots): nothing to move and nothing may be added.
        if (sameDir(io, dir, base)) return base;

        io.checkCancel() catch return error.Canceled;
        try self.writeMarker(c, d, dir);
        const dst = fsutil.unique(arena, io, cwd, base) catch |err| return c.ioFail(err, "unique destination", null);
        const parent = fsutil.dir(arena, dst) catch return error.OutOfMemory;
        cwd.createDirPath(io, parent) catch |err| {
            const folder = fsutil.deepestExistingParent(io, parent);
            return c.ioFail(err, "create parent", folder);
        };

        var res: MoveResult = .{};
        const moved = if (self.deps.mover) |m| m.move_fn(m.ctx, self.gpa, io, dir, dst, &res) else move.moveDir(self.gpa, io, dir, dst, &res, null);
        if (res.cross_device) {
            const msg = std.fmt.allocPrint(arena, "the finished download was copied instead of renamed from {f} to {f} because the folders are on different filesystems (slower); Sonarr/Radarr can only hardlink it into the library if the library shares the filesystem of the complete folder", .{ q(dir), q(dst) }) catch return error.OutOfMemory;
            self.warn(.{ .kind = .cross_device, .download_id = d.id, .path = dst, .message = msg });
        }
        moved catch |err| return moveFail(c, err, dir, dst, parent);
        return dst;
    }

    /// The directory `finish` would move `d` to, before a name clash is
    /// resolved with Unique: the Category's folder override for the Download's
    /// protocol, else <complete root>/<category>, else <complete root>, then
    /// the sanitized Download name. The torrent runner uses it to relocate a
    /// torrent to the same place without a rehash.
    pub fn destination(self: *Finisher, arena: Allocator, io: Io, d: dl.Download, diag: *Diag) PortError![]const u8 {
        const c: Ctx = .{ .arena = arena, .io = io, .diag = diag };
        const view = try self.readSettings(c);
        return self.destinationOf(c, d, view);
    }

    fn readSettings(self: *Finisher, c: Ctx) PortError!SettingsSource.View {
        const s = self.deps.settings;
        return s.read_fn(s.ctx, c.arena) catch |err| switch (err) {
            error.Canceled => error.Canceled,
            error.OutOfMemory => error.OutOfMemory,
            else => c.fail("finalize: settings: {s}", .{@errorName(err)}),
        };
    }

    /// The wanted final dir, before Unique. The result is always inside the
    /// chosen root.
    fn destinationOf(self: *Finisher, c: Ctx, d: dl.Download, view: SettingsSource.View) PortError![]const u8 {
        const root_in = switch (d.source_kind) {
            .nzb => view.usenet_complete,
            .torrent => view.torrent_complete,
        };
        if (!fsutil.isAbs(root_in)) return c.fail("finalize: complete root {f} is not an absolute path", .{q(root_in)});
        var root: []const u8 = fsutil.clean(c.arena, root_in) catch return error.OutOfMemory;

        if (d.category_id.len > 0) {
            const src = self.deps.categories;
            const found = src.find_fn(src.ctx, c.arena, d.category_id) catch |err| switch (err) {
                error.Canceled => return error.Canceled,
                error.OutOfMemory => return error.OutOfMemory,
                else => return c.fail("finalize: category: {s}", .{@errorName(err)}),
            };
            if (found) |cat| {
                root = try categoryRoot(c, root, cat, d.source_kind);
            } else {
                const msg = std.fmt.allocPrint(c.arena, "category {f} no longer exists; using the complete root", .{q(d.category_id)}) catch return error.OutOfMemory;
                self.warn(.{ .kind = .category_missing, .download_id = d.id, .message = msg });
            }
        }
        return fsutil.safeJoin(c.arena, c.io, root, try dirName(c, d)) catch |err| switch (err) {
            error.Canceled => error.Canceled,
            error.OutOfMemory => error.OutOfMemory,
            error.UnsafePath => c.fail("finalize: destination: fsutil: unsafe path", .{}),
            else => c.ioFail(err, "destination", null),
        };
    }

    fn warn(self: *Finisher, w: Warning) void {
        if (self.deps.warn) |s| return s.warn_fn(s.ctx, w);
        if (self.deps.log) |l| l.warn("finalize: warning", &.{
            log.Attr.string("kind", @tagName(w.kind)),
            log.Attr.string("download", w.download_id),
            log.Attr.string("path", w.path),
            log.Attr.string("detail", w.message),
        });
    }

    /// The UnderscoresToDots option (replace_for.py, ADR-0009): renames the
    /// top-level entries of `dir`, which by now holds the merged Unpack
    /// output, so the media manager sees the dotted names. It runs for NZB
    /// Downloads only (a torrent's files are being seeded) and before the
    /// marker and the move. A failure is logged and never fails the
    /// Download; a retry after a crash finds nothing left to rename. Renames
    /// are logged at info level, not sent to the warner: they are not
    /// warnings.
    fn dotUnderscores(self: *Finisher, c: Ctx, d: dl.Download, dir: []const u8) void {
        var done: std.ArrayList(tidy.Rename) = .empty;
        const result = tidy.dotUnderscores(c.arena, c.io, dir, &done);
        if (self.deps.log) |l| {
            for (done.items) |r| l.info("finalize: renamed underscores to dots", &.{
                log.Attr.string("download", d.id),
                log.Attr.string("from", r.from),
                log.Attr.string("to", r.to),
            });
            if (result) |_| {} else |err| l.warn("finalize: could not rename underscores to dots", &.{
                log.Attr.string("download", d.id),
                log.Attr.string("error", @errorName(err)),
            });
        }
    }

    /// Gives an obfuscated video its Download's name (tidy.nameVideo): a fully
    /// obfuscated post without a usable PAR2 set leaves the movie under a
    /// random name without an extension, which media managers do not import.
    /// A failure is logged and never fails the Download.
    fn nameVideo(self: *Finisher, c: Ctx, d: dl.Download, dir: []const u8) void {
        const result = tidy.nameVideo(c.arena, c.io, dir, d.name);
        const l = self.deps.log orelse return;
        if (result) |renamed| {
            if (renamed) |r| l.info("finalize: named the obfuscated video after the download", &.{
                log.Attr.string("download", d.id),
                log.Attr.string("from", r.from),
                log.Attr.string("to", r.to),
            });
        } else |err| l.warn("finalize: could not name the obfuscated video", &.{
            log.Attr.string("download", d.id),
            log.Attr.string("error", @errorName(err)),
        });
    }

    /// Writes the restart marker inside `dir`. An existing marker is never
    /// overwritten: it is ours from an earlier attempt, or one of the
    /// Download's own files.
    fn writeMarker(self: *Finisher, c: Ctx, d: dl.Download, dir: []const u8) PortError!void {
        var dh = Io.Dir.cwd().openDir(c.io, dir, .{ .follow_symlinks = false }) catch |err| return c.ioFail(err, "marker", null);
        defer dh.close(c.io);
        const file = dh.createFile(c.io, marker_name, .{ .exclusive = true, .permissions = .fromMode(0o644) }) catch |err| switch (err) {
            error.PathAlreadyExists => {
                if (!markerHolds(c.io, dir, d.id)) {
                    const p = fsutil.join(c.arena, &.{ dir, marker_name }) catch return error.OutOfMemory;
                    const msg = std.fmt.allocPrint(c.arena, "{f} already exists and is not ours; a retry after a crash cannot be detected", .{q(p)}) catch return error.OutOfMemory;
                    self.warn(.{ .kind = .marker_conflict, .download_id = d.id, .path = p, .message = msg });
                }
                return;
            },
            else => return c.ioFail(err, "marker", dir),
        };
        defer file.close(c.io);

        const now_ms = (self.deps.clock orelse realClock)(c.io);
        var aw: Io.Writer.Allocating = .init(c.arena);
        aw.writer.print("{s}\n", .{d.id}) catch return error.OutOfMemory;
        // time.RFC3339: whole seconds.
        time_fmt.writeRfc3339(&aw.writer, now_ms - @mod(now_ms, 1000)) catch return error.OutOfMemory;
        aw.writer.writeByte('\n') catch return error.OutOfMemory;
        file.writeStreamingAll(c.io, aw.written()) catch |err| return c.ioFail(err, "marker", dir);
    }
};

/// The text and class of a failed move (Go: fsutil.WriteError of the error).
fn moveFail(c: Ctx, err: anyerror, src: []const u8, dst: []const u8, parent: []const u8) PortError {
    return switch (err) {
        error.Exists => c.fail("finalize: fsutil: destination exists: {f}", .{q(dst)}),
        error.Nested => c.fail("finalize: fsutil: destination inside source: {f} in {f}", .{ q(dst), q(src) }),
        error.Symlink => c.fail("finalize: fsutil: symlink refused: {f}", .{q(src)}),
        error.SpecialFile => c.fail("finalize: fsutil: special file refused in {f}", .{q(src)}),
        error.TooDeep => c.fail("finalize: {f} is nested too deeply to copy", .{q(src)}),
        else => c.ioFail(err, "move", parent),
    };
}

/// The dir the Download dir goes into for `cat`: the Category's folder
/// override for the protocol of the Download, else <complete root>/<name>.
/// Names and overrides were validated when the Category was stored; this
/// defends anyway.
fn categoryRoot(c: Ctx, complete_root: []const u8, cat: Category, kind: dl.SourceKind) PortError![]const u8 {
    categories_types.validate(cat) catch |err| {
        const text = switch (err) {
            error.InvalidName => "invalid category name",
            error.InvalidDirOverride => "a dir override is not a clean absolute path",
        };
        return c.fail("finalize: category {f}: core: {f}: {s}", .{ q(cat.id), q(cat.name), text });
    };
    const override = cat.dirOverride(kind);
    if (override.len > 0) {
        var parts = std.mem.tokenizeAny(u8, override, "/\\");
        while (parts.next()) |part| {
            if (std.mem.eql(u8, part, "..")) {
                return c.fail("fsutil: unsafe path: dir override {f} has a .. element", .{q(override)});
            }
        }
        return fsutil.clean(c.arena, override) catch error.OutOfMemory;
    }
    return fsutil.safeJoin(c.arena, c.io, complete_root, cat.name) catch |err| switch (err) {
        error.Canceled => error.Canceled,
        error.OutOfMemory => error.OutOfMemory,
        error.UnsafePath => c.fail("finalize: category dir: fsutil: unsafe path", .{}),
        else => c.ioFail(err, "category dir", null),
    };
}

/// The name of the final dir: the sanitized Download name, else one made of
/// its ID.
fn dirName(c: Ctx, d: dl.Download) PortError![]const u8 {
    if (fsutil.safeName(c.arena, d.name)) |n| return n else |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.InvalidName => {},
    }
    const fallback = std.fmt.allocPrint(c.arena, "download-{s}", .{d.id}) catch return error.OutOfMemory;
    if (fsutil.safeName(c.arena, fallback)) |n| return n else |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.InvalidName => {},
    }
    return "download";
}

/// Whether `a` and `b` are the same existing dir.
fn sameDir(io: Io, a: []const u8, b: []const u8) bool {
    var ba: [Io.Dir.max_path_bytes]u8 = undefined;
    var bb: [Io.Dir.max_path_bytes]u8 = undefined;
    const cwd = Io.Dir.cwd();
    const na = cwd.realPathFile(io, a, &ba) catch return false;
    const nb = cwd.realPathFile(io, b, &bb) catch return false;
    return std.mem.eql(u8, ba[0..na], bb[0..nb]);
}

/// Looks for the dir a previous `finish` moved to: `base`, or a Unique'd
/// variant of it (base.1, base.2, ...; the name may be shortened to fit).
/// Only a dir whose marker holds `id` counts, so a dir that merely has the
/// same name is never mistaken for ours.
fn findMoved(c: Ctx, base: []const u8, id: []const u8) PortError!?[]const u8 {
    if (markerHolds(c.io, base, id)) return base;
    const parent = fsutil.dir(c.arena, base) catch return error.OutOfMemory;
    const name = fsutil.base(base);
    var pd = Io.Dir.cwd().openDir(c.io, parent, .{ .iterate = true }) catch |err| switch (err) {
        error.Canceled => return error.Canceled,
        else => return null,
    };
    defer pd.close(c.io);
    const entries = fs.list(c.arena, c.io, pd) catch |err| switch (err) {
        error.Canceled => return error.Canceled,
        error.OutOfMemory => return error.OutOfMemory,
        else => return null,
    };
    for (entries) |e| {
        const stem = variantStem(e.name);
        if (e.kind != .directory or stem.len == e.name.len or !std.mem.startsWith(u8, name, stem)) continue;
        const p = fsutil.join(c.arena, &.{ parent, e.name }) catch return error.OutOfMemory;
        if (markerHolds(c.io, p, id)) return p;
    }
    return null;
}

/// `name` without the trailing `\.[0-9a-f]+` of a Unique'd variant.
fn variantStem(name: []const u8) []const u8 {
    var end = name.len;
    while (end > 0 and (std.ascii.isDigit(name[end - 1]) or (name[end - 1] >= 'a' and name[end - 1] <= 'f'))) end -= 1;
    if (end == name.len or end == 0 or name[end - 1] != '.') return name;
    return name[0 .. end - 1];
}

/// Whether `dir` is a real directory whose regular marker file starts with
/// `id`.
fn markerHolds(io: Io, dir: []const u8, id: []const u8) bool {
    var dh = Io.Dir.cwd().openDir(io, dir, .{ .follow_symlinks = false }) catch return false;
    defer dh.close(io);
    const st = dh.statFile(io, marker_name, .{ .follow_symlinks = false }) catch return false;
    if (st.kind != .file or st.size > 1024) return false;
    var f = dh.openFile(io, marker_name, .{ .follow_symlinks = false, .allow_directory = false }) catch return false;
    defer f.close(io);
    var buf: [1024]u8 = undefined;
    const n = f.readPositionalAll(io, &buf, 0) catch return false;
    const first = if (std.mem.indexOfScalar(u8, buf[0..n], '\n')) |i| buf[0..i] else buf[0..n];
    return std.mem.eql(u8, first, id);
}

// ---- adapters to the services ----

const settings_service = @import("settings_service.zig");
const categories_service = @import("categories_service.zig");

/// The Settings service as the Finisher's SettingsSource. It must outlive the
/// Finisher.
pub fn settingsFromService(svc: *settings_service.Service) SettingsSource {
    const A = struct {
        fn read(ctx: ?*anyopaque, arena: Allocator) anyerror!SettingsSource.View {
            const s: *settings_service.Service = @ptrCast(@alignCast(ctx.?));
            const snap = s.current();
            defer snap.release();
            return .{
                .usenet_complete = try arena.dupe(u8, snap.settings.paths.usenet.complete),
                .torrent_complete = try arena.dupe(u8, snap.settings.paths.torrent.complete),
                .underscores_to_dots = snap.settings.postprocess.underscores_to_dots,
            };
        }
    };
    return .{ .ctx = svc, .read_fn = A.read };
}

/// The Categories service as the Finisher's CategorySource. It must outlive
/// the Finisher.
pub fn categoriesFromService(svc: *categories_service.Service) CategorySource {
    const A = struct {
        fn find(ctx: ?*anyopaque, arena: Allocator, id: []const u8) anyerror!?Category {
            const s: *categories_service.Service = @ptrCast(@alignCast(ctx.?));
            return s.repo.get(arena, id) catch |err| switch (err) {
                error.NotFound => null,
                else => err,
            };
        }
    };
    return .{ .ctx = svc, .find_fn = A.find };
}
