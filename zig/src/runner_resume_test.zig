//! Tests of the resume state and of the deferred recovery volumes
//! (internal/usenet/runner runner_test.go and volumes_test.go): a restart
//! continues where the first run stopped, and the volumes Repair asks for are
//! fetched on top of a Download that was Fetched already.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const dl = @import("download.zig");
const nntptest = @import("nntptest.zig");
const runner = @import("runner.zig");
const tu = @import("runner_testutil.zig");

const Harness = tu.Harness;
const io = tu.io;

fn requestsAtLeast(srv: *nntptest.Server, n: usize) !void {
    var waited: i64 = 0;
    while (waited < 15_000) : (waited += 1) {
        if (srv.totalRequests() >= n) return;
        tu.sleepMs(1);
    }
    return error.Timeout;
}

fn waitFetchedEvent(h: *Harness, id: []const u8) !void {
    var waited: i64 = 0;
    while (waited < 15_000) : (waited += 2) {
        if (h.rec.count(.fetched, id) > 0) return;
        tu.sleepMs(2);
    }
    return error.Timeout;
}

/// 102 articles of one slow connection, with a short save interval.
fn resumeHarness() !struct { h: *Harness, srv: *nntptest.Server, fx: *nntptest.Fixture, id: []const u8 } {
    const h = try Harness.create();
    errdefer h.destroy();
    h.opts.save_interval_ms = 30;
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(3);
    const fx = try h.fixture(&.{
        .{ .name = "movie.mkv", .data = try tu.payload(h.a(), 1_000_000, 1) },
        .{ .name = "movie.nfo", .data = try tu.payload(h.a(), 20_000, 2) },
    }, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    return .{ .h = h, .srv = srv, .fx = fx, .id = id };
}

/// Starts the runner, waits for periodic saves after at least `min_requests`
/// requests and then stops it without a graceful flush: what it saves while
/// shutting down vanishes, as in a crash.
fn killMidRun(h: *Harness, srv: *nntptest.Server, min_requests: usize) !void {
    _ = try h.startRunner();
    var waited: i64 = 0;
    while ((srv.totalRequests() < min_requests or h.mem.saveCount() < 2) and waited < 15_000) : (waited += 1) tu.sleepMs(1);
    try testing.expect(srv.totalRequests() >= min_requests);
    h.mem.setFrozen(true);
    h.stopRunner();
    h.mem.setFrozen(false);
}

test "a resume after a crash refetches only what was not saved" {
    const s = try resumeHarness();
    const h = s.h;
    defer h.destroy();
    try killMidRun(h, s.srv, 40);
    try testing.expectEqual(dl.State.fetching, h.stateOf(s.id).?); // the crash leaves it Fetching
    const first_run = s.srv.totalRequests();
    try testing.expect(first_run < 102);

    const r = try h.startRunnerRecovered();
    _ = r;
    _ = try h.waitState(s.id, .fetched);

    try h.expectFiles(s.id, s.fx.files);
    var refetched: usize = 0;
    for (try s.fx.messageIds(h.a())) |mid| {
        try testing.expect(s.srv.requests(mid) > 0);
        if (s.srv.requests(mid) > 1) refetched += 1;
    }
    try testing.expect(refetched < first_run - 10); // most of the first run's articles were skipped
    try testing.expect(s.srv.totalRequests() < first_run + 102 - 10);
    try waitFetchedEvent(h, s.id);
    try testing.expect(!h.mem.hasBlob(s.id)); // the blob is deleted once Fetched
}

test "a resume starts over when the saved files are gone" {
    const s = try resumeHarness();
    const h = s.h;
    defer h.destroy();
    try killMidRun(h, s.srv, 40);
    var dir = try Io.Dir.cwd().openDir(io, h.dirOf(s.id), .{ .iterate = true });
    var names: std.ArrayList([]const u8) = .empty;
    var it = dir.iterate();
    while (try it.next(io)) |e| {
        if (e.kind == .file and !std.ascii.endsWithIgnoreCase(e.name, ".nzb")) try names.append(h.a(), try h.a().dupe(u8, e.name));
    }
    for (names.items) |n| try dir.deleteFile(io, n);
    dir.close(io);

    _ = try h.startRunner(); // Fetching without a job is picked up again
    _ = try h.waitState(s.id, .fetched);

    try h.expectFiles(s.id, s.fx.files);
}

test "without a progress store a restart fetches everything again without duplicates" {
    const h = try Harness.create();
    defer h.destroy();
    h.opts.persistent = false;
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(3);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    _ = try h.startRunner();
    try requestsAtLeast(srv, 15);
    h.stopRunner();
    try testing.expectEqual(dl.State.fetching, h.stateOf(id).?);

    _ = try h.startRunner(); // a new Runner: its in-memory resume state is empty
    _ = try h.waitState(id, .fetched);

    try h.expectFiles(id, fx.files); // no movie.mkv.1 next to movie.mkv
    const nzb_path = try std.fmt.allocPrint(h.a(), "{s}/movie.nzb", .{h.dirOf(id)});
    _ = try Io.Dir.cwd().statFile(io, nzb_path, .{}); // the stored NZB is kept
}

test "the resume state of the SQLite store refetches only what was not saved" {
    const s = try resumeHarness();
    const h = s.h;
    defer h.destroy();
    h.opts.use_sqlite = true;
    _ = try h.startRunner();
    // Saves go to the database; wait for a few of them to land.
    var waited: i64 = 0;
    const store = h.sqlite_stores.progress();
    while (waited < 15_000) : (waited += 2) {
        if (s.srv.totalRequests() >= 40 and (try store.load(h.a(), s.id)) != null) break;
        tu.sleepMs(2);
    }
    h.stopRunner(); // a shutdown saves a final blob, so this plays a clean stop
    const first_run = s.srv.totalRequests();
    try testing.expect(first_run < 102);
    try testing.expect((try store.load(h.a(), s.id)) != null);
    try testing.expectEqual(dl.State.fetching, h.stateOf(s.id).?);

    _ = try h.startRunnerRecovered();
    _ = try h.waitState(s.id, .fetched);

    try h.expectFiles(s.id, s.fx.files);
    var refetched: usize = 0;
    for (try s.fx.messageIds(h.a())) |mid| {
        if (s.srv.requests(mid) > 1) refetched += 1;
    }
    try testing.expect(refetched <= 2); // a clean stop keeps everything but the articles in flight
    try waitFetchedEvent(h, s.id);
    try testing.expectEqual(null, try store.load(h.a(), s.id)); // the blob is deleted once Fetched
}

test "a damaged resume blob makes the fetch start over" {
    const s = try resumeHarness();
    const h = s.h;
    defer h.destroy();
    _ = try h.startRunner();
    try requestsAtLeast(s.srv, 20);
    h.stopRunner();
    try h.mem.progress().save(s.id, "this is not a blob");

    _ = try h.startRunnerRecovered();
    _ = try h.waitState(s.id, .fetched);

    try h.expectFiles(s.id, s.fx.files);
}

// ---- deferred recovery volumes ----

/// A release with an index and two recovery volumes. The NZB lists its files
/// by name: 0 movie.mkv, 1 movie.par2, 2 movie.vol000+02.par2, 3
/// movie.vol002+04.par2.
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

fn idsOf(fx: *nntptest.Fixture, name: []const u8) []const []const u8 {
    for (fx.files) |f| if (std.mem.eql(u8, f.name, name)) return f.message_ids;
    unreachable;
}

fn toPostProcessing(h: *Harness, id: []const u8) !void {
    _ = try h.rig.repo.updateState(h.a(), id, .{ .to = .post_processing, .at = tu.epoch });
}

/// Plays the adapter: PostProcessing to Fetching.
fn requeueForVolumes(h: *Harness, id: []const u8) !void {
    const d = try h.rig.repo.updateState(h.a(), id, .{ .to = .fetching, .at = tu.epoch });
    h.bus.publish(.{ .download_updated = d });
}

fn deferredBlocks(r: *runner.Runner, h: *Harness, id: []const u8) !u32 {
    var total: u32 = 0;
    for (try r.listDeferred(h.a(), id)) |f| total += f.recovery_blocks;
    return total;
}

test "undefer fetches only the chosen volumes, once, and returns to fetched" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    for (idsOf(fx, "movie.vol000+02.par2")) |m| try testing.expectEqual(@as(usize, 0), srv.requests(m));
    try toPostProcessing(h, id);

    const deferred = try r.listDeferred(h.a(), id);
    try testing.expectEqual(@as(usize, 2), deferred.len);
    try testing.expectEqual(@as(u32, 2), deferred[0].recovery_blocks);
    try testing.expectEqual(@as(u32, 4), deferred[1].recovery_blocks);
    try testing.expectEqual(@as(u32, 6), try deferredBlocks(r, h, id));

    try r.undefer(id, &.{1}); // movie.vol002+04.par2
    try testing.expectEqual(@as(u32, 6), try deferredBlocks(r, h, id)); // a volume counts as deferred until it is fetched
    try requeueForVolumes(h, id);
    const got = try h.waitState(id, .fetched);

    try testing.expect(got.fetched_at != 0);
    for (idsOf(fx, "movie.vol002+04.par2")) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m)); // exactly once
    for ([_][]const u8{ "movie.mkv", "movie.par2" }) |name| {
        for (idsOf(fx, name)) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m)); // not fetched again
    }
    for (idsOf(fx, "movie.vol000+02.par2")) |m| try testing.expectEqual(@as(usize, 0), srv.requests(m)); // stays deferred
    var dir = try h.readDir(id);
    try testing.expectEqual(@as(usize, 3), dir.count());
    try testing.expect(dir.contains("movie.vol002+04.par2"));
    const left = try r.listDeferred(h.a(), id);
    try testing.expectEqual(@as(usize, 1), left.len);
    try testing.expectEqual(@as(u32, 2), left[0].recovery_blocks);
}

