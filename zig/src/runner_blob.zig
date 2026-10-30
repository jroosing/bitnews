//! The resume blob of a fetch (internal/usenet/runner progress.go): the file
//! writer's snapshot plus the Articles already resolved. Byte ranges cannot
//! say which Articles are done (NZB sizes are encoded sizes), so the Articles
//! are recorded themselves. The format is the Go one, so a database written
//! by either daemon resumes in the other:
//!
//!     "GNRP" version(1 byte)
//!     len(writer snapshot) as uvarint, writer snapshot
//!     nsegments as uvarint, nsegments x { file uvarint, segment uvarint, status (1 byte) }
//!     crc32 (IEEE, 4 bytes little endian) over everything before it
//!
//! The blob is read back from the database, so it is untrusted input: the
//! decoder checks the checksum, every length against what is left, and every
//! number against the range the rest of the runner uses.
const std = @import("std");
const Allocator = std.mem.Allocator;

pub const magic = "GNRP";
pub const version: u8 = 1;
/// The largest file index or segment number a record may carry (Go: int32).
pub const max_index: u64 = std.math.maxInt(i32);
/// The largest blob the store keeps (MaxUsenetProgressBlob).
pub const max_bytes: usize = 16 << 20;

pub const DecodeError = error{InvalidBlob} || Allocator.Error;

/// The final outcome of one Article. 0 is "not resolved yet" and never stored.
pub const Status = enum(u8) {
    unresolved = 0,
    done = 1,
    damaged = 2,
    missing = 3,
};

pub const SegRecord = struct {
    file: u32,
    seg: u32,
    status: Status,
};

pub const Blob = struct {
    /// The file writer's snapshot (`filewriter.Writer.snapshot`).
    writer: []const u8,
    /// Sorted by (file, seg) when produced by the runner; the decoder does not
    /// require it.
    segments: []const SegRecord,
};

fn appendUvarint(a: Allocator, b: *std.ArrayList(u8), v_in: u64) Allocator.Error!void {
    var v = v_in;
    while (v >= 0x80) {
        try b.append(a, @as(u8, @truncate(v)) | 0x80);
        v >>= 7;
    }
    try b.append(a, @truncate(v));
}

/// Encodes `blob` into `a`.
pub fn encode(a: Allocator, blob: Blob) Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    errdefer out.deinit(a);
    try out.ensureTotalCapacity(a, magic.len + 1 + 10 + blob.writer.len + 10 + blob.segments.len * 6 + 4);
    try out.appendSlice(a, magic);
    try out.append(a, version);
    try appendUvarint(a, &out, blob.writer.len);
    try out.appendSlice(a, blob.writer);
    try appendUvarint(a, &out, blob.segments.len);
    for (blob.segments) |s| {
        try appendUvarint(a, &out, s.file);
        try appendUvarint(a, &out, s.seg);
        try out.append(a, @backingInt(s.status));
    }
    const sum = std.hash.Crc32.hash(out.items);
    try out.appendSlice(a, &std.mem.toBytes(std.mem.nativeToLittle(u32, sum)));
    return out.toOwnedSlice(a);
}

const Reader = struct {
    rest: []const u8,

    fn uvarint(r: *Reader) error{InvalidBlob}!u64 {
        var v: u64 = 0;
        var shift: u7 = 0;
        for (r.rest, 0..) |byte, i| {
            if (i >= 10) return error.InvalidBlob;
            const low: u64 = byte & 0x7f;
            if (shift == 63 and low > 1) return error.InvalidBlob;
            v |= low << @intCast(shift);
            if (byte < 0x80) {
                r.rest = r.rest[i + 1 ..];
                return v;
            }
            shift += 7;
        }
        return error.InvalidBlob;
    }
};

/// Decodes a blob. `writer` points into `data`; `segments` is allocated in `a`.
pub fn decode(a: Allocator, data: []const u8) DecodeError!Blob {
    const hdr = magic.len + 1;
    if (data.len < hdr + 4 or !std.mem.eql(u8, data[0..magic.len], magic)) return error.InvalidBlob;
    if (data[magic.len] != version) return error.InvalidBlob;
    const body = data[0 .. data.len - 4];
    const want = std.mem.readInt(u32, data[data.len - 4 ..][0..4], .little);
    if (std.hash.Crc32.hash(body) != want) return error.InvalidBlob;

    var r: Reader = .{ .rest = body[hdr..] };
    const wl = try r.uvarint();
    if (wl > r.rest.len) return error.InvalidBlob;
    const writer = r.rest[0..@intCast(wl)];
    r.rest = r.rest[@intCast(wl)..];

    const n = try r.uvarint();
    // A record is at least three bytes.
    if (n > r.rest.len / 3) return error.InvalidBlob;
    const segs = try a.alloc(SegRecord, @intCast(n));
    errdefer a.free(segs);
    for (segs) |*s| {
        const file = try r.uvarint();
        const seg = try r.uvarint();
        if (r.rest.len < 1 or file > max_index or seg > max_index) return error.InvalidBlob;
        const st = r.rest[0];
        if (st < @backingInt(Status.done) or st > @backingInt(Status.missing)) return error.InvalidBlob;
        s.* = .{ .file = @intCast(file), .seg = @intCast(seg), .status = @fromBackingInt(@intCast(st)) };
        r.rest = r.rest[1..];
    }
    if (r.rest.len != 0) return error.InvalidBlob;
    return .{ .writer = writer, .segments = segs };
}

