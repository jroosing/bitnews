//! End-to-end tests on the real, pinned 7zz (bin/tools/7zz): 7z archives from
//! the fixtures, limits, the lifecycle of the staging directory, passwords
//! and the hostile archives that Go's tests build with archive/zip and 7zz
//! (internal/postproc/unpack sevenz_test.go, limits_test.go,
//! lifecycle_test.go, hostile_test.go). A missing 7zz fails the tests, as in
//! Go: run `make tools`.
const std = @import("std");
const testing = std.testing;
const tu = @import("unpack_testutil.zig");
const unpack = @import("unpack.zig");
const detect = @import("unpack_detect.zig");
const env_mod = @import("unpack_env.zig");
const zipw = @import("unpack_zip_testutil.zig");

pub const Rig = tu.Rig;

pub fn detectOne(rig: *Rig, rel: []const u8) !detect.ArchiveSet {
    const sets = try detect.detect(&rig.env, if (rel.len == 0) rig.root else rig.path(rel));
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expect(sets[0].err == null);
    return sets[0];
}

/// A set for one file; 7zz recognizes the real format from the content.
pub fn oneSet(rig: *Rig, rel: []const u8) !detect.ArchiveSet {
    const vols = try rig.arena().alloc([]const u8, 1);
    vols[0] = rig.path(rel);
    return .{ .format = .seven_z, .name = std.fs.path.basename(rel), .volumes = vols };
}

pub fn run(rig: *Rig, set: detect.ArchiveSet) !unpack.Result {
    return unpack.unpack(&rig.env, set, rig.path("p/out"));
}

/// Copies the fixtures t0.7z etc. into <rig>/in.
pub fn copy7z(rig: *Rig, names: []const []const u8) !void {
    for (names) |n| {
        try rig.fixture(try std.fmt.allocPrint(rig.arena(), "7z/{s}", .{n}), try std.fmt.allocPrint(rig.arena(), "in/{s}", .{n}));
    }
}

/// Only the jail may be in the parent of the destination: nothing escaped and
/// no staging directory or destination is left.
pub fn requireNothingLeft(rig: *Rig) !void {
    const top = try rig.children("");
    for (top) |t| try testing.expect(std.mem.eql(u8, t, "in") or std.mem.eql(u8, t, "arch") or std.mem.eql(u8, t, "p") or std.mem.eql(u8, t, "src"));
    if (rig.exists("p")) try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
}

fn files(res: unpack.Result, want: []const []const u8) !void {
    try tu.expectStrings(want, res.files);
}

// ---- 7z fixtures ----

test "a real 7z archive is extracted" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{"t0.7z"});
    const res = try run(rig, try detectOne(rig, "in"));
    try files(res, &.{ "bar", "foo" });
    try testing.expectEqual(@as(i64, 8), res.bytes);
    try testing.expectEqualStrings("foo\n", try rig.read("p/out/foo"));
    try testing.expectEqualStrings("bar\n", try rig.read("p/out/bar"));
    try tu.expectStrings(&.{"out"}, try rig.children("p"));
}

test "a multi-volume 7z set is extracted" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{ "multi.7z.001", "multi.7z.002", "multi.7z.003", "multi.7z.004", "multi.7z.005", "multi.7z.006" });
    const set = try detectOne(rig, "in");
    try testing.expectEqual(@as(usize, 6), set.volumes.len);
    const res = try run(rig, set);
    try testing.expectEqual(@as(usize, 10), res.files.len);
}

test "a multi-volume 7z set with a missing middle volume is refused by Detect" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{ "multi.7z.001", "multi.7z.002", "multi.7z.004" });
    const sets = try detect.detect(&rig.env, rig.path("in"));
    try testing.expectEqual(@as(usize, 1), sets.len);
    try testing.expectError(error.MissingVolume, run(rig, sets[0]));
    try requireNothingLeft(rig);
}

test "a multi-volume 7z set whose tail was lost is corrupt (Detect cannot know)" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{ "multi.7z.001", "multi.7z.002", "multi.7z.003" });
    try testing.expectError(error.Corrupt, run(rig, try detectOne(rig, "in")));
    try requireNothingLeft(rig);
}

