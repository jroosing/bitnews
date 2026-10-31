//! The destination tests of internal/postproc/finalize (destination_test.go).
const std = @import("std");
const testing = std.testing;
const finalize = @import("finalize.zig");
const env_mod = @import("finalize_env.zig");
const Env = env_mod.Env;
const ports = @import("postprocess_ports.zig");

fn contains(hay: []const u8, needle: []const u8) bool {
    return std.mem.indexOf(u8, hay, needle) != null;
}

test "destination: Usenet Category dir" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var d = try env.usenet("Show");
    d.category_id = try env.addCategory("tv", "", "");

    const out = try env.finishUsenet(d);
    try testing.expectEqual(@as(?ports.PortError, null), out.err);
    try testing.expectEqualStrings(try env.join(&.{ env.usenet_complete, "tv", "Show" }), out.path);
    try testing.expect(env.has(try env.join(&.{ out.path, "a.mkv" })));
}

test "destination: no Category uses the complete root" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const out = try env.finishUsenet(try env.usenet("Show"));
    try testing.expectEqualStrings(try env.join(&.{ env.usenet_complete, "Show" }), out.path);
}

test "destination: a dir override wins over the Category dir" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const override = try env.join(&.{ env.tree.path, "elsewhere", "movies" });
    var d = try env.usenet("Film");
    d.category_id = try env.addCategory("movies", override, "");

    const out = try env.finishUsenet(d);
    try testing.expectEqualStrings(try env.join(&.{ override, "Film" }), out.path);
    try testing.expect(!env.has(try env.join(&.{ env.usenet_complete, "movies" })));
}

test "destination: the folder override of the Download's protocol is used" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const usenet_dir = try env.join(&.{ env.tree.path, "usenet-movies" });
    const torrent_dir = try env.join(&.{ env.tree.path, "torrent-movies" });
    const split = try env.addCategory("movies", usenet_dir, torrent_dir);
    const usenet_only = try env.addCategory("tv", usenet_dir, "");

    var film_u = try env.usenet("Film");
    film_u.category_id = split;
    var film_t = try env.torrent("Film");
    film_t.category_id = split;
    var show_t = try env.torrent("Show");
    show_t.category_id = usenet_only;
    var diag: ports.Diag = .{};
    try testing.expectEqualStrings(try env.join(&.{ usenet_dir, "Film" }), try env.fin.destination(env.a(), env_mod.io, film_u, &diag));
    try testing.expectEqualStrings(try env.join(&.{ torrent_dir, "Film" }), try env.fin.destination(env.a(), env_mod.io, film_t, &diag));
    try testing.expectEqualStrings(try env.join(&.{ env.torrent_complete, "tv", "Show" }), try env.fin.destination(env.a(), env_mod.io, show_t, &diag));
}

test "destination: resolved at move time" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var d = try env.usenet("Show");
    d.category_id = try env.addCategory("tv", "", "");
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{.{ "a", "a" }});

    // Category renamed and settings changed after the Download was queued.
    env.cats.items[0].name = "series";
    env.usenet_complete = try env.join(&.{ env.tree.path, "moved-root" });

    const out = try env.ok(d, dir, "");
    try testing.expectEqualStrings(try env.join(&.{ env.tree.path, "moved-root", "series", "Show" }), out);
}

test "destination: a torrent uses the torrent complete root" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var d = try env.torrent("Linux.ISO");
    d.category_id = try env.addCategory("iso", "", "");
    const dir = try env.torrentDir("Linux.ISO");
    try env.put(dir, &.{.{ "a.iso", "iso" }});

    const out = try env.ok(d, dir, "");
    try testing.expectEqualStrings(try env.join(&.{ env.torrent_complete, "iso", "Linux.ISO" }), out);
}

test "destination: a deleted Category falls back to the root with a warning" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var d = try env.usenet("Show");
    d.category_id = try env.newId(); // not known

    const out = try env.finishUsenet(d);
    try testing.expectEqualStrings(try env.join(&.{ env.usenet_complete, "Show" }), out.path);
    try testing.expectEqual(@as(usize, 1), env.warnings.items.len);
    try testing.expectEqual(finalize.Warning.Kind.category_missing, env.warnings.items[0].kind);
}