const testing = std.testing;

fn sample() Blob {
    return .{
        .writer = "writer-snapshot",
        .segments = &.{
            .{ .file = 0, .seg = 1, .status = .done },
            .{ .file = 0, .seg = 3, .status = .missing },
            .{ .file = 12, .seg = 99, .status = .damaged },
        },
    };
}

test "a blob round trips" {
    const bytes = try encode(testing.allocator, sample());
    defer testing.allocator.free(bytes);
    const got = try decode(testing.allocator, bytes);
    defer testing.allocator.free(got.segments);
    try testing.expectEqualStrings("writer-snapshot", got.writer);
    try testing.expectEqualSlices(SegRecord, sample().segments, got.segments);
}

test "the format is the Go one" {
    // Produced by the Go runner's encodeBlob for an empty snapshot and one
    // resolved Article (file 3, segment 130, damaged).
    const bytes = try encode(testing.allocator, .{ .writer = "", .segments = &.{.{ .file = 3, .seg = 130, .status = .damaged }} });
    defer testing.allocator.free(bytes);
    try testing.expectEqualSlices(u8, "GNRP\x01\x00\x01\x03\x82\x01\x02", bytes[0 .. bytes.len - 4]);
    try testing.expectEqual(std.hash.Crc32.hash(bytes[0 .. bytes.len - 4]), std.mem.readInt(u32, bytes[bytes.len - 4 ..][0..4], .little));
}

test "corruption is refused" {
    const good = try encode(testing.allocator, sample());
    defer testing.allocator.free(good);
    var flipped = try testing.allocator.dupe(u8, good);
    defer testing.allocator.free(flipped);
    flipped[flipped.len / 2] ^= 0x40;
    var wrong_version = try testing.allocator.dupe(u8, good);
    defer testing.allocator.free(wrong_version);
    wrong_version[magic.len] = 9;
    var trailing = try testing.allocator.alloc(u8, good.len + 1);
    defer testing.allocator.free(trailing);
    @memcpy(trailing[0..good.len], good);
    trailing[good.len] = 0;

    const bad = [_][]const u8{ "", good[0 .. good.len - 3], flipped, wrong_version, trailing };
    for (bad) |data| try testing.expectError(error.InvalidBlob, decode(testing.allocator, data));
}

test "a count or a length beyond the data is refused, not allocated" {
    // 2^40 records claimed with nothing behind them, with a valid checksum.
    var body: std.ArrayList(u8) = .empty;
    defer body.deinit(testing.allocator);
    try body.appendSlice(testing.allocator, "GNRP\x01\x00");
    try appendUvarint(testing.allocator, &body, 1 << 40);
    try body.appendSlice(testing.allocator, &std.mem.toBytes(std.mem.nativeToLittle(u32, std.hash.Crc32.hash(body.items))));
    try testing.expectError(error.InvalidBlob, decode(testing.allocator, body.items));
}

fn checkDecode(data: []const u8) !void {
    const got = decode(testing.allocator, data) catch |err| switch (err) {
        error.InvalidBlob => return,
        error.OutOfMemory => return,
    };
    defer testing.allocator.free(got.segments);
    // Whatever decodes must re-encode to something that decodes equal.
    const again = try encode(testing.allocator, got);
    defer testing.allocator.free(again);
    const got2 = try decode(testing.allocator, again);
    defer testing.allocator.free(got2.segments);
    try testing.expectEqualSlices(u8, got.writer, got2.writer);
    try testing.expectEqualSlices(SegRecord, got.segments, got2.segments);
}

/// The fuzz input is the body of a blob: the target adds the header and the
/// checksum, so the decoder is reached past them.
fn fuzzDecode(_: void, smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    var framed: std.ArrayList(u8) = .empty;
    defer framed.deinit(testing.allocator);
    try framed.appendSlice(testing.allocator, "GNRP\x01");
    try framed.appendSlice(testing.allocator, buf[0..n]);
    try framed.appendSlice(testing.allocator, &std.mem.toBytes(std.mem.nativeToLittle(u32, std.hash.Crc32.hash(framed.items))));
    try checkDecode(framed.items);
    try checkDecode(buf[0..n]);
}

fn seedOf(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

test "fuzz: decode refuses or accepts canonically, never crashes" {
    const corpus = [_][]const u8{
        seedOf("\x00\x00"),
        seedOf("\x00\x01\x03\x82\x01\x02"),
        seedOf("\x03abc\x02\x00\x01\x01\x00\x02\x03"),
        seedOf("\xff\xff\xff\xff\xff\xff\xff\xff\xff\x7f"),
        seedOf("\x00\x01\xff\xff\xff\xff\x0f\x01\x01"),
        seedOf(""),
    };
    for (corpus) |input| {
        var smith: testing.Smith = .{ .in = input };
        try fuzzDecode({}, &smith);
    }
    try testing.fuzz({}, fuzzDecode, .{ .corpus = &corpus });
}
