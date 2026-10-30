//! Tests of the Usenet runner (internal/usenet/runner/runner_test.go): a real
//! Runner on a real database, fake NNTP servers on loopback sockets, temp
//! directories and a recorder of the bus events.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const dl = @import("download.zig");
const event = @import("event.zig");
const nntptest = @import("nntptest.zig");
const queue = @import("queue.zig");
const tu = @import("runner_testutil.zig");

const Harness = tu.Harness;
const gpa = tu.gpa;
const io = tu.io;

fn inputs(comptime n: usize, names: [n][]const u8, datas: [n][]const u8) [n]nntptest.FixtureInput {
    var out: [n]nntptest.FixtureInput = undefined;
    for (&out, names, datas) |*o, name, data| o.* = .{ .name = name, .data = data };
    return out;
}

test "fetches an NZB to fetched with byte-exact files" {
    const h = try Harness.create();
    defer h.destroy();
    const movie = try tu.payload(h.a(), 300_000, 1);
    const nfo = try tu.payload(h.a(), 5_000, 2);
    const fx = try h.fixture(&.{ .{ .name = "movie.mkv", .data = movie }, .{ .name = "movie.nfo", .data = nfo } }, 50_000);
    const srv = try h.addServer("main", 0, 4);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{ .total_bytes = 400_000 });
    _ = try h.startRunner();

    const got = try h.waitState(id, .fetched);

    try testing.expect(got.fetched_at != 0);
    try h.expectFiles(id, fx.files);
    try testing.expectEqual(@as(i64, 305_000), got.done_bytes);
    // The state is stored before its events are published, and the recorder
    // reads the bus on its own task.
    try Harness.waitFor(*tu.Recorder, h.rec, struct {
        fn f(r: *tu.Recorder) bool {
            return r.count(.fetched, "dl-1") > 0;
        }
    }.f, 10_000);
    var kinds: std.ArrayList(event.Kind) = .empty;
    for (try h.rec.all(h.a())) |s| {
        if (s.kind != .download_progress and s.kind != .download_added) try kinds.append(h.a(), s.kind);
    }
    try testing.expectEqualSlices(event.Kind, &.{ .download_updated, .download_updated, .fetched }, kinds.items);
}

fn fetchedEventSeen(r: *tu.Recorder, id: []const u8) bool {
    return r.count(.fetched, id) > 0;
}

fn waitFetchedEvent(h: *Harness, id: []const u8) !void {
    var waited: i64 = 0;
    while (waited < 15_000) : (waited += 2) {
        if (fetchedEventSeen(h.rec, id)) return;
        tu.sleepMs(2);
    }
    return error.Timeout;
}

test "with a limit of one two Downloads run one after the other" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fa = try h.fixture(&.{.{ .name = "a.bin", .data = try tu.payload(h.a(), 60_000, 1) }}, 10_000);
    const fb = try h.fixture(&.{.{ .name = "b.bin", .data = try tu.payload(h.a(), 60_000, 2) }}, 10_000);
    try srv.installFixture(fa);
    try srv.installFixture(fb);
    const a = try h.addDownload("a", fa, .{});
    const b = try h.addDownload("b", fb, .{});
    _ = try h.startRunner();

    _ = try h.waitState(a, .fetched);
    _ = try h.waitState(b, .fetched);
    try waitFetchedEvent(h, b);

    const a_done = h.rec.firstIndex(.fetched, a, null).?;
    const b_start = h.rec.firstIndex(.download_updated, b, .fetching).?;
    try testing.expect(a_done < b_start);
}

