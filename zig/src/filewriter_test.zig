//! Tests of the file writer: the cases of internal/usenet/filewriter
//! (writer_test.go, snapshot_test.go, permission_test.go), the symlink games
//! of the data remover findings, a stress test with many keys and a small
//! open-file bound, and allocation failures.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const filewriter = @import("filewriter.zig");
const fsutil = @import("fsutil.zig");
const yenc_types = @import("yenc_types.zig");
const Writer = filewriter.Writer;
const Range = filewriter.Range;
const Part = yenc_types.Part;

pub fn rg(a: i64, b: i64) Range {
    return .{ .start = a, .end = b };
}

pub fn expectRanges(want: []const Range, got: []const Range) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| {
        testing.expect(w.eql(g)) catch |err| {
            std.debug.print("want [{d},{d}) got [{d},{d})\n", .{ w.start, w.end, g.start, g.end });
            return err;
        };
    }
}

/// Deterministic test bytes.
pub fn pattern(a: Allocator, n: usize) ![]u8 {
    const b = try a.alloc(u8, n);
    for (b, 0..) |*x, i| x.* = @truncate(i *% 7 +% (i >> 8));
    return b;
}

/// Cuts `data` into parts of at most `part_size` bytes, as a yEnc encoder
/// would, with a matching announced CRC32.
pub fn splitParts(a: Allocator, name: []const u8, data: []const u8, part_size: usize) ![]Part {
    var parts: std.ArrayList(Part) = .empty;
    const total = (data.len + part_size - 1) / part_size;
    var i: usize = 0;
    while (i * part_size < data.len) : (i += 1) {
        const lo = i * part_size;
        const hi = @min(lo + part_size, data.len);
        const sum = std.hash.Crc32.hash(data[lo..hi]);
        try parts.append(a, .{
            .name = name,
            .size = @intCast(data.len),
            .part_number = @intCast(i + 1),
            .total_parts = @intCast(total),
            .begin = @intCast(lo + 1),
            .end = @intCast(hi),
            .data = data[lo..hi],
            .crc32 = sum,
            .expected_crc32 = sum,
            .has_expected_crc = true,
        });
    }
    return parts.items;
}

/// A copy of `p` with other bytes and a CRC32 that no longer matches the
/// announced one.
pub fn corrupt(a: Allocator, p: Part) !Part {
    var q = p;
    const bad = try a.alloc(u8, p.data.len);
    @memset(bad, 0xee);
    q.data = bad;
    q.crc32 = std.hash.Crc32.hash(bad);
    return q;
}

/// A part for the 1-based inclusive range `begin`..`end` of `data`, with a
/// matching CRC32.
fn partOf(name: []const u8, data: []const u8, begin: i64, end: i64) Part {
    const d = data[@intCast(begin - 1)..@intCast(end)];
    const sum = std.hash.Crc32.hash(d);
    return .{ .name = name, .size = @intCast(data.len), .begin = begin, .end = end, .data = d, .crc32 = sum, .expected_crc32 = sum, .has_expected_crc = true };
}

pub const Rig = struct {
    tmp: testing.TmpDir,
    /// The absolute path of the temporary directory.
    root: []u8,
    /// The Download directory the Writer was made for (<root>/incomplete/dl).
    dir_path: []u8,
    w: *Writer,

    pub const rel_dir = "incomplete/dl";

    pub fn start(options: filewriter.Options) !*Rig {
        const rig = try testing.allocator.create(Rig);
        errdefer testing.allocator.destroy(rig);
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        const real = try rig.tmp.dir.realPathFileAlloc(testing.io, ".", testing.allocator);
        defer testing.allocator.free(real);
        rig.root = try testing.allocator.dupe(u8, real);
        errdefer testing.allocator.free(rig.root);
        rig.dir_path = try std.fmt.allocPrint(testing.allocator, "{s}/" ++ rel_dir, .{rig.root});
        errdefer testing.allocator.free(rig.dir_path);
        rig.w = try Writer.init(testing.allocator, testing.io, rig.dir_path, options);
        return rig;
    }

    pub fn deinit(rig: *Rig) void {
        rig.w.deinit();
        rig.tmp.cleanup();
        testing.allocator.free(rig.dir_path);
        testing.allocator.free(rig.root);
        testing.allocator.destroy(rig);
    }

    /// A new Writer for the same directory (the first one is left alone).
    pub fn another(rig: *Rig, options: filewriter.Options) !*Writer {
        return Writer.init(testing.allocator, testing.io, rig.dir_path, options);
    }

    pub fn relPath(a: Allocator, name: []const u8) ![]u8 {
        return std.fmt.allocPrint(a, rel_dir ++ "/{s}", .{name});
    }

    /// The contents of the file for `key`.
    pub fn read(rig: *Rig, a: Allocator, key: u32) ![]u8 {
        const st = (try rig.w.ranges(a, key)).?;
        return rig.readNamed(a, st.name);
    }

    pub fn readNamed(rig: *Rig, a: Allocator, name: []const u8) ![]u8 {
        return rig.tmp.dir.readFileAlloc(testing.io, try relPath(a, name), a, .limited(1 << 26));
    }

    pub fn put(rig: *Rig, a: Allocator, name: []const u8, data: []const u8) !void {
        try rig.tmp.dir.writeFile(testing.io, .{ .sub_path = try relPath(a, name), .data = data });
    }

    /// The names in the Download directory, sorted.
    pub fn list(rig: *Rig, a: Allocator) ![][]const u8 {
        return listIn(rig.tmp.dir, a, rel_dir);
    }
};

pub fn listIn(parent: Io.Dir, a: Allocator, sub: []const u8) ![][]const u8 {
    var d = try parent.openDir(testing.io, sub, .{ .iterate = true });
    defer d.close(testing.io);
    var out: std.ArrayList([]const u8) = .empty;
    var it = d.iterate();
    while (try it.next(testing.io)) |e| try out.append(a, try a.dupe(u8, e.name));
    std.mem.sort([]const u8, out.items, {}, struct {
        fn less(_: void, x: []const u8, y: []const u8) bool {
            return std.mem.lessThan(u8, x, y);
        }
    }.less);
    return out.items;
}

fn newArena() std.heap.ArenaAllocator {
    return .init(testing.allocator);
}

fn writeAll(w: *Writer, key: u32, parts: []const Part) !void {
    for (parts) |p| try w.writePart(key, p);
}

// ---- writing ----

test "a single part file is byte exact" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 1000);

    try writeAll(rig.w, 0, try splitParts(a, "movie.mkv", data, 1000));

    try testing.expectEqualSlices(u8, data, try rig.readNamed(a, "movie.mkv"));
}

