//! The yEnc decoder (internal/usenet/yenc): one Article body in, one decoded
//! `Part` out. It is fed one line at a time, the way the NNTP body sink
//! delivers them: dot-unstuffed, without the line terminator (a trailing CR
//! LF is ignored anyway).
//!
//! Memory: the decoder allocates the decoded data (sized by the header, at
//! most `max_part_size`) and a copy of the file name from the allocator given
//! to `init`. Hand it an arena per Article: nothing is freed one by one. The
//! `Part` that `finish` returns points into those allocations and lives as
//! long as the allocator's memory does; once `finish` has succeeded the
//! decoder no longer frees them, `deinit` and `reset` leave them to the
//! caller (to the arena). Before that, `deinit`, `reset` and any error free
//! them.
//!
//! Hostile input: the declared sizes are checked before anything is
//! allocated, data beyond the declared range is rejected as it arrives, and
//! the first error sticks (every later call returns it). The line length is
//! not capped here, the NNTP layer caps it (MaxLineLength).
const std = @import("std");
const Allocator = std.mem.Allocator;
const Part = @import("yenc_types.zig").Part;

pub const Error = error{
    /// The header, trailer or data violates the yEnc syntax (Go: ErrMalformed).
    Malformed,
    /// The decoded length differs from the declared range or the trailer size
    /// (ErrLength).
    Length,
    /// A =ypart range that is empty, inverted or outside the file (ErrRange).
    Range,
    /// A file or part larger than the limits below (ErrTooLarge).
    TooLarge,
    /// No =ybegin line (ErrNoHeader).
    NoHeader,
    OutOfMemory,
};

/// The largest file size a =ybegin may announce (1 TiB).
pub const max_file_size: i64 = 1 << 40;
/// The largest decoded part (16 MiB); it also bounds the memory of one Decoder.
pub const max_part_size: i64 = 16 << 20;

const State = enum {
    /// Lines before =ybegin are ignored.
    seeking_header,
    /// A multipart =ybegin was seen, =ypart must follow.
    awaiting_part,
    in_data,
    /// =yend was seen; text after it, such as a signature, is ignored.
    done,
};

