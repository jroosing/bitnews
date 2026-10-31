//! RAR and nested-archive tests on the real 7zz (internal/postproc/unpack
//! rar_test.go, rar_hostile_test.go, nested_test.go). The RAR files are
//! fixtures under testdata/unpack/rar, written by Go's builder
//! (rar_builder_test.go, stored files only; there is no RAR tool); 7zz is
//! the reader under test. See testdata/unpack/README.
const std = @import("std");
const testing = std.testing;
const tu = @import("unpack_testutil.zig");
const unpack = @import("unpack.zig");
const detect = @import("unpack_detect.zig");
const e2e = @import("unpack_e2e_test.zig");

const Rig = tu.Rig;
const Gens = [_][]const u8{ "rar4", "rar5" };

fn payload(a: std.mem.Allocator, n: usize, seed: u8) ![]u8 {
    const b = try a.alloc(u8, n);
    for (b, 0..) |*c, i| c.* = @as(u8, @truncate(i *% 7)) ^ seed;
    return b;
}

/// Puts rar/<fixture> into <rig>/in/<as> (a bare file name).
fn put(rig: *Rig, fixture: []const u8, as: []const u8) !void {
    try rig.fixture(try std.fmt.allocPrint(rig.arena(), "rar/{s}", .{fixture}), try std.fmt.allocPrint(rig.arena(), "in/{s}", .{as}));
}

fn putSet(rig: *Rig, dir: []const u8, names: []const []const u8) !void {
    for (names) |n| {
        try rig.fixture(try std.fmt.allocPrint(rig.arena(), "rar/{s}/{s}", .{ dir, n }), try std.fmt.allocPrint(rig.arena(), "in/{s}", .{n}));
    }
}

fn extract(rig: *Rig) !unpack.Result {
    return e2e.run(rig, try e2e.detectOne(rig, "in"));
}

fn gen(comptime g: []const u8, comptime rest: []const u8) []const u8 {
    return g ++ "-" ++ rest ++ ".rar";
}

/// Nothing may be left next to the destination: no destination, no staging.
fn requireClean(rig: *Rig) !void {
    try testing.expect(!rig.exists("p/out"));
    try testing.expect(!rig.exists("p/out.tmp"));
}

test "a real RAR4 archive is extracted and loses its executable bit" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "container.rar", "container.rar");
    const res = try extract(rig);
    try tu.expectStrings(&.{"container"}, res.files);
    try testing.expectEqualStrings("#!/bin/sh\nexit 0\n", try rig.read("p/out/container"));
    try testing.expectEqual(@as(u32, 0o644), try tu.modeOf(rig, "p/out/container"));
}

test "built single-volume RAR4 and RAR5 archives extract" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "built"), "built.rar");
        const res = try extract(rig);
        try tu.expectStrings(&.{ "docs/readme.txt", "empty" }, res.files);
        try testing.expectEqualStrings(if (std.mem.eql(u8, g, "rar4")) "hello RAR4" else "hello RAR5", try rig.read("p/out/docs/readme.txt"));
    }
}

test "multi-volume RAR sets extract: new style, old style, RAR5" {
    const Case = struct { dir: []const u8, names: []const []const u8 };
    const cases = [_]Case{
        .{ .dir = "multi-rar4-new", .names = &.{ "rel.part1.rar", "rel.part2.rar", "rel.part3.rar" } },
        .{ .dir = "multi-rar4-old", .names = &.{ "rel.rar", "rel.r00", "rel.r01" } },
        .{ .dir = "multi-rar5", .names = &.{ "rel.part01.rar", "rel.part02.rar", "rel.part03.rar" } },
    };
    for (cases) |c| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try putSet(rig, c.dir, c.names);
        const res = try extract(rig);
        try tu.expectStrings(&.{ "big.bin", "small.txt" }, res.files);
        try testing.expectEqualSlices(u8, try payload(rig.arena(), 5000, 1), try rig.read("p/out/big.bin"));
        try testing.expectEqualStrings("tail file", try rig.read("p/out/small.txt"));
    }
}

