//! The cleanup, dots and torrent tests of internal/postproc/finalize
//! (finalize_test.go, dots_test.go, torrent_test.go).
const std = @import("std");
const testing = std.testing;
const finalize = @import("finalize.zig");
const env_mod = @import("finalize_env.zig");
const Env = env_mod.Env;
const ports = @import("postprocess_ports.zig");

fn contains(hay: []const u8, needle: []const u8) bool {
    return std.mem.indexOf(u8, hay, needle) != null;
}

// ---- cleanup ----

test "Usenet without archives removes only par2 and nzb" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("Show.S01E01");
    try env.put(dir, &.{
        .{ "show.mkv", "video" },
        .{ "show.nfo", "nfo" },
        .{ "sample/sample.mkv", "sample" },
        .{ "show.par2", "p" },
        .{ "show.vol00+01.PAR2", "p" },
        .{ "Show.S01E01.nzb", "n" },
        .{ "extras.rar", "kept: nothing was unpacked" },
    });

    const out = try env.ok(try env.usenet("Show.S01E01"), dir, "");

    try testing.expectEqualStrings(try env.join(&.{ env.usenet_complete, "Show.S01E01" }), out);
    try testing.expect(!env.has(dir));
    try env.expectTree(out, &.{
        .{ "show.mkv", "video" },
        .{ "show.nfo", "nfo" },
        .{ "sample", "<dir>" },
        .{ "sample/sample.mkv", "sample" },
        .{ "extras.rar", "kept: nothing was unpacked" },
    });
}

test "Usenet with archives delivers the unpacked content and keeps sample and nfo" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("Movie.2024");
    try env.put(dir, &.{
        .{ "movie.part01.rar", "rar" },
        .{ "movie.part02.rar", "rar" },
        .{ "old.rar", "rar" },
        .{ "old.r00", "rar" },
        .{ "old.R01", "rar" },
        .{ "old.s00", "rar" },
        .{ "set.7z.001", "7z" },
        .{ "set.7z.002", "7z" },
        .{ "single.7z", "7z" },
        .{ "movie.par2", "p" },
        .{ "movie.vol01+02.par2", "p" },
        .{ "movie.nzb", "n" },
        .{ "movie.nfo", "nfo" },
        .{ "movie-sample.mkv", "sample" },
        .{ "notes.txt", "kept" },
        .{ "_unpack.tmp/half.bin", "half" },
        .{ "_unpack/Movie.2024.mkv", "movie" },
        .{ "_unpack/Subs/en.srt", "subs" },
    });

    const out = try env.ok(try env.usenet("Movie.2024"), dir, try env.join(&.{ dir, "_unpack" }));

    try env.expectTree(out, &.{
        .{ "Movie.2024.mkv", "movie" },
        .{ "Subs", "<dir>" },
        .{ "Subs/en.srt", "subs" },
        .{ "movie.nfo", "nfo" },
        .{ "movie-sample.mkv", "sample" },
        .{ "notes.txt", "kept" },
    });
    try testing.expect(!env.has(dir));
}

const cleanup = @import("finalize_cleanup.zig");

test "archive and garbage names match the regexes of Go" {
    for ([_][]const u8{ "a.rar", "A.RAR", "a.r00", "a.R01", "a.s99", "a.r1234", "a.7z", "a.7Z", "a.7z.001", "a.7z.12345", "x.part01.rar", "a.\u{17f}00" }) |n| {
        testing.expect(cleanup.isArchive(n)) catch |e| {
            std.debug.print("should be an archive: {s}\n", .{n});
            return e;
        };
    }
    for ([_][]const u8{ "a.r0", "a.r", "a.7z.01", "a.7z.", "a.7z1", "arar", "a.rar.txt", "a.r00x", "a.t00", "r00", "a.zip", "", ".", "a.7z.0a1", "a.rar1" }) |n| {
        testing.expect(!cleanup.isArchive(n)) catch |e| {
            std.debug.print("should not be an archive: {s}\n", .{n});
            return e;
        };
    }
    for ([_][]const u8{ "a.par2", "a.PAR2", "a.nzb", "A.NZB", "vol00+01.par2" }) |n| try testing.expect(cleanup.isGarbage(n));
    for ([_][]const u8{ "a.par", "par2", "a.nzb.bak", "a.par2x", "" }) |n| try testing.expect(!cleanup.isGarbage(n));
}

