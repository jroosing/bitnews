//! Small adapters from the rest of the daemon to the post-processor
//! (internal/daemon: newPostProcessor, osFS, workDirs; internal/usenet/runner:
//! report.go and volumes.go):
//!
//!   * `RunnerPorts`: the FetchReporter and DeferredVolumes ports on the Usenet
//!     runner. `fetchDeferred` of the runner un-defers the cheapest volumes and
//!     moves the Download from post_processing back to fetching itself, as in
//!     Go.
//!   * `WorkDirs`: DirFor. The working directory of a Usenet Download is the
//!     parent of its staged NZB (the add service created it as
//!     `<incomplete>/<id>`, and changing the incomplete root in the settings
//!     must not move the directory of an existing Download); only a Download
//!     without a source row falls back to the current settings. For a torrent
//!     the layout belongs to the torrent runner (Z7): its `TorrentDir` seam.
//!   * `osFs`: FS on the real filesystem.
//!   * `FetchedPoke`: the runner's `on_fetched` hook, which wakes the
//!     post-processor.
//!
//! Hand-off with the runner, as in Go: the runner publishes `fetched` after a
//! Download became Fetched, also after a supplement run that only fetched the
//! recovery volumes Repair asked for; the post-processor reacts to that event
//! (and to the hook) and takes the Fetched Download from its first stage, so a
//! supplement run needs no case of its own.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const log = @import("log.zig");
const ports = @import("postprocess_ports.zig");
const pp_mod = @import("postprocess.zig");
const runner_mod = @import("runner.zig");
const seams = @import("runner_seams.zig");
const settings_service = @import("settings_service.zig");

const PortError = ports.PortError;

/// The class of an error from the rest of the daemon. Only cancellation and
/// memory keep their meaning; the rest is an internal failure whose detail the
/// caller logs (the ports without a `Diag` cannot carry text).
pub fn portError(err: anyerror) PortError {
    return switch (err) {
        error.Canceled => error.Canceled,
        error.OutOfMemory => error.OutOfMemory,
        else => error.Failed,
    };
}

/// FetchReporter and DeferredVolumes on the Usenet runner. `runner` must
/// outlive the ports.
pub const RunnerPorts = struct {
    runner: *runner_mod.Runner,
    log: ?log.Logger = null,

    pub fn fetchReporter(self: *RunnerPorts) ports.FetchReporter {
        return .{ .ctx = self, .report_fn = report };
    }

    pub fn deferredVolumes(self: *RunnerPorts) ports.DeferredVolumes {
        return .{ .ctx = self, .fetch_fn = fetchDeferred };
    }

    fn report(ctx: *anyopaque, _: Allocator, _: Io, d: dl.Download) PortError!ports.FetchReport {
        const self: *RunnerPorts = @ptrCast(@alignCast(ctx));
        const found = self.runner.fetchReport(d.id) catch |err| {
            self.warn("fetch report not read", d.id, err);
            return portError(err);
        };
        // Go: an error wrapping ErrNotFound, an internal failure.
        const r = found orelse {
            self.warn("no fetch report", d.id, error.NotFound);
            return error.Failed;
        };
        return .{ .missing_articles = r.missing_articles, .damaged_articles = r.damaged_articles };
    }

    fn fetchDeferred(ctx: *anyopaque, _: Io, d: dl.Download, min_blocks: i64) PortError!bool {
        const self: *RunnerPorts = @ptrCast(@alignCast(ctx));
        return self.runner.fetchDeferred(d.id, min_blocks) catch |err| {
            self.warn("deferred volumes not requested", d.id, err);
            return portError(err);
        };
    }

    fn warn(self: *RunnerPorts, msg: []const u8, id: []const u8, err: anyerror) void {
        if (err == error.Canceled) return;
        if (self.log) |l| l.warn(msg, &.{ log.Attr.string("id", id), log.Attr.string("err", @errorName(err)) });
    }
};

