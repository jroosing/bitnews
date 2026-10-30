//! The snapshot of the file writer's bookkeeping (internal/usenet/filewriter
//! snapshot.go), byte-compatible with Go: a blob written by Go restores here
//! and the other way round. The queue runner stores it in the database.
//!
//! Wire format (version 1), all integers are unsigned varints (LEB128, as
//! Go's encoding/binary) unless noted:
//!
//!     "GNFW" version(1 byte)
//!     nfiles
//!     nfiles x { key, len(name), name, size, ranges(done), ranges(damaged), ranges(lost) }
//!     crc32 (IEEE, 4 bytes little endian) over everything before it
//!
//! ranges is: n, then n x { start - previousEnd, end - start }, so a file
//! with long runs costs a few bytes per run regardless of its size.
//!
//! The decoder is for hostile input (a database can be edited, a blob can
//! rot): every count is bounded by the bytes left, every name must be a
//! `fsutil.safeName` result, sizes and ranges are bounded, and nothing is
//! trusted before the checksum matched. Records live in the arena the caller
//! passes.
const std = @import("std");
const Allocator = std.mem.Allocator;
const ranges_mod = @import("filewriter_ranges.zig");
const Range = ranges_mod.Range;
const fsutil = @import("fsutil.zig");

pub const magic = "GNFW";
pub const version: u8 = 1;
/// The largest file size a part may announce (1 TiB).
pub const max_file_size: i64 = 1 << 40;
/// The largest key: Go's FileKey is an int and its decoder refuses what does
/// not fit 31 bits, so a key beyond it would not restore there.
pub const max_key: u32 = std.math.maxInt(i32);

pub const DecodeError = error{InvalidSnapshot} || Allocator.Error;

pub const Record = struct {
    key: u32,
    name: []const u8,
    size: i64,
    done: []const Range,
    damaged: []const Range,
    lost: []const Range,
};

fn appendUvarint(a: Allocator, b: *std.ArrayList(u8), v_in: u64) Allocator.Error!void {
    var v = v_in;
    while (v >= 0x80) {
        try b.append(a, @as(u8, @truncate(v)) | 0x80);
        v >>= 7;
    }
    try b.append(a, @truncate(v));
}

fn appendRanges(a: Allocator, b: *std.ArrayList(u8), rs: []const Range) Allocator.Error!void {
    try appendUvarint(a, b, rs.len);
    var prev: i64 = 0;
    for (rs) |r| {
        try appendUvarint(a, b, @intCast(r.start - prev));
        try appendUvarint(a, b, @intCast(r.len()));
        prev = r.end;
    }
}

/// Collects records and produces the blob. The body is built in `a`.
pub const Encoder = struct {
    body: std.ArrayList(u8) = .empty,
    count: u64 = 0,

    pub fn deinit(e: *Encoder, a: Allocator) void {
        e.body.deinit(a);
    }

    pub fn add(e: *Encoder, a: Allocator, rec: Record) Allocator.Error!void {
        try appendUvarint(a, &e.body, rec.key);
        try appendUvarint(a, &e.body, rec.name.len);
        try e.body.appendSlice(a, rec.name);
        try appendUvarint(a, &e.body, @intCast(rec.size));
        try appendRanges(a, &e.body, rec.done);
        try appendRanges(a, &e.body, rec.damaged);
        try appendRanges(a, &e.body, rec.lost);
        e.count += 1;
    }

    /// The blob, allocated in `a`.
    pub fn finish(e: *Encoder, a: Allocator) Allocator.Error![]u8 {
        var out: std.ArrayList(u8) = try .initCapacity(a, e.body.items.len + 24);
        errdefer out.deinit(a);
        out.appendSliceAssumeCapacity(magic);
        out.appendAssumeCapacity(version);
        try appendUvarint(a, &out, e.count);
        try out.appendSlice(a, e.body.items);
        const crc = std.hash.Crc32.hash(out.items);
        try out.appendSlice(a, &std.mem.toBytes(std.mem.nativeToLittle(u32, crc)));
        return out.toOwnedSlice(a);
    }
};

pub fn encode(a: Allocator, recs: []const Record) Allocator.Error![]u8 {
    var e: Encoder = .{};
    defer e.deinit(a);
    for (recs) |rec| try e.add(a, rec);
    return e.finish(a);
}

