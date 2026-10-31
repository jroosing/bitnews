//! Tests of the PAR2 name guard (internal/postproc/par2 names_test.go and the
//! name cases of hostile_test.go), including the fuzz target.
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const Io = std.Io;
const names = @import("par2_names.zig");
const ports = @import("postprocess_ports.zig");
const b = @import("par2_testbuilder.zig");

fn spec(a: Allocator, name: []const u8) !b.FileSpec {
    return .{ .name = name, .data = try std.fmt.allocPrint(a, "data of {s}", .{name}) };
}

fn specs(a: Allocator, ns: []const []const u8) ![]b.FileSpec {
    const out = try a.alloc(b.FileSpec, ns.len);
    for (ns, out) |n, *o| o.* = try spec(a, n);
    return out;
}

fn readAll(a: Allocator, data: []const u8, diag: *ports.Diag) ![]const []const u8 {
    var sc = try names.Scanner.init(a);
    var set: names.NameSet = .{};
    var src: names.SliceSource = .{ .data = data };
    try names.readNames(a, &sc, &src, data.len, &set, diag);
    return set.names();
}

fn expectNames(want: []const []const u8, data: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var diag: ports.Diag = .{};
    const got = try readAll(arena.allocator(), data, &diag);
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g);
}

fn expectUnsafe(data: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var diag: ports.Diag = .{};
    try testing.expectError(error.UnsafeSet, readAll(arena.allocator(), data, &diag));
    try testing.expect(std.mem.startsWith(u8, diag.message, "par2: unsafe PAR2 set: path violation: "));
}

test "readNames finds every file description" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{ "a.bin", "sub/b.bin", "c d.bin" }));
    try expectNames(&.{ "a.bin", "sub/b.bin", "c d.bin" }, try set.index());
}

test "readNames ignores padding NULs and keeps order and drops duplicates" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{ "x.bin", "y.bin" }));
    const data = try b.concat(a, &.{ try set.index(), try set.volume(&.{"0123456789abcdef"}), try set.index() });
    try expectNames(&.{ "x.bin", "y.bin" }, data);
}

test "readNames skips garbage and damaged packets" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{ "good.bin", "hurt.bin" }));
    const idx = try set.index();
    const hurt = std.mem.indexOf(u8, idx, "hurt.bin").?;
    idx[hurt] ^= 0x20; // breaks that FileDesc MD5
    try expectNames(&.{"good.bin"}, try b.concat(a, &.{ "garbage", idx, "PAR2\x00PKT\xff\xff" }));
}

test "readNames finds a packet hidden inside another packet" {
    // par2 may find a FileDesc where a damaged packet was expected, so the
    // reader must look inside recovery data too.
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const hidden = try b.TestSet.init(a, 16, try specs(a, &.{"../../evil"}));
    const outer = try b.TestSet.init(a, 16, try specs(a, &.{"ok.bin"}));
    const data = try outer.volume(&.{try hidden.fileDescPacket(0)});
    try expectNames(&.{"../../evil"}, data);
}

test "readNames finds magics across chunk boundaries" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{"edge.bin"}));
    const pkt = try set.fileDescPacket(0);
    var pad: usize = names.scan_chunk - 12;
    while (pad < names.scan_chunk + 12) : (pad += 1) {
        const padding = try a.alloc(u8, pad);
        @memset(padding, 'x');
        try expectNames(&.{"edge.bin"}, try b.concat(a, &.{ padding, pkt }));
    }
}

test "readNames refuses an oversized file description packet" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{"a"}));
    const pkt = try set.fileDescPacket(0);
    std.mem.writeInt(u64, pkt[8..16], 8 << 20, .little);
    const zeros = try a.alloc(u8, 8 << 20);
    @memset(zeros, 0);
    try expectUnsafe(try b.concat(a, &.{ pkt, zeros }));
}

test "readNames bounds the work on repeated broken packets" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{"a"}));
    const pkt = try set.fileDescPacket(0);
    pkt[20] ^= 1; // MD5 mismatch
    const data = try a.alloc(u8, pkt.len * 5000);
    for (0..5000) |i| @memcpy(data[i * pkt.len ..][0..pkt.len], pkt);
    try expectUnsafe(data);
}