/// The torrent runner's save path. After a relocate this is the complete
/// directory; otherwise `{torrent incomplete}/{id}`.
pub const TorrentDir = struct {
    ctx: *anyopaque,
    dir_fn: *const fn (ctx: *anyopaque, arena: Allocator, d: dl.Download) anyerror![]const u8,
};

/// The incomplete root of a Source kind in the settings in effect now.
pub const Roots = struct {
    ctx: *anyopaque,
    incomplete_fn: *const fn (ctx: *anyopaque, arena: Allocator, kind: dl.SourceKind) anyerror![]const u8,
};

/// `Roots` on the Settings service, which must outlive it.
pub const SettingsRoots = struct {
    service: *settings_service.Service,

    pub fn roots(self: *SettingsRoots) Roots {
        return .{ .ctx = self, .incomplete_fn = incomplete };
    }

    fn incomplete(ctx: *anyopaque, arena: Allocator, kind: dl.SourceKind) anyerror![]const u8 {
        const self: *SettingsRoots = @ptrCast(@alignCast(ctx));
        const snap = self.service.current();
        defer snap.release();
        return arena.dupe(u8, switch (kind) {
            .nzb => snap.settings.paths.usenet.incomplete,
            .torrent => snap.settings.paths.torrent.incomplete,
        });
    }
};

/// DirFor (internal/daemon/workdir.go). Go caches the directory of a Download
/// until it is removed; here the lookup is one indexed read per Download and
/// stage, so there is no cache to invalidate.
pub const WorkDirs = struct {
    sources: seams.NzbSource,
    roots: Roots,
    torrent: ?TorrentDir = null,

    pub fn dirFor(self: *WorkDirs) pp_mod.DirFor {
        return .{ .ctx = self, .dir_fn = dir };
    }

    fn dir(ctx: *anyopaque, arena: Allocator, _: Io, d: dl.Download) anyerror![]const u8 {
        const self: *WorkDirs = @ptrCast(@alignCast(ctx));
        switch (d.source_kind) {
            .torrent => if (self.torrent) |t| return t.dir_fn(t.ctx, arena, d),
            .nzb => if (try self.sources.path(arena, d.id)) |path| {
                if (path.len > 0) return std.fs.path.dirname(path) orelse ".";
            },
        }
        const root = try self.roots.incomplete_fn(self.roots.ctx, arena, d.source_kind);
        return std.fs.path.join(arena, &.{ root, d.id });
    }
};

var os_fs_marker: u8 = 0;

/// FS on the real filesystem. A symlink to a directory is not a directory
/// (Go: `os.Lstat`).
pub const os_fs: pp_mod.FS = .{ .ctx = &os_fs_marker, .dir_exists_fn = osDirExists };

fn osDirExists(_: *anyopaque, io: Io, dir: []const u8) anyerror!bool {
    const st = Io.Dir.cwd().statFile(io, dir, .{ .follow_symlinks = false }) catch |err| switch (err) {
        error.FileNotFound => return false,
        else => return err,
    };
    return st.kind == .directory;
}

/// The runner's `on_fetched` hook for the post-processor. The runner is created
/// before the post-processor (the ports need the runner), so the hook is
/// handed out first and pointed at the post-processor later with `set`. Until
/// then the hook does nothing: the post-processor's poll finds the Download.
pub const FetchedPoke = struct {
    pp: std.atomic.Value(?*pp_mod.PostProcessor) = .init(null),

    pub fn set(self: *FetchedPoke, pp: *pp_mod.PostProcessor) void {
        self.pp.store(pp, .release);
    }

    pub fn hook(self: *FetchedPoke) seams.FetchedHook {
        return .{ .ctx = self, .call = call };
    }

    fn call(ctx: *anyopaque, _: []const u8, _: bool) void {
        const self: *FetchedPoke = @ptrCast(@alignCast(ctx));
        if (self.pp.load(.acquire)) |pp| pp.poke();
    }
};

