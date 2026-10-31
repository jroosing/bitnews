//! Tests of the `postprocess_ports.Unpacker` adapter on the real 7zz
//! (internal/postproc/unpack ports_test.go).
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const tu = @import("unpack_testutil.zig");
const tool = @import("tool.zig");
const tool_types = @import("tool_types.zig");
const ports = @import("postprocess_ports.zig");
const adapter = @import("unpack_ports.zig");
const unpack = @import("unpack.zig");
const env_mod = @import("unpack_env.zig");
const errors = @import("unpack_errors.zig");
const zipw = @import("unpack_zip_testutil.zig");

const Harness = struct {
    rig: *tu.Rig,
    ad: adapter.Adapter,
    diag: ports.Diag = .{},

    fn init() !*Harness {
        const h = try testing.allocator.create(Harness);
        const z = try tool.pinned(tu.gpa, tu.io, "7zz");
        defer tu.gpa.free(z);
        const rig = try tu.Rig.init(tool.system(), .{});
        h.* = .{ .rig = rig, .ad = .{ .gpa = tu.gpa, .runner = tool.system(), .seven_zip = try rig.arena().dupe(u8, z) } };
        return h;
    }

    fn deinit(h: *Harness) void {
        h.rig.deinit();
        testing.allocator.destroy(h);
    }

    fn fixtures(h: *Harness, comptime files: []const []const u8) !void {
        inline for (files) |f| try h.rig.fixture(f, "in/" ++ comptime std.fs.path.basename(f));
    }

    fn as(h: *Harness, fixture: []const u8, name: []const u8) !void {
        try h.rig.fixture(fixture, try std.fmt.allocPrint(h.rig.arena(), "in/{s}", .{name}));
    }

    fn detect(h: *Harness) ports.PortError!bool {
        h.diag = .{};
        return h.ad.unpacker().detect(h.rig.arena(), tu.io, h.rig.path("in"), &h.diag);
    }

    fn unpackTo(h: *Harness, dst_rel: []const u8) ports.PortError!ports.UnpackResult {
        h.diag = .{};
        return h.ad.unpacker().unpack(h.rig.arena(), tu.io, h.rig.path("in"), h.rig.path(dst_rel), "dl1", &h.diag);
    }
};

test "detect and unpack a 7z" {
    const h = try Harness.init();
    defer h.deinit();
    try h.fixtures(&.{"7z/t0.7z"});
    try testing.expect(try h.detect());
    const res = try h.unpackTo("base/out");
    try testing.expectEqual(@as(i64, 2), res.files);
    try testing.expectEqual(@as(i64, 8), res.bytes);
    try tu.expectStrings(&.{ "bar", "foo" }, try h.rig.tree("base/out"));
    try testing.expectEqualStrings("foo\n", try h.rig.read("base/out/foo"));
    try testing.expect(h.rig.exists("in/t0.7z"));
}

test "a stored RAR and a multi-volume 7z" {
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"rar/container.rar"});
        const res = try h.unpackTo("base/out");
        try testing.expectEqual(@as(i64, 1), res.files);
        try tu.expectStrings(&.{"container"}, try h.rig.tree("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{ "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.003", "7z/multi.7z.004", "7z/multi.7z.005", "7z/multi.7z.006" });
        try testing.expect(try h.detect());
        const res = try h.unpackTo("base/out");
        try testing.expectEqual(@as(i64, 10), res.files);
        try testing.expectEqual(@as(usize, 10), (try h.rig.tree("base/out")).len);
    }
}

test "nothing to detect" {
    const h = try Harness.init();
    defer h.deinit();
    try h.rig.write("in/movie.mkv", "not an archive");
    try testing.expect(!(try h.detect()));
}

test "a missing volume is corrupt at detect, and at unpack when it vanishes later" {
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{ "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.004" });
        try testing.expectError(error.ArchiveCorrupt, h.detect());
        try testing.expect(std.mem.indexOf(u8, h.diag.message, "missing archive volume") != null);
        try testing.expect(std.mem.startsWith(u8, h.diag.message, "archive corrupt: archive \"multi\": "));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{ "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.003", "7z/multi.7z.004", "7z/multi.7z.005", "7z/multi.7z.006" });
        try testing.expect(try h.detect());
        try h.rig.tmp.dir.deleteFile(tu.io, "in/multi.7z.002");
        try testing.expectError(error.ArchiveCorrupt, h.unpackTo("base/out"));
        try testing.expect(!h.rig.exists("base/out"));
    }
}

