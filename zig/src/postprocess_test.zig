//! Tests of the post-processing pipeline (internal/core/postprocess_test.go),
//! through `PostProcessor.process`, with fake ports and a real database.
const std = @import("std");
const testing = std.testing;
const dl = @import("download.zig");
const ports = @import("postprocess_ports.zig");
const pp_mod = @import("postprocess.zig");
const fk = @import("postprocess_fakes.zig");

const Harness = fk.Harness;
const io = fk.io;

fn rep(comptime s: []const u8, comptime n: usize) [s.len * n]u8 {
    var out: [s.len * n]u8 = undefined;
    for (0..n) |i| @memcpy(out[i * s.len ..][0..s.len], s);
    return out;
}

fn expectTexts(got: []const fk.Rec, want: []const []const u8) !void {
    errdefer for (got) |r| std.debug.print("event: {s}\n", .{r.text});
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, r| try testing.expectEqualStrings(w, r.text);
}

/// Processes the Download and expects it to reach `state`.
fn processTo(h: *Harness, id: []const u8, state: dl.State) !dl.Download {
    try h.pp.process(id);
    const got = try h.get(id);
    try testing.expectEqual(state, got.state);
    return got;
}

test "a clean usenet download becomes ready" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    const id = try h.add("Show.S01E01", .fetched, .nzb);

    const got = try processTo(&h, id, .ready);

    try testing.expectEqualStrings("/complete/Show.S01E01", got.output_path);
    try h.log.expect(&.{ "repair", "detect", "finish" });
}

test "events are download_updated, the stages, then ready" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.unpack.found = true;
    h.repair.outcome = .{ .has_par2 = true, .repaired_slices = 3 };
    const id = try h.add("x", .fetched, .nzb);

    try h.pp.process(id);

    const evs = try h.events();
    try expectTexts(evs, &.{
        "download_updated post_processing", "stage verifying",        "stage unpacking",
        "stage finishing",                  "download_updated ready", "ready /complete/x",
    });
    for (evs) |r| try testing.expectEqualStrings(id, r.id);
    try testing.expectEqual(@import("event.zig").Kind.postprocess_stage, evs[1].kind);
}

test "a damaged download is repaired, then ready" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.outcome = .{ .has_par2 = true, .repaired_slices = 3 };
    const id = try h.add("x", .fetched, .nzb);

    _ = try processTo(&h, id, .ready);

    try h.log.expect(&.{ "repair", "detect", "finish" });
}

test "a failed repair fails the download and publishes failed" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.RepairFailed;
    h.repair.diag = .{ .message = "repair failed: solve" };
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.repair_insufficient, got.failure_reason.?);
    try testing.expectEqualStrings("repairing: repair failed: solve", got.failure_detail);
    try testing.expectEqual(@as(usize, 0), h.log.count("finish"));
    const evs = try h.events();
    try testing.expect(evs.len >= 2);
    try testing.expectEqualStrings("download_updated failed", evs[evs.len - 2].text);
    const want = try std.fmt.allocPrint(testing.allocator, "failed repair_insufficient {s}", .{got.failure_detail});
    defer testing.allocator.free(want);
    try testing.expectEqualStrings(want, evs[evs.len - 1].text);
}

test "short of recovery asks for deferred volumes and hands off" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    h.repair.diag = .{ .needed = 12, .available = 5 };
    h.deferred.more = true;
    const id = try h.add("x", .fetched, .nzb);

    // The fetch side moves the Download back to fetching; here the fake does
    // not, so it stays in post_processing, and nothing failed.
    _ = try processTo(&h, id, .post_processing);

    try h.log.expect(&.{ "repair", "deferred" });
    try testing.expectEqualSlices(i64, &.{7}, h.deferred.min_blocks.items); // 12 damaged - 5 on disk
    try testing.expect(!try h.sawKind(.failed));
    try testing.expect(!try h.sawKind(.ready));
}