test "raising the limit at runtime activates the second Download" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(10);
    const fa = try h.fixture(&.{.{ .name = "a.bin", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    const fb = try h.fixture(&.{.{ .name = "b.bin", .data = try tu.payload(h.a(), 10_000, 2) }}, 10_000);
    try srv.installFixture(fa);
    try srv.installFixture(fb);
    const a = try h.addDownload("a", fa, .{});
    const b = try h.addDownload("b", fb, .{});
    _ = try h.startRunner();
    _ = try h.waitState(a, .fetching);
    try testing.expectEqual(dl.State.queued, h.stateOf(b).?);

    h.setLimit(2);

    _ = try h.waitState(b, .fetching);
    try testing.expectEqual(dl.State.fetching, h.stateOf(a).?); // a keeps running
}

test "a higher priority Download runs first" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fa = try h.fixture(&.{.{ .name = "a.bin", .data = try tu.payload(h.a(), 30_000, 1) }}, 10_000);
    const fb = try h.fixture(&.{.{ .name = "b.bin", .data = try tu.payload(h.a(), 30_000, 2) }}, 10_000);
    try srv.installFixture(fa);
    try srv.installFixture(fb);
    const low = try h.addDownload("low", fa, .{ .priority = .low });
    const high = try h.addDownload("high", fb, .{ .priority = .high });
    _ = try h.startRunner();

    _ = try h.waitState(low, .fetched);
    _ = try h.waitState(high, .fetched);

    try testing.expect(h.rec.firstIndex(.fetched, high, null).? < h.rec.firstIndex(.download_updated, low, .fetching).?);
}

test "an NZB that does not parse fails with source_invalid" {
    const h = try Harness.create();
    defer h.destroy();
    _ = try h.addServer("main", 0, 1);
    const fx = try h.fixture(&.{.{ .name = "a.bin", .data = "x" }}, 10_000);
    const id = try h.addDownload("junk", fx, .{ .nzb_bytes = "this is not xml" });
    _ = try h.startRunner();

    const got = try h.waitState(id, .failed);

    try testing.expectEqual(dl.FailureReason.source_invalid, got.failure_reason.?);
    try testing.expect(std.mem.startsWith(u8, got.failure_detail, "parse nzb: nzb: "));
    try Harness.waitFor(*tu.Recorder, h.rec, struct {
        fn f(r: *tu.Recorder) bool {
            return r.count(.failed, "dl-1") > 0;
        }
    }.f, 5_000);
}

test "an unwritable download folder fails naming the folder and the user" {
    if (std.posix.system.geteuid() == 0) return error.SkipZigTest;
    const h = try Harness.create();
    defer h.destroy();
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 10_000, 1) }}, 5_000);
    const srv = try h.addServer("main", 0, 1);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{ .make_dir_readonly = true });
    defer Io.Dir.cwd().setFilePermissions(io, h.dirOf(id), .fromMode(0o755), .{}) catch {};
    _ = try h.startRunner();

    const got = try h.waitState(id, .failed);

    try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
    try testing.expect(std.mem.indexOf(u8, got.failure_detail, h.dirOf(id)) != null);
    const uid = try std.fmt.allocPrint(h.a(), "uid {d}", .{std.posix.system.geteuid()});
    try testing.expect(std.mem.indexOf(u8, got.failure_detail, uid) != null);
}

test "an NZB with only recovery volumes fails with source_invalid" {
    const h = try Harness.create();
    defer h.destroy();
    _ = try h.addServer("main", 0, 1);
    const fx = try h.fixture(&.{.{ .name = "movie.vol000+02.par2", .data = try tu.payload(h.a(), 5_000, 1) }}, 10_000);
    const id = try h.addDownload("only-par2", fx, .{});
    _ = try h.startRunner();

    const got = try h.waitState(id, .failed);

    try testing.expectEqual(dl.FailureReason.source_invalid, got.failure_reason.?);
    try testing.expectEqualStrings("the NZB has nothing to fetch", got.failure_detail);
}