const PwState = struct {
    asked: [16]u8 = undefined,
    asked_len: usize = 0,
    text: ?[]const u8,
    fail: bool = false,

    fn source(s: *PwState) adapter.PasswordSource {
        return .{ .ctx = s, .get_fn = get };
    }

    fn get(ctx: ?*anyopaque, _: Allocator, id: []const u8) anyerror!?[]const u8 {
        const s: *PwState = @ptrCast(@alignCast(ctx.?));
        @memcpy(s.asked[0..id.len], id);
        s.asked_len = id.len;
        if (s.fail) return error.VaultSealed;
        return s.text;
    }
};

test "passwords come from the source and never into an error" {
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t2.7z"});
        var pw: PwState = .{ .text = "password" };
        h.ad.passwords = pw.source();
        _ = try h.unpackTo("base/out");
        try testing.expectEqualStrings("dl1", pw.asked[0..pw.asked_len]);
        try tu.expectStrings(&.{ "bar", "foo" }, try h.rig.tree("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t2.7z"});
        var pw: PwState = .{ .text = "s3cret-wrong" };
        h.ad.passwords = pw.source();
        try testing.expectError(error.ArchivePassword, h.unpackTo("base/out"));
        try testing.expect(std.mem.indexOf(u8, h.diag.message, "s3cret") == null);
        try testing.expect(!h.rig.exists("base/out"));
    }
    for ([_]?[]const u8{ null, "" }) |text| {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t2.7z"});
        var pw: PwState = .{ .text = text };
        h.ad.passwords = pw.source();
        try testing.expectError(error.ArchivePassword, h.unpackTo("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t2.7z"});
        try testing.expectError(error.ArchivePassword, h.unpackTo("base/out"));
    }
    {
        // An error of the source comes back as it is, not as a password error.
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t2.7z"});
        var pw: PwState = .{ .text = null, .fail = true };
        h.ad.passwords = pw.source();
        try testing.expectError(error.Failed, h.unpackTo("base/out"));
        try testing.expect(std.mem.indexOf(u8, h.diag.message, "VaultSealed") != null);
    }
}