test "short of recovery with no more volumes fails repair_insufficient" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    h.repair.diag = .{ .needed = 12, .available = 5 };
    h.deferred.more = false;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.repair_insufficient, got.failure_reason.?);
    try testing.expectEqualStrings("repairing: not enough recovery data: need 12 blocks, have 5", got.failure_detail);
    try h.log.expect(&.{ "repair", "deferred" });
}

test "a shortfall that needs fewer blocks than are on disk still asks for one" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    h.repair.diag = .{ .needed = 3, .available = 9 };
    h.deferred.more = true;
    const id = try h.add("x", .fetched, .nzb);

    _ = try processTo(&h, id, .post_processing);

    try testing.expectEqualSlices(i64, &.{1}, h.deferred.min_blocks.items);
}

test "an error of the deferred volumes port fails internal" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    h.repair.diag = .{ .needed = 12, .available = 5 };
    h.deferred.err = error.Failed;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
}

test "short of recovery without the deferred port is final" {
    var h: Harness = undefined;
    try h.init(.{ .deferred = false });
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    h.repair.diag = .{ .needed = 12, .available = 5 };
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.repair_insufficient, got.failure_reason.?);
}

test "a shortfall without counts is a plain verifying failure, as Go's bare sentinel" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.InsufficientRecovery;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqualStrings("verifying: not enough recovery data", got.failure_detail);
    try h.log.expect(&.{"repair"});
}

test "no PAR2 set with missing or damaged articles fails missing_articles" {
    const reports = [_]ports.FetchReport{ .{ .missing_articles = 2 }, .{ .damaged_articles = 1 } };
    const details = [_][]const u8{
        "verifying: missing articles: 2 missing, 0 damaged and no PAR2 set",
        "verifying: missing articles: 0 missing, 1 damaged and no PAR2 set",
    };
    for (reports, details) |report, want| {
        var h: Harness = undefined;
        try h.init(.{});
        defer h.deinit();
        h.repair.outcome = .{ .has_par2 = false };
        h.reporter.report_value = report;
        const id = try h.add("x", .fetched, .nzb);

        const got = try processTo(&h, id, .failed);

        try testing.expectEqual(dl.FailureReason.missing_articles, got.failure_reason.?);
        try testing.expectEqualStrings(want, got.failure_detail);
        try h.log.expect(&.{ "repair", "fetchreport" });
    }
}

test "no PAR2 set and a clean fetch continues to ready" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.outcome = .{ .has_par2 = false };
    const id = try h.add("x", .fetched, .nzb);

    _ = try processTo(&h, id, .ready);

    try h.log.expect(&.{ "repair", "fetchreport", "detect", "finish" });
}

test "a clean PAR2 set ignores the fetch report" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.reporter.report_value = .{ .missing_articles = 5 }; // PAR2 says the files are fine
    const id = try h.add("x", .fetched, .nzb);

    _ = try processTo(&h, id, .ready);

    try testing.expectEqual(@as(usize, 0), h.log.count("fetchreport"));
}

test "an error of the fetch report fails internal" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.outcome = .{ .has_par2 = false };
    h.reporter.err = error.Failed;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
}

test "unpack runs only when archives are detected" {
    {
        var h: Harness = undefined;
        try h.init(.{});
        defer h.deinit();
        const id = try h.add("x", .fetched, .nzb);

        _ = try processTo(&h, id, .ready);

        try h.log.expect(&.{ "repair", "detect", "finish" });
        try testing.expectEqualStrings("", h.finisher.got_unpacked.get());
        try testing.expectEqualStrings("/work/x", h.finisher.got_dir.get());
    }
    {
        var h: Harness = undefined;
        try h.init(.{});
        defer h.deinit();
        h.unpack.found = true;
        const id = try h.add("x", .fetched, .nzb);

        _ = try processTo(&h, id, .ready);

        try h.log.expect(&.{ "repair", "detect", "unpack", "finish" });
        try testing.expectEqualStrings(id, h.unpack.got_id.get());
        try testing.expectEqualStrings("/work/x", h.unpack.got_dir.get());
        try testing.expectEqualStrings("/work/x/_unpack", h.unpack.got_dst.get());
        try testing.expectEqualStrings("/work/x/_unpack", h.finisher.got_unpacked.get());
        try testing.expectEqual(dl.SourceKind.nzb, h.finisher.gotKind());
    }
}