test "a second round fetches only the new volume" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try toPostProcessing(h, id);
    try r.undefer(id, &.{1});
    try requeueForVolumes(h, id);
    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);
    // The Fetched event of the first round is in; wait for the second.
    try toPostProcessing(h, id);
    try testing.expectEqual(@as(usize, 1), (try r.listDeferred(h.a(), id)).len);

    try r.undefer(id, &.{0});
    try requeueForVolumes(h, id);
    _ = try h.waitState(id, .fetched);

    for (fx.files) |f| for (f.message_ids) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m)); // every article exactly once
    try h.expectFiles(id, fx.files);
    try testing.expectEqual(@as(u32, 0), try deferredBlocks(r, h, id));
}

test "undefer is refused outside post-processing and rejects bad indexes" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);

    try testing.expectError(error.IllegalTransition, r.undefer(id, &.{0}));

    try toPostProcessing(h, id);
    try testing.expectError(error.IndexOutOfRange, r.undefer(id, &.{2}));
    try testing.expectError(error.IndexOutOfRange, r.undefer(id, &.{ 0, 5 }));
    try testing.expectEqual(@as(u32, 6), try deferredBlocks(r, h, id)); // nothing was recorded
    try testing.expectError(error.NotFound, r.listDeferred(h.a(), "no-such-download"));
}