test "header-compressed, LZMA2 and directory fixtures extract" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{"t1.7z"});
    try files(try run(rig, try detectOne(rig, "in")), &.{ "bar", "foo" });

    const rig2 = try Rig.real(.{});
    defer rig2.deinit();
    try copy7z(rig2, &.{"empty.7z"});
    const res = try run(rig2, try detectOne(rig2, "in"));
    try testing.expect(res.files.len > 0);
    const tree = try rig2.tree("p/out");
    var found = false;
    for (tree) |t| if (std.mem.eql(u8, t, "01")) {
        found = true;
    };
    try testing.expect(found);

    const rig3 = try Rig.real(.{});
    defer rig3.deinit();
    try copy7z(rig3, &.{"lzma2.7z"});
    try testing.expect((try run(rig3, try detectOne(rig3, "in"))).files.len > 0);
}

test "7z passwords: encrypted headers, compressed, plain headers" {
    const cases = [_][]const u8{ "t2.7z", "t3.7z", "t4.7z", "t5.7z" };
    for (cases) |file| {
        {
            const rig = try Rig.real(.{ .password = "password" });
            defer rig.deinit();
            try copy7z(rig, &.{file});
            const res = try run(rig, try detectOne(rig, "in"));
            try files(res, &.{ "bar", "foo" });
            try testing.expectEqualStrings("foo\n", try rig.read("p/out/foo"));
        }
        {
            const rig = try Rig.real(.{ .password = "notpassword" });
            defer rig.deinit();
            try copy7z(rig, &.{file});
            try testing.expectError(error.Password, run(rig, try detectOne(rig, "in")));
            try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
        }
        {
            const rig = try Rig.real(.{});
            defer rig.deinit();
            try copy7z(rig, &.{file});
            try testing.expectError(error.Password, run(rig, try detectOne(rig, "in")));
            try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
        }
    }
}

test "a password on an unencrypted 7z is ignored" {
    const rig = try Rig.real(.{ .password = "whatever" });
    defer rig.deinit();
    try copy7z(rig, &.{"t0.7z"});
    _ = try run(rig, try detectOne(rig, "in"));
}

test "passwords with awkward characters travel on stdin" {
    for ([_][]const u8{ "pässwörd 日本語", "with spaces  and  trailing ", "-pquote\"s'&;$(x)", "-ppw", "@list" }) |pw| {
        const rig = try Rig.real(.{ .password = pw });
        defer rig.deinit();
        try rig.write("src/secret.txt", "s3cret");
        try rig.mkdir("arch");
        const dialogue = try std.fmt.allocPrint(rig.arena(), "{s}\n{s}\n", .{ pw, pw });
        try rig.run7zz("src", dialogue, &.{ "a", "-p", "-mhe=on", "-bd", "-mx1", rig.path("arch/enc.7z"), "." });
        const res = try run(rig, try detectOne(rig, "arch"));
        try files(res, &.{"secret.txt"});
        try testing.expectEqualStrings("s3cret", try rig.read("p/out/secret.txt"));
    }
}

test "a password with a line break is refused" {
    const rig = try Rig.real(.{ .password = "pass\nword" });
    defer rig.deinit();
    try copy7z(rig, &.{"t3.7z"});
    try testing.expectError(error.Password, run(rig, try detectOne(rig, "in")));
}