const Port = enum { repair, detect, unpack, finish };
const Class = struct { err: ports.PortError, reason: dl.FailureReason };
const classes = [_]Class{
    .{ .err = error.InsufficientRecovery, .reason = .repair_insufficient },
    .{ .err = error.RepairFailed, .reason = .repair_insufficient },
    .{ .err = error.MissingArticles, .reason = .missing_articles },
    .{ .err = error.ArchiveUnsupported, .reason = .archive_unsupported },
    .{ .err = error.ArchivePassword, .reason = .archive_password },
    .{ .err = error.ArchiveCorrupt, .reason = .archive_corrupt },
    .{ .err = error.PathViolation, .reason = .path_violation },
    .{ .err = error.DiskFull, .reason = .disk_full },
    .{ .err = error.Failed, .reason = .internal },
    .{ .err = error.OutOfMemory, .reason = .internal },
};

test "each error class maps to its failure reason, from every port that can return it" {
    for (std.enums.values(Port)) |port| {
        for (classes) |c| {
            var h: Harness = undefined;
            try h.init(.{});
            defer h.deinit();
            h.deferred.more = false;
            switch (port) {
                .repair => h.repair.err = c.err,
                .detect => h.unpack.detect_err = c.err,
                .unpack => {
                    h.unpack.found = true;
                    h.unpack.unpack_err = c.err;
                },
                .finish => h.finisher.err = c.err,
            }
            const id = try h.add("x", .fetched, .nzb);

            const got = try processTo(&h, id, .failed);

            testing.expectEqual(c.reason, got.failure_reason.?) catch |err| {
                std.debug.print("port {s}, class {s}\n", .{ @tagName(port), @errorName(c.err) });
                return err;
            };
        }
    }
}

test "a port that vouches for its text keeps its folder path" {
    if (!@hasField(ports.Diag, "vouched")) return error.SkipZigTest; // waits for Diag.vouched
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.unpack.found = true;
    h.unpack.unpack_err = error.Failed;
    h.unpack.diag = .{ .message = "cannot write to folder \"/srv/complete\": permission denied for uid 1000" };
    @field(h.unpack.diag, "vouched") = true;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqualStrings("cannot write to folder \"/srv/complete\": permission denied for uid 1000", got.failure_detail);
}

test "the detail of an unknown error is redacted and truncated" {
    {
        var h: Harness = undefined;
        try h.init(.{});
        defer h.deinit();
        h.unpack.found = true;
        h.unpack.unpack_err = error.Failed;
        h.unpack.diag = .{ .message = "open /srv/incomplete/Secret.Name/a.rar: permission denied; also \"/etc/passwd\" and C:\\Users\\me\\x.rar failed" };
        const id = try h.add("x", .fetched, .nzb);

        const got = try processTo(&h, id, .failed);

        try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
        try testing.expectEqualStrings("unpacking: open <dir>: permission denied; also \"<dir>\" and <dir> failed", got.failure_detail);
    }
    {
        var h: Harness = undefined;
        try h.init(.{});
        defer h.deinit();
        h.repair.err = error.Failed;
        h.repair.diag = .{ .message = &rep("é", 500) };
        const id = try h.add("y", .fetched, .nzb);

        const got = try processTo(&h, id, .failed);

        try testing.expect(try std.unicode.utf8CountCodepoints(got.failure_detail) <= 200);
        try testing.expect(std.unicode.utf8ValidateSlice(got.failure_detail));
    }
}

test "an empty output path from the finisher fails internal" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.finisher.out = "";
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
    try testing.expectEqualStrings("finishing: core: ready without output path: invalid state change", got.failure_detail);
}