test "the directory is created, with its parents" {
    var ar = newArena();
    defer ar.deinit();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    _ = try rig.tmp.dir.statFile(testing.io, Rig.rel_dir, .{});
    try testing.expectEqualStrings(rig.dir_path, rig.w.dirPath());
    try testing.expectError(error.EmptyDirectory, Writer.init(testing.allocator, testing.io, "", .{}));
}

const Job = struct {
    w: *Writer,
    key: u32,
    parts: []const Part,
    err: ?anyerror = null,

    fn run(j: *Job) void {
        for (j.parts) |p| j.w.writePart(j.key, p) catch |err| {
            j.err = err;
            return;
        };
    }
};

fn shuffle(prng: *std.Random.DefaultPrng, parts: []Part) void {
    prng.random().shuffleWithIndex(Part, parts, u32);
}

test "out of order parts of files written in parallel are byte exact" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    var prng: std.Random.DefaultPrng = .init(1);
    const nfiles = 6;
    var contents: [nfiles][]u8 = undefined;
    var jobs: [nfiles]Job = undefined;
    var threads: [nfiles]std.Thread = undefined;
    for (0..nfiles) |k| {
        contents[k] = try pattern(a, 5000 + k * 777);
        const parts = try splitParts(a, try std.fmt.allocPrint(a, "f{d}.bin", .{k}), contents[k], 97);
        shuffle(&prng, parts);
        jobs[k] = .{ .w = rig.w, .key = @intCast(k), .parts = parts };
    }
    for (&threads, &jobs) |*t, *j| t.* = try std.Thread.spawn(.{}, Job.run, .{j});
    for (threads) |t| t.join();
    for (jobs) |j| if (j.err) |err| return err;

    for (0..nfiles) |k| {
        const got = try rig.readNamed(a, try std.fmt.allocPrint(a, "f{d}.bin", .{k}));
        try testing.expectEqualSlices(u8, contents[k], got);
    }
}

test "hostile names stay inside the directory" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const long_a = try a.alloc(u8, 600);
    @memset(long_a, 'a');
    const long = try std.mem.concat(a, u8, &.{ long_a, ".rar" });
    const names = [_][]const u8{
        "../../etc/passwd", "/etc/passwd",     "C:\\Windows\\evil.dll", "a\x00b.txt",
        "con.txt",          long,              "",                      "..",
        ".",                "\u{202e}exe.txt", "tab\there\n.bin",
    };
    for (names, 0..) |name, i| {
        const data = try pattern(a, 50);
        try writeAll(rig.w, @intCast(i), try splitParts(a, name, data, 50));
        const st = (try rig.w.ranges(a, @intCast(i))).?;
        try testing.expect(std.mem.indexOfScalar(u8, st.name, '/') == null);
        try testing.expect(!std.mem.eql(u8, st.name, "..") and !std.mem.eql(u8, st.name, "."));
        try testing.expect(st.name.len <= 255);
        try testing.expectEqualSlices(u8, data, try rig.readNamed(a, st.name));
    }
    try testing.expectEqual(names.len, (try rig.list(a)).len);
    // Nothing appears next to the directory.
    const parent = try listIn(rig.tmp.dir, a, "incomplete");
    try testing.expectEqual(@as(usize, 1), parent.len);
    try testing.expectEqualStrings("dl", parent[0]);
    const top = try listIn(rig.tmp.dir, a, ".");
    try testing.expectEqual(@as(usize, 1), top.len);
}

test "the same name for two keys gets distinct files" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const d0 = try pattern(a, 100);
    const d1 = try a.dupe(u8, d0[1..]);
    const d1x = try std.mem.concat(a, u8, &.{ d1, &[_]u8{9} });
    try rig.w.writePart(0, (try splitParts(a, "same.bin", d0, 100))[0]);
    try rig.w.writePart(1, (try splitParts(a, "same.bin", d1x, 100))[0]);

    const s0 = (try rig.w.ranges(a, 0)).?;
    const s1 = (try rig.w.ranges(a, 1)).?;
    try testing.expect(!std.mem.eql(u8, s0.name, s1.name));
    try testing.expectEqualStrings("same.bin", s0.name);
    try testing.expectEqualStrings("same.1.bin", s1.name);
    try testing.expectEqualSlices(u8, d0, try rig.readNamed(a, s0.name));
    try testing.expectEqualSlices(u8, d1x, try rig.readNamed(a, s1.name));
}

test "a file that exists is not overwritten" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.put(a, "x.bin", "precious");

    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 10), 10))[0]);

    try testing.expectEqualStrings("precious", try rig.readNamed(a, "x.bin"));
    try testing.expect(!std.mem.eql(u8, "x.bin", (try rig.w.ranges(a, 0)).?.name));
}

test "a symlink at the target name is not followed" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.tmp.dir.writeFile(testing.io, .{ .sub_path = "victim", .data = "victim" });
    try rig.tmp.dir.symLink(testing.io, "../../victim", Rig.rel_dir ++ "/x.bin", .{});
    try rig.tmp.dir.symLink(testing.io, "../../not-there", Rig.rel_dir ++ "/y.bin", .{});

    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 10), 10))[0]);
    try rig.w.writePart(1, (try splitParts(a, "y.bin", try pattern(a, 10), 10))[0]);

    try testing.expectEqualStrings("victim", try rig.tmp.dir.readFileAlloc(testing.io, "victim", a, .limited(100)));
    try testing.expectError(error.FileNotFound, rig.tmp.dir.statFile(testing.io, "not-there", .{}));
    try testing.expectEqualStrings("x.1.bin", (try rig.w.ranges(a, 0)).?.name);
    try testing.expectEqualStrings("y.1.bin", (try rig.w.ranges(a, 1)).?.name);
}

test "a symlinked directory planted at the target name is not entered" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.tmp.dir.createDir(testing.io, "outside", .default_dir);
    try rig.tmp.dir.symLink(testing.io, "../../outside", Rig.rel_dir ++ "/x.bin", .{ .is_directory = true });

    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 10), 10))[0]);

    try testing.expectEqual(@as(usize, 0), (try listIn(rig.tmp.dir, a, "outside")).len);
}

test "a size that differs between parts is refused" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 200), 100);
    try rig.w.writePart(0, parts[0]);

    var bad = parts[1];
    bad.size = 300;
    try testing.expectError(error.SizeMismatch, rig.w.writePart(0, bad));

    const st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{rg(0, 100)}, st.done);
}