// ---- tests ----

const testing = std.testing;
const nntptest = @import("nntptest.zig");
const tu = @import("runner_testutil.zig");
const fk = @import("postprocess_fakes.zig");

const Harness = tu.Harness;

fn idsOf(fx: *nntptest.Fixture, name: []const u8) []const []const u8 {
    for (fx.files) |f| if (std.mem.eql(u8, f.name, name)) return f.message_ids;
    unreachable;
}

fn volumeFixture(h: *Harness, srv: *nntptest.Server) !*nntptest.Fixture {
    const fx = try h.fixture(&.{
        .{ .name = "movie.mkv", .data = try tu.payload(h.a(), 40_000, 1) },
        .{ .name = "movie.par2", .data = try tu.payload(h.a(), 3_000, 2) },
        .{ .name = "movie.vol000+02.par2", .data = try tu.payload(h.a(), 20_000, 3) },
        .{ .name = "movie.vol002+04.par2", .data = try tu.payload(h.a(), 30_000, 4) },
    }, 10_000);
    try srv.installFixture(fx);
    return fx;
}

fn waitFetchedEvent(h: *Harness, id: []const u8) !void {
    var waited: i64 = 0;
    while (waited < 15_000) : (waited += 2) {
        if (h.rec.count(.fetched, id) > 0) return;
        tu.sleepMs(2);
    }
    return error.Timeout;
}

test "the fetch reporter gives the report the runner stored, and fails internal without one" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    try srv.missing(&.{idsOf(fx, "movie.mkv")[0]});
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);
    var rp: RunnerPorts = .{ .runner = r };
    const d = try h.get(id);

    const rep = try rp.fetchReporter().report(h.a(), tu.io, d);

    try testing.expectEqual(@as(i64, 1), rep.missing_articles);
    try testing.expectEqual(@as(i64, 0), rep.damaged_articles);
    try testing.expect(rep.damaged());

    var other = d;
    other.id = "never-fetched";
    try testing.expectError(error.Failed, rp.fetchReporter().report(h.a(), tu.io, other));
}

test "the deferred volumes port un-defers volumes and moves the download back to fetching" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);
    _ = try h.rig.repo.updateState(h.a(), id, .{ .to = .post_processing, .at = tu.epoch });
    var rp: RunnerPorts = .{ .runner = r };
    const d = try h.get(id);

    // The two deferred volumes hold 6 blocks together: 7 cannot be covered.
    try testing.expect(!try rp.deferredVolumes().fetchDeferred(tu.io, d, 7));
    try testing.expectEqual(dl.State.post_processing, h.stateOf(id).?);

    try testing.expect(try rp.deferredVolumes().fetchDeferred(tu.io, d, 3));
    _ = try h.waitState(id, .fetched);
    for (idsOf(fx, "movie.vol002+04.par2")) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m));

    var gone = d;
    gone.id = "no-such-download";
    try testing.expectError(error.Failed, rp.deferredVolumes().fetchDeferred(tu.io, gone, 1));
}

test "a download short of recovery volumes is fetched again and then post-processed to ready" {
    const h = try Harness.create();
    defer h.destroy();
    var poke: FetchedPoke = .{};
    h.opts.on_fetched = poke.hook();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    var rp: RunnerPorts = .{ .runner = r };

    var log_: fk.Log = .{};
    defer log_.deinit();
    // The first repair finds 3 blocks missing; the second, after the volumes
    // came in, is clean.
    var repair: ShortOnce = .{ .log = &log_ };
    var unpack: fk.Unpack = .{ .log = &log_ };
    defer unpack.deinit();
    var finisher: fk.Finisher = .{ .log = &log_ };
    defer finisher.deinit();
    var dirs: fk.DirFake = .{};
    const pp = try pp_mod.PostProcessor.create(testing.allocator, tu.io, .{
        .downloads = h.rig.repo,
        .bus = h.bus,
        .repairer = repair.port(),
        .unpacker = unpack.port(),
        .finisher = finisher.port(),
        .fetch_reporter = rp.fetchReporter(),
        .deferred = rp.deferredVolumes(),
        .dir_for = dirs.port(),
        .poll_interval_ms = 3_600_000,
    });
    defer pp.destroy();
    poke.set(pp);
    try pp.start();

    const got = try h.waitState(id, .ready);

    try testing.expectEqualStrings("/complete/movie", got.output_path);
    // The 4-block volume alone covers the 3 blocks and the margin of one.
    for (idsOf(fx, "movie.vol002+04.par2")) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m));
    for (idsOf(fx, "movie.vol000+02.par2")) |m| try testing.expectEqual(@as(usize, 0), srv.requests(m));
    try log_.expect(&.{ "repair", "repair", "detect", "finish" });
    pp.stop();
}