// ---- dots ----

test "Usenet renames underscores to dots when the option is on" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.underscores_to_dots = true;
    const dir = try env.usenetDir("Show");
    try env.put(dir, &.{
        .{ "my_show_s01e01.mkv", "video" },
        .{ "my_show.nfo", "nfo" },
        .{ "sub_dir/keep_inner.srt", "subs" },
        .{ "plain.txt", "plain" },
        .{ "set_one.part01.rar", "rar" },
        .{ "_unpack/unpacked_file.mp4", "unpacked" },
    });

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));

    try env.expectTree(out, &.{
        .{ "my.show.s01e01.mkv", "video" },
        .{ "my.show.nfo", "nfo" },
        .{ "sub.dir", "<dir>" },
        .{ "sub.dir/keep_inner.srt", "subs" }, // not recursive
        .{ "plain.txt", "plain" },
        .{ "unpacked.file.mp4", "unpacked" },
    });
    try testing.expectEqual(@as(usize, 0), env.warnings.items.len); // a rename is not a warning
    try testing.expect(contains(env.logText(), "my_show.nfo"));
    try testing.expect(contains(env.logText(), "my.show.nfo"));
    try testing.expect(!contains(env.logText(), "level=WARN"));
}

test "Usenet keeps underscores when the option is off" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("Show");
    try env.put(dir, &.{ .{ "my_show.mkv", "video" }, .{ "a_dir/x.txt", "x" } });

    const out = try env.ok(try env.usenet("Show"), dir, "");

    try env.expectTree(out, &.{ .{ "my_show.mkv", "video" }, .{ "a_dir", "<dir>" }, .{ "a_dir/x.txt", "x" } });
}

test "torrent files keep their underscores even when the option is on" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.underscores_to_dots = true;
    const dir = try env.torrentDir("Show");
    try env.put(dir, &.{.{ "my_show.mkv", "video" }});

    const out = try env.ok(try env.torrent("Show"), dir, "");

    try env.expectTree(out, &.{.{ "my_show.mkv", "video" }});
}

test "underscores to dots skips an entry whose dotted name is taken" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.underscores_to_dots = true;
    const dir = try env.usenetDir("Show");
    try env.put(dir, &.{ .{ "a_b.mkv", "underscored" }, .{ "a.b.mkv", "dotted" }, .{ "c_d.mkv", "other" } });

    const out = try env.ok(try env.usenet("Show"), dir, "");

    try env.expectTree(out, &.{ .{ "a_b.mkv", "underscored" }, .{ "a.b.mkv", "dotted" }, .{ "c.d.mkv", "other" } });
}

test "underscores to dots does nothing the second time after a crash before the move" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.underscores_to_dots = true;
    env.move_mode = .fail_once;
    const dir = try env.usenetDir("Show");
    try env.put(dir, &.{.{ "my_show.mkv", "video" }});
    const d = try env.usenet("Show");

    _ = try env.failed(d, dir, "");
    try testing.expectEqual(@as(usize, 1), std.mem.count(u8, env.logText(), "my.show.mkv"));
    const logged = env.logText().len;
    const out = try env.ok(d, dir, "");

    try testing.expectEqual(logged, env.logText().len); // nothing left to rename on the retry
    try env.expectTree(out, &.{.{ "my.show.mkv", "video" }});
}

// A fully obfuscated post whose movie kept its random name (no usable PAR2
// set) is named after the Download, so media managers find a video file.
test "Usenet names an obfuscated video after the Download" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("Film");
    try env.put(dir, &.{
        .{ "13184cde52aaba45f8491bbf0db79b93", "\x1a\x45\xdf\xa3 matroska video" },
        .{ "release.nfo", "nfo" },
    });

    const out = try env.ok(try env.usenet("Film.2025.1080p"), dir, "");

    try env.expectTree(out, &.{
        .{ "Film.2025.1080p.mkv", "\x1a\x45\xdf\xa3 matroska video" },
        .{ "release.nfo", "nfo" },
    });
    try testing.expect(contains(env.logText(), "named the obfuscated video after the download"));
}