test "truncated and garbage 7z archives are corrupt" {
    const full = try tu.Rig.real(.{});
    defer full.deinit();
    try full.fixture("7z/t0.7z", "t0.7z");
    const bytes = try full.read("t0.7z");
    const Cut = struct { label: []const u8, data: []const u8 };
    const magic = "7z\xbc\xaf\x27\x1c";
    const zeros: [100]u8 = @splat(0);
    const flipped_tail = try full.arena().dupe(u8, bytes);
    flipped_tail[bytes.len - 3] ^= 0xff;
    const flipped_sig = try full.arena().dupe(u8, bytes);
    flipped_sig[10] ^= 0xff;
    const cases = [_]Cut{
        .{ .label = "magic only", .data = magic },
        .{ .label = "magic and zeros", .data = magic ++ zeros },
        .{ .label = "cut in the header", .data = bytes[0 .. bytes.len - 10] },
        .{ .label = "cut after signature", .data = bytes[0..40] },
        .{ .label = "flipped header byte", .data = flipped_tail },
        .{ .label = "flipped signature byte", .data = flipped_sig },
    };
    for (cases) |c| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try rig.write("in/bad.7z", c.data);
        testing.expectError(error.Corrupt, run(rig, try oneSet(rig, "in/bad.7z"))) catch |err| {
            std.debug.print("case: {s}\n", .{c.label});
            return err;
        };
        try requireNothingLeft(rig);
    }
}

test "an archive named like a switch works" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try rig.fixture("7z/t0.7z", "in/-so.7z");
    try files(try run(rig, try oneSet(rig, "in/-so.7z")), &.{ "bar", "foo" });
}

test "a missing 7zz program is not the archive's fault" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{"t0.7z"});
    rig.env.cfg.seven_zip = rig.path("no-such-7zz");
    try testing.expectError(error.Internal, run(rig, try detectOne(rig, "in")));
    try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
}

// ---- limits ----

test "limits on a real archive: entries, bytes, free space" {
    {
        const rig = try Rig.real(.{ .max_entries = 1 });
        defer rig.deinit();
        try copy7z(rig, &.{"t0.7z"});
        try testing.expectError(error.TooManyEntries, run(rig, try detectOne(rig, "in")));
        try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
    }
    {
        const rig = try Rig.real(.{ .max_entries = 2 });
        defer rig.deinit();
        try copy7z(rig, &.{"t0.7z"});
        _ = try run(rig, try detectOne(rig, "in"));
    }
    {
        const rig = try Rig.real(.{ .max_bytes = 7 });
        defer rig.deinit();
        try copy7z(rig, &.{"t0.7z"});
        try testing.expectError(error.TooLarge, run(rig, try detectOne(rig, "in")));
        try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
    }
    {
        const rig = try Rig.real(.{ .max_bytes = 8 });
        defer rig.deinit();
        try copy7z(rig, &.{"t0.7z"});
        _ = try run(rig, try detectOne(rig, "in"));
    }
}

test "the default free space probe works" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    const n = try (env_mod.FreeSpace{}).probe(rig.root);
    try testing.expect(n > 0);
}

test "default limits are the documented ones" {
    const c: env_mod.Config = .{};
    try testing.expectEqual(@as(usize, 1_000_000), c.max_entries);
    try testing.expectEqual(@as(i64, 1 << 30), c.bomb_min);
    try testing.expectEqual(@as(i64, 100), c.bomb_ratio);
    try testing.expectEqual(@as(i64, 0), c.max_bytes);
}

// ---- lifecycle ----

test "a re-run after a success is refused unless replace is set, and replace swaps" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{"t0.7z"});
    const set = try detectOne(rig, "in");
    _ = try run(rig, set);
    try testing.expectError(error.Exists, run(rig, set));
    try tu.expectStrings(&.{ "bar", "foo" }, try rig.tree("p/out"));
    try tu.expectStrings(&.{"out"}, try rig.children("p"));
    rig.env.cfg.replace = true;
    try rig.write("p/out/stale", "x");
    _ = try run(rig, set);
    try tu.expectStrings(&.{ "bar", "foo" }, try rig.tree("p/out"));
}