/// A Repairer that finds 3 blocks missing on its first call and nothing wrong
/// on the later ones.
const ShortOnce = struct {
    log: *fk.Log,
    calls: std.atomic.Value(u32) = .init(0),

    fn port(s: *ShortOnce) ports.Repairer {
        return .{ .ctx = s, .repair_fn = repair };
    }

    fn repair(ctx: *anyopaque, _: Allocator, _: Io, _: []const u8, diag: *ports.Diag) PortError!ports.RepairOutcome {
        const s: *ShortOnce = @ptrCast(@alignCast(ctx));
        s.log.add("repair");
        if (s.calls.fetchAdd(1, .acq_rel) == 0) {
            diag.* = .{ .needed = 3, .available = 0 };
            return error.InsufficientRecovery;
        }
        return .{ .has_par2 = true };
    }
};

const FakeSources = struct {
    path: ?[]const u8,

    fn source(s: *FakeSources) seams.NzbSource {
        return .{ .ctx = s, .path_fn = get };
    }

    fn get(ctx: *anyopaque, arena: Allocator, _: []const u8) anyerror!?[]const u8 {
        const s: *FakeSources = @ptrCast(@alignCast(ctx));
        return if (s.path) |p| try arena.dupe(u8, p) else null;
    }
};

const FakeRoots = struct {
    fn roots(s: *FakeRoots) Roots {
        return .{ .ctx = s, .incomplete_fn = incomplete };
    }

    fn incomplete(_: *anyopaque, arena: Allocator, kind: dl.SourceKind) anyerror![]const u8 {
        return arena.dupe(u8, switch (kind) {
            .nzb => "/data/usenet/incomplete",
            .torrent => "/data/torrents",
        });
    }
};

const FakeTorrentDir = struct {
    fn port(s: *FakeTorrentDir) TorrentDir {
        return .{ .ctx = s, .dir_fn = dir };
    }

    fn dir(_: *anyopaque, arena: Allocator, d: dl.Download) anyerror![]const u8 {
        return std.fmt.allocPrint(arena, "/seed/{s}", .{d.name});
    }
};

fn blank(id: []const u8, kind: dl.SourceKind) dl.Download {
    return .{ .id = id, .name = "Name", .source_kind = kind, .state = .fetched, .added_at = 1, .updated_at = 1 };
}

test "the working directory of a usenet download is the parent of its staged NZB" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var sources: FakeSources = .{ .path = "/old/incomplete/abc/Name.nzb" };
    var roots: FakeRoots = .{};
    var wd: WorkDirs = .{ .sources = sources.source(), .roots = roots.roots() };

    const dir = try wd.dirFor().dirFor(arena.allocator(), testing.io, blank("abc", .nzb));

    try testing.expectEqualStrings("/old/incomplete/abc", dir);
}