test "torrent files are never renamed even when obfuscated" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.torrentDir("Film");
    try env.put(dir, &.{.{ "13184cde", "\x1a\x45\xdf\xa3 seeding" }});

    const out = try env.ok(try env.torrent("Film"), dir, "");

    try testing.expect(env.has(try env.join(&.{ out, "13184cde" })));
}

// ---- torrents ----

test "torrent originals are never touched and _unpack is kept" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = try env.torrent("Release");
    const dir = try env.torrentDir("Release");
    try env.put(dir, &.{
        .{ "release.rar", "rar" },
        .{ "release.r00", "rar" },
        .{ "release.par2", "par" },
        .{ "release.nzb", "nzb" },
        .{ "release.nfo", "nfo" },
        .{ "_unpack/film.mkv", "film" },
        .{ "_unpack.tmp/x", "leftover" },
    });
    const before = try env.hashTree(dir);
    const want = try env.listing(dir);

    const out = try env.ok(d, dir, try env.join(&.{ dir, "_unpack" }));

    try testing.expectEqualStrings(try env.join(&.{ env.torrent_complete, "Release" }), out);
    try testing.expect(!env.has(dir));
    // Originals, _unpack/ and leftovers all moved intact.
    try testing.expectEqualStrings(want, try env.listingNoMarker(out));
    // Same names, modes, mtimes and bytes as before, apart from the marker.
    try std.Io.Dir.cwd().deleteFile(env_mod.io, try env.join(&.{ out, finalize.marker_name }));
    try testing.expectEqualSlices(u8, &before, &(try env.hashTree(out)));
}

test "a torrent already in place is a no-op" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = try env.torrent("Release");
    env.torrent_incomplete = env.torrent_complete;
    const dir = try env.join(&.{ env.torrent_complete, "Release" });
    try env.put(dir, &.{ .{ "a.mkv", "a" }, .{ "a.par2", "keep" } });
    const before = try env.hashTree(dir);

    const out = try env.ok(d, dir, "");

    try testing.expectEqualStrings(dir, out);
    try testing.expectEqualSlices(u8, &before, &(try env.hashTree(dir))); // nothing added, nothing changed
    try testing.expectEqual(@as(usize, 0), env.warnings.items.len);
}

test "a torrent already in place with a Category" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var d = try env.torrent("Release");
    d.category_id = try env.addCategory("tv", "", "");
    const dir = try env.join(&.{ env.torrent_complete, "tv", "Release" });
    try env.put(dir, &.{.{ "a.mkv", "a" }});

    const out = try env.ok(d, dir, "");

    try testing.expectEqualStrings(dir, out);
    try env.expectTree(dir, &.{.{ "a.mkv", "a" }});
}

test "a torrent in the same root but the wrong Category dir is moved" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.torrent_incomplete = env.torrent_complete;
    var d = try env.torrent("Release");
    d.category_id = try env.addCategory("tv", "", "");
    const dir = try env.join(&.{ env.torrent_complete, "Release" });
    try env.put(dir, &.{.{ "a.mkv", "a" }});

    const out = try env.ok(d, dir, "");

    try testing.expectEqualStrings(try env.join(&.{ env.torrent_complete, "tv", "Release" }), out);
    try testing.expect(!env.has(dir));
}

test "destination is where finish would move a torrent" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var d = try env.torrent("Release");
    d.category_id = try env.addCategory("tv", "", "");
    var diag: ports.Diag = .{};

    const got = try env.fin.destination(env.a(), env_mod.io, d, &diag);

    try testing.expectEqualStrings(try env.join(&.{ env.torrent_complete, "tv", "Release" }), got);
}

test "the adapters to the services compile" {
    _ = &finalize.settingsFromService;
    _ = &finalize.categoriesFromService;
}