/// Reads varints from a bounded buffer; the first problem sticks.
const Reader = struct {
    b: []const u8,
    failed: bool = false,

    fn fail(r: *Reader) void {
        r.failed = true;
    }

    /// Go's binary.Uvarint: at most 10 bytes, the tenth must be 0 or 1.
    /// Non-minimal encodings are accepted, as there.
    fn uvarint(r: *Reader) u64 {
        if (r.failed) return 0;
        var x: u64 = 0;
        var shift: u7 = 0;
        for (r.b, 0..) |byte, i| {
            if (i == 10) break;
            if (byte < 0x80) {
                if (i == 9 and byte > 1) break;
                r.b = r.b[i + 1 ..];
                return x | (@as(u64, byte) << @intCast(shift));
            }
            x |= @as(u64, byte & 0x7f) << @intCast(shift);
            shift += 7;
        }
        r.fail();
        return 0;
    }

    /// An element count, bounded by the bytes left given the minimum encoded
    /// size of one element.
    fn count(r: *Reader, min_elem: usize) usize {
        const n = r.uvarint();
        if (!r.failed and n > r.b.len / min_elem) {
            r.fail();
            return 0;
        }
        return @intCast(n);
    }

    fn ranges(r: *Reader, a: Allocator, size: i64) Allocator.Error![]const Range {
        const n = r.count(2);
        if (r.failed or n == 0) return &.{};
        const out = try a.alloc(Range, n);
        var prev: i64 = 0;
        var i: usize = 0;
        while (i < n) : (i += 1) {
            const gap = r.uvarint();
            const length = r.uvarint();
            if (r.failed) break;
            const usize_: u64 = @intCast(size);
            if (gap > usize_ or length == 0 or length > usize_ or (i > 0 and gap == 0)) {
                r.fail();
                break;
            }
            const start = prev + @as(i64, @intCast(gap));
            const end = start + @as(i64, @intCast(length));
            if (end > size) {
                r.fail();
                break;
            }
            out[i] = .{ .start = start, .end = end };
            prev = end;
        }
        return out;
    }
};

/// Decodes and validates a blob. The records, names and range lists are
/// allocated in `a` (pass an arena). Everything accepted is canonical: names
/// are safe and distinct, keys distinct, ranges sorted, non-empty, disjoint
/// and non-adjacent, and the three sets of a file do not overlap.
pub fn decode(a: Allocator, data: []const u8) DecodeError![]Record {
    const hdr = magic.len + 1;
    if (data.len < hdr + 1 + 4 or !std.mem.eql(u8, data[0..magic.len], magic)) return error.InvalidSnapshot;
    if (data[magic.len] != version) return error.InvalidSnapshot;
    const body = data[0 .. data.len - 4];
    const sum = std.mem.readInt(u32, data[data.len - 4 ..][0..4], .little);
    if (std.hash.Crc32.hash(body) != sum) return error.InvalidSnapshot;

    var r: Reader = .{ .b = body[hdr..] };
    const n = r.count(5);
    if (r.failed) return error.InvalidSnapshot;
    var recs: std.ArrayList(Record) = try .initCapacity(a, n);
    var keys: std.AutoHashMapUnmanaged(u32, void) = .empty;
    var names: std.StringHashMapUnmanaged(void) = .empty;
    defer keys.deinit(a);
    defer names.deinit(a);
    try keys.ensureTotalCapacity(a, @intCast(n));
    try names.ensureTotalCapacity(a, @intCast(n));

    var i: usize = 0;
    while (i < n) : (i += 1) {
        const key = r.uvarint();
        const name_len = r.count(1);
        if (r.failed) return error.InvalidSnapshot;
        const name = r.b[0..name_len];
        r.b = r.b[name_len..];
        const size = r.uvarint();
        if (r.failed or size < 1 or size > max_file_size) return error.InvalidSnapshot;
        if (key > max_key or keys.contains(@intCast(key))) return error.InvalidSnapshot;
        const safe = fsutil.safeName(a, name) catch |err| switch (err) {
            error.InvalidName => return error.InvalidSnapshot,
            error.OutOfMemory => return error.OutOfMemory,
        };
        if (!std.mem.eql(u8, safe, name) or names.contains(name)) return error.InvalidSnapshot;
        const sz: i64 = @intCast(size);
        const done = try r.ranges(a, sz);
        const damaged = try r.ranges(a, sz);
        const lost = try r.ranges(a, sz);
        if (r.failed) return error.InvalidSnapshot;
        if (!ranges_mod.setsDisjoint(&.{ done, damaged, lost })) return error.InvalidSnapshot;
        keys.putAssumeCapacity(@intCast(key), {});
        names.putAssumeCapacity(safe, {});
        recs.appendAssumeCapacity(.{ .key = @intCast(key), .name = safe, .size = sz, .done = done, .damaged = damaged, .lost = lost });
    }
    if (r.b.len != 0) return error.InvalidSnapshot;
    return recs.items;
}

