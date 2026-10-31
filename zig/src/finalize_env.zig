//! The test rig of the finalize group: a temp tree with incomplete and
//! complete roots for both protocols, the Finisher wired to fakes of its
//! seams, and listings to compare trees. Test code only.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const id_mod = @import("id.zig");
const log = @import("log.zig");
const finalize = @import("finalize.zig");
const move = @import("finalize_move.zig");
const util = @import("tidy_testutil.zig");
const ports = @import("postprocess_ports.zig");

pub const io = util.io;

pub const OwnedWarning = struct { kind: finalize.Warning.Kind, download_id: []const u8, message: []const u8, path: []const u8 };

pub const MoveMode = enum {
    /// The real move.
    real,
    /// The real move, reported as a copy across file systems.
    pretend_cross_device,
    /// Fails without moving.
    fail,
    /// Fails the first time only.
    fail_once,
    /// Records the listing of the source, then moves for real.
    record,
};

/// Does not move after `init`: the Finisher holds pointers into it.
pub const Env = struct {
    tree: util.Tree,
    /// Holds the strings of the rig and of every result.
    arena: std.heap.ArenaAllocator,
    usenet_incomplete: []const u8,
    usenet_complete: []const u8,
    torrent_incomplete: []const u8,
    torrent_complete: []const u8,
    underscores_to_dots: bool = false,
    cats: std.ArrayList(finalize.Category) = .empty,
    warnings: std.ArrayList(OwnedWarning) = .empty,
    move_mode: MoveMode = .real,
    failed_once: bool = false,
    at_move: []const u8 = "",
    /// Set by a fake seam to make the next category lookup fail.
    category_error: ?anyerror = null,
    /// Runs when the category lookup is entered (to test cancellation).
    category_hook: ?*const fn (*Env) anyerror!void = null,
    sink: Io.Writer.Allocating,
    core: log.Core,
    fin: finalize.Finisher,
    next_id: u32 = 0,

    pub fn init(env: *Env) !void {
        env.* = undefined;
        env.tree = try util.Tree.init();
        errdefer env.tree.deinit();
        env.arena = .init(testing.allocator);
        errdefer env.arena.deinit();
        const al = env.arena.allocator();
        env.usenet_incomplete = try env.tree.abs(al, "usenet/incomplete");
        env.usenet_complete = try env.tree.abs(al, "usenet/complete");
        env.torrent_incomplete = try env.tree.abs(al, "torrents/incomplete");
        env.torrent_complete = try env.tree.abs(al, "torrents/complete");
        try env.tree.mkdir("usenet/incomplete");
        try env.tree.mkdir("torrents/incomplete");
        env.underscores_to_dots = false;
        env.cats = .empty;
        env.warnings = .empty;
        env.move_mode = .real;
        env.failed_once = false;
        env.at_move = "";
        env.category_error = null;
        env.category_hook = null;
        env.next_id = 0;
        env.sink = .init(testing.allocator);
        env.core = .init(io, &env.sink.writer, .{ .level = .debug, .clock = fixedClock });
        env.fin = .init(testing.allocator, .{
            .settings = .{ .ctx = env, .read_fn = readSettings },
            .categories = .{ .ctx = env, .find_fn = findCategory },
            .warn = .{ .ctx = env, .warn_fn = onWarn },
            .log = env.core.logger(),
            .mover = .{ .ctx = env, .move_fn = doMove },
            .clock = fixedMs,
        });
    }

    pub fn deinit(env: *Env) void {
        env.sink.deinit();
        env.arena.deinit();
        env.tree.deinit();
    }

    pub fn a(env: *Env) Allocator {
        return env.arena.allocator();
    }

    fn fixedClock() log.Time {
        return .{ .unix_ns = 1793361600 * std.time.ns_per_s };
    }

    fn fixedMs(_: Io) i64 {
        return 1793361600123;
    }

    fn readSettings(ctx: ?*anyopaque, arena: Allocator) anyerror!finalize.SettingsSource.View {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        return .{
            .usenet_complete = try arena.dupe(u8, env.usenet_complete),
            .torrent_complete = try arena.dupe(u8, env.torrent_complete),
            .underscores_to_dots = env.underscores_to_dots,
        };
    }

    fn findCategory(ctx: ?*anyopaque, _: Allocator, id: []const u8) anyerror!?finalize.Category {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        if (env.category_hook) |h| try h(env);
        if (env.category_error) |e| return e;
        for (env.cats.items) |c| if (std.mem.eql(u8, c.id, id)) return c;
        return null;
    }

    fn onWarn(ctx: ?*anyopaque, w: finalize.Warning) void {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        const al = env.a();
        env.warnings.append(al, .{
            .kind = w.kind,
            .download_id = al.dupe(u8, w.download_id) catch @panic("oom"),
            .message = al.dupe(u8, w.message) catch @panic("oom"),
            .path = al.dupe(u8, w.path) catch @panic("oom"),
        }) catch @panic("oom");
    }

    fn doMove(ctx: ?*anyopaque, gpa: Allocator, io_: Io, src: []const u8, dst: []const u8, res: *move.MoveResult) anyerror!void {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        switch (env.move_mode) {
            .real => return move.moveDir(gpa, io_, src, dst, res, null),
            .pretend_cross_device => {
                try move.moveDir(gpa, io_, src, dst, res, null);
                res.cross_device = true;
            },
            .fail => return error.DiskOnFire,
            .fail_once => {
                if (!env.failed_once) {
                    env.failed_once = true;
                    return error.Crash;
                }
                return move.moveDir(gpa, io_, src, dst, res, null);
            },
            .record => {
                env.at_move = try util.listingOf(env.a(), src);
                return move.moveDir(gpa, io_, src, dst, res, null);
            },
        }
    }

    // ---- building ----

    pub fn addCategory(env: *Env, name: []const u8, usenet_dir: []const u8, torrent_dir: []const u8) ![]const u8 {
        const cid = try env.newId();
        try env.cats.append(env.a(), .{ .id = cid, .name = name, .usenet_dir = usenet_dir, .torrent_dir = torrent_dir });
        return cid;
    }

    pub fn newId(env: *Env) ![]const u8 {
        var raw: [16]u8 = undefined;
        io.random(&raw);
        const idv = id_mod.fromBytes(raw);
        return env.a().dupe(u8, &idv);
    }

    pub fn download(env: *Env, name: []const u8, kind: dl.SourceKind) !dl.Download {
        return .{
            .id = try env.newId(),
            .name = name,
            .source_kind = kind,
            .state = .post_processing,
            .added_at = 1,
            .updated_at = 1,
        };
    }

    pub fn usenet(env: *Env, name: []const u8) !dl.Download {
        return env.download(name, .nzb);
    }

    pub fn torrent(env: *Env, name: []const u8) !dl.Download {
        return env.download(name, .torrent);
    }

    /// Makes <usenet incomplete>/<name> and returns its path.
    pub fn usenetDir(env: *Env, name: []const u8) ![]const u8 {
        return env.makeDir("usenet/incomplete", name);
    }

    pub fn torrentDir(env: *Env, name: []const u8) ![]const u8 {
        return env.makeDir("torrents/incomplete", name);
    }

    fn makeDir(env: *Env, root: []const u8, name: []const u8) ![]const u8 {
        const sub = try std.fs.path.join(env.a(), &.{ root, name });
        try env.tree.mkdir(sub);
        return env.tree.abs(env.a(), sub);
    }

    /// The path of `abs_path` relative to the temp dir.
    pub fn rel(env: *Env, abs_path: []const u8) []const u8 {
        if (abs_path.len <= env.tree.path.len) return "";
        return abs_path[env.tree.path.len + 1 ..];
    }

    /// Writes files (name, content) under the absolute dir, making parents.
    pub fn put(env: *Env, dir: []const u8, files: []const [2][]const u8) !void {
        for (files) |f| {
            const r = try std.fs.path.join(env.a(), &.{ env.rel(dir), f[0] });
            try env.tree.write(r, f[1]);
        }
    }

    pub fn link(env: *Env, target: []const u8, abs_link: []const u8) !void {
        try env.tree.symlink(target, env.rel(abs_link));
    }

    pub fn join(env: *Env, parts: []const []const u8) ![]const u8 {
        return std.fs.path.join(env.a(), parts);
    }

    // ---- running ----

    pub const Out = struct {
        err: ?ports.PortError,
        path: []const u8,
        message: []const u8,
        vouched: bool = false,
    };

    pub fn run(env: *Env, d: dl.Download, dir: []const u8, unpacked: []const u8) !Out {
        var diag: ports.Diag = .{};
        const fin = env.fin.port();
        if (fin.finish(env.a(), io, d, dir, unpacked, &diag)) |p| {
            return .{ .err = null, .path = p, .message = "" };
        } else |err| return .{ .err = err, .path = "", .message = diag.message, .vouched = diag.vouched };
    }

    /// Runs and expects success.
    pub fn ok(env: *Env, d: dl.Download, dir: []const u8, unpacked: []const u8) ![]const u8 {
        const out = try env.run(d, dir, unpacked);
        if (out.err) |e| {
            std.debug.print("finish failed: {s}: {s}\n", .{ @errorName(e), out.message });
            return e;
        }
        return out.path;
    }

    /// Runs and expects the failure `Failed`; returns the message.
    pub fn failed(env: *Env, d: dl.Download, dir: []const u8, unpacked: []const u8) ![]const u8 {
        const out = try env.run(d, dir, unpacked);
        try testing.expectEqual(@as(?ports.PortError, error.Failed), out.err);
        return out.message;
    }

    /// Makes <incomplete>/work-<id> with one file and finishes it.
    pub fn finishUsenet(env: *Env, d: dl.Download) !Out {
        const name = try std.fmt.allocPrint(env.a(), "work-{s}", .{d.id});
        const dir = try env.usenetDir(name);
        try env.put(dir, &.{.{ "a.mkv", "a" }});
        return env.run(d, dir, "");
    }

    // ---- looking ----

    pub fn listing(env: *Env, dir: []const u8) ![]const u8 {
        return util.listingOf(env.a(), dir);
    }

    /// The listing without the marker line.
    pub fn listingNoMarker(env: *Env, dir: []const u8) ![]const u8 {
        const full = try env.listing(dir);
        // The marker holds the ID and a time on two lines.
        return dropLines(env.a(), full, &.{ finalize.marker_name ++ " = ", "2026-10-30T12:00:00Z" });
    }

    pub fn expectTree(env: *Env, dir: []const u8, expected: []const [2][]const u8) !void {
        try util.expectListing(try env.listingNoMarker(dir), expected);
    }

    pub fn has(_: *Env, abs_path: []const u8) bool {
        _ = Io.Dir.cwd().statFile(io, abs_path, .{ .follow_symlinks = false }) catch return false;
        return true;
    }

    pub fn read(env: *Env, abs_path: []const u8) ![]u8 {
        return Io.Dir.cwd().readFileAlloc(io, abs_path, env.a(), .limited(1 << 20));
    }

    pub fn logText(env: *Env) []const u8 {
        return env.sink.written();
    }

    /// A digest of names, modes, mtimes (of files) and contents.
    pub fn hashTree(env: *Env, dir: []const u8) ![32]u8 {
        var h = std.crypto.hash.sha2.Sha256.init(.{});
        var root = try Io.Dir.cwd().openDir(io, dir, .{ .iterate = true, .follow_symlinks = false });
        defer root.close(io);
        try hashDir(env.a(), &h, root, "");
        var out: [32]u8 = undefined;
        h.final(&out);
        return out;
    }
};