test "a multi-volume RAR set with a missing last volume is corrupt, a missing middle one is refused" {
    {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try putSet(rig, "multi-rar5-seed2", &.{ "rel.part1.rar", "rel.part2.rar" });
        try testing.expectError(error.Corrupt, extract(rig));
        try requireClean(rig);
    }
    {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try putSet(rig, "multi-rar5-seed2", &.{ "rel.part1.rar", "rel.part3.rar" });
        const sets = try detect.detect(&rig.env, rig.path("in"));
        try testing.expectError(error.MissingVolume, e2e.run(rig, sets[0]));
        try requireClean(rig);
    }
}

test "encrypted RAR: right, wrong and no password" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{ .password = "hunter2", .replace = true });
        defer rig.deinit();
        try put(rig, gen(g, "enc"), "enc.rar");
        const res = try extract(rig);
        try tu.expectStrings(&.{"s.txt"}, res.files);
        try testing.expectEqualStrings("the secret payload, long enough to span more than one AES block", try rig.read("p/out/s.txt"));
        rig.env.cfg.password = "hunter3";
        rig.env.cfg.replace = false;
        try rig.tmp.dir.deleteTree(tu.io, "p");
        try testing.expectError(error.Password, extract(rig));
        try requireClean(rig);
        rig.env.cfg.password = "";
        try testing.expectError(error.Password, extract(rig));
        try requireClean(rig);
    }
}

test "RAR archives with hostile names fail safely" {
    inline for (Gens) |g| {
        for (0..7) |i| {
            const rig = try Rig.real(.{});
            defer rig.deinit();
            try put(rig, try std.fmt.allocPrint(rig.arena(), "{s}-hostile-{d}.rar", .{ g, i }), "evil.rar");
            const sets = try detect.detect(&rig.env, rig.path("in"));
            testing.expectError(error.PathViolation, e2e.run(rig, sets[0])) catch |err| {
                std.debug.print("{s} hostile {d}\n", .{ g, i });
                return err;
            };
            try tu.expectStrings(&.{ "in", "p" }, try rig.children(""));
            try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
        }
    }
}

test "RAR archives with links and special entries are refused" {
    const Case = struct { name: []const u8, rar5_only: bool = false, copy: bool = false };
    const cases = [_]Case{
        .{ .name = "symlink-mode" },
        .{ .name = "unix-symlink", .rar5_only = true },
        .{ .name = "win-symlink", .rar5_only = true },
        .{ .name = "junction", .rar5_only = true },
        .{ .name = "hardlink", .rar5_only = true },
        .{ .name = "file-copy", .rar5_only = true, .copy = true },
        .{ .name = "fifo" },
        .{ .name = "char-device" },
        .{ .name = "socket" },
    };
    inline for (Gens) |g| {
        for (cases) |c| {
            if (c.rar5_only and std.mem.eql(u8, g, "rar4")) continue;
            const rig = try Rig.real(.{});
            defer rig.deinit();
            try put(rig, try std.fmt.allocPrint(rig.arena(), "{s}-special-{s}.rar", .{ g, c.name }), "links.rar");
            // 7zz lists a RAR5 file copy as a file and cannot extract it.
            const want: anyerror = if (c.copy) error.Unsupported else error.PathViolation;
            testing.expectError(want, extract(rig)) catch |err| {
                std.debug.print("{s} {s}\n", .{ g, c.name });
                return err;
            };
            try testing.expectEqual(@as(usize, 0), (try rig.children("p")).len);
        }
    }
}

test "RAR archives drop setuid and other mode bits" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "modes"), "modes.rar");
        _ = try extract(rig);
        for (try rig.tree("p/out")) |p| {
            const rel = try std.fmt.allocPrint(rig.arena(), "p/out/{s}", .{p});
            const st = try rig.tmp.dir.statFile(tu.io, rel, .{ .follow_symlinks = false });
            const mode = try tu.modeOf(rig, rel);
            try testing.expectEqual(@as(u32, if (st.kind == .directory) 0o755 else 0o644), mode);
        }
    }
}