test "invalid parts are refused and create no file" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const base = (try splitParts(a, "x.bin", try pattern(a, 200), 100))[0];
    const Case = struct { name: []const u8, expect: anyerror, f: *const fn (p: *Part) void };
    const cases = [_]Case{
        .{ .name = "begin zero", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.begin = 0;
            }
        }.f },
        .{ .name = "begin negative", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.begin = -5;
            }
        }.f },
        .{ .name = "end past size", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.begin = 150;
                p.end = 249;
            }
        }.f },
        .{ .name = "end before begin", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.begin = 50;
                p.end = 10;
            }
        }.f },
        .{ .name = "short data", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.data = p.data[0..99];
            }
        }.f },
        .{ .name = "long data", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.end = 99;
            }
        }.f },
        .{ .name = "size zero", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.size = 0;
            }
        }.f },
        .{ .name = "size negative", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.size = -1;
            }
        }.f },
        .{ .name = "size over 1 TiB", .expect = error.TooLarge, .f = struct {
            fn f(p: *Part) void {
                p.size = (1 << 40) + 1;
            }
        }.f },
        .{ .name = "empty data", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.data = &.{};
                p.begin = 5;
                p.end = 4;
            }
        }.f },
        .{ .name = "huge range", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.begin = 1;
                p.end = std.math.maxInt(i64);
            }
        }.f },
        .{ .name = "begin max", .expect = error.InvalidPart, .f = struct {
            fn f(p: *Part) void {
                p.begin = std.math.maxInt(i64);
                p.end = std.math.maxInt(i64);
            }
        }.f },
        .{ .name = "size max", .expect = error.TooLarge, .f = struct {
            fn f(p: *Part) void {
                p.size = std.math.maxInt(i64);
            }
        }.f },
    };
    for (cases) |c| {
        const rig = try Rig.start(.{});
        defer rig.deinit();
        var p = base;
        c.f(&p);
        testing.expectError(c.expect, rig.w.writePart(0, p)) catch |err| {
            std.debug.print("case: {s}\n", .{c.name});
            return err;
        };
        // An invalid first part must not create a file.
        try testing.expectEqual(@as(usize, 0), (try rig.list(a)).len);
        try testing.expect((try rig.w.ranges(a, 0)) == null);
    }
}

test "a key that does not fit 31 bits is refused" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const p = (try splitParts(a, "x.bin", try pattern(a, 10), 10))[0];
    try testing.expectError(error.InvalidPart, rig.w.writePart(filewriter.max_key + 1, p));
    try rig.w.writePart(filewriter.max_key, p);
}

test "a CRC mismatch is damaged, then healed by a good duplicate" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 300);
    const parts = try splitParts(a, "x.bin", data, 100);
    try rig.w.writePart(0, parts[0]);
    try rig.w.writePart(0, try corrupt(a, parts[1]));
    try rig.w.writePart(0, parts[2]);

    var st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{rg(100, 200)}, st.damaged);
    try expectRanges(&.{ rg(0, 100), rg(200, 300) }, st.done);
    try expectRanges(&.{}, st.missing);
    try testing.expect(!st.complete());
    const got = try rig.read(a, 0);
    try testing.expect(std.mem.allEqual(u8, got[100..200], 0xee)); // the best copy is kept

    try rig.w.writePart(0, parts[1]);

    st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{}, st.damaged);
    try testing.expect(st.complete());
    try testing.expectEqualSlices(u8, data, try rig.read(a, 0));
}

test "a damaged duplicate never clobbers a good part" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 200);
    const parts = try splitParts(a, "x.bin", data, 100);
    try writeAll(rig.w, 0, parts);

    try rig.w.writePart(0, try corrupt(a, parts[0]));

    const st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{}, st.damaged);
    try testing.expect(st.complete());
    try testing.expectEqualSlices(u8, data, try rig.read(a, 0));
}

test "a damaged overlap only writes the bytes without a good copy" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 100);
    try rig.w.writePart(0, partOf("x.bin", data, 21, 40)); // a good copy of [20,40)

    const whole = partOf("x.bin", data, 1, 100);
    try rig.w.writePart(0, try corrupt(a, whole)); // a damaged copy of [0,100)

    const st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{rg(20, 40)}, st.done);
    try expectRanges(&.{ rg(0, 20), rg(40, 100) }, st.damaged);
    const got = try rig.read(a, 0);
    try testing.expectEqualSlices(u8, data[20..40], got[20..40]);
    try testing.expect(std.mem.allEqual(u8, got[0..20], 0xee));
    try testing.expect(std.mem.allEqual(u8, got[40..], 0xee));
}

test "a part without an announced CRC counts as good" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    var p = (try splitParts(a, "x.bin", try pattern(a, 50), 50))[0];
    p.has_expected_crc = false;
    p.expected_crc32 = 0;

    try rig.w.writePart(0, p);

    try testing.expect((try rig.w.ranges(a, 0)).?.complete());
}

test "ranges: done, damaged and missing with gaps and overlaps" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 1000);
    for ([_][2]i64{ .{ 101, 200 }, .{ 151, 300 }, .{ 201, 250 }, .{ 501, 600 } }) |be| {
        try rig.w.writePart(0, partOf("x.bin", data, be[0], be[1]));
    }
    try rig.w.writePart(0, try corrupt(a, partOf("x.bin", data, 701, 800)));

    const st = (try rig.w.ranges(a, 0)).?;

    try testing.expectEqual(@as(i64, 1000), st.size);
    try testing.expectEqualStrings("x.bin", st.name);
    try expectRanges(&.{ rg(100, 300), rg(500, 600) }, st.done);
    try expectRanges(&.{rg(700, 800)}, st.damaged);
    try expectRanges(&.{ rg(0, 100), rg(300, 500), rg(600, 700), rg(800, 1000) }, st.missing);
    try expectRanges(st.missing, st.pending);
    try expectRanges(&.{}, st.lost);
    try testing.expectEqual(@as(i64, 300), st.doneBytes());
    try testing.expectEqual(@as(i64, 100), st.damagedBytes());
    try testing.expectEqual(@as(i64, 600), st.missingBytes());
    try testing.expect(!st.complete());
}

test "ranges of an unknown key" {
    var ar = newArena();
    defer ar.deinit();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try testing.expect((try rig.w.ranges(ar.allocator(), 42)) == null);
}

test "markMissing separates lost from pending" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 400), 100);
    try rig.w.writePart(0, parts[0]);

    try rig.w.markMissing(0, rg(100, 200));
    try rig.w.markMissing(0, rg(0, 150)); // done bytes are unaffected

    var st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{rg(100, 200)}, st.lost);
    try expectRanges(&.{rg(100, 400)}, st.missing);
    try expectRanges(&.{rg(200, 400)}, st.pending);

    try rig.w.writePart(0, parts[1]); // a late arrival clears the mark
    st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{}, st.lost);
    try expectRanges(&.{rg(200, 400)}, st.missing);
}