test "a failed replace keeps the existing destination" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try copy7z(rig, &.{"t0.7z"});
    _ = try run(rig, try detectOne(rig, "in"));
    try rig.fixture("7z/t3.7z", "enc/t3.7z");
    rig.env.cfg.replace = true;
    try testing.expectError(error.Password, run(rig, try detectOne(rig, "enc")));
    try tu.expectStrings(&.{ "bar", "foo" }, try rig.tree("p/out"));
    try tu.expectStrings(&.{"out"}, try rig.children("p"));
    // A failure after the extraction (a hostile name found by the listing)
    // keeps it as well.
    try zipw.write(rig, "evil/evil.zip", &.{ .{ .name = "g", .data = "two" }, .{ .name = "../x", .data = "evil" } });
    try testing.expectError(error.PathViolation, run(rig, try oneSet(rig, "evil/evil.zip")));
    try tu.expectStrings(&.{ "bar", "foo" }, try rig.tree("p/out"));
}

test "zip archives are extracted too" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try zipw.write(rig, "in/a.zip", &.{ .{ .name = "d/", .data = "" }, .{ .name = "d/x.txt", .data = "xx" }, .{ .name = "y", .data = "y" } });
    const res = try run(rig, try oneSet(rig, "in/a.zip"));
    try files(res, &.{ "d/x.txt", "y" });
}

// ---- hostile archives ----

test "zip with hostile names is refused and nothing escapes" {
    const names = [_][]const u8{ "../x", "d/../../up.txt", "/abs.txt", "C:/win.txt", "..\\x", "a\\..\\..\\x", "..", "../", "\\\\server\\share\\x", "a//../../x" };
    for (names) |name| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        const data: []const u8 = if (std.mem.endsWith(u8, name, "/")) "" else "evil";
        try zipw.write(rig, "arch/evil.zip", &.{ .{ .name = "ok/fine.txt", .data = "ok" }, .{ .name = name, .data = data } });
        testing.expectError(error.PathViolation, run(rig, try oneSet(rig, "arch/evil.zip"))) catch |err| {
            std.debug.print("name: {s}\n", .{name});
            return err;
        };
        try requireNothingLeft(rig);
    }
}

test "zip with odd but harmless names stays inside or is refused" {
    const long: [250]u8 = @splat('n');
    const names = [_][]const u8{ "nul\x00byte.txt", "a\x00/../../x", "a\\b.txt", "a:b.txt", "tab\tname\x01.txt", "line\nbreak.txt", "x\nSize = 0\nAttributes = D", "con.txt", "evil\u{202e}gpj.exe", "-rf", "@list.txt", &long };
    for (names) |name| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try zipw.write(rig, "arch/odd.zip", &.{.{ .name = name, .data = "content" }});
        if (run(rig, try oneSet(rig, "arch/odd.zip"))) |res| {
            for (res.files) |f| {
                try testing.expect(std.mem.indexOf(u8, f, "..") == null);
                try testing.expect(rig.exists(try std.fmt.allocPrint(rig.arena(), "p/out/{s}", .{f})));
            }
        } else |err| {
            try testing.expect(err == error.PathViolation or err == error.Corrupt);
            try requireNothingLeft(rig);
        }
        // Nothing next to the destination either way.
        for (try rig.children("")) |t| try testing.expect(!std.mem.eql(u8, t, "x") and !std.mem.eql(u8, t, "up.txt"));
    }
}

test "a zip with many entries is refused before extraction, and extracts within the cap" {
    {
        const rig = try Rig.real(.{ .max_entries = 1000 });
        defer rig.deinit();
        var ents: std.ArrayList(zipw.Ent) = .empty;
        for (0..3000) |i| try ents.append(rig.arena(), .{ .name = try std.fmt.allocPrint(rig.arena(), "d{d}/f{d}", .{ i % 50, i }), .data = "" });
        try zipw.write(rig, "arch/many.zip", ents.items);
        try testing.expectError(error.TooManyEntries, run(rig, try oneSet(rig, "arch/many.zip")));
        try requireNothingLeft(rig);
    }
    {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        var ents: std.ArrayList(zipw.Ent) = .empty;
        for (0..3000) |i| try ents.append(rig.arena(), .{ .name = try std.fmt.allocPrint(rig.arena(), "d{d}/f{d}", .{ i % 50, i }), .data = "x" });
        try zipw.write(rig, "arch/many.zip", ents.items);
        const res = try run(rig, try oneSet(rig, "arch/many.zip"));
        try testing.expectEqual(@as(usize, 3000), res.files.len);
    }
}