// ---- tests ----

const testing = std.testing;

fn rg(a: i64, b: i64) Range {
    return .{ .start = a, .end = b };
}

test "an empty snapshot is the header, a zero count and the checksum" {
    const blob = try encode(testing.allocator, &.{});
    defer testing.allocator.free(blob);
    try testing.expectEqualSlices(u8, "GNFW\x01\x00", blob[0..6]);
    try testing.expectEqual(@as(usize, 10), blob.len);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqual(@as(usize, 0), (try decode(arena.allocator(), blob)).len);
}

test "records round-trip" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const recs = [_]Record{
        .{ .key = 3, .name = "a.bin", .size = 100, .done = &.{ rg(0, 10), rg(20, 30) }, .damaged = &.{rg(40, 50)}, .lost = &.{rg(90, 100)} },
        .{ .key = 4000000, .name = "日本.mkv", .size = max_file_size, .done = &.{rg(0, max_file_size)}, .damaged = &.{}, .lost = &.{} },
    };
    const blob = try encode(a, &recs);
    const got = try decode(a, blob);
    try testing.expectEqual(@as(usize, 2), got.len);
    for (recs, got) |w, g| {
        try testing.expectEqual(w.key, g.key);
        try testing.expectEqualStrings(w.name, g.name);
        try testing.expectEqual(w.size, g.size);
        inline for (.{ "done", "damaged", "lost" }) |f| {
            try testing.expectEqual(@field(w, f).len, @field(g, f).len);
            for (@field(w, f), @field(g, f)) |x, y| try testing.expect(x.eql(y));
        }
    }
    // Canonical: encoding what was decoded gives the same bytes.
    try testing.expectEqualSlices(u8, blob, try encode(a, got));
}

test "the wire format is the Go one (a record spelled out)" {
    // key 3, "a.bin", size 100, done [0,10) [20,30), damaged [40,50), lost [90,100).
    const want = "GNFW\x01\x01" ++ "\x03\x05a.bin\x64" ++ "\x02\x00\x0a\x0a\x0a" ++ "\x01\x28\x0a" ++ "\x01\x5a\x0a";
    const blob = try encode(testing.allocator, &.{.{ .key = 3, .name = "a.bin", .size = 100, .done = &.{ rg(0, 10), rg(20, 30) }, .damaged = &.{rg(40, 50)}, .lost = &.{rg(90, 100)} }});
    defer testing.allocator.free(blob);
    try testing.expectEqualSlices(u8, want, blob[0 .. blob.len - 4]);
    const crc = std.hash.Crc32.hash(want);
    try testing.expectEqual(crc, std.mem.readInt(u32, blob[blob.len - 4 ..][0..4], .little));
}

/// Wraps `body` (everything between the version byte and the checksum) into a
/// valid blob.
fn wrap(a: Allocator, body: []const u8) ![]u8 {
    const head = try std.mem.concat(a, u8, &.{ "GNFW\x01", body });
    var out = try a.alloc(u8, head.len + 4);
    @memcpy(out[0..head.len], head);
    std.mem.writeInt(u32, out[head.len..][0..4], std.hash.Crc32.hash(head), .little);
    return out;
}