test "markMissing refuses an unknown key and a bad range" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try testing.expectError(error.UnknownFile, rig.w.markMissing(7, rg(0, 1)));
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 100))[0]);
    try testing.expectError(error.InvalidPart, rig.w.markMissing(0, rg(50, 101)));
    try testing.expectError(error.InvalidPart, rig.w.markMissing(0, rg(-1, 10)));
    try testing.expectError(error.InvalidPart, rig.w.markMissing(0, rg(10, 5)));
}

test "the state is a copy" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 50))[0]);
    const before = (try rig.w.ranges(a, 0)).?;
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 50))[1]);
    try expectRanges(&.{rg(0, 50)}, before.done);
    try expectRanges(&.{rg(0, 100)}, (try rig.w.ranges(a, 0)).?.done);
}

test "concurrent duplicates of one key are safe" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 20000);
    const parts = try splitParts(a, "x.bin", data, 100);
    const n = 8;
    var jobs: [n]Job = undefined;
    var threads: [n]std.Thread = undefined;
    for (0..n) |g| {
        var prng: std.Random.DefaultPrng = .init(g);
        const ps = try a.dupe(Part, parts);
        shuffle(&prng, ps);
        // Odd workers also send damaged copies of some parts first.
        var mixed: std.ArrayList(Part) = .empty;
        for (ps) |p| {
            if (g % 2 == 1 and @rem(p.begin, 300) == 1) try mixed.append(a, try corrupt(a, p));
            try mixed.append(a, p);
        }
        jobs[g] = .{ .w = rig.w, .key = 0, .parts = mixed.items };
    }
    for (&threads, &jobs) |*t, *j| t.* = try std.Thread.spawn(.{}, Job.run, .{j});
    for (threads) |t| t.join();
    for (jobs) |j| if (j.err) |err| return err;

    try testing.expect((try rig.w.ranges(a, 0)).?.complete());
    try testing.expectEqualSlices(u8, data, try rig.read(a, 0));
    try testing.expectEqual(@as(usize, 1), (try rig.list(a)).len);
}

// ---- the pool of open files ----

fn openFds(a: Allocator) ?usize {
    var d = Io.Dir.openDirAbsolute(testing.io, "/dev/fd", .{ .iterate = true }) catch return null;
    defer d.close(testing.io);
    var n: usize = 0;
    var it = d.iterate();
    while (it.next(testing.io) catch return null) |_| n += 1;
    _ = a;
    return n;
}

test "the number of open files is bounded and evicted files stay writable" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const files = 200;
    const max_open = 8;
    const base = openFds(a) orelse return error.SkipZigTest;
    const rig = try Rig.start(.{ .max_open_files = max_open });
    defer rig.deinit();
    var contents: [files][]u8 = undefined;
    var parts: [files][]Part = undefined;
    for (0..files) |k| {
        contents[k] = try pattern(a, 200 + k);
        parts[k] = try splitParts(a, try std.fmt.allocPrint(a, "f{d:0>3}.bin", .{k}), contents[k], 100);
        try rig.w.writePart(@intCast(k), parts[k][0]);
        // The pool, the Download directory, the temporary directory and the
        // listing of /dev/fd itself.
        try testing.expect(openFds(a).? - base <= max_open + 3);
    }
    for (0..files) |k| {
        for (parts[k][1..]) |p| try rig.w.writePart(@intCast(k), p);
        try testing.expect(openFds(a).? - base <= max_open + 3);
    }
    for (0..files) |k| try testing.expectEqualSlices(u8, contents[k], try rig.read(a, @intCast(k)));
}

test "the bound holds under concurrency" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const files = 40;
    const max_open = 4;
    const rig = try Rig.start(.{ .max_open_files = max_open });
    defer rig.deinit();
    var contents: [files][]u8 = undefined;
    var jobs: [files]Job = undefined;
    var threads: [files]std.Thread = undefined;
    for (0..files) |k| {
        contents[k] = try pattern(a, 3000 + k);
        const ps = try splitParts(a, try std.fmt.allocPrint(a, "f{d:0>3}.bin", .{k}), contents[k], 50);
        var prng: std.Random.DefaultPrng = .init(k);
        shuffle(&prng, ps);
        jobs[k] = .{ .w = rig.w, .key = @intCast(k), .parts = ps };
    }
    for (&threads, &jobs) |*t, *j| t.* = try std.Thread.spawn(.{}, Job.run, .{j});
    for (threads) |t| t.join();
    for (jobs) |j| if (j.err) |err| return err;
    for (0..files) |k| try testing.expectEqualSlices(u8, contents[k], try rig.read(a, @intCast(k)));
    // Best effort while threads contend, tight once they are done: a last
    // write evicts down to the bound.
    try testing.expect(rig.w.lru_len <= max_open + files);
}

// ---- finishing ----

test "finish syncs and closes, and keeps the bookkeeping" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data = try pattern(a, 300);
    const parts = try splitParts(a, "x.bin", data, 100);
    try rig.w.writePart(0, parts[0]);
    try rig.w.writePart(0, parts[1]);

    try rig.w.finish(0);

    try testing.expectEqualSlices(u8, data[0..200], (try rig.read(a, 0))[0..200]);
    try expectRanges(&.{rg(0, 200)}, (try rig.w.ranges(a, 0)).?.done);
    try testing.expectEqual(@as(usize, 0), rig.w.lru_len);
    try rig.w.writePart(0, parts[2]); // a finished file is reopened on demand
    try testing.expectEqualSlices(u8, data, try rig.read(a, 0));
}

test "finish of an unknown key" {
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try testing.expectError(error.UnknownFile, rig.w.finish(3));
}

test "sync and close succeed, close is idempotent, writes after it fail" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{ .max_open_files = 2 });
    defer rig.deinit();
    const data = try pattern(a, 500);
    for (0..5) |k| {
        try rig.w.writePart(@intCast(k), (try splitParts(a, try std.fmt.allocPrint(a, "f{d}", .{k}), data, 500))[0]);
    }

    try rig.w.sync();
    try rig.w.close();
    try rig.w.close();

    try testing.expectError(error.Closed, rig.w.writePart(0, (try splitParts(a, "f0", data, 500))[0]));
    try testing.expectError(error.Closed, rig.w.restore(a, ""));
    // The bookkeeping and the snapshot still answer after the close.
    try testing.expect((try rig.w.ranges(a, 0)).?.complete());
    _ = try rig.w.snapshot(a);
    try rig.w.sync();
    for (0..5) |k| {
        try testing.expectEqualSlices(u8, data, try rig.readNamed(a, try std.fmt.allocPrint(a, "f{d}", .{k})));
    }
}