test "a directory without permissions blocks neither the result nor the cleanup" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "locked"), "locked.rar");
        const res = try extract(rig);
        try tu.expectStrings(&.{"d/f"}, res.files);
        try testing.expectEqualStrings("inside", try rig.read("p/out/d/f"));
        // And when the unpack fails later, the staging directory still goes.
        try rig.tmp.dir.deleteTree(tu.io, "p");
        rig.env.cfg.max_bytes = 1;
        try testing.expectError(error.TooLarge, extract(rig));
        try testing.expect(!rig.exists("p/out.tmp"));
    }
}

test "RAR archives reject case duplicates" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "dup"), "dup.rar");
        try testing.expectError(error.PathViolation, extract(rig));
        try requireClean(rig);
    }
}

test "solid RAR archives extract" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "solid"), "solid.rar");
        const res = try extract(rig);
        try tu.expectStrings(&.{ "a", "b" }, res.files);
        try testing.expectEqualStrings("second", try rig.read("p/out/b"));
    }
}

test "names that sanitizing changes are renamed in place" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "names"), "names.rar");
        const res = try extract(rig);
        try tu.expectStrings(&.{ "dir/_con.txt", "dir/a_b_.txt", "trailing" }, res.files);
        try tu.expectStrings(&.{ "dir", "dir/_con.txt", "dir/a_b_.txt", "trailing" }, try rig.tree("p/out"));
    }
}

test "corrupt RAR archives fail cleanly" {
    inline for (Gens) |g| {
        const rig0 = try Rig.real(.{});
        defer rig0.deinit();
        try rig0.fixture("rar/" ++ g ++ "-corrupt-whole.rar", "whole.rar");
        const whole = try rig0.read("whole.rar");
        const magic_len: usize = if (std.mem.eql(u8, g, "rar5")) 8 else 7;
        const flip20 = try rig0.arena().dupe(u8, whole);
        flip20[20] ^= 0xff;
        const flip200 = try rig0.arena().dupe(u8, whole);
        flip200[200] ^= 0xff;
        const Case = struct { label: []const u8, data: []const u8 };
        const cases = [_]Case{
            .{ .label = "magic only", .data = whole[0..magic_len] },
            .{ .label = "cut right after magic", .data = whole[0..12] },
            .{ .label = "cut in the first header", .data = whole[0..30] },
            .{ .label = "cut inside file data", .data = whole[0 .. whole.len / 2] },
            .{ .label = "flipped header byte", .data = flip20 },
            .{ .label = "flipped data byte", .data = flip200 },
        };
        for (cases) |c| {
            const rig = try Rig.real(.{});
            defer rig.deinit();
            try rig.write("in/bad.rar", c.data);
            testing.expectError(error.Corrupt, extract(rig)) catch |err| {
                std.debug.print("{s} {s}\n", .{ g, c.label });
                return err;
            };
            try requireClean(rig);
        }
    }
}

// ---- nested ----

test "one level of nested archives is unpacked in place" {
    inline for (Gens) |g| {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, gen(g, "nested-basic"), "outer.rar");
        const res = try extract(rig);
        try testing.expect(res.nested);
        try tu.expectStrings(&.{ "movie.mkv", "readme.txt", "sub/x.nfo" }, res.files);
        try testing.expectEqual(@as(i64, "the movie".len + "read me".len + "nfo".len), res.bytes);
        try tu.expectStrings(&.{ "movie.mkv", "readme.txt", "sub", "sub/x.nfo" }, try rig.tree("p/out"));
        try testing.expectEqualStrings("the movie", try rig.read("p/out/movie.mkv"));
    }
}

test "a nested archive in a subdirectory is unpacked there" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-subdir.rar", "outer.rar");
    const res = try extract(rig);
    try tu.expectStrings(&.{"disc1/a.bin"}, res.files);
    try tu.expectStrings(&.{ "disc1", "disc1/a.bin" }, try rig.tree("p/out"));
}