test "zip entries that are links or special files are refused" {
    const S_IFLNK: u32 = 0o120000;
    const Case = struct { name: []const u8, ents: []const zipw.Ent };
    const cases = [_]Case{
        .{ .name = "abs link", .ents = &.{ .{ .name = "ok.txt", .data = "x" }, .{ .name = "link", .data = "/etc/passwd", .mode = S_IFLNK | 0o777 } } },
        .{ .name = "up link", .ents = &.{.{ .name = "up", .data = "..", .mode = S_IFLNK | 0o777 }} },
        .{ .name = "link then file through", .ents = &.{ .{ .name = "esc", .data = "../..", .mode = S_IFLNK | 0o777 }, .{ .name = "esc/pwned.txt", .data = "evil" } } },
        .{ .name = "dangling", .ents = &.{.{ .name = "l", .data = "no-such-target", .mode = S_IFLNK | 0o777 }} },
        .{ .name = "device", .ents = &.{.{ .name = "dev", .data = "", .mode = 0o020000 | 0o666 }} },
        .{ .name = "fifo", .ents = &.{.{ .name = "pipe", .data = "", .mode = 0o010000 | 0o666 }} },
        .{ .name = "socket", .ents = &.{.{ .name = "sock", .data = "", .mode = 0o140000 | 0o666 }} },
    };
    for (cases) |c| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try zipw.write(rig, "arch/links.zip", c.ents);
        testing.expectError(error.PathViolation, run(rig, try oneSet(rig, "arch/links.zip"))) catch |err| {
            std.debug.print("case: {s}\n", .{c.name});
            return err;
        };
        try requireNothingLeft(rig);
    }
}

test "7z archives with symlinks are refused" {
    for ([_]struct { name: []const u8, target: []const u8 }{
        .{ .name = "abs-link", .target = "/etc/passwd" },
        .{ .name = "rel-link", .target = "a.txt" },
        .{ .name = "up-link", .target = "../../.." },
        .{ .name = "dangling", .target = "nowhere" },
    }) |l| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try rig.write("src/a.txt", "hello");
        try rig.tmp.dir.symLink(tu.io, l.target, try std.fmt.allocPrint(rig.arena(), "src/{s}", .{l.name}), .{});
        try rig.mkdir("arch");
        // -snl stores the link itself instead of what it points to.
        try rig.run7zz("src", "", &.{ "a", "-snl", "-bd", rig.path("arch/links.7z"), "a.txt", l.name });
        try testing.expectError(error.PathViolation, run(rig, try oneSet(rig, "arch/links.7z")));
        try requireNothingLeft(rig);
    }
}

test "a 7z archive with a hardlink entry extracts no link" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try rig.write("src/a.txt", "hello");
    try rig.tmp.dir.hardLink("src/a.txt", rig.tmp.dir, "src/b.txt", tu.io, .{});
    try rig.mkdir("arch");
    try rig.run7zz("src", "", &.{ "a", "-snh", "-bd", rig.path("arch/hard.7z"), "a.txt", "b.txt" });
    if (run(rig, try oneSet(rig, "arch/hard.7z"))) |res| {
        for (res.files) |f| {
            const st = try rig.tmp.dir.statFile(tu.io, try std.fmt.allocPrint(rig.arena(), "p/out/{s}", .{f}), .{ .follow_symlinks = false });
            try testing.expectEqual(@as(@TypeOf(st.nlink), 1), st.nlink);
        }
    } else |err| {
        try testing.expectEqual(error.PathViolation, err);
        try requireNothingLeft(rig);
    }
}