fn hashDir(a: Allocator, h: *std.crypto.hash.sha2.Sha256, d: Io.Dir, prefix: []const u8) anyerror!void {
    const fs = @import("tidy_fs.zig");
    const entries = try fs.list(a, io, d);
    for (entries) |e| {
        const st = try d.statFile(io, e.name, .{ .follow_symlinks = false });
        const p = try std.fs.path.join(a, &.{ prefix, e.name });
        h.update(p);
        h.update(std.mem.asBytes(&st.permissions.toMode()));
        h.update(@tagName(st.kind));
        switch (st.kind) {
            .file => {
                h.update(std.mem.asBytes(&st.mtime.nanoseconds));
                h.update(try d.readFileAlloc(io, e.name, a, .limited(1 << 24)));
            },
            .directory => {
                var sub = try d.openDir(io, e.name, .{ .iterate = true, .follow_symlinks = false });
                defer sub.close(io);
                try hashDir(a, h, sub, p);
            },
            else => {},
        }
    }
}

/// `text` without the lines that start with one of `prefixes`.
pub fn dropLines(a: Allocator, text: []const u8, prefixes: []const []const u8) ![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var it = std.mem.splitScalar(u8, text, '\n');
    while (it.next()) |line| {
        if (line.len == 0) continue;
        var drop = false;
        for (prefixes) |p| if (std.mem.startsWith(u8, line, p)) {
            drop = true;
        };
        if (drop) continue;
        try out.appendSlice(a, line);
        try out.append(a, '\n');
    }
    return out.items;
}