test "recovery volumes are deferred but the index par2 is fetched" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try h.fixture(&.{
        .{ .name = "movie.mkv", .data = try tu.payload(h.a(), 40_000, 1) },
        .{ .name = "movie.par2", .data = try tu.payload(h.a(), 3_000, 2) },
        .{ .name = "movie.vol000+02.par2", .data = try tu.payload(h.a(), 20_000, 3) },
        .{ .name = "movie.vol002+04.par2", .data = try tu.payload(h.a(), 30_000, 4) },
    }, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    _ = try h.startRunner();

    _ = try h.waitState(id, .fetched);

    for (fx.files) |f| {
        const wanted = std.mem.eql(u8, f.name, "movie.mkv") or std.mem.eql(u8, f.name, "movie.par2");
        for (f.message_ids) |mid| try testing.expectEqual(@as(usize, if (wanted) 1 else 0), srv.requests(mid));
    }
    var want: [2]nntptest.FixtureFile = undefined;
    var n: usize = 0;
    for (fx.files) |f| {
        if (std.mem.eql(u8, f.name, "movie.mkv") or std.mem.eql(u8, f.name, "movie.par2")) {
            want[n] = f;
            n += 1;
        }
    }
    try h.expectFiles(id, &want);
}

test "a missing article of the main server is filled by the fill server" {
    const h = try Harness.create();
    defer h.destroy();
    const main = try h.addServer("main", 0, 2);
    const fill = try h.addServer("fill", 1, 2);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 200_000, 1) }}, 10_000); // 20 articles
    try main.installFixture(fx);
    try fill.installFixture(fx);
    const lost = fx.files[0].message_ids[7];
    try main.missing(&.{lost});
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();

    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);

    try h.expectFiles(id, fx.files);
    const detail = (try r.fetchDetailOf(id)).?;
    try testing.expectEqual(@as(i64, 0), detail.missing_articles + detail.damaged_articles + detail.missing_files);
    try testing.expectEqual(@as(usize, 1), fill.totalRequests());
    try testing.expectEqual(@as(usize, 1), fill.requests(lost));
}

test "articles missing everywhere still reach fetched with a report" {
    const h = try Harness.create();
    defer h.destroy();
    const main = try h.addServer("main", 0, 2);
    const fill = try h.addServer("fill", 1, 2);
    const movie = try tu.payload(h.a(), 100_000, 1);
    const extra = try tu.payload(h.a(), 20_000, 2);
    const fx = try h.fixture(&.{ .{ .name = "movie.mkv", .data = movie }, .{ .name = "extra.bin", .data = extra } }, 10_000);
    try main.installFixture(fx);
    try fill.installFixture(fx);
    // One article of movie.mkv and every article of extra.bin are gone.
    var gone: std.ArrayList([]const u8) = .empty;
    for (fx.files) |f| {
        if (std.mem.eql(u8, f.name, "movie.mkv")) try gone.append(h.a(), f.message_ids[3]) else try gone.appendSlice(h.a(), f.message_ids);
    }
    try main.missing(gone.items);
    try fill.missing(gone.items);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();

    _ = try h.waitState(id, .fetched);
    try waitFetchedEvent(h, id);

    const detail = (try r.fetchDetailOf(id)).?;
    try testing.expectEqual(@as(i64, 3), detail.missing_articles);
    try testing.expectEqual(@as(i64, 1), detail.missing_files); // extra.bin never got created
    try testing.expect(detail.missing_bytes > 0);
    try testing.expectEqual(@as(i64, 3), (try r.fetchReport(id)).?.missing_articles);
    var dir = try h.readDir(id);
    try testing.expectEqual(@as(usize, 1), dir.count());
    const got = dir.get("movie.mkv").?;
    try testing.expectEqual(movie.len, got.len);
    try testing.expectEqualSlices(u8, movie[0..30_000], got[0..30_000]);
    try testing.expectEqualSlices(u8, movie[40_000..], got[40_000..]);
    try testing.expect(std.mem.allEqual(u8, got[30_000..40_000], 0)); // the lost range stays sparse
}

fn totalOf(srv: *nntptest.Server) usize {
    return srv.totalRequests();
}