test "readNames refuses a set with more names than the cap" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var sc = try names.Scanner.init(a);
    var set: names.NameSet = .{};
    for (0..names.max_names) |i| try set.map.put(a, try std.fmt.allocPrint(a, "n{d}", .{i}), {});
    const one = try b.TestSet.init(a, 16, try specs(a, &.{"one-more"}));
    const data = try one.index();
    var src: names.SliceSource = .{ .data = data };
    var diag: ports.Diag = .{};
    try testing.expectError(error.UnsafeSet, names.readNames(a, &sc, &src, data.len, &set, &diag));
}

test "readNamesFile reads from disk and says which file failed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const set = try b.TestSet.init(a, 16, try specs(a, &.{"d.bin"}));
    try tmp.dir.writeFile(testing.io, .{ .sub_path = "s.par2", .data = try set.index() });
    var sc = try names.Scanner.init(a);
    var ns: names.NameSet = .{};
    var diag: ports.Diag = .{};
    try names.readNamesFile(a, testing.io, &sc, tmp.dir, "s.par2", &ns, &diag);
    try testing.expectEqual(1, ns.names().len);
    try testing.expectEqualStrings("d.bin", ns.names()[0]);

    try testing.expectError(error.Failed, names.readNamesFile(a, testing.io, &sc, tmp.dir, "missing.par2", &ns, &diag));
    try testing.expect(std.mem.startsWith(u8, diag.message, "par2: read \"missing.par2\": "));
}

test "checkName accepts plain relative paths" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var diag: ports.Diag = .{};
    for ([_][]const u8{ "a.bin", "Movie (2020) [x264].mkv", "sub/dir/a.bin", "ß 日本.bin", "..a", "a..", ".hidden", "a b.bin", "xy:z.bin", "Star Wars: IV.mkv" }) |n| {
        try names.checkName(arena.allocator(), n, &diag);
    }
}

test "checkName refuses hostile names" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const long = try a.alloc(u8, 300);
    @memset(long, 'a');
    var many: std.ArrayList(u8) = .empty;
    for (0..600) |_| try many.appendSlice(a, "a/");
    const cases = [_]struct { []const u8, []const u8 }{
        .{ "empty", "" },
        .{ "dot dot", ".." },
        .{ "dot", "." },
        .{ "parent", "../evil" },
        .{ "parent deep", "a/../../evil" },
        .{ "parent mid", "a/../b" },
        .{ "parent end", "a/.." },
        .{ "absolute", "/etc/passwd" },
        .{ "double slash", "a//b" },
        .{ "trailing slash", "dir/" },
        .{ "dot component", "a/./b" },
        .{ "drive letter", "C:evil" },
        .{ "drive path", "c:/evil" },
        .{ "backslash", "..\\evil" },
        .{ "backslash name", "a\\b" },
        .{ "unc", "\\\\host\\share\\x" },
        .{ "nul", "a\x00../b" },
        .{ "newline", "a\nRepair is possible." },
        .{ "carriage return", "a\rb" },
        .{ "escape", "a\x1b[2Jb" },
        .{ "del", "a\x7fb" },
        .{ "par2 target", "set.vol0+4.par2" },
        .{ "par2 target upper", "SET.PAR2" },
        .{ "long component", long },
        .{ "long name", many.items },
    };
    for (cases) |c| {
        var diag: ports.Diag = .{};
        testing.expectError(error.UnsafeSet, names.checkName(a, c[1], &diag)) catch |e| {
            std.debug.print("case {s}\n", .{c[0]});
            return e;
        };
        try testing.expect(std.mem.startsWith(u8, diag.message, "par2: unsafe PAR2 set: path violation: file name \""));
    }
}

test "checkName message names the reason and truncates the name" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var diag: ports.Diag = .{};
    try testing.expectError(error.UnsafeSet, names.checkName(a, "../evil", &diag));
    try testing.expectEqualStrings("par2: unsafe PAR2 set: path violation: file name \"../evil\": relative path component", diag.message);

    const long = try a.alloc(u8, 100);
    @memset(long, 'a');
    try testing.expectError(error.UnsafeSet, names.checkName(a, try b.concat(a, &.{ "/", long }), &diag));
    try testing.expect(std.mem.endsWith(u8, diag.message, "...\": absolute path"));
}