test "destination: a collision uses Unique" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const taken = try env.join(&.{ env.usenet_complete, "Show" });
    try env.put(taken, &.{.{ "old.mkv", "old" }});

    const out = try env.finishUsenet(try env.usenet("Show"));
    try testing.expectEqualStrings(try std.fmt.allocPrint(env.a(), "{s}.1", .{taken}), out.path);
    try testing.expectEqualStrings("old", try env.read(try env.join(&.{ taken, "old.mkv" })));
    try testing.expect(env.has(try env.join(&.{ out.path, "a.mkv" })));
}

test "destination: a hostile Category name is rejected and nothing happens" {
    for ([_][]const u8{ "..", "../escape", "a/../../x", "/abs", "..\\x" }) |name| {
        var env: Env = undefined;
        try env.init();
        defer env.deinit();
        var d = try env.usenet("Show");
        d.category_id = try env.addCategory(name, "", "");
        const dir = try env.usenetDir("w");
        try env.put(dir, &.{ .{ "junk.par2", "p" }, .{ "a.mkv", "a" } });

        _ = try env.failed(d, dir, "");
        // Not moved, nothing deleted, nothing escaped.
        try testing.expect(env.has(try env.join(&.{ dir, "junk.par2" })));
        try testing.expect(!env.has(try env.join(&.{ env.tree.path, "escape" })));
        try testing.expect(!env.has(try env.join(&.{ env.tree.path, "usenet", "escape" })));
    }
}

test "destination: a hostile folder override is rejected" {
    for ([_][]const u8{ "relative/dir", "/data/../../etc", "/data/x/../../y", "/data\\..\\x", "" }) |override| {
        var env: Env = undefined;
        try env.init();
        defer env.deinit();
        var d = try env.usenet("Show");
        d.category_id = try env.addCategory("tv", override, "");
        const dir = try env.usenetDir("w");
        try env.put(dir, &.{.{ "a.mkv", "a" }});

        const out = try env.run(d, dir, "");
        if (override.len == 0) { // no override is the normal case
            try testing.expectEqual(@as(?ports.PortError, null), out.err);
            try testing.expect(std.mem.startsWith(u8, out.path, env.usenet_complete));
            continue;
        }
        try testing.expectEqual(@as(?ports.PortError, error.Failed), out.err);
        try testing.expect(env.has(dir));
    }
}

test "destination: a Category dir symlinked outside the root is rejected" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const outside = try env.join(&.{ env.tree.path, "outside" });
    try env.tree.mkdir("outside");
    try env.tree.mkdir("usenet/complete");
    try env.link(outside, try env.join(&.{ env.usenet_complete, "tv" }));
    var d = try env.usenet("Show");
    d.category_id = try env.addCategory("tv", "", "");

    const out = try env.finishUsenet(d);
    try testing.expectEqual(@as(?ports.PortError, error.Failed), out.err);
    try testing.expect(contains(out.message, "unsafe path"));
    try testing.expectEqualStrings("", try env.listing(outside));
}

test "destination: names are sanitized" {
    const long_buf: [400]u8 = @splat('x');
    const long: []const u8 = &long_buf;
    const Case = struct { in: []const u8, want: ?[]const u8 }; // null: download-<id>
    const table = [_]Case{
        .{ .in = "../x", .want = ".._x" },
        .{ .in = "a/b\\c", .want = "a_b_c" },
        .{ .in = "", .want = null },
        .{ .in = "...", .want = null },
        .{ .in = long, .want = long[0..255] },
    };
    for (table) |c| {
        var env: Env = undefined;
        try env.init();
        defer env.deinit();
        const d = try env.usenet(c.in);
        const out = try env.finishUsenet(d);
        const want_name = c.want orelse try std.fmt.allocPrint(env.a(), "download-{s}", .{d.id});
        try testing.expectEqualStrings(try env.join(&.{ env.usenet_complete, want_name }), out.path);
    }
}

test "destination: the parent is created" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try testing.expect(!env.has(env.usenet_complete));
    const out = try env.finishUsenet(try env.usenet("Show"));
    try testing.expect(env.has(env.usenet_complete));
    try testing.expectEqual(@as(?ports.PortError, null), out.err);
}

test "destination: a complete root that is not absolute fails" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.usenet_complete = "relative/complete";
    const out = try env.finishUsenet(try env.usenet("Show"));
    try testing.expectEqual(@as(?ports.PortError, error.Failed), out.err);
    try testing.expect(contains(out.message, "not an absolute path"));
}