fn requestsAtLeast(srv: *nntptest.Server, n: usize) !void {
    var waited: i64 = 0;
    while (waited < 15_000) : (waited += 1) {
        if (srv.totalRequests() >= n) return;
        tu.sleepMs(1);
    }
    return error.Timeout;
}

test "pausing mid download stops fetching and resuming continues without refetching" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000); // 40 articles
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    _ = try h.startRunner();
    try requestsAtLeast(srv, 8);

    try h.setPaused(id, true);

    _ = try h.waitState(id, .queued);
    const at_pause = try Harness.stableRequests(srv);
    try testing.expect(at_pause < 40); // fetching stopped

    try h.setPaused(id, false);

    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
    var refetched: usize = 0;
    for (fx.files[0].message_ids) |mid| {
        if (srv.requests(mid) > 1) refetched += 1;
    }
    try testing.expect(refetched <= 1); // only the article in flight at the pause may be fetched twice
}

test "removing a Download midway cancels its work and frees the slot" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const big = try h.fixture(&.{.{ .name = "big.bin", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    const small = try h.fixture(&.{.{ .name = "small.bin", .data = try tu.payload(h.a(), 20_000, 2) }}, 10_000);
    try srv.installFixture(big);
    try srv.installFixture(small);
    const a = try h.addDownload("big", big, .{});
    _ = try h.startRunner();
    try requestsAtLeast(srv, 5);

    try h.remove(a);

    const at_remove = try Harness.stableRequests(srv);
    try testing.expect(at_remove < 40); // fetching stopped
    const b = try h.addDownload("small", small, .{});
    _ = try h.waitState(b, .fetched);
}

test "a Download goes back to queued when every Server rejects the login" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    srv.failAuth(true);
    h.opts.failure_backoff_ms = 300;
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 50_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();

    var waited: i64 = 0;
    while (srv.authAttempts() < 1 and waited < 10_000) : (waited += 1) tu.sleepMs(1);
    tu.sleepMs(100);
    try testing.expectEqual(dl.State.queued, h.stateOf(id).?); // not marked fetched with everything missing
    try testing.expectEqual(null, try r.fetchDetailOf(id));
    try testing.expectEqual(@as(usize, 0), srv.totalRequests());
    try testing.expectEqual(@as(usize, 0), h.rec.count(.failed, id));

    srv.failAuth(false); // the password was corrected
    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
}

test "the disk guard pauses a running Download and resumes it later" {
    const h = try Harness.create();
    defer h.destroy();
    h.opts.disk_check_interval_ms = 20;
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{ .total_bytes = 400_000 });
    const r = try h.startRunner();
    try requestsAtLeast(srv, 5);

    h.setFree(1 << 20);

    _ = try h.waitState(id, .queued);
    const info = (try r.queueOf().paused(h.a())).?;
    try testing.expectEqualStrings(queue.reason_disk_full, info.reason);
    const at_pause = try Harness.stableRequests(srv);
    try testing.expect(at_pause < 40);

    h.setFree(10 << 30);

    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
}

test "pausing the Queue stops the fetch and resuming continues it" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 300_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    try requestsAtLeast(srv, 5);

    try r.queueOf().pause(queue.reason_user, "");

    _ = try h.waitState(id, .queued);
    _ = try Harness.stableRequests(srv);
    try testing.expectEqual(dl.State.queued, h.stateOf(id).?);

    r.queueOf().unpause(queue.reason_user);

    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
}

fn progressOf(h: *Harness, id: []const u8) ![]const tu.Seen {
    var out: std.ArrayList(tu.Seen) = .empty;
    for (try h.rec.all(h.a())) |s| {
        if (s.kind == .download_progress and std.mem.eql(u8, s.id, id)) try out.append(h.a(), s);
    }
    return out.items;
}

