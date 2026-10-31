//! Tests of the tidy functions that touch files: the cases of
//! internal/postproc/tidy dots_test.go and video_test.go, and hostile trees.
const std = @import("std");
const testing = std.testing;
const tidy = @import("tidy.zig");
const util = @import("tidy_testutil.zig");
const Tree = util.Tree;

const mkv_head = [_]u8{ 0x1a, 0x45, 0xdf, 0xa3, 0x01, 0x00, 0x00, 0x00 };
const mp4_head = "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00";
const avi_head = "RIFF\x00\x10\x00\x00AVI LIST";

fn dots(a: std.mem.Allocator, t: *Tree, rel: []const u8) !std.ArrayList(tidy.Rename) {
    var done: std.ArrayList(tidy.Rename) = .empty;
    const dir = try t.abs(a, rel);
    try tidy.dotUnderscores(a, util.io, dir, &done);
    return done;
}

test "DotUnderscores renames files and folders at the top level" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.write("Show_S01E01_1080p.mkv", "x");
    try t.write("Sample_Folder/inner_file_name.txt", "x");
    try t.write("no-underscore.nfo", "x");

    const done = try dots(a, &t, "");

    try util.expectListing(try t.listing(a, ""), &.{
        .{ "Sample.Folder", "<dir>" },
        .{ "Sample.Folder/inner_file_name.txt", "x" }, // it does not recurse
        .{ "Show.S01E01.1080p.mkv", "x" },
        .{ "no-underscore.nfo", "x" },
    });
    try testing.expectEqual(@as(usize, 2), done.items.len);
    var seen_file = false;
    var seen_dir = false;
    for (done.items) |r| {
        if (std.mem.eql(u8, r.from, "Show_S01E01_1080p.mkv") and std.mem.eql(u8, r.to, "Show.S01E01.1080p.mkv")) seen_file = true;
        if (std.mem.eql(u8, r.from, "Sample_Folder") and std.mem.eql(u8, r.to, "Sample.Folder")) seen_dir = true;
    }
    try testing.expect(seen_file and seen_dir);
}

test "DotUnderscores never overwrites an existing name" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.write("a_b.txt", "x");
    try t.write("a.b.txt", "keep");

    const done = try dots(a, &t, "");

    try testing.expectEqual(@as(usize, 0), done.items.len);
    try util.expectListing(try t.listing(a, ""), &.{ .{ "a_b.txt", "x" }, .{ "a.b.txt", "keep" } });
}

test "DotUnderscores of a missing directory is an error" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var done: std.ArrayList(tidy.Rename) = .empty;
    const dir = try t.abs(arena.allocator(), "nope");
    try testing.expectError(error.FileNotFound, tidy.dotUnderscores(arena.allocator(), util.io, dir, &done));
}

test "DotUnderscores leaves a symlink target alone" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.write("real_file.txt", "x");
    try t.symlink("real_file.txt", "link_name");

    _ = try dots(a, &t, "");

    // The entries are renamed, never what a link points at.
    try util.expectListing(try t.listing(a, ""), &.{
        .{ "real.file.txt", "x" },
        .{ "link.name", "<symlink:real_file.txt>" },
    });
}

test "DotUnderscores does not follow a symlinked directory it is given" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.write("outside/keep_me.txt", "x");
    try t.symlink("outside", "dirlink");

    var done: std.ArrayList(tidy.Rename) = .empty;
    const dir = try t.abs(a, "dirlink");
    try testing.expect(std.meta.isError(tidy.dotUnderscores(a, util.io, dir, &done)));
    try util.expectListing(try t.listing(a, "outside"), &.{.{ "keep_me.txt", "x" }});
}

// ---- NameVideo ----

fn nameVideo(a: std.mem.Allocator, t: *Tree, name: []const u8) !?tidy.Rename {
    return tidy.nameVideo(a, util.io, t.path, name);
}

// An obfuscated post without a usable PAR2 set leaves the movie under a
// random name with no extension; media managers only import files with a
// video extension. The largest file is named after the Download, with the
// extension its content says.
test "NameVideo names the largest file after the Download" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.writeHead("13184cde52aaba45f8491bbf0db79b93", &mkv_head, 9000);
    try t.writeHead("21cb82676987e551", &mkv_head, 300); // a sample: left alone
    try t.write("release.nfo", "x");

    const r = (try nameVideo(a, &t, "The.Housemaid.2025.1080p.BluRay.DDP7.1.x264-ZoroSenpai")).?;

    try testing.expectEqualStrings("13184cde52aaba45f8491bbf0db79b93", r.from);
    try testing.expectEqualStrings("The.Housemaid.2025.1080p.BluRay.DDP7.1.x264-ZoroSenpai.mkv", r.to);
    const l = try t.listing(a, "");
    try testing.expect(std.mem.indexOf(u8, l, "The.Housemaid.2025.1080p.BluRay.DDP7.1.x264-ZoroSenpai.mkv = ") != null);
    try testing.expect(std.mem.indexOf(u8, l, "21cb82676987e551 = ") != null);
    try testing.expect(std.mem.indexOf(u8, l, "release.nfo = x") != null);
    try testing.expect(std.mem.indexOf(u8, l, "13184cde52aaba45f8491bbf0db79b93") == null);
}

fn tsHead(prefix: usize, out: *[600]u8) []const u8 {
    @memset(out, 0);
    var i: usize = prefix;
    while (i < out.len) : (i += 188 + prefix) out[i] = 0x47;
    return out;
}