// ---- hostile file systems ----

test "the reopen of a finished file refuses a symlink swapped in" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 200), 100);
    try rig.w.writePart(0, parts[0]);
    try rig.w.finish(0);
    try rig.tmp.dir.writeFile(testing.io, .{ .sub_path = "victim", .data = "victim" });
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/x.bin");
    try rig.tmp.dir.symLink(testing.io, "../../victim", Rig.rel_dir ++ "/x.bin", .{});

    try testing.expectError(error.SymLinkLoop, rig.w.writePart(0, parts[1]));

    try testing.expectEqualStrings("victim", try rig.tmp.dir.readFileAlloc(testing.io, "victim", a, .limited(100)));
    try expectRanges(&.{rg(0, 100)}, (try rig.w.ranges(a, 0)).?.done);
}

test "a symlink swapped in while the file is open is not followed after an eviction" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{ .max_open_files = 1 });
    defer rig.deinit();
    const parts0 = try splitParts(a, "a.bin", try pattern(a, 200), 100);
    const parts1 = try splitParts(a, "b.bin", try pattern(a, 200), 100);
    try rig.w.writePart(0, parts0[0]);
    try rig.w.writePart(1, parts1[0]); // evicts a.bin
    try rig.tmp.dir.writeFile(testing.io, .{ .sub_path = "victim", .data = "victim" });
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/a.bin");
    try rig.tmp.dir.symLink(testing.io, "../../victim", Rig.rel_dir ++ "/a.bin", .{});

    try testing.expectError(error.SymLinkLoop, rig.w.writePart(0, parts0[1]));
    try rig.w.writePart(1, parts1[1]); // the other file is fine

    try testing.expectEqualStrings("victim", try rig.tmp.dir.readFileAlloc(testing.io, "victim", a, .limited(100)));
}

extern "c" fn mkfifo(path: [*:0]const u8, mode: std.posix.mode_t) c_int;

test "a FIFO swapped in is refused without blocking" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 200), 100);
    try rig.w.writePart(0, parts[0]);
    try rig.w.finish(0);
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/x.bin");
    const path = try std.fmt.allocPrintSentinel(a, "{s}/x.bin", .{rig.dir_path}, 0);
    if (mkfifo(path, 0o600) != 0) return error.SkipZigTest;

    try testing.expectError(error.UnsafeFile, rig.w.writePart(0, parts[1]));
}

// The Writer holds the directory open, so swapping the directory for a
// symlink afterwards cannot redirect its writes.
test "writes stay in the directory even when it is swapped for a symlink" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.tmp.dir.createDir(testing.io, "outside", .default_dir);
    try rig.tmp.dir.rename(Rig.rel_dir, rig.tmp.dir, "incomplete/dl.moved", testing.io);
    try rig.tmp.dir.symLink(testing.io, "../outside", Rig.rel_dir, .{ .is_directory = true });

    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 10), 10))[0]);
    try rig.w.writePart(1, (try splitParts(a, "y.bin", try pattern(a, 10), 10))[0]);
    try rig.w.finish(0);
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 10), 10))[0]); // a reopen
    try rig.w.close();

    try testing.expectEqual(@as(usize, 0), (try listIn(rig.tmp.dir, a, "outside")).len);
    const moved = try listIn(rig.tmp.dir, a, "incomplete/dl.moved");
    try testing.expectEqual(@as(usize, 2), moved.len);
}

test "a failed write leaves the bookkeeping unchanged" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 200), 100);
    try rig.w.writePart(0, parts[0]);
    try rig.w.finish(0);
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/x.bin");
    try rig.tmp.dir.createDir(testing.io, Rig.rel_dir ++ "/x.bin", .default_dir); // the reopen must fail

    try testing.expectError(error.IsDir, rig.w.writePart(0, parts[1]));

    const st = (try rig.w.ranges(a, 0)).?;
    try expectRanges(&.{rg(0, 100)}, st.done);
    try expectRanges(&.{rg(100, 200)}, st.missing);
}

test "a file that vanished comes back as not found" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 200), 100);
    try rig.w.writePart(0, parts[0]);
    try rig.w.finish(0);
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/x.bin");

    try testing.expectError(error.FileNotFound, rig.w.writePart(0, parts[1]));
}

fn expectPermissionDetail(a: Allocator, folder: []const u8) !void {
    const msg = try fsutil.writeErrorDetail(a, folder, fsutil.effectiveUid());
    try testing.expect(std.mem.indexOf(u8, msg, folder) != null);
    const uid = try std.fmt.allocPrint(a, "uid {d}", .{fsutil.effectiveUid()});
    try testing.expect(std.mem.indexOf(u8, msg, uid) != null);
}

test "an unwritable parent fails naming the folder and the user" {
    if (std.c.geteuid() == 0) return error.SkipZigTest;
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.createDir(testing.io, "incomplete", .default_dir);
    try tmp.dir.setFilePermissions(testing.io, "incomplete", .fromMode(0o555), .{});
    defer tmp.dir.setFilePermissions(testing.io, "incomplete", .fromMode(0o755), .{}) catch {};
    const root = try tmp.dir.realPathFileAlloc(testing.io, ".", a);
    const path = try std.fmt.allocPrint(a, "{s}/incomplete/dl", .{root});

    if (Writer.init(testing.allocator, testing.io, path, .{})) |w| {
        w.deinit();
        return error.TestUnexpectedResult;
    } else |err| try testing.expect(fsutil.isPermissionError(err));
    const folder = fsutil.deepestExistingParent(testing.io, path);
    try testing.expectEqualStrings(try std.fmt.allocPrint(a, "{s}/incomplete", .{root}), folder);
    try expectPermissionDetail(a, folder);
}

test "an unwritable Download folder fails the first write naming the folder and the user" {
    if (std.c.geteuid() == 0) return error.SkipZigTest;
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.tmp.dir.setFilePermissions(testing.io, Rig.rel_dir, .fromMode(0o555), .{});
    defer rig.tmp.dir.setFilePermissions(testing.io, Rig.rel_dir, .fromMode(0o755), .{}) catch {};

    if (rig.w.writePart(0, (try splitParts(a, "movie.mkv", try pattern(a, 10), 10))[0])) |_| {
        return error.TestUnexpectedResult;
    } else |err| try testing.expect(fsutil.isPermissionError(err));
    try expectPermissionDetail(a, rig.w.dirPath());
    try testing.expect((try rig.w.ranges(a, 0)) == null);
}

// ---- snapshot and restore ----

