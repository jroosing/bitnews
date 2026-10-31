//! Tests of the concurrency limit, recovery and the background loop of the
//! post-processor (internal/core/postprocess_run_test.go).
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const dl = @import("download.zig");
const pp_mod = @import("postprocess.zig");
const fk = @import("postprocess_fakes.zig");

const Harness = fk.Harness;
const io = fk.io;
const PostProcessor = pp_mod.PostProcessor;

fn waitForCount(g: *fk.Gate, n: usize) !void {
    try g.waitEntered(n);
}

test "process enforces the concurrency limit" {
    var h: Harness = undefined;
    try h.init(.{ .concurrency = 2 });
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    var ids: [5][]const u8 = undefined;
    for (&ids, [_][]const u8{ "a", "b", "c", "d", "e" }) |*id, name| id.* = try h.add(name, .fetched, .nzb);

    var group: Io.Group = .init;
    defer group.cancel(io);
    for (ids) |id| group.concurrent(io, processOne, .{ h.pp, id }) catch return error.SkipZigTest;
    try gate.waitEntered(2);
    try gate.expectNoMoreThan(2);
    var waiting: usize = 0;
    for (ids) |id| {
        if (h.stateOf(id).? == .fetched) waiting += 1;
    }
    try testing.expectEqual(@as(usize, 3), waiting); // Downloads waiting for a slot stay fetched

    gate.release();
    try group.await(io);

    try testing.expectEqual(@as(usize, 2), gate.maxSeen());
    for (ids) |id| try testing.expectEqual(dl.State.ready, h.stateOf(id).?);
}

fn processOne(pp: *PostProcessor, id: []const u8) void {
    pp.process(id) catch |err| std.debug.print("process {s}: {s}\n", .{ id, @errorName(err) });
}

test "process waiting for a slot stops when cancelled" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    const first = try h.add("a", .fetched, .nzb);
    const second = try h.add("b", .fetched, .nzb);
    var group: Io.Group = .init;
    defer group.cancel(io);
    group.concurrent(io, processOne, .{ h.pp, first }) catch return error.SkipZigTest;
    try gate.waitEntered(1);
    var fut = io.concurrent(PostProcessor.process, .{ h.pp, second }) catch return error.SkipZigTest;
    fk.sleepMs(100); // let it reach the wait for a slot

    try testing.expectError(error.Canceled, fut.cancel(io));

    try testing.expectEqual(dl.State.fetched, h.stateOf(second).?);
    gate.release();
}

test "process refuses a download that is already being processed" {
    var h: Harness = undefined;
    try h.init(.{ .concurrency = 2 });
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    const id = try h.add("a", .fetched, .nzb);
    var group: Io.Group = .init;
    defer group.cancel(io);
    group.concurrent(io, processOne, .{ h.pp, id }) catch return error.SkipZigTest;
    try gate.waitEntered(1);

    try testing.expectError(error.AlreadyPostProcessing, h.pp.process(id));

    gate.release();
}

test "recover reprocesses stuck downloads from the first stage" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.unpack.found = true;
    const stuck = try h.add("stuck", .post_processing, .nzb);
    const fetched = try h.add("fetched", .fetched, .nzb);
    const ready = try h.add("ready", .ready, .nzb);

    try h.pp.recover();

    try testing.expectEqual(dl.State.ready, h.stateOf(stuck).?);
    try h.log.expect(&.{ "repair", "detect", "unpack", "finish" }); // from verifying, once
    try testing.expectEqual(dl.State.fetched, h.stateOf(fetched).?); // recover only touches post_processing
    try testing.expectEqual(dl.State.ready, h.stateOf(ready).?);
    // No second fetched -> post_processing for the stuck Download.
    for (try h.events()) |r| try testing.expect(!std.mem.eql(u8, r.text, "download_updated post_processing"));
}

test "a restart in the middle of a stage re-enters at verifying" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.unpack.found = true;
    h.unpack.stall_first = true; // the daemon stops during the first unpack
    const id = try h.add("x", .fetched, .nzb);

    var fut = io.concurrent(PostProcessor.process, .{ h.pp, id }) catch return error.SkipZigTest;
    var waited: i64 = 0;
    while (h.unpack.calls() == 0 and waited < 5000) : (waited += 2) fk.sleepMs(2);
    try testing.expectError(error.Canceled, fut.cancel(io));
    try testing.expectEqual(dl.State.post_processing, h.stateOf(id).?);

    try h.pp.recover();

    try testing.expectEqual(dl.State.ready, h.stateOf(id).?);
    try h.log.expect(&.{ "repair", "detect", "unpack", "repair", "detect", "unpack", "finish" });
}