test "a compression bomb is refused from the listing" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    const zeros = try rig.arena().alloc(u8, 16 << 20);
    @memset(zeros, 0);
    try rig.write("src/zeros.bin", zeros);
    try rig.mkdir("arch");
    try rig.run7zz("src", "", &.{ "a", "-bd", "-mx5", rig.path("arch/bomb.7z"), "zeros.bin" });
    const st = try rig.tmp.dir.statFile(tu.io, "arch/bomb.7z", .{});
    try testing.expect(st.size < (16 << 20) / 100);
    const set = try oneSet(rig, "arch/bomb.7z");
    {
        rig.env.cfg.bomb_min = 1 << 20;
        rig.env.cfg.bomb_ratio = 100;
        try testing.expectError(error.Bomb, run(rig, set));
        try requireNothingLeft(rig);
    }
    {
        rig.env.cfg.bomb_min = 1 << 30;
        _ = try run(rig, set);
    }
    {
        rig.env.cfg.replace = true;
        rig.env.cfg.bomb_min = 1 << 20;
        rig.env.cfg.bomb_ratio = 1 << 20;
        _ = try run(rig, set);
    }
}

test "a zip whose header understates the size is corrupt and writes no more" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    const zeros = try rig.arena().alloc(u8, 8 << 20);
    @memset(zeros, 0);
    try rig.write("src/lie.bin", zeros);
    try rig.mkdir("arch");
    try rig.run7zz("src", "", &.{ "a", "-tzip", "-mx9", "-bd", rig.path("arch/lie.zip"), "lie.bin" });
    // The local header and the central directory both say 100 bytes.
    const raw = try rig.read("arch/lie.zip");
    try zipw.patchSizes(raw, 100);
    try rig.write("arch/lie.zip", raw);
    try testing.expectError(error.Corrupt, run(rig, try oneSet(rig, "arch/lie.zip")));
    try requireNothingLeft(rig);
}

test "encrypted zip and 7z archives: right, wrong and no password" {
    const Case = struct { file: []const u8, args: []const []const u8 };
    const cases = [_]Case{
        .{ .file = "enc.zip", .args = &.{ "-tzip", "-mem=ZipCrypto" } },
        .{ .file = "enc.zip", .args = &.{ "-tzip", "-mem=AES256" } },
        .{ .file = "enc.7z", .args = &.{"-mhe=off"} },
        .{ .file = "enc.7z", .args = &.{"-mhe=on"} },
    };
    for (cases) |c| {
        const base = try Rig.real(.{});
        defer base.deinit();
        try base.write("src/s.txt", "the secret");
        try base.mkdir("arch");
        var args: std.ArrayList([]const u8) = .empty;
        try args.appendSlice(base.arena(), &.{ "a", "-p", "-bd", base.path(try std.fmt.allocPrint(base.arena(), "arch/{s}", .{c.file})) });
        try args.appendSlice(base.arena(), c.args);
        try args.append(base.arena(), "s.txt");
        try base.run7zz("src", "right\nright\n", args.items);
        const rel = try std.fmt.allocPrint(base.arena(), "arch/{s}", .{c.file});
        const set = try oneSet(base, rel);

        base.env.cfg.password = "right";
        const res = try run(base, set);
        try files(res, &.{"s.txt"});
        try testing.expectEqualStrings("the secret", try base.read("p/out/s.txt"));

        base.env.cfg.replace = true;
        base.env.cfg.password = "wrong";
        try testing.expectError(error.Password, run(base, set));
        base.env.cfg.password = "";
        try testing.expectError(error.Password, run(base, set));
        try tu.expectStrings(&.{"out"}, try base.children("p"));
    }
}