pub const Decoder = struct {
    allocator: Allocator,
    state: State = .seeking_header,
    part: Part = .{ .name = "", .size = 0, .begin = 0, .end = 0, .data = "", .crc32 = 0 },
    multipart: bool = false,
    /// Decoded data, sized to the declared part length, and the file name:
    /// both from `allocator`.
    buf: []u8 = &.{},
    name: []u8 = &.{},
    /// Bytes of `buf` in use.
    n: usize = 0,
    /// The first error; once set, every call returns it.
    err: ?Error = null,
    /// `finish` succeeded: the caller owns `buf` and `name`.
    handed_over: bool = false,

    pub fn init(allocator: Allocator) Decoder {
        return .{ .allocator = allocator };
    }

    /// Frees what the decoder still owns (nothing after a successful `finish`).
    pub fn deinit(d: *Decoder) void {
        d.release();
        d.* = undefined;
    }

    /// Makes the decoder ready for the next Article.
    pub fn reset(d: *Decoder) void {
        const allocator = d.allocator;
        d.release();
        d.* = .init(allocator);
    }

    fn release(d: *Decoder) void {
        if (!d.handed_over) {
            d.allocator.free(d.buf);
            d.allocator.free(d.name);
        }
        d.buf = &.{};
        d.name = &.{};
    }

    /// Decodes one line of the Article body. After an error the decoder is
    /// unusable (until `reset`) and every call returns that error.
    pub fn feed(d: *Decoder, raw: []const u8) Error!void {
        if (d.err) |e| return e;
        d.step(raw) catch |e| {
            d.err = e;
            d.release();
            return e;
        };
    }

    fn step(d: *Decoder, raw: []const u8) Error!void {
        const line = std.mem.trimEnd(u8, raw, "\r\n");
        switch (d.state) {
            .seeking_header => {
                if (std.mem.cutPrefix(u8, line, "=ybegin ")) |rest| return d.begin(rest);
            },
            .awaiting_part => {
                const rest = std.mem.cutPrefix(u8, line, "=ypart ") orelse return error.Malformed;
                return d.ypart(rest);
            },
            .in_data => {
                if (!std.mem.startsWith(u8, line, "=y")) return d.data(line);
                if (std.mem.cutPrefix(u8, line, "=yend ")) |rest| return d.end(rest);
                return error.Malformed;
            },
            .done => {},
        }
    }

    fn begin(d: *Decoder, line: []const u8) Error!void {
        const named = try cutName(line);
        const f = try parseFields(&.{ "size", "line", "part", "total" }, named.rest);
        const size = try number(f[0] orelse return error.Malformed);
        if (size > max_file_size) return error.TooLarge;
        _ = try optNumber(f[1]);
        const part = try optNumber(f[2]);
        const total = try optNumber(f[3]);
        const part_n: i64 = part orelse 0;
        const max_i32 = std.math.maxInt(i32);
        if ((part != null and (part_n < 1 or part_n > max_i32)) or
            (total != null and (total.? < 1 or total.? > max_i32 or part_n > total.?)))
            return error.Malformed;

        d.name = try d.allocator.dupe(u8, named.name);
        d.part = .{ .name = d.name, .size = size, .begin = 1, .end = size, .data = "", .crc32 = 0 };
        if (part != null) {
            d.multipart = true;
            // Without total= a multipart file claims 0 parts, as in Go.
            d.part.part_number = @intCast(part_n);
            d.part.total_parts = @intCast(total orelse 0);
            d.state = .awaiting_part;
            return;
        }
        d.state = .in_data;
        return d.allocate();
    }

    /// Checks the declared part length and reserves room for it.
    fn allocate(d: *Decoder) Error!void {
        const len = d.part.end - d.part.begin + 1;
        if (len > max_part_size) return error.TooLarge;
        d.buf = try d.allocator.alloc(u8, @intCast(len));
    }

    fn ypart(d: *Decoder, line: []const u8) Error!void {
        const f = try parseFields(&.{ "begin", "end" }, line);
        const first = try number(f[0] orelse return error.Malformed);
        const last = try number(f[1] orelse return error.Malformed);
        if (first < 1 or last < first or last > d.part.size) return error.Range;
        d.part.begin = first;
        d.part.end = last;
        d.state = .in_data;
        return d.allocate();
    }

    fn end(d: *Decoder, line: []const u8) Error!void {
        d.state = .done;
        const f = try parseFields(&.{ "size", "part", "pcrc32", "crc32" }, line);
        const size = try number(f[0] orelse return error.Malformed);
        if (size != @as(i64, @intCast(d.n))) return error.Length;
        if (try optNumber(f[1])) |part| {
            if (part != d.part.part_number) return error.Malformed;
        }
        const pcrc = try optCrc(f[2]);
        const crc = try optCrc(f[3]);
        if (pcrc) |c| {
            d.part.expected_crc32 = c;
            d.part.has_expected_crc = true;
        } else if (crc) |c| {
            // For a single-part file the whole-file CRC is the part's CRC; in a
            // multipart trailer it covers the whole file and is ignored.
            if (!d.multipart) {
                d.part.expected_crc32 = c;
                d.part.has_expected_crc = true;
            }
        }
    }

    fn data(d: *Decoder, line: []const u8) Error!void {
        d.n += try decodeLine(d.buf[d.n..], line);
    }

    /// Validates the part and returns it. `data` and `name` of the result live
    /// in the decoder's allocator (see the top of the file). May be called
    /// again for the same Part.
    pub fn finish(d: *Decoder) Error!Part {
        if (d.err) |e| return e;
        if (d.state == .seeking_header) return error.NoHeader;
        if (d.state != .done) return error.Malformed;
        if (d.n != d.buf.len) return error.Length;
        d.part.data = d.buf;
        d.part.crc32 = std.hash.Crc32.hash(d.buf);
        d.handed_over = true;
        return d.part;
    }
};

/// Decodes `src` into `dst` and returns the number of bytes written. CR and
/// LF anywhere in `src` are dropped first. Fails rather than write past the
/// end of `dst`.
///
/// Hot path: eight bytes at a time while none of them is '=', CR or LF
/// (subtracting 42 in every byte lane of a u64 without letting carries cross
/// lanes), then byte by byte.
fn decodeLine(dst: []u8, src: []const u8) error{ Malformed, Length }!usize {
    var i: usize = 0;
    var n: usize = 0;
    while (i < src.len) {
        if (i + 8 <= src.len and n + 8 <= dst.len) {
            const x = std.mem.readInt(u64, src[i..][0..8], .little);
            if (!hasSpecial(x)) {
                std.mem.writeInt(u64, dst[n..][0..8], subtract42(x), .little);
                i += 8;
                n += 8;
                continue;
            }
        }
        const c = src[i];
        i += 1;
        switch (c) {
            '\r', '\n' => {},
            '=' => {
                while (i < src.len and (src[i] == '\r' or src[i] == '\n')) i += 1;
                if (i == src.len) return error.Malformed;
                if (n == dst.len) return error.Length;
                dst[n] = src[i] -% (64 + 42);
                i += 1;
                n += 1;
            },
            else => {
                if (n == dst.len) return error.Length;
                dst[n] = c -% 42;
                n += 1;
            },
        }
    }
    return n;
}