test "checkNames refuses a symlinked parent and a symlinked target" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var dir_tmp = testing.tmpDir(.{});
    defer dir_tmp.cleanup();
    var out_tmp = testing.tmpDir(.{});
    defer out_tmp.cleanup();
    var buf: [Io.Dir.max_path_bytes]u8 = undefined;
    const dir = try a.dupe(u8, buf[0..try dir_tmp.dir.realPath(testing.io, &buf)]);
    const outside = try a.dupe(u8, buf[0..try out_tmp.dir.realPath(testing.io, &buf)]);
    try dir_tmp.dir.symLink(testing.io, outside, "link", .{});
    try out_tmp.dir.writeFile(testing.io, .{ .sub_path = "x", .data = "x" });
    const xpath = try std.fmt.allocPrint(a, "{s}/x", .{outside});
    try dir_tmp.dir.symLink(testing.io, xpath, "t.bin", .{});
    try dir_tmp.dir.writeFile(testing.io, .{ .sub_path = "ok.bin", .data = "x" });
    try dir_tmp.dir.createDirPath(testing.io, "adir");

    var diag: ports.Diag = .{};
    try names.checkNames(a, testing.io, dir, &.{ "ok.bin", "missing.bin", "newdir/new.bin" }, &diag);
    try testing.expectError(error.UnsafeSet, names.checkNames(a, testing.io, dir, &.{"link/x"}, &diag));
    try testing.expectError(error.UnsafeSet, names.checkNames(a, testing.io, dir, &.{"t.bin"}, &diag));
    // A symlink that stays inside the directory is refused as a target too.
    try dir_tmp.dir.symLink(testing.io, "ok.bin", "inner.bin", .{});
    try testing.expectError(error.UnsafeSet, names.checkNames(a, testing.io, dir, &.{"inner.bin"}, &diag));
    try testing.expect(std.mem.indexOf(u8, diag.message, "exists and is not a regular file") != null);
    try testing.expectError(error.UnsafeSet, names.checkNames(a, testing.io, dir, &.{"adir"}, &diag));
}

fn seed(comptime s: []const u8) []const u8 {
    return &std.mem.toBytes(@as(u32, s.len)) ++ s;
}

/// The fuzz seeds: sets made by the test builder (the same packets the Go
/// target's seeds are), as comptime text.
fn seeds(a: Allocator) ![]const []const u8 {
    const set = try b.TestSet.init(a, 16, try specs(a, &.{ "a.bin", "../b", "c/d" }));
    const idx = try set.index();
    var out: std.ArrayList([]const u8) = .empty;
    const frame = struct {
        fn f(al: Allocator, d: []const u8) ![]const u8 {
            return std.mem.concat(al, u8, &.{ &std.mem.toBytes(@as(u32, @intCast(d.len))), d });
        }
    }.f;
    try out.append(a, try frame(a, ""));
    try out.append(a, try frame(a, "PAR2\x00PKT"));
    try out.append(a, try frame(a, idx[0..@min(idx.len, 2000)]));
    try out.append(a, try frame(a, try b.concat(a, &.{ "junk", try set.fileDescPacket(1), "PAR2\x00PKT\xff\xff\xff\xff\xff\xff\xff\xff" })));
    const huge = try set.fileDescPacket(0);
    std.mem.writeInt(u64, huge[8..16], 1 << 40, .little);
    try out.append(a, try frame(a, huge));
    return out.items;
}

// Any input: no crash, no leak, memory inside the arena, names distinct, every
// name occurs in the input (its packet's MD5 verified), checkName answers.
test "fuzz: readNames on any bytes" {
    var seed_arena: std.heap.ArenaAllocator = .init(std.heap.page_allocator);
    defer seed_arena.deinit();
    const corpus = try seeds(seed_arena.allocator());
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [4096]u8 = undefined;
            const len = smith.slice(&buf);
            const data = buf[0..len];
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            const a = arena.allocator();
            var diag: ports.Diag = .{};
            const got = readAll(a, data, &diag) catch |e| switch (e) {
                error.UnsafeSet => return,
                else => return e,
            };
            try testing.expect(got.len <= names.max_names);
            for (got, 0..) |n, i| {
                for (got[0..i]) |prev| try testing.expect(!std.mem.eql(u8, prev, n));
                try testing.expect(std.mem.indexOf(u8, data, n) != null);
                names.checkName(a, n, &diag) catch |e| switch (e) {
                    error.UnsafeSet => {},
                    else => return e,
                };
            }
        }
    }.one, .{ .corpus = corpus });
}