test "progress events follow the throttle" {
    var frozen_count: usize = 0;
    var moving: []const tu.Seen = &.{};
    var moving_h: ?*Harness = null;
    defer if (moving_h) |m| m.destroy();
    for ([_]i64{ 0, 2000 }) |step| {
        const h = try Harness.create();
        var keep = false;
        defer if (!keep) h.destroy();
        var clk: tu.FakeClock = .{ .step_ms = step };
        h.opts.clock = clk.clock();
        const srv = try h.addServer("main", 0, 2);
        const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000); // 40 articles
        try srv.installFixture(fx);
        const id = try h.addDownload("movie", fx, .{ .total_bytes = 400_000 });
        _ = try h.startRunner();
        _ = try h.waitState(id, .fetched);
        try waitFetchedEvent(h, id);
        h.stopRunner();
        const seen = try progressOf(h, id);
        if (step == 0) {
            frozen_count = seen.len;
        } else {
            moving = seen;
            moving_h = h;
            keep = true;
        }
    }
    try testing.expect(frozen_count <= 2); // a frozen clock passes the first report and the final one
    try testing.expect(moving.len > 10); // reports pass once the interval has elapsed
    try testing.expectEqual(@as(i64, 400_000), moving[moving.len - 1].done);
    try testing.expectEqual(@as(i64, 400_000), moving[moving.len - 1].total);
    for (moving[1..], moving[0 .. moving.len - 1]) |cur, prev| try testing.expect(cur.done >= prev.done);
}

test "done bytes are stored while fetching" {
    const h = try Harness.create();
    defer h.destroy();
    h.opts.done_bytes_interval_ms = 20;
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{ .total_bytes = 400_000 });
    _ = try h.startRunner();

    var seen_partial = false;
    var waited: i64 = 0;
    while (waited < 10_000 and !seen_partial) : (waited += 5) {
        const d = try h.get(id);
        seen_partial = d.state == .fetching and d.done_bytes > 0 and d.done_bytes < 400_000;
        tu.sleepMs(5);
    }
    try testing.expect(seen_partial);

    try testing.expectEqual(@as(i64, 400_000), (try h.waitState(id, .fetched)).done_bytes);
}

fn wireBytes(fx: *const nntptest.Fixture) i64 {
    var n: i64 = 0;
    for (fx.articles) |a| {
        for (a.lines) |l| n += @intCast(l.len + 2);
    }
    return n;
}

test "the speed limit holds the rate" {
    const limit: i64 = 400_000; // bytes per second on the wire
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 4);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 300_000, 1) }}, 20_000);
    try srv.installFixture(fx);
    const wire = wireBytes(fx);
    const id = try h.addDownload("movie", fx, .{ .total_bytes = wire });
    const r = try h.startRunner();
    r.setSpeedLimit(limit);
    const t0 = Io.Clock.now(.awake, io);

    _ = try h.waitState(id, .fetched);

    const ms = t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds();
    const rate = @divTrunc(wire * 1000, @max(ms, 1));
    // Never faster than the limit (the first line is free), and not far
    // under it: the setup of the connections and the polls cost a little.
    try testing.expect(rate <= limit + @divTrunc(limit, 10));
    try testing.expect(rate >= @divTrunc(limit * 6, 10));
    try h.expectFiles(id, fx.files);
}

test "the speed limit can be removed at runtime" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 2);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 200_000, 1) }}, 20_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    const r = try h.startRunner();
    r.setSpeedLimit(1_000); // would take minutes
    r.setSpeedLimit(0);
    const t0 = Io.Clock.now(.awake, io);

    _ = try h.waitState(id, .fetched);

    try testing.expect(t0.durationTo(Io.Clock.now(.awake, io)).toMilliseconds() < 5_000);
}