test "a nested multi-volume set is unpacked" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-multivol.rar", "outer.rar");
    const res = try extract(rig);
    try tu.expectStrings(&.{"big.bin"}, res.files);
    try testing.expectEqualSlices(u8, try payload(rig.arena(), 3000, 9), try rig.read("p/out/big.bin"));
}

test "a nested 7z inside a RAR is unpacked" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-7z.rar", "outer.rar");
    const res = try extract(rig);
    try tu.expectStrings(&.{ "bar", "foo" }, res.files);
}

test "the second nesting level is left as is" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-second-level.rar", "outer.rar");
    const res = try extract(rig);
    try testing.expect(res.nested);
    try tu.expectStrings(&.{ "deep.rar", "middle.txt" }, res.files);
    try rig.fixture("rar/nested-second-level-deep.rar", "deep-ref.rar");
    try testing.expectEqualSlices(u8, try rig.read("deep-ref.rar"), try rig.read("p/out/deep.rar"));
}

test "a nested archive never overwrites what the outer one produced" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-same-name.rar", "outer.rar");
    const res = try extract(rig);
    try testing.expectEqual(@as(usize, 2), res.files.len);
    try testing.expectEqualStrings("from outer", try rig.read("p/out/same.txt"));
    try testing.expectEqualStrings("from inner", try rig.read("p/out/same.1.txt"));
}

test "hostile names in a nested archive fail the whole unpack" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-hostile.rar", "outer.rar");
    try testing.expectError(error.PathViolation, extract(rig));
    try requireClean(rig);
    try testing.expect(std.mem.startsWith(u8, rig.diag.message, "nested archive \"inner\": unpack: unsafe path in archive: path violation"));
}

test "a corrupt nested archive fails the unpack" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-corrupt.rar", "outer.rar");
    try testing.expectError(error.Corrupt, extract(rig));
    try requireClean(rig);
}

test "a nested set with a missing volume fails the unpack" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-missing-vol.rar", "outer.rar");
    try testing.expectError(error.MissingVolume, extract(rig));
    try requireClean(rig);
}

test "nested archives use the same password" {
    const rig = try Rig.real(.{ .password = "pw" });
    defer rig.deinit();
    try put(rig, "nested-password.rar", "outer.rar");
    const res = try extract(rig);
    try tu.expectStrings(&.{"s.txt"}, res.files);
    try testing.expectEqualStrings("secret", try rig.read("p/out/s.txt"));
    try rig.tmp.dir.deleteTree(tu.io, "p");
    rig.env.cfg.password = "other";
    try testing.expectError(error.Password, extract(rig));
    try requireClean(rig);
}

test "the byte cap covers the outer and nested archives together" {
    {
        const rig = try Rig.real(.{ .max_bytes = 4000 });
        defer rig.deinit();
        try put(rig, "nested-bytecap.rar", "outer.rar");
        _ = try extract(rig);
    }
    {
        const rig = try Rig.real(.{});
        defer rig.deinit();
        try put(rig, "nested-bytecap.rar", "outer.rar");
        try rig.fixture("rar/nested-bytecap-inner.rar", "inner-ref.rar");
        const inner_len = (try rig.read("inner-ref.rar")).len;
        // The outer archive holds 600 + inner bytes; the inner adds 600.
        rig.env.cfg.max_bytes = @intCast(600 + inner_len + 300);
        try testing.expectError(error.TooLarge, extract(rig));
    }
}

test "files without archive magic in the output are not touched" {
    const rig = try Rig.real(.{});
    defer rig.deinit();
    try put(rig, "nested-nomagic.rar", "outer.rar");
    const res = try extract(rig);
    try testing.expect(!res.nested);
    try tu.expectStrings(&.{ "fake.rar", "movie.mkv" }, res.files);
    try testing.expectEqualStrings("not an archive", try rig.read("p/out/fake.rar"));
}