test "a torrent without archives is ready via the finisher, without verify" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.repair.err = error.RepairFailed; // must never be consulted
    const id = try h.add("Movie", .fetched, .torrent);

    _ = try processTo(&h, id, .ready);

    try h.log.expect(&.{ "detect", "finish" });
    try testing.expectEqualStrings("", h.finisher.got_unpacked.get());
    try testing.expectEqual(dl.SourceKind.torrent, h.finisher.gotKind());
    for (try h.events()) |r| {
        try testing.expect(!std.mem.eql(u8, r.text, "stage verifying"));
        try testing.expect(!std.mem.eql(u8, r.text, "stage repairing"));
    }
}

test "a torrent with archives unpacks into the unpack dir and passes it to the finisher" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.unpack.found = true;
    const id = try h.add("Movie", .fetched, .torrent);

    _ = try processTo(&h, id, .ready);

    try h.log.expect(&.{ "detect", "unpack", "finish" });
    try testing.expectEqualStrings("/work/Movie", h.unpack.got_dir.get());
    try testing.expectEqualStrings("/work/Movie/_unpack", h.unpack.got_dst.get());
    try testing.expectEqualStrings("/work/Movie/_unpack", h.finisher.got_unpacked.get());
}

test "a torrent whose unpack fails is failed without touching its data or finishing" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.writeFile(io, .{ .sub_path = "movie.part01.rar", .data = "seed me" });
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(io, &buf);

    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.dirs.fixed = buf[0..n];
    h.unpack.found = true;
    h.unpack.unpack_err = error.ArchivePassword;
    const id = try h.add("Movie", .fetched, .torrent);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.archive_password, got.failure_reason.?);
    try testing.expectEqual(@as(usize, 0), h.log.count("finish"));
    var read: [16]u8 = undefined;
    const data = try tmp.dir.readFile(io, "movie.part01.rar", &read);
    try testing.expectEqualStrings("seed me", data);
    var it = (try tmp.dir.openDir(io, ".", .{ .iterate = true })).iterateAssumeFirstIteration();
    var entries: usize = 0;
    while (try it.next(io)) |_| entries += 1;
    try testing.expectEqual(@as(usize, 1), entries); // the orchestrator created and removed nothing
}

test "a download whose work is cancelled mid-stage stays post_processing with no outcome" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    h.unpack.found = true;
    h.unpack.stall_always = true;
    const id = try h.add("x", .fetched, .nzb);

    var fut = io.concurrent(pp_mod.PostProcessor.process, .{ h.pp, id }) catch return error.SkipZigTest;
    try waitFor(&h.unpack, calledUnpack);
    try testing.expectError(error.Canceled, fut.cancel(io));

    try testing.expectEqual(dl.State.post_processing, h.stateOf(id).?);
    try testing.expectEqual(@as(usize, 0), h.log.count("finish"));
    for (try h.events()) |r| {
        try testing.expect(r.kind != .ready and r.kind != .failed);
        if (r.kind == .download_updated) try testing.expectEqualStrings("download_updated post_processing", r.text);
    }
}

fn calledUnpack(u: *fk.Unpack) bool {
    return u.calls() > 0;
}

pub fn waitFor(ctx: anytype, comptime cond: fn (@TypeOf(ctx)) bool) !void {
    var waited: i64 = 0;
    while (waited < 5000) : (waited += 2) {
        if (cond(ctx)) return;
        fk.sleepMs(2);
    }
    return error.Timeout;
}

test "work stopped by stop() never records a verdict, whatever the port says on cancel" {
    var h: Harness = undefined;
    try h.init(.{ .poll_ms = 3_600_000 });
    defer h.deinit();
    h.unpack.found = true;
    h.unpack.stall_always = true;
    // Not recognisable as a cancellation: an ordinary failure.
    h.unpack.after_cancel = error.Failed;
    const id = try h.add("x", .fetched, .nzb);
    try h.pp.start();

    try waitFor(&h.unpack, calledUnpack);
    h.pp.stop();

    try testing.expectEqual(dl.State.post_processing, h.stateOf(id).?);
    for (try h.events()) |r| try testing.expect(r.kind != .ready and r.kind != .failed);
}