test "a Server added while fetching joins the running Download" {
    const h = try Harness.create();
    defer h.destroy();
    const main = try h.addServer("main", 0, 1);
    main.setLatencyMs(8);
    const fx = try h.fixture(&.{.{ .name = "movie.mkv", .data = try tu.payload(h.a(), 500_000, 1) }}, 10_000);
    try main.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    _ = try h.startRunner();
    try requestsAtLeast(main, 5);

    const extra = try h.addServer("extra", 0, 4);
    try extra.installFixture(fx);
    h.bus.publish(.servers_changed);

    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
    try testing.expect(extra.totalRequests() > 0); // the new Server took part
}

test "no enabled Server leaves Downloads queued" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    const fx = try h.fixture(&.{.{ .name = "a.bin", .data = try tu.payload(h.a(), 5_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    h.setServerEnabled(0, false);
    const id = try h.addDownload("a", fx, .{});
    _ = try h.startRunner();

    tu.sleepMs(300);
    try testing.expectEqual(dl.State.queued, h.stateOf(id).?);

    h.setServerEnabled(0, true);
    h.bus.publish(.servers_changed);
    _ = try h.waitState(id, .fetched);
}

test "three Downloads over three Servers, each lacking different articles" {
    const h = try Harness.create();
    defer h.destroy();
    h.setLimit(3);
    const servers = [_]*nntptest.Server{ try h.addServer("a", 0, 3), try h.addServer("b", 0, 3), try h.addServer("fill", 1, 4) };
    for (servers) |s| s.setLatencyMs(1);
    var ids: [3][]const u8 = undefined;
    var fxs: [3]*nntptest.Fixture = undefined;
    for (0..3) |i| {
        const mkv = try std.fmt.allocPrint(h.a(), "movie{d}.mkv", .{i});
        const nfo = try std.fmt.allocPrint(h.a(), "movie{d}.nfo", .{i});
        const fx = try h.fixture(&.{
            .{ .name = mkv, .data = try tu.payload(h.a(), 300_000 + i * 1000, @intCast(10 + i)) },
            .{ .name = nfo, .data = try tu.payload(h.a(), 3_000, @intCast(20 + i)) },
        }, 7_000);
        for (servers) |s| try s.installFixture(fx);
        // The two main servers each lack a different subset; fill has all.
        const all = try fx.messageIds(h.a());
        for (all, 0..) |mid, j| switch (j % 5) {
            0 => try servers[0].missing(&.{mid}),
            1 => try servers[1].missing(&.{mid}),
            2 => {
                try servers[0].missing(&.{mid});
                try servers[1].missing(&.{mid});
            },
            else => {},
        };
        fxs[i] = fx;
        ids[i] = try h.addDownload(try std.fmt.allocPrint(h.a(), "dl{d}", .{i}), fx, .{});
    }
    const r = try h.startRunner();

    for (ids, fxs) |id, fx| {
        _ = try h.waitState(id, .fetched);
        try waitFetchedEvent(h, id);
        const detail = (try r.fetchDetailOf(id)).?;
        try testing.expectEqual(@as(i64, 0), detail.missing_articles + detail.damaged_articles + detail.missing_files);
        try h.expectFiles(id, fx.files);
    }
}

test "a hold stops the fetch and keeps the Download from restarting until released" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const fx = try h.fixture(&.{.{ .name = "big.bin", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("big", fx, .{});
    const r = try h.startRunner();
    try requestsAtLeast(srv, 5);

    try r.queueOf().hold(id);

    const at_hold = srv.totalRequests();
    tu.sleepMs(200); // several control-loop passes
    try testing.expectEqual(at_hold, srv.totalRequests()); // no request after hold returned
    try testing.expect(h.stateOf(id).? != .fetched);
    try testing.expect(!h.mem.hasBlob(id)); // a removal takes the resume state with it

    _ = r.queueOf().release(id);
    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
}

test "the removed event of a held Download ends its hold" {
    const h = try Harness.create();
    defer h.destroy();
    h.opts.poll_interval_ms = 3_600_000; // only the event can end the hold
    const srv = try h.addServer("main", 0, 1);
    const fx = try h.fixture(&.{.{ .name = "a.bin", .data = try tu.payload(h.a(), 1_000, 1) }}, 10_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("a", fx, .{});
    const r = try h.startRunner();
    _ = try h.waitState(id, .fetched); // the events task is subscribed

    try r.queueOf().hold("other");
    try testing.expect(r.queueOf().held("other"));
    h.bus.publish(.{ .removed = "other" });

    var waited: i64 = 0;
    while (r.queueOf().held("other") and waited < 10_000) : (waited += 1) tu.sleepMs(1);
    try testing.expect(!r.queueOf().held("other"));
}

test "a higher priority Download takes the place of the running one, which continues where it was" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 1);
    srv.setLatencyMs(5);
    const low_fx = try h.fixture(&.{.{ .name = "low.bin", .data = try tu.payload(h.a(), 400_000, 1) }}, 10_000);
    const high_fx = try h.fixture(&.{.{ .name = "high.bin", .data = try tu.payload(h.a(), 30_000, 2) }}, 10_000);
    try srv.installFixture(low_fx);
    try srv.installFixture(high_fx);
    const low = try h.addDownload("low", low_fx, .{});
    _ = try h.startRunner();
    try requestsAtLeast(srv, 6);

    const high = try h.addDownload("high", high_fx, .{ .priority = .high });

    _ = try h.waitState(high, .fetched);
    _ = try h.waitState(low, .fetched);
    try waitFetchedEvent(h, low);
    try h.expectFiles(low, low_fx.files);
    try h.expectFiles(high, high_fx.files);
    const states = try h.rec.states(h.a(), low);
    try testing.expectEqualSlices(dl.State, &.{ .queued, .fetching, .queued, .fetching, .fetched }, states);
    // The pre-empted Download continued where it was.
    try testing.expect(srv.totalRequests() <= low_fx.articles.len + high_fx.articles.len + 2);
}

test "pausing and resuming at random moments never loses or hangs a Download" {
    const h = try Harness.create();
    defer h.destroy();
    const srv = try h.addServer("main", 0, 3);
    srv.setLatencyMs(2);
    const fx = try h.fixture(&.{
        .{ .name = "a.bin", .data = try tu.payload(h.a(), 500_000, 1) },
        .{ .name = "b.bin", .data = try tu.payload(h.a(), 100_000, 2) },
    }, 5_000);
    try srv.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    _ = try h.startRunner();
    var prng: std.Random.DefaultPrng = .init(7);
    const rnd = prng.random();

    for (0..40) |_| {
        tu.sleepMs(rnd.intRangeAtMost(i64, 0, 12));
        try h.setPaused(id, true);
        tu.sleepMs(rnd.intRangeAtMost(i64, 0, 12));
        try h.setPaused(id, false);
    }

    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
    try testing.expectEqual(@as(usize, 1), h.rec.count(.fetched, id));
}

test "the Servers changing again and again during a fetch lose nothing" {
    const h = try Harness.create();
    defer h.destroy();
    const a = try h.addServer("a", 0, 2);
    a.setLatencyMs(2);
    const b = try h.addServer("b", 1, 2);
    b.setLatencyMs(2);
    const fx = try h.fixture(&.{.{ .name = "a.bin", .data = try tu.payload(h.a(), 600_000, 1) }}, 5_000);
    try a.installFixture(fx);
    try b.installFixture(fx);
    const id = try h.addDownload("movie", fx, .{});
    _ = try h.startRunner();
    var prng: std.Random.DefaultPrng = .init(9);
    const rnd = prng.random();

    for (0..30) |_| {
        tu.sleepMs(rnd.intRangeAtMost(i64, 0, 15));
        h.setServerEnabled(1, rnd.boolean());
        h.bus.publish(.servers_changed);
    }
    h.setServerEnabled(1, true);
    h.bus.publish(.servers_changed);

    _ = try h.waitState(id, .fetched);
    try h.expectFiles(id, fx.files);
}