const lanes: u64 = 0x0101010101010101;

/// Whether any byte of `x` is '=', CR or LF.
fn hasSpecial(x: u64) bool {
    return hasZero(x ^ (lanes * '=')) or hasZero(x ^ (lanes * '\r')) or hasZero(x ^ (lanes * '\n'));
}

fn hasZero(v: u64) bool {
    return ((v -% lanes) & ~v & (lanes * 0x80)) != 0;
}

/// x[i] -% 42 in every byte lane.
fn subtract42(x: u64) u64 {
    const low = lanes * 0x7f;
    const high = lanes * 0x80;
    const k = lanes * 0xd6; // -42
    return ((x & low) +% (k & low)) ^ ((x ^ k) & high);
}

/// Splits a =ybegin argument list into the name, which is the rest of the
/// line after "name=", and the fields before it.
fn cutName(s: []const u8) Error!struct { name: []const u8, rest: []const u8 } {
    if (std.mem.cutPrefix(u8, s, "name=")) |v| return .{ .name = v, .rest = "" };
    if (std.mem.cut(u8, s, " name=")) |c| return .{ .name = c[1], .rest = c[0] };
    return error.Malformed;
}

/// The length of the Unicode white space (Go's unicode.IsSpace, which
/// strings.Fields splits on) that starts `s`, or 0.
fn spaceLen(s: []const u8) usize {
    switch (s[0]) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ' => return 1,
        0xc2 => if (s.len >= 2 and (s[1] == 0x85 or s[1] == 0xa0)) return 2,
        0xe1 => if (s.len >= 3 and s[1] == 0x9a and s[2] == 0x80) return 3, // U+1680
        0xe2 => if (s.len >= 3) {
            if (s[1] == 0x80 and ((s[2] >= 0x80 and s[2] <= 0x8a) or s[2] == 0xa8 or s[2] == 0xa9 or s[2] == 0xaf)) return 3;
            if (s[1] == 0x81 and s[2] == 0x9f) return 3; // U+205F
        },
        0xe3 => if (s.len >= 3 and s[1] == 0x80 and s[2] == 0x80) return 3, // U+3000
        else => {},
    }
    return 0;
}

/// Collects the known keys of a keyword line, in the order of `known`. Real
/// posters add odd extras, so tokens that are not a known key=value pair are
/// ignored. A repeated known key is malformed. A known key with an empty value
/// is kept so that its typed accessor rejects it.
fn parseFields(comptime known: []const []const u8, s: []const u8) Error![known.len]?[]const u8 {
    var out: [known.len]?[]const u8 = @splat(null);
    var i: usize = 0;
    while (i < s.len) {
        const sp = spaceLen(s[i..]);
        if (sp > 0) {
            i += sp;
            continue;
        }
        const start = i;
        while (i < s.len and spaceLen(s[i..]) == 0) i += 1;
        const tok = s[start..i];
        const eq = std.mem.findScalar(u8, tok, '=') orelse tok.len;
        const key = tok[0..eq];
        const value = if (eq < tok.len) tok[eq + 1 ..] else "";
        inline for (known, 0..) |k, idx| {
            if (std.mem.eql(u8, key, k)) {
                if (out[idx] != null) return error.Malformed;
                out[idx] = value;
            }
        }
    }
    return out;
}

/// A decimal number: only plain digits, no sign, prefix or separators.
fn number(v: []const u8) Error!i64 {
    if (v.len == 0) return error.Malformed;
    for (v) |c| if (c < '0' or c > '9') return error.Malformed;
    return std.fmt.parseInt(i64, v, 10) catch error.Malformed;
}

fn optNumber(v: ?[]const u8) Error!?i64 {
    return try number(v orelse return null);
}

/// A CRC32 in hexadecimal, at most eight significant digits.
fn optCrc(v: ?[]const u8) Error!?u32 {
    const s = v orelse return null;
    if (s.len == 0) return error.Malformed;
    for (s) |c| if (!std.ascii.isHex(c)) return error.Malformed;
    return std.fmt.parseInt(u32, s, 16) catch error.Malformed;
}