test "un-deferred volumes survive a restart of the runner" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try toPostProcessing(h, id);
    try r.undefer(id, &.{1});
    h.stopRunner();
    // The daemon died after the requeue and before the volume was fetched.
    _ = try h.rig.repo.updateState(h.a(), id, .{ .to = .fetching, .at = tu.epoch });

    const r2 = try h.startRunnerRecovered();
    _ = try h.waitState(id, .fetched);

    for (idsOf(fx, "movie.mkv")) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m)); // the data file is not fetched again
    for (idsOf(fx, "movie.vol002+04.par2")) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m));
    try testing.expectEqual(@as(u32, 2), try deferredBlocks(r2, h, id));
}

test "a supplement run keeps the stored fetch report" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    try srv.missing(&.{idsOf(fx, "movie.mkv")[0]});
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);
    const before = (try r.fetchReport(id)).?;
    try testing.expectEqual(@as(i64, 1), before.missing_articles);
    try toPostProcessing(h, id);
    try r.undefer(id, &.{0});
    try requeueForVolumes(h, id);
    _ = try h.waitState(id, .fetched);

    const after = (try r.fetchReport(id)).?;

    try testing.expectEqual(before, after);
}

test "fetchDeferred picks the cheapest volumes and requeues the Download" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);
    try toPostProcessing(h, id);

    // 7 blocks are more than the two volumes hold: nothing changes.
    try testing.expect(!try r.fetchDeferred(id, 7));
    try testing.expectEqual(dl.State.post_processing, h.stateOf(id).?);
    try testing.expectEqual(@as(usize, 0), srv.requests(idsOf(fx, "movie.vol002+04.par2")[0]));

    // 3 blocks and the default margin of one: the 4-block volume alone is the
    // cheaper way to 4.
    try testing.expect(try r.fetchDeferred(id, 3));
    _ = try h.waitState(id, .fetched);
    for (idsOf(fx, "movie.vol002+04.par2")) |m| try testing.expectEqual(@as(usize, 1), srv.requests(m));
    for (idsOf(fx, "movie.vol000+02.par2")) |m| try testing.expectEqual(@as(usize, 0), srv.requests(m));
}

const HookLog = struct {
    mu: Io.Mutex = .init,
    calls: std.ArrayList(bool) = .empty,

    fn hook(l: *HookLog) @import("runner_seams.zig").FetchedHook {
        return .{ .ctx = l, .call = call };
    }

    fn call(ctx: *anyopaque, _: []const u8, supplement: bool) void {
        const l: *HookLog = @ptrCast(@alignCast(ctx));
        l.mu.lockUncancelable(io);
        defer l.mu.unlock(io);
        l.calls.append(testing.allocator, supplement) catch {};
    }
};

test "the fetched hook runs once per fetch, telling a supplement run apart" {
    const h = try Harness.create();
    defer h.destroy();
    var log: HookLog = .{};
    defer log.calls.deinit(testing.allocator);
    h.opts.on_fetched = log.hook();
    const srv = try h.addServer("main", 0, 2);
    const fx = try volumeFixture(h, srv);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);
    try toPostProcessing(h, id);
    try r.undefer(id, &.{0});
    try requeueForVolumes(h, id);
    _ = try h.waitState(id, .fetched);
    var waited: i64 = 0;
    while (waited < 5_000 and log.calls.items.len < 2) : (waited += 2) tu.sleepMs(2);

    try testing.expectEqualSlices(bool, &.{ false, true }, log.calls.items);
}