fn expectSameState(want: filewriter.FileState, got: filewriter.FileState) !void {
    try testing.expectEqual(want.key, got.key);
    try testing.expectEqualStrings(want.name, got.name);
    try testing.expectEqual(want.size, got.size);
    try expectRanges(want.done, got.done);
    try expectRanges(want.damaged, got.damaged);
    try expectRanges(want.missing, got.missing);
    try expectRanges(want.lost, got.lost);
    try expectRanges(want.pending, got.pending);
}

test "snapshot and restore resume in place" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const data_a = try pattern(a, 1000);
    const data_b = try pattern(a, 700);
    const parts_a = try splitParts(a, "a.bin", data_a, 100);
    const parts_b = try splitParts(a, "b.bin", data_b, 100);
    for ([_]usize{ 0, 1, 5, 9 }) |i| try rig.w.writePart(0, parts_a[i]);
    try rig.w.writePart(0, try corrupt(a, parts_a[3]));
    try rig.w.markMissing(0, rg(600, 700));
    try rig.w.writePart(4, parts_b[2]);
    const want0 = (try rig.w.ranges(a, 0)).?;
    const want4 = (try rig.w.ranges(a, 4)).?;

    const snap = try rig.w.snapshot(a);
    try rig.w.close();

    const w2 = try rig.another(.{});
    defer w2.deinit();
    const reset = try w2.restore(a, snap);

    try testing.expectEqual(@as(usize, 0), reset.len);
    try expectSameState(want0, (try w2.ranges(a, 0)).?);
    try expectSameState(want4, (try w2.ranges(a, 4)).?);
    try writeAll(w2, 0, parts_a);
    try writeAll(w2, 4, parts_b);
    try testing.expectEqualSlices(u8, data_a, try rig.readNamed(a, "a.bin"));
    try testing.expectEqualSlices(u8, data_b, try rig.readNamed(a, "b.bin"));
    const names = try rig.list(a);
    try testing.expectEqual(@as(usize, 2), names.len); // the names are reused, not made unique again
    try testing.expect((try w2.ranges(a, 0)).?.complete());
}

test "the snapshot of an empty writer round-trips" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const snap = try rig.w.snapshot(a);
    const w2 = try rig.another(.{});
    defer w2.deinit();
    try testing.expectEqual(@as(usize, 0), (try w2.restore(a, snap)).len);
}

test "restore resets missing and resized files" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const names = [_][]const u8{ "keep.bin", "short.bin", "gone.bin", "long.bin" };
    for (names, 0..) |name, k| {
        const parts = try splitParts(a, name, try pattern(a, 400), 100);
        try writeAll(rig.w, @intCast(k), parts[0..2]);
    }
    const snap = try rig.w.snapshot(a);
    try rig.w.close();
    {
        const f = try rig.tmp.dir.openFile(testing.io, Rig.rel_dir ++ "/short.bin", .{ .mode = .read_write });
        defer f.close(testing.io);
        try f.setLength(testing.io, 150);
    }
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/gone.bin");
    {
        const f = try rig.tmp.dir.openFile(testing.io, Rig.rel_dir ++ "/long.bin", .{ .mode = .read_write });
        defer f.close(testing.io);
        try f.setLength(testing.io, 900);
    }

    const w2 = try rig.another(.{});
    defer w2.deinit();
    const reset = try w2.restore(a, snap);

    try testing.expectEqualSlices(u32, &.{ 1, 2, 3 }, reset);
    const keep = (try w2.ranges(a, 0)).?;
    try expectRanges(&.{rg(0, 200)}, keep.done);
    for ([_]u32{ 1, 3 }) |k| {
        const st = (try w2.ranges(a, k)).?;
        try expectRanges(&.{}, st.done);
        try expectRanges(&.{rg(0, 400)}, st.missing);
        const stat = try rig.tmp.dir.statFile(testing.io, try Rig.relPath(a, st.name), .{});
        try testing.expectEqual(@as(u64, 400), stat.size); // re-allocated to its recorded size
    }
    try testing.expect((try w2.ranges(a, 2)) == null); // a vanished file is forgotten

    const data = try pattern(a, 400);
    try writeAll(w2, 2, try splitParts(a, "gone.bin", data, 100));
    try testing.expectEqualSlices(u8, data, try (struct {
        fn f(r: *Rig, al: Allocator, w: *Writer) ![]u8 {
            const st = (try w.ranges(al, 2)).?;
            return r.readNamed(al, st.name);
        }
    }).f(rig, a, w2));
}

test "restore refuses a non-regular file at a recorded name" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 50))[0]);
    const snap = try rig.w.snapshot(a);
    try rig.w.close();
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/x.bin");
    try rig.tmp.dir.createDir(testing.io, Rig.rel_dir ++ "/x.bin", .default_dir);

    const w2 = try rig.another(.{});
    defer w2.deinit();
    const reset = try w2.restore(a, snap);

    try testing.expectEqualSlices(u32, &.{0}, reset);
    try testing.expect((try w2.ranges(a, 0)) == null);
}

test "restore refuses a symlink at a recorded name" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 50))[0]);
    const snap = try rig.w.snapshot(a);
    try rig.w.close();
    try rig.tmp.dir.writeFile(testing.io, .{ .sub_path = "victim", .data = try pattern(a, 100) });
    try rig.tmp.dir.deleteFile(testing.io, Rig.rel_dir ++ "/x.bin");
    try rig.tmp.dir.symLink(testing.io, "../../victim", Rig.rel_dir ++ "/x.bin", .{});

    const w2 = try rig.another(.{});
    defer w2.deinit();
    const reset = try w2.restore(a, snap);

    try testing.expectEqualSlices(u32, &.{0}, reset);
    // The target of the link kept its size: it was not re-allocated.
    const stat = try rig.tmp.dir.statFile(testing.io, "victim", .{});
    try testing.expectEqual(@as(u64, 100), stat.size);
}

test "restore refuses malformed snapshots and stays empty" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 50))[0]);
    const snap = try rig.w.snapshot(a);

    const flipped = try a.dupe(u8, snap);
    flipped[flipped.len / 2] ^= 0xff;
    const bad_version = try a.dupe(u8, snap);
    bad_version[4] = 99;
    const cases = [_][]const u8{ "", "hello world, not a snapshot", snap[0 .. snap.len - 3], flipped, bad_version };
    for (cases) |c| {
        const w2 = try rig.another(.{});
        defer w2.deinit();
        try testing.expectError(error.InvalidSnapshot, w2.restore(a, c));
        try testing.expect((try w2.ranges(a, 0)) == null);
    }
}