test "malformed snapshots are refused" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const good = try encode(a, &.{.{ .key = 0, .name = "x.bin", .size = 100, .done = &.{rg(0, 50)}, .damaged = &.{}, .lost = &.{} }});

    var flipped = try a.dupe(u8, good);
    flipped[flipped.len / 2] ^= 0xff;
    var bad_version = try a.dupe(u8, good);
    bad_version[4] = 99;
    const cases = [_][]const u8{
        "",
        "hello world, not a snapshot",
        good[0 .. good.len - 3],
        flipped,
        bad_version,
        "GNFW\x01\x00", // too short
        try wrap(a, "\x00\x00"), // trailing byte
        try wrap(a, "\x01\x00\x05x.bin\x00\x00\x00\x00"), // size 0
        try wrap(a, "\x01\x00\x05x.bin\x64\x01\x00\x65\x00\x00"), // range longer than the file
        try wrap(a, "\x01\x00\x05x.bin\x64\x02\x00\x0a\x00\x0a\x00\x00"), // adjacent ranges (gap 0 after the first)
        try wrap(a, "\x01\x00\x05x.bin\x64\x01\x00\x0a\x01\x00\x05\x00"), // done and damaged overlap
        try wrap(a, "\x01\x00\x05x/bin\x64\x00\x00\x00"), // unsafe name
        try wrap(a, "\x01\x80\x80\x80\x80\x08\x05x.bin\x64\x00\x00\x00"), // key 2^31
        try wrap(a, "\x02\x00\x05x.bin\x64\x00\x00\x00\x01\x05x.bin\x64\x00\x00\x00"), // duplicate name
        try wrap(a, "\x02\x00\x05x.bin\x64\x00\x00\x00\x00\x05y.bin\x64\x00\x00\x00"), // duplicate key
        try wrap(a, "\xff\xff\xff\xff\xff\xff\xff\xff\xff\x7f"), // count beyond the data
        try wrap(a, "\x01\x00\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\x01"), // varint overflow
        try wrap(a, "\x01\x00\x05x.bin\x64\x00\x00"), // truncated
    };
    for (cases, 0..) |c, i| {
        testing.expectError(error.InvalidSnapshot, decode(a, c)) catch |err| {
            std.debug.print("case {d}\n", .{i});
            return err;
        };
    }
}

test "a count larger than the data allows allocates nothing" {
    // 2^62 files in a 12 byte blob: refused before anything is reserved.
    var fa: testing.FailingAllocator = .init(testing.allocator, .{ .fail_index = 0 });
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const blob = try wrap(arena.allocator(), "\xff\xff\xff\xff\xff\xff\xff\xff\x3f");
    try testing.expectError(error.InvalidSnapshot, decode(fa.allocator(), blob));
}

test "the snapshot is compact" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var done: std.ArrayList(Range) = .empty;
    var i: i64 = 0;
    while (i < 100) : (i += 1) try done.append(a, rg(i * 1000, i * 1000 + 900));
    const blob = try encode(a, &.{.{ .key = 0, .name = "big.bin", .size = 100_000, .done = done.items, .damaged = &.{}, .lost = &.{} }});
    try testing.expect(blob.len < 400);
}

/// The Go FuzzDecodeSnapshot: whatever is accepted is canonical.
fn checkDecode(data: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const recs = decode(a, data) catch |err| switch (err) {
        error.InvalidSnapshot => return,
        error.OutOfMemory => return err,
    };
    for (recs) |rec| {
        try testing.expect(rec.size >= 1 and rec.size <= max_file_size);
        try testing.expect(ranges_mod.setsDisjoint(&.{ rec.done, rec.damaged, rec.lost }));
        for ([_][]const Range{ rec.done, rec.damaged, rec.lost }) |s| {
            var prev: i64 = -1;
            for (s) |x| {
                try testing.expect(x.start >= 0 and x.end > x.start and x.end <= rec.size);
                try testing.expect(x.start > prev);
                prev = x.end;
            }
        }
    }
    // What a decode accepts, an encode reproduces in a form that decodes to
    // the same records (a non-minimal varint in the input is the only way the
    // bytes can differ).
    const again = try encode(a, recs);
    const recs2 = try decode(a, again);
    try testing.expectEqual(recs.len, recs2.len);
    try testing.expectEqualSlices(u8, again, try encode(a, recs2));
}

/// The fuzz input is the body of a snapshot: the target adds the header and
/// the checksum, so the decoder is reached past them.
fn fuzzDecode(_: void, smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try checkDecode(try wrap(arena.allocator(), buf[0..n]));
    try checkDecode(buf[0..n]);
}

fn seedOf(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

test "fuzz: decode refuses or accepts canonically, never crashes" {
    const corpus = [_][]const u8{
        seedOf("\x00"),
        seedOf("\x01\x03\x05a.bin\x64\x02\x00\x0a\x0a\x0a\x01\x28\x0a\x01\x5a\x0a"),
        seedOf("\x01\x00\x05x.bin\x64\x00\x00\x00"),
        seedOf("\x02\x00\x01a\x0a\x00\x00\x00\x01\x01b\x0a\x00\x00\x00"),
        seedOf("\x01\x00\x05x.bin\x64\x01\x00\x65\x00\x00"),
        seedOf("\xff\xff\xff\xff\xff\xff\xff\xff\xff\x7f"),
        seedOf(""),
    };
    for (corpus) |input| {
        var smith: testing.Smith = .{ .in = input };
        try fuzzDecode({}, &smith);
    }
    try testing.fuzz({}, fuzzDecode, .{ .corpus = &corpus });
}