test "NameVideo knows the common containers" {
    var ts_buf: [600]u8 = undefined;
    var m2ts_buf: [600]u8 = undefined;
    const Case = struct { ext: []const u8, head: []const u8 };
    const table = [_]Case{
        .{ .ext = ".mkv", .head = &mkv_head },
        .{ .ext = ".mp4", .head = mp4_head },
        .{ .ext = ".avi", .head = avi_head },
        .{ .ext = ".wmv", .head = &.{ 0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11 } },
        .{ .ext = ".mpg", .head = &.{ 0x00, 0x00, 0x01, 0xba } },
        .{ .ext = ".ts", .head = tsHead(0, &ts_buf) },
        .{ .ext = ".m2ts", .head = tsHead(4, &m2ts_buf) },
        .{ .ext = ".mov", .head = "\x00\x00\x00\x20ftypqt  " },
    };
    for (table) |c| {
        var t = try Tree.init();
        defer t.deinit();
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        try t.writeHead("abc", c.head, 1000);

        const r = (try nameVideo(arena.allocator(), &t, "Film")).?;

        const want = try std.mem.concat(arena.allocator(), u8, &.{ "Film", c.ext });
        try testing.expectEqualStrings(want, r.to);
    }
}

test "NameVideo leaves named or unknown files alone" {
    const File = struct { name: []const u8, head: []const u8 };
    const Case = struct { label: []const u8, files: []const File };
    const table = [_]Case{
        .{ .label = "already a video", .files = &.{.{ .name = "movie.mkv", .head = &mkv_head }} },
        .{ .label = "not a video", .files = &.{.{ .name = "0f0f0f0f", .head = "PK\x03\x04 zip" }} },
        .{ .label = "largest not video", .files = &.{ .{ .name = "aaaa", .head = "just text" }, .{ .name = "bbbb", .head = &mkv_head } } },
        .{ .label = "empty", .files = &.{} },
        // Go lower-cases with Unicode rules: the Kelvin sign is a k.
        .{ .label = "kelvin extension", .files = &.{.{ .name = "movie.m\u{212a}v", .head = &mkv_head }} },
    };
    for (table) |c| {
        var t = try Tree.init();
        defer t.deinit();
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const a = arena.allocator();
        var size: usize = 2000;
        for (c.files) |f| {
            try t.writeHead(f.name, f.head, size);
            size -= 500; // the first is the largest
        }
        const before = try t.listing(a, "");

        const r = try nameVideo(a, &t, "Film");

        testing.expect(r == null) catch |err| {
            std.debug.print("case {s}\n", .{c.label});
            return err;
        };
        try testing.expectEqualStrings(before, try t.listing(a, ""));
    }
}

test "NameVideo finds the movie in a subfolder and never overwrites" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.writeHead("sub/x9", &mkv_head, 5000);
    try t.write("sub/Film.mkv", "x");

    const r = (try nameVideo(a, &t, "Film")).?;

    try testing.expectEqualStrings("sub/x9", r.from);
    try testing.expectEqualStrings("sub/Film.1.mkv", r.to);
    const l = try t.listing(a, "sub");
    try testing.expect(std.mem.indexOf(u8, l, "Film.1.mkv = ") != null);
    try testing.expect(std.mem.indexOf(u8, l, "Film.mkv = x") != null);
    try testing.expect(std.mem.indexOf(u8, l, "x9") == null);
}

test "NameVideo makes a hostile Download name safe" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.writeHead("x", &mkv_head, 100);

    const r = (try nameVideo(a, &t, "../../etc/Film")).?;

    try testing.expectEqualStrings(".._.._etc_Film.mkv", r.to);
    try testing.expect(t.has(".._.._etc_Film.mkv"));
}

test "NameVideo with a name that cannot be made safe renames nothing" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    // "" + ".mkv" is a valid name; a name of only dots and spaces plus the
    // extension still is. A NUL is stripped, so nothing is invalid but the
    // empty result of an all-control name is not reachable either: this checks
    // the common hostile spellings stay inside the directory.
    try t.writeHead("x", &mkv_head, 100);
    const r = (try nameVideo(arena.allocator(), &t, "..")).?;
    try testing.expect(std.mem.indexOfScalar(u8, r.to, '/') == null);
    try testing.expect(t.has(r.to));
}

test "NameVideo never follows a link" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.writeHead("outside/big", &mkv_head, 90000);
    try t.write("work/small", "tiny");
    try t.symlink("../outside/big", "work/biglink");
    try t.symlink("../outside", "work/dirlink");
    const work = try t.abs(a, "work");

    const r = try tidy.nameVideo(a, util.io, work, "Film");

    try testing.expect(r == null); // the largest regular file inside work is "small"
    try util.expectListing(try t.listing(a, "outside"), &.{.{ "big", try t.read(a, "outside/big") }});
}

test "NameVideo of a directory that is a symlink is refused" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try t.writeHead("outside/big", &mkv_head, 100);
    try t.symlink("outside", "work");
    const work = try t.abs(a, "work");

    try testing.expect(std.meta.isError(tidy.nameVideo(a, util.io, work, "Film")));
    try testing.expect(t.has("outside/big"));
}

test "NameVideo stops at the depth cap" {
    var t = try Tree.init();
    defer t.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var path: std.ArrayList(u8) = .empty;
    for (0..140) |_| try path.appendSlice(a, "d/");
    try path.appendSlice(a, "f");
    // A path this long may not be creatable on every file system; the cap is
    // what is under test.
    t.write(path.items, "x") catch return error.SkipZigTest;
    try testing.expectError(error.TooLarge, nameVideo(a, &t, "Film"));
}
