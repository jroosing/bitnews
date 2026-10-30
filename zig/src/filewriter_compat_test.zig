//! Compatibility with the Go file writer, both directions (M15, ADR-0014).
//!
//! testdata/compat/filewriter_snapshots.json is written by a throwaway Go
//! program from the real filewriter package: scenarios (a list of writes,
//! damaged writes and markMissing calls), the snapshot Go produced for each
//! and the bookkeeping Go reports. Here every scenario is replayed through
//! the Zig writer and must give the same names, the same ranges and exactly
//! the same snapshot bytes; and the Go snapshot must restore into a Zig
//! writer and give the same ranges again.
//!
//! The other direction: with BITNEWS_ZIG_SNAP_OUT=<path> set, the Zig
//! snapshots are written there in the same schema, for the Go program to
//! restore (go run ... check <path>). The bytes are equal to Go's, so that
//! file is not committed. Tests run with the working directory zig/.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const filewriter = @import("filewriter.zig");
const Writer = filewriter.Writer;
const Range = filewriter.Range;
const t = @import("filewriter_test.zig");

const Op = struct {
    kind: []const u8,
    key: u32,
    name: []const u8 = "",
    size: i64 = 0,
    begin: i64 = 0,
    end: i64 = 0,
};

const File = struct {
    key: u32,
    name: []const u8,
    size: i64,
    done: []const [2]i64,
    damaged: []const [2]i64,
    missing: []const [2]i64,
    lost: []const [2]i64,
    pending: []const [2]i64,
};

const Scenario = struct {
    name: []const u8,
    ops: []const Op,
    snapshot: []const u8,
    files: []const File,
};

const Doc = struct { scenarios: []const Scenario };

fn byteAt(off: i64) u8 {
    return @truncate(@as(u64, @bitCast(off *% 7 +% (off >> 8))));
}

fn dataOf(a: Allocator, begin: i64, end: i64) ![]u8 {
    const b = try a.alloc(u8, @intCast(end - begin + 1));
    for (b, 0..) |*x, i| x.* = byteAt(begin - 1 + @as(i64, @intCast(i)));
    return b;
}

fn apply(a: Allocator, w: *Writer, op: Op) !void {
    if (std.mem.eql(u8, op.kind, "lost")) return w.markMissing(op.key, .{ .start = op.begin, .end = op.end });
    const good = try dataOf(a, op.begin, op.end);
    const sum = std.hash.Crc32.hash(good);
    var p: filewriter.Part = .{
        .name = op.name,
        .size = op.size,
        .begin = op.begin,
        .end = op.end,
        .data = good,
        .crc32 = sum,
        .expected_crc32 = sum,
        .has_expected_crc = true,
    };
    if (std.mem.eql(u8, op.kind, "damaged")) {
        const bad = try a.alloc(u8, good.len);
        @memset(bad, 0xee);
        p.data = bad;
        p.crc32 = std.hash.Crc32.hash(bad);
    } else if (std.mem.eql(u8, op.kind, "nocrc")) {
        p.has_expected_crc = false;
        p.expected_crc32 = 0;
    }
    try w.writePart(op.key, p);
}

fn expectPairs(want: []const [2]i64, got: []const Range) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| {
        try testing.expectEqual(w[0], g.start);
        try testing.expectEqual(w[1], g.end);
    }
}

fn expectFile(want: File, got: filewriter.FileState) !void {
    try testing.expectEqual(want.key, got.key);
    try testing.expectEqualStrings(want.name, got.name);
    try testing.expectEqual(want.size, got.size);
    try expectPairs(want.done, got.done);
    try expectPairs(want.damaged, got.damaged);
    try expectPairs(want.missing, got.missing);
    try expectPairs(want.lost, got.lost);
    try expectPairs(want.pending, got.pending);
}

fn toPairs(a: Allocator, rs: []const Range) ![]const [2]i64 {
    const out = try a.alloc([2]i64, rs.len);
    for (rs, out) |r, *o| o.* = .{ r.start, r.end };
    return out;
}

test "the scenarios of the Go writer give the same files, ranges and snapshot bytes, and the Go snapshots restore" {
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();
    const text = try Io.Dir.cwd().readFileAlloc(testing.io, "testdata/compat/filewriter_snapshots.json", arena, .limited(8 << 20));
    const doc = try std.json.parseFromSliceLeaky(Doc, arena, text, .{});
    try testing.expect(doc.scenarios.len >= 6);

    for (doc.scenarios) |sc| {
        var ar: std.heap.ArenaAllocator = .init(testing.allocator);
        defer ar.deinit();
        const a = ar.allocator();
        const rig = try t.Rig.start(.{ .max_open_files = 2 });
        defer rig.deinit();

        for (sc.ops) |op| try apply(a, rig.w, op);
        for (sc.files) |f| try expectFile(f, (try rig.w.ranges(a, f.key)).?);

        const want = try a.alloc(u8, sc.snapshot.len / 2);
        _ = try std.fmt.hexToBytes(want, sc.snapshot);
        const mine = try rig.w.snapshot(a);
        testing.expectEqualSlices(u8, want, mine) catch |err| {
            std.debug.print("scenario: {s}\n", .{sc.name});
            return err;
        };
        try rig.w.close();

        // The Go blob restores into a fresh Zig writer over the same files.
        const w2 = try rig.another(.{});
        defer w2.deinit();
        const reset = try w2.restore(a, want);
        try testing.expectEqual(@as(usize, 0), reset.len);
        for (sc.files) |f| try expectFile(f, (try w2.ranges(a, f.key)).?);
        try testing.expectEqualSlices(u8, want, try w2.snapshot(a));
    }

    if (testing.environ.getPosix("BITNEWS_ZIG_SNAP_OUT")) |path| {
        try writeOut(arena, path, doc);
    }
}

/// Writes the snapshots the Zig writer produces for the scenarios of `doc`
/// in the schema of the Go file.
fn writeOut(arena: Allocator, path: []const u8, doc: Doc) !void {
    var out: std.ArrayList(Scenario) = .empty;
    for (doc.scenarios) |sc| {
        const rig = try t.Rig.start(.{});
        defer rig.deinit();
        for (sc.ops) |op| try apply(arena, rig.w, op);
        const mine = try rig.w.snapshot(arena);
        const hex = try arena.alloc(u8, mine.len * 2);
        for (mine, 0..) |b, i| {
            hex[i * 2] = std.fmt.hex_charset[b >> 4];
            hex[i * 2 + 1] = std.fmt.hex_charset[b & 15];
        }
        var files: std.ArrayList(File) = .empty;
        for (sc.files) |f| {
            const st = (try rig.w.ranges(arena, f.key)).?;
            try files.append(arena, .{ .key = st.key, .name = st.name, .size = st.size, .done = try toPairs(arena, st.done), .damaged = try toPairs(arena, st.damaged), .missing = try toPairs(arena, st.missing), .lost = try toPairs(arena, st.lost), .pending = try toPairs(arena, st.pending) });
        }
        try out.append(arena, .{ .name = sc.name, .ops = sc.ops, .snapshot = hex, .files = files.items });
    }
    const json = try std.json.Stringify.valueAlloc(arena, Doc{ .scenarios = out.items }, .{});
    try Io.Dir.cwd().writeFile(testing.io, .{ .sub_path = path, .data = json });
}