test "truncated, flipped and half-present archives are corrupt" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    const data = try rig.arena().alloc(u8, 600 << 10);
    for (data, 0..) |*b, i| b.* = @truncate(i *% 31 +% (i >> 8));
    try rig.write("src/big.bin", data);
    try rig.mkdir("arch");
    try rig.run7zz("src", "", &.{ "a", "-bd", "-mx0", rig.path("arch/big.7z"), "big.bin" });
    try rig.run7zz("src", "", &.{ "a", "-bd", "-mx0", rig.path("arch/big.zip"), "big.bin" });
    for ([_][]const u8{ "big.7z", "big.zip" }) |name| {
        const whole = try rig.read(try std.fmt.allocPrint(rig.arena(), "arch/{s}", .{name}));
        for ([_]usize{ whole.len / 2, whole.len - 20, 40 }) |n| {
            const sub = try std.fmt.allocPrint(rig.arena(), "cut/{s}", .{name});
            try rig.write(sub, whole[0..n]);
            rig.env.cfg.replace = true;
            testing.expectError(error.Corrupt, run(rig, try oneSet(rig, sub))) catch |err| {
                std.debug.print("{s} cut at {d}\n", .{ name, n });
                return err;
            };
            try testing.expect(!rig.exists("p/out"));
        }
    }
    // A flipped byte in the data.
    const flip_src = try rig.arena().alloc(u8, 640 << 10);
    for (flip_src, 0..) |*b, i| b.* = "0123456789abcdef"[i % 16];
    try rig.write("src2/big.bin", flip_src);
    try rig.run7zz("src2", "", &.{ "a", "-bd", "-mx0", rig.path("arch/flip.7z"), "big.bin" });
    const w = try rig.read("arch/flip.7z");
    w[300 << 10] ^= 0xff;
    try rig.write("cut/flip.7z", w);
    try testing.expectError(error.Corrupt, run(rig, try oneSet(rig, "cut/flip.7z")));
}

test "7z volumes made by 7zz: complete, a missing middle volume and a lost tail" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    const data = try rig.arena().alloc(u8, 300 << 10);
    for (data, 0..) |*b, i| b.* = @truncate(i *% 7);
    try rig.write("src/big.bin", data);
    try rig.mkdir("arch");
    try rig.run7zz("src", "", &.{ "a", "-bd", "-mx0", "-v100k", rig.path("arch/vol.7z"), "big.bin" });
    const names = try rig.children("arch");
    try testing.expect(names.len >= 3);
    {
        const res = try run(rig, try detectOne(rig, "arch"));
        try files(res, &.{"big.bin"});
        try testing.expectEqualSlices(u8, data, try rig.read("p/out/big.bin"));
    }
    rig.env.cfg.replace = true;
    {
        try rig.write("mid/x", "");
        try rig.tmp.dir.deleteFile(tu.io, "mid/x");
        for ([_]usize{ 0, 2 }) |i| try rig.write(try std.fmt.allocPrint(rig.arena(), "mid/{s}", .{names[i]}), try rig.read(try std.fmt.allocPrint(rig.arena(), "arch/{s}", .{names[i]})));
        const sets = try detect.detect(&rig.env, rig.path("mid"));
        try testing.expectError(error.MissingVolume, run(rig, sets[0]));
    }
    {
        for (names[0 .. names.len - 1]) |n| try rig.write(try std.fmt.allocPrint(rig.arena(), "tail/{s}", .{n}), try rig.read(try std.fmt.allocPrint(rig.arena(), "arch/{s}", .{n})));
        try testing.expectError(error.Corrupt, run(rig, try detectOne(rig, "tail")));
    }
}

test "garbage that looks like an archive is corrupt" {
    const magic_rar5 = "Rar!\x1a\x07\x01\x00";
    const magic_7z = "7z\xbc\xaf\x27\x1c";
    const pad: [64]u8 = @splat(0xA5);
    const zeros: [4096]u8 = @splat(0);
    const cases = [_][]const u8{ &zeros, "this is not an archive at all\n", magic_7z ++ pad, magic_rar5 ++ pad, "", "PK\x03\x04" };
    for (cases) |content| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try rig.write("in/junk.rar", content);
        try testing.expectError(error.Corrupt, run(rig, try oneSet(rig, "in/junk.rar")));
        try requireNothingLeft(rig);
    }
}

test "an empty-directory archive gives a destination with only the directory" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try rig.mkdir("src/d");
    try rig.mkdir("arch");
    try rig.run7zz("src", "", &.{ "a", "-bd", rig.path("arch/empty.7z"), "d" });
    const res = try run(rig, try oneSet(rig, "arch/empty.7z"));
    try testing.expectEqual(@as(usize, 0), res.files.len);
    try tu.expectStrings(&.{"d"}, try rig.tree("p/out"));
}