test "without a source row the settings decide, for usenet and for torrents without a torrent runner" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var sources: FakeSources = .{ .path = null };
    var roots: FakeRoots = .{};
    var wd: WorkDirs = .{ .sources = sources.source(), .roots = roots.roots() };

    try testing.expectEqualStrings("/data/usenet/incomplete/abc", try wd.dirFor().dirFor(arena.allocator(), testing.io, blank("abc", .nzb)));
    try testing.expectEqualStrings("/data/torrents/abc", try wd.dirFor().dirFor(arena.allocator(), testing.io, blank("abc", .torrent)));
}

test "a torrent's directory comes from the torrent seam, never from a source row" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var sources: FakeSources = .{ .path = "/must/not/be/used/x.nzb" };
    var roots: FakeRoots = .{};
    var torrent: FakeTorrentDir = .{};
    var wd: WorkDirs = .{ .sources = sources.source(), .roots = roots.roots(), .torrent = torrent.port() };

    try testing.expectEqualStrings("/seed/Name", try wd.dirFor().dirFor(arena.allocator(), testing.io, blank("abc", .torrent)));
    try testing.expectEqualStrings("/must/not/be/used", try wd.dirFor().dirFor(arena.allocator(), testing.io, blank("abc", .nzb)));
}

test "the settings roots read the incomplete folders now in effect" {
    var rig: @import("downloads_testutil.zig").Rig = undefined;
    try rig.open();
    defer rig.close();
    var svc = try settings_service.Service.init(testing.allocator, testing.io, .{ .store = rig.store }, @import("event.zig").Publisher.discard, null);
    defer svc.deinit();
    var sr: SettingsRoots = .{ .service = &svc };
    const snap = svc.current();
    defer snap.release();

    const nzb_root = try sr.roots().incomplete_fn(sr.roots().ctx, rig.arena(), .nzb);
    const torrent_root = try sr.roots().incomplete_fn(sr.roots().ctx, rig.arena(), .torrent);

    try testing.expectEqualStrings(snap.settings.paths.usenet.incomplete, nzb_root);
    try testing.expectEqualStrings(snap.settings.paths.torrent.incomplete, torrent_root);
}

test "the real filesystem check: a directory is one, a file, a missing path and a symlink are not" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.createDir(testing.io, "dir", .default_dir);
    try tmp.dir.writeFile(testing.io, .{ .sub_path = "file", .data = "x" });
    try tmp.dir.symLink(testing.io, "dir", "link", .{ .is_directory = true });
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    const root = buf[0..n];
    var a: std.heap.ArenaAllocator = .init(testing.allocator);
    defer a.deinit();
    const p = struct {
        fn at(arena: Allocator, base: []const u8, name: []const u8) ![]const u8 {
            return std.fmt.allocPrint(arena, "{s}/{s}", .{ base, name });
        }
    };

    try testing.expect(try os_fs.dirExists(testing.io, try p.at(a.allocator(), root, "dir")));
    try testing.expect(!try os_fs.dirExists(testing.io, try p.at(a.allocator(), root, "file")));
    try testing.expect(!try os_fs.dirExists(testing.io, try p.at(a.allocator(), root, "missing")));
    try testing.expect(!try os_fs.dirExists(testing.io, try p.at(a.allocator(), root, "link")));
}

test "error classes: cancellation and memory keep their meaning, the rest is a failure" {
    try testing.expectEqual(@as(PortError, error.Canceled), portError(error.Canceled));
    try testing.expectEqual(@as(PortError, error.OutOfMemory), portError(error.OutOfMemory));
    try testing.expectEqual(@as(PortError, error.Failed), portError(error.NotFound));
}

test "the fetched hook pokes the post-processor once it is set, and is harmless before" {
    var poke: FetchedPoke = .{};
    const hk = poke.hook();
    hk.call(hk.ctx, "id", false); // nobody to wake yet

    var h: fk.Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    poke.set(h.pp);
    hk.call(hk.ctx, "id", true);

    h.pp.wake_mu.lockUncancelable(testing.io);
    defer h.pp.wake_mu.unlock(testing.io);
    try testing.expect(h.pp.pending);
}