test "resource limits map to the classes of Go" {
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        h.ad.max_entries = 1;
        try testing.expectError(error.ArchiveCorrupt, h.unpackTo("base/out"));
        try testing.expect(!h.rig.exists("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        h.ad.max_bytes = 3;
        try testing.expectError(error.ArchiveCorrupt, h.unpackTo("base/out"));
    }
    const Free = struct {
        fn probe(_: ?*anyopaque, _: []const u8) anyerror!u64 {
            return 1004;
        }
        fn four(_: ?*anyopaque, _: []const u8) anyerror!u64 {
            return 4;
        }
        var reserve: u64 = 1000;
        fn minFree(_: ?*anyopaque) u64 {
            return reserve;
        }
    };
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        h.ad.free_space = .{ .probe_fn = Free.four };
        try testing.expectError(error.DiskFull, h.unpackTo("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        h.ad.free_space = .{ .probe_fn = Free.probe };
        h.ad.min_free = 1000;
        try testing.expectError(error.DiskFull, h.unpackTo("base/out"));
    }
    {
        // The reserve is read at each unpack.
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        h.ad.free_space = .{ .probe_fn = Free.probe };
        h.ad.min_free_source = .{ .get_fn = Free.minFree };
        try testing.expectError(error.DiskFull, h.unpackTo("base/out"));
        Free.reserve = 0;
        _ = try h.unpackTo("base/out");
    }
}

test "garbage, a vanished archive and a missing program" {
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.rig.fixture("7z/t0.7z", "full.7z");
        const full = try h.rig.read("full.7z");
        try h.rig.write("in/t.7z", full[0 .. full.len - 40]);
        try testing.expect(try h.detect());
        try testing.expectError(error.ArchiveCorrupt, h.unpackTo("base/out"));
        try testing.expect(!h.rig.exists("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        try testing.expect(try h.detect());
        try h.rig.tmp.dir.deleteFile(tu.io, "in/t0.7z");
        try testing.expectError(error.ArchiveCorrupt, h.unpackTo("base/out"));
        try testing.expect(!h.rig.exists("base/out"));
    }
    {
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{"7z/t0.7z"});
        h.ad.seven_zip = h.rig.path("no-such-7zz");
        try testing.expectError(error.Failed, h.unpackTo("base/out"));
        try testing.expect(!h.rig.exists("base/out"));
    }
}

test "a cancellation comes through as Canceled and changes nothing" {
    const Cancels = struct {
        fn run(_: *anyopaque, _: Allocator, _: std.Io, _: tool_types.Spec) tool_types.Error!tool_types.Result {
            return error.Canceled;
        }
    };
    const h = try Harness.init();
    defer h.deinit();
    try h.fixtures(&.{"7z/t0.7z"});
    var dummy: u8 = 0;
    h.ad.runner = .{ .ctx = &dummy, .run_fn = Cancels.run };
    try testing.expectError(error.Canceled, h.unpackTo("base/out"));
    try testing.expect(!h.rig.exists("base/out"));
}

test "a single set replaces an existing destination" {
    const h = try Harness.init();
    defer h.deinit();
    try h.fixtures(&.{"7z/t0.7z"});
    try h.rig.write("base/out/stale", "x");
    _ = try h.unpackTo("base/out");
    try tu.expectStrings(&.{ "bar", "foo" }, try h.rig.tree("base/out"));
}

test "several sets merge into one directory" {
    {
        // t0 (bar, foo) and the stored RAR (container) do not collide.
        const h = try Harness.init();
        defer h.deinit();
        try h.fixtures(&.{ "7z/t0.7z", "rar/container.rar" });
        try testing.expect(try h.detect());
        const res = try h.unpackTo("base/out");
        try testing.expectEqual(@as(i64, 3), res.files);
        try testing.expectEqual(@as(i64, 8 + "#!/bin/sh\nexit 0\n".len), res.bytes);
        try tu.expectStrings(&.{ "bar", "container", "foo" }, try h.rig.tree("base/out"));
        try tu.expectStrings(&.{"out"}, try h.rig.children("base"));
    }
    {
        // t0 and t1 both hold bar and foo; the later set's files are renamed.
        const h = try Harness.init();
        defer h.deinit();
        try h.as("7z/t0.7z", "a.7z");
        try h.as("7z/t1.7z", "b.7z");
        const res = try h.unpackTo("base/out");
        try testing.expectEqual(@as(i64, 4), res.files);
        try tu.expectStrings(&.{ "bar", "bar.1", "foo", "foo.1" }, try h.rig.tree("base/out"));
        try tu.expectStrings(&.{"out"}, try h.rig.children("base"));
    }
    {
        // empty.7z holds directories 01..10 (and files 06..10 at the top);
        // multi.7z holds the files 01..10, so 01..05 collide as file versus
        // directory and 06..10 as file versus file.
        const h = try Harness.init();
        defer h.deinit();
        try h.as("7z/empty.7z", "a.7z");
        try h.fixtures(&.{ "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.003", "7z/multi.7z.004", "7z/multi.7z.005", "7z/multi.7z.006" });
        _ = try h.unpackTo("base/out");
        const got = try h.rig.tree("base/out");
        try testing.expectEqual(@as(usize, 20), got.len);
        for ([_][]const u8{ "01", "02", "03", "04", "05" }) |n| {
            try testing.expect(contains(got, n));
            try testing.expect(contains(got, try std.fmt.allocPrint(h.rig.arena(), "{s}.1", .{n})));
        }
        for ([_][]const u8{ "06", "07", "08", "09", "10" }) |n| {
            try testing.expect(contains(got, n));
            try testing.expect(contains(got, try std.fmt.allocPrint(h.rig.arena(), "{s}.1", .{n})));
        }
    }
    {
        // Stale leftovers of an interrupted run are replaced.
        const h = try Harness.init();
        defer h.deinit();
        try h.as("7z/t0.7z", "a.7z");
        try h.as("7z/t1.7z", "b.7z");
        try h.rig.mkdir("base/out/old");
        try h.rig.mkdir("base/out.sets/0/junk");
        _ = try h.unpackTo("base/out");
        try tu.expectStrings(&.{ "bar", "bar.1", "foo", "foo.1" }, try h.rig.tree("base/out"));
        try tu.expectStrings(&.{"out"}, try h.rig.children("base"));
    }
    {
        // The second set needs a password nobody supplies: nothing is left.
        const h = try Harness.init();
        defer h.deinit();
        try h.as("7z/t0.7z", "a.7z");
        try h.as("7z/t2.7z", "b.7z");
        try testing.expectError(error.ArchivePassword, h.unpackTo("base/out"));
        try testing.expectEqual(@as(usize, 0), (try h.rig.children("base")).len);
    }
}

fn contains(list: []const []const u8, want: []const u8) bool {
    for (list) |l| if (std.mem.eql(u8, l, want)) return true;
    return false;
}

test "mergeInto merges directories recursively and renames other collisions" {
    const rig = try tu.Rig.init(tool.system(), .{});
    defer rig.deinit();
    try rig.write("a/Movie/movie.mkv", "A-movie");
    try rig.write("a/Movie/Subs/en.srt", "A-en");
    try rig.write("b/Movie/movie.mkv", "B-movie");
    try rig.write("b/Movie/Subs/nl.srt", "B-nl");
    try rig.write("b/Movie/readme.txt", "B-readme");
    try rig.mkdir("m");
    try adapter.mergeInto(&rig.env, rig.path("a"), rig.path("m"));
    try adapter.mergeInto(&rig.env, rig.path("b"), rig.path("m"));
    try tu.expectStrings(&.{ "Movie", "Movie/Subs", "Movie/Subs/en.srt", "Movie/Subs/nl.srt", "Movie/movie.1.mkv", "Movie/movie.mkv", "Movie/readme.txt" }, try rig.tree("m"));
    try testing.expectEqualStrings("A-movie", try rig.read("m/Movie/movie.mkv"));
    try testing.expectEqualStrings("B-movie", try rig.read("m/Movie/movie.1.mkv"));
}

test "a symlink in the archive is a path violation through the adapter's Unpack" {
    const h = try Harness.init();
    defer h.deinit();
    try zipw.write(h.rig, "other/links.zip", &.{.{ .name = "link", .data = "/etc/passwd", .mode = 0o120777 }});
    // Detect does not offer a zip, so the set goes to Unpack directly.
    var env: env_mod.Env = .{ .io = tu.io, .gpa = tu.gpa, .arena = h.rig.arena(), .runner = tool.system(), .cfg = .{ .seven_zip = h.ad.seven_zip, .replace = true }, .diag = &h.diag };
    const vols = try h.rig.arena().alloc([]const u8, 1);
    vols[0] = h.rig.path("other/links.zip");
    try testing.expectError(error.PathViolation, unpack.unpack(&env, .{ .format = .seven_z, .name = "links", .volumes = vols }, h.rig.path("base/out")));
    try testing.expectEqual(@as(usize, 0), (try h.rig.children("base")).len);
}

test "the error classes map to the port classes" {
    const Pair = struct { e: errors.Error, p: ports.PortError };
    const pairs = [_]Pair{
        .{ .e = error.Password, .p = error.ArchivePassword },
        .{ .e = error.Unsupported, .p = error.ArchiveUnsupported },
        .{ .e = error.Corrupt, .p = error.ArchiveCorrupt },
        .{ .e = error.MissingVolume, .p = error.ArchiveCorrupt },
        .{ .e = error.Bomb, .p = error.ArchiveCorrupt },
        .{ .e = error.TooLarge, .p = error.ArchiveCorrupt },
        .{ .e = error.TooManyEntries, .p = error.ArchiveCorrupt },
        .{ .e = error.PathViolation, .p = error.PathViolation },
        .{ .e = error.DiskFull, .p = error.DiskFull },
        .{ .e = error.Exists, .p = error.Failed },
        .{ .e = error.Internal, .p = error.Failed },
        .{ .e = error.Canceled, .p = error.Canceled },
    };
    for (pairs) |p| try testing.expectEqual(p.p, errors.toPort(p.e));
}