test "recover honours the concurrency limit" {
    var h: Harness = undefined;
    try h.init(.{ .concurrency = 2 });
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    for ([_][]const u8{ "a", "b", "c", "d" }) |name| _ = try h.add(name, .post_processing, .nzb);
    var fut = io.concurrent(PostProcessor.recover, .{h.pp}) catch return error.SkipZigTest;

    try gate.waitEntered(2);
    try gate.expectNoMoreThan(2);
    gate.release();

    try fut.await(io);
    try testing.expectEqual(@as(usize, 2), gate.maxSeen());
}

test "recover starts nothing once the post-processor is stopping" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    _ = try h.add("a", .post_processing, .nzb);
    h.pp.stopping.store(true, .release);

    try testing.expectError(error.Canceled, h.pp.recover());

    try h.log.expect(&.{});
}

test "run processes fetched downloads and those that become fetched later" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    const early = try h.add("early", .fetched, .nzb);
    const stuck = try h.add("stuck", .post_processing, .nzb);
    try h.pp.start();

    try h.eventually(early, .ready);
    try h.eventually(stuck, .ready);
    const late = try h.add("late", .fetched, .nzb);
    h.bus.publish(.{ .fetched = late });
    try h.eventually(late, .ready);

    h.pp.stop();
}

test "run never exceeds the concurrency limit and drains the backlog" {
    var h: Harness = undefined;
    try h.init(.{ .concurrency = 2 });
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    var ids: [5][]const u8 = undefined;
    for (&ids, [_][]const u8{ "a", "b", "c", "d", "e" }) |*id, name| id.* = try h.add(name, .fetched, .nzb);
    try h.pp.start();

    try gate.waitEntered(2);
    try gate.expectNoMoreThan(2);
    gate.release();

    for (ids) |id| try h.eventually(id, .ready);
    try testing.expectEqual(@as(usize, 2), gate.maxSeen());
    h.pp.stop();
}

test "run polls for fetched downloads it was not told about" {
    var h: Harness = undefined;
    try h.init(.{ .poll_ms = 10 });
    defer h.deinit();
    try h.pp.start();
    const id = try h.add("silent", .fetched, .nzb); // no fetched event

    try h.eventually(id, .ready);

    h.pp.stop();
}

test "run reacts to a fetched event at once, without waiting for the poll" {
    var h: Harness = undefined;
    try h.init(.{ .poll_ms = 3_600_000 });
    defer h.deinit();
    try h.pp.start();
    fk.sleepMs(50); // the first pass has looked and found nothing
    const id = try h.add("x", .fetched, .nzb);

    h.bus.publish(.{ .fetched = id });

    try h.eventually(id, .ready);
    h.pp.stop();
}

test "run stops running downloads on stop and leaves them post_processing" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    const id = try h.add("a", .fetched, .nzb);
    try h.pp.start();
    try gate.waitEntered(1);

    h.pp.stop();

    try testing.expectEqual(dl.State.post_processing, h.stateOf(id).?);
    for (try h.events()) |r| try testing.expect(r.kind != .ready and r.kind != .failed);
}

test "a download handed back for recovery volumes is processed again when fetched again" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    h.repair.diag = .{ .needed = 4, .available = 1 };
    h.deferred.more = true;
    const id = try h.add("x", .fetched, .nzb);
    try h.pp.start();
    try h.eventually(id, .post_processing);
    while (h.log.count("deferred") == 0) fk.sleepMs(2);

    // The fetch side moves the Download back to fetching and, when the
    // volumes are in, to fetched again, announcing it.
    h.repair.err = null;
    _ = try h.rig.repo.updateState(h.rig.arena(), id, .{ .to = .fetching, .at = 5 });
    _ = try h.rig.repo.updateState(h.rig.arena(), id, .{ .to = .fetched, .at = 6 });
    h.bus.publish(.{ .fetched = id });

    try h.eventually(id, .ready);
    try testing.expectEqual(@as(usize, 2), h.log.count("repair"));
    h.pp.stop();
}

test "recover after a crash between finish and ready records ready" {
    // The first run finished (the working directory was moved away) and died
    // before the ready update. Finish is idempotent: it finds the moved
    // directory again.
    var h: Harness = undefined;
    try h.init(.{ .fs = true });
    defer h.deinit();
    h.fs.exists = false;
    const id = try h.add("x", .post_processing, .nzb);

    try h.pp.recover();

    const got = try h.get(id);
    try testing.expectEqual(dl.State.ready, got.state);
    try testing.expectEqualStrings("/complete/x", got.output_path);
    try h.log.expect(&.{"finish"});
}