test "restore needs an empty writer" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    try rig.w.writePart(0, (try splitParts(a, "x.bin", try pattern(a, 100), 50))[0]);
    const snap = try rig.w.snapshot(a);

    try testing.expectError(error.NotEmpty, rig.w.restore(a, snap));
}

test "a snapshot is compact" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "big.bin", try pattern(a, 100_000), 100);
    for (parts, 0..) |p, i| {
        if (i % 10 != 3) try rig.w.writePart(0, p); // 10% gaps: 100 separate ranges
    }
    const snap = try rig.w.snapshot(a);
    try testing.expect(snap.len < 400); // about 3 bytes per range plus the header
}

// ---- a model of the whole writer ----

/// Runs write, damaged write and markMissing operations on three files
/// through the public API and compares the bookkeeping and the bytes on disk
/// with a per-byte model. Each operation is four bytes: key and kind, start,
/// end, and (damaged only) nothing. A snapshot and a restore into a second
/// writer run at the end and must agree.
fn runWriterModel(ops: []const u8) !void {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{ .max_open_files = 2 });
    defer rig.deinit();
    const size = 64;
    const Cell = enum { none, done, damaged, lost };
    var model: [3][size]Cell = undefined;
    for (&model) |*m| m.* = @splat(.none);
    var created: [3]bool = .{ false, false, false };
    var datas: [3][]u8 = undefined;
    for (&datas, 0..) |*d, k| {
        d.* = try pattern(a, size);
        for (d.*) |*b| b.* +%= @as(u8, @intCast(k)) +% 1;
    }
    const names = [_][]const u8{ "k0.bin", "k1.bin", "k2.bin" };

    var i: usize = 0;
    while (i + 4 <= ops.len) : (i += 4) {
        const k: usize = ops[i] % 3;
        const kind = (ops[i] / 3) % 4; // good, damaged, lost, finish
        var x: i64 = ops[i + 1] % (size + 1);
        var y: i64 = ops[i + 2] % (size + 1);
        if (x > y) std.mem.swap(i64, &x, &y);
        switch (kind) {
            0, 1 => {
                if (x == y) continue; // a part has at least one byte
                var p = partOf(names[k], datas[k], x + 1, y);
                const xs: usize = @intCast(x);
                const ys: usize = @intCast(y);
                if (kind == 1) p = try corrupt(a, p);
                try rig.w.writePart(@intCast(k), p);
                created[k] = true;
                for (xs..ys) |c| {
                    if (kind == 0) {
                        model[k][c] = .done;
                    } else if (model[k][c] != .done) model[k][c] = .damaged;
                }
            },
            2 => {
                const r = rig.w.markMissing(@intCast(k), rg(x, y));
                if (!created[k]) {
                    try testing.expectError(error.UnknownFile, r);
                    continue;
                }
                try r;
                for (@as(usize, @intCast(x))..@as(usize, @intCast(y))) |c| {
                    if (model[k][c] == .none) model[k][c] = .lost;
                }
            },
            else => if (created[k]) try rig.w.finish(@intCast(k)),
        }
    }

    for (0..3) |k| {
        const st = (try rig.w.ranges(a, @intCast(k))) orelse {
            try testing.expect(!created[k]);
            continue;
        };
        for (0..size) |c| {
            const cell = model[k][c];
            const cr = rg(@intCast(c), @intCast(c + 1));
            try testing.expectEqual(cell == .done, covered(st.done, cr));
            try testing.expectEqual(cell == .damaged, covered(st.damaged, cr));
            try testing.expectEqual(cell == .none or cell == .lost, covered(st.missing, cr));
            try testing.expectEqual(cell == .lost, covered(st.lost, cr));
            try testing.expectEqual(cell == .none, covered(st.pending, cr));
        }
        const bytes = try rig.read(a, @intCast(k));
        try testing.expectEqual(@as(usize, size), bytes.len);
        for (0..size) |c| {
            const want: u8 = switch (model[k][c]) {
                .done => datas[k][c],
                .damaged => 0xee,
                .none, .lost => 0,
            };
            try testing.expectEqual(want, bytes[c]);
        }
    }

    const snap = try rig.w.snapshot(a);
    try rig.w.close();
    const w2 = try rig.another(.{});
    defer w2.deinit();
    const reset = try w2.restore(a, snap);
    try testing.expectEqual(@as(usize, 0), reset.len);
    const snap2 = try w2.snapshot(a);
    try testing.expectEqualSlices(u8, snap, snap2);
}

fn covered(rs: []const Range, r: Range) bool {
    for (rs) |x| if (x.start <= r.start and x.end >= r.end) return true;
    return false;
}

test "the writer follows a per-byte model" {
    try runWriterModel(&.{ 0, 3, 10, 0, 1, 5, 20, 0, 6, 0, 63, 0, 4, 8, 9, 0 });
    try runWriterModel(&.{ 3, 0, 64, 0, 0, 10, 30, 0, 9, 0, 64, 0, 1, 10, 30, 0 });
    try runWriterModel(&.{});
    var prng: std.Random.DefaultPrng = .init(5);
    var ops: [4 * 120]u8 = undefined;
    prng.random().bytes(&ops);
    try runWriterModel(&ops);
}

fn fuzzWriter(_: void, smith: *testing.Smith) !void {
    var buf: [4 * 24]u8 = undefined;
    const n = smith.slice(&buf);
    try runWriterModel(buf[0..n]);
}