test "process rejects illegal states without running anything" {
    const states = [_]dl.State{ .added, .queued, .fetching, .post_processing, .ready, .failed };
    for (states) |state| {
        var h: Harness = undefined;
        try h.init(.{});
        defer h.deinit();
        const id = try h.add("x", state, .nzb);

        try testing.expectError(error.IllegalTransition, h.pp.process(id));

        try testing.expectEqual(state, h.stateOf(id).?);
        try h.log.expect(&.{});
        try testing.expectEqual(@as(usize, 0), (try h.events()).len);
    }
}

test "processing an unknown download is not found" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();

    try testing.expectError(error.NotFound, h.pp.process("nope"));
}

test "a download removed while it is processed ends with not found and no outcome" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    var gate: fk.Gate = .{};
    defer gate.release();
    h.repair.gate = &gate;
    const id = try h.add("x", .fetched, .nzb);

    var fut = io.concurrent(pp_mod.PostProcessor.process, .{ h.pp, id }) catch return error.SkipZigTest;
    try gate.waitEntered(1);
    try h.rig.repo.delete(id);
    gate.release();

    try testing.expectError(error.NotFound, fut.await(io));
    for (try h.events()) |r| try testing.expect(r.kind != .ready and r.kind != .failed);
}

test "state changes are fetched to post_processing to ready, with the injected clock" {
    var h: Harness = undefined;
    try h.init(.{});
    defer h.deinit();
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .ready);

    try testing.expectEqual(@as(i64, 2000), got.ready_at);
    try testing.expectEqual(@as(i64, 2000), got.updated_at);
    try expectTexts(try h.events(), &.{
        "download_updated post_processing", "stage verifying",   "stage finishing",
        "download_updated ready",           "ready /complete/x",
    });
}

test "the default clock works" {
    var h: Harness = undefined;
    try h.init(.{ .fixed_clock = false });
    defer h.deinit();
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .ready);

    const now = std.Io.Clock.now(.real, io).toMilliseconds();
    try testing.expect(@abs(now - got.ready_at) < 60_000);
}

test "a missing working directory goes straight to finish" {
    var h: Harness = undefined;
    try h.init(.{ .fs = true });
    defer h.deinit();
    h.fs.exists = false;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .ready);

    try testing.expectEqualStrings("/complete/x", got.output_path);
    try testing.expectEqualStrings("/work/x", h.fs.seen.get());
    try h.log.expect(&.{"finish"}); // no verify, repair, detect or unpack
    try testing.expectEqualStrings("/work/x", h.finisher.got_dir.get());
    try testing.expectEqualStrings("", h.finisher.got_unpacked.get());
}

test "a missing working directory with a finisher error fails internal, redacted" {
    var h: Harness = undefined;
    try h.init(.{ .fs = true });
    defer h.deinit();
    h.fs.exists = false;
    h.finisher.err = error.Failed;
    h.finisher.diag = .{ .message = "no marker in /work/x" };
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
    try testing.expect(std.mem.indexOf(u8, got.failure_detail, "/work") == null);
    try h.log.expect(&.{"finish"});
}

test "an error checking the working directory fails internal" {
    var h: Harness = undefined;
    try h.init(.{ .fs = true });
    defer h.deinit();
    h.fs.err = true;
    const id = try h.add("x", .fetched, .nzb);

    const got = try processTo(&h, id, .failed);

    try testing.expectEqual(dl.FailureReason.internal, got.failure_reason.?);
    try testing.expectEqualStrings("check working directory: InputOutput", got.failure_detail);
}

test "an existing working directory proceeds" {
    var h: Harness = undefined;
    try h.init(.{ .fs = true });
    defer h.deinit();
    const id = try h.add("x", .fetched, .nzb);

    _ = try processTo(&h, id, .ready);
}