fn seedOf(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

test "fuzz: the writer follows a per-byte model for any operations" {
    const corpus = [_][]const u8{
        seedOf(&.{ 0, 3, 10, 0, 1, 5, 20, 0, 6, 0, 63, 0, 4, 8, 9, 0 }),
        seedOf(&.{ 3, 0, 64, 0, 0, 10, 30, 0, 9, 0, 64, 0, 1, 10, 30, 0 }),
        seedOf(&.{ 6, 0, 10, 0, 0, 0, 10, 0 }),
        seedOf(&.{}),
    };
    for (corpus) |input| {
        var smith: testing.Smith = .{ .in = input };
        try fuzzWriter({}, &smith);
    }
    try testing.fuzz({}, fuzzWriter, .{ .corpus = &corpus });
}

// ---- stress ----

const Stress = struct {
    w: *Writer,
    files: []const []const Part,
    corrupt_parts: []const []const Part,
    first_key: u32,
    stride: u32,
    seed: u64,
    err: ?anyerror = null,

    fn run(s: *Stress) void {
        s.work() catch |err| {
            s.err = err;
        };
    }

    fn work(s: *Stress) !void {
        var prng: std.Random.DefaultPrng = .init(s.seed);
        const rnd = prng.random();
        var key = s.first_key;
        while (key < s.files.len) : (key += s.stride) {
            const parts = s.files[key];
            for (parts, 0..) |p, i| {
                // Some damaged copies first, healed by the good one after.
                if (rnd.uintLessThan(u8, 5) == 0) try s.w.writePart(key, s.corrupt_parts[key][i]);
                try s.w.writePart(key, p);
                if (rnd.uintLessThan(u8, 9) == 0) try s.w.writePart(key, p); // a duplicate
                if (i == parts.len / 2 and rnd.boolean()) try s.w.finish(key);
            }
            if (rnd.boolean()) try s.w.finish(key);
        }
    }
};

const Saver = struct {
    w: *Writer,
    stop: *std.atomic.Value(bool),
    snapshots: usize = 0,
    err: ?anyerror = null,

    fn run(s: *Saver) void {
        s.loop() catch |err| {
            s.err = err;
        };
    }

    fn loop(s: *Saver) !void {
        while (!s.stop.load(.acquire)) {
            var ar = std.heap.ArenaAllocator.init(testing.allocator);
            defer ar.deinit();
            const blob = try s.w.snapshot(ar.allocator());
            // Whatever the snapshot caught is a valid snapshot.
            _ = try @import("filewriter_snapshot.zig").decode(ar.allocator(), blob);
            try s.w.sync();
            s.snapshots += 1;
        }
    }
};

test "stress: many keys, a tiny open-file bound, snapshots and finishes in between" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const nkeys = 150;
    const nthreads = 6;
    const rig = try Rig.start(.{ .max_open_files = 3 });
    defer rig.deinit();
    var datas: [nkeys][]u8 = undefined;
    var files: [nkeys][]Part = undefined;
    var bad: [nkeys][]Part = undefined;
    for (0..nkeys) |k| {
        datas[k] = try pattern(a, 1500 + k * 3);
        // Every file has the same announced name: the names must be made unique.
        files[k] = try splitParts(a, "same.bin", datas[k], 64);
        bad[k] = try a.alloc(Part, files[k].len);
        for (files[k], bad[k]) |p, *b| b.* = try corrupt(a, p);
    }

    var stresses: [nthreads]Stress = undefined;
    var threads: [nthreads]std.Thread = undefined;
    for (&stresses, 0..) |*s, t| s.* = .{ .w = rig.w, .files = &files, .corrupt_parts = &bad, .first_key = @intCast(t), .stride = nthreads, .seed = t + 100 };
    var stop: std.atomic.Value(bool) = .init(false);
    var saver: Saver = .{ .w = rig.w, .stop = &stop };
    const saver_thread = try std.Thread.spawn(.{}, Saver.run, .{&saver});
    for (&threads, &stresses) |*th, *s| th.* = try std.Thread.spawn(.{}, Stress.run, .{s});
    for (threads) |th| th.join();
    stop.store(true, .release);
    saver_thread.join();
    for (stresses) |s| if (s.err) |err| return err;
    if (saver.err) |err| return err;

    // Every file is complete and byte exact, under a distinct name.
    var seen: std.StringHashMapUnmanaged(void) = .empty;
    for (0..nkeys) |k| {
        const st = (try rig.w.ranges(a, @intCast(k))).?;
        try testing.expect(st.complete());
        try testing.expect(!seen.contains(st.name));
        try seen.put(a, st.name, {});
        try testing.expectEqualSlices(u8, datas[k], try rig.readNamed(a, st.name));
    }
    try testing.expectEqual(@as(usize, nkeys), (try rig.list(a)).len);
    try testing.expect(rig.w.lru_len <= 3 + nthreads);

    // A snapshot of the result restores to the same states.
    const snap = try rig.w.snapshot(a);
    try rig.w.close();
    const w2 = try rig.another(.{ .max_open_files = 3 });
    defer w2.deinit();
    try testing.expectEqual(@as(usize, 0), (try w2.restore(a, snap)).len);
    for (0..nkeys) |k| try expectSameState((try rig.w.ranges(a, @intCast(k))).?, (try w2.ranges(a, @intCast(k))).?);
}

// ---- allocation failures ----

fn scenario(gpa: Allocator, root: []const u8, round: usize) !void {
    var ar: std.heap.ArenaAllocator = .init(testing.allocator);
    defer ar.deinit();
    const a = ar.allocator();
    const dir_path = try std.fmt.allocPrint(a, "{s}/r{d}", .{ root, round });
    const data = try pattern(a, 300);
    const parts = try splitParts(a, "x.bin", data, 100);
    {
        const w = try Writer.init(gpa, testing.io, dir_path, .{ .max_open_files = 1 });
        defer w.deinit();
        try w.writePart(0, parts[0]);
        try w.writePart(1, parts[1]); // evicts the first
        try w.writePart(0, try corrupt(a, parts[2]));
        try w.markMissing(0, rg(100, 200));
        var scratch: std.heap.ArenaAllocator = .init(gpa);
        defer scratch.deinit();
        _ = try w.ranges(scratch.allocator(), 0) orelse return error.TestUnexpectedResult;
        _ = try w.snapshot(scratch.allocator());
        try w.finish(0);
        try w.sync();
        try w.close();
    }
}

test "allocation failures leak nothing and leave no handle open" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var ar = newArena();
    defer ar.deinit();
    const root = try tmp.dir.realPathFileAlloc(testing.io, ".", ar.allocator());
    var round: usize = 0;
    const Ctx = struct {
        fn run(gpa: Allocator, r: []const u8, n: *usize) !void {
            n.* += 1;
            try scenario(gpa, r, n.*);
        }
    };
    try testing.checkAllAllocationFailures(testing.allocator, Ctx.run, .{ root, &round });
}

test "restore after allocation failures leaks nothing" {
    var ar = newArena();
    defer ar.deinit();
    const a = ar.allocator();
    const rig = try Rig.start(.{});
    defer rig.deinit();
    const parts = try splitParts(a, "x.bin", try pattern(a, 300), 100);
    try writeAll(rig.w, 0, parts);
    try writeAll(rig.w, 1, try splitParts(a, "y.bin", try pattern(a, 300), 100));
    const snap = try rig.w.snapshot(a);
    try rig.w.close();

    const Ctx = struct {
        fn run(gpa: Allocator, r: *Rig, blob: []const u8) !void {
            const w2 = try Writer.init(gpa, testing.io, r.dir_path, .{});
            defer w2.deinit();
            var arena: std.heap.ArenaAllocator = .init(gpa);
            defer arena.deinit();
            _ = try w2.restore(arena.allocator(), blob);
        }
    };
    try testing.checkAllAllocationFailures(testing.allocator, Ctx.run, .{ rig, snap });
}
