//! Media types and their parameters (Go: mime.ParseMediaType, RFC 2045 and
//! RFC 2231), for the Content-Type and Content-Disposition headers of an
//! upload. The behaviour is Go's, quirks included: a media type with a bad
//! parameter, or the same parameter twice with different values, is an error;
//! a quoted string accepts MSIE's unescaped backslashes; `name*0=` pieces and
//! `name*=utf-8''...` values are stitched together.
//!
//! Strings of the result live in the arena the caller passes.
const std = @import("std");
const Allocator = std.mem.Allocator;

pub const Param = struct { key: []const u8, value: []const u8 };

pub const Media = struct {
    /// Lower case, trimmed.
    type: []const u8,
    /// Keys are lower case; values keep their case.
    params: []const Param,

    pub fn get(m: Media, key: []const u8) ?[]const u8 {
        for (m.params) |p| if (std.mem.eql(u8, p.key, key)) return p.value;
        return null;
    }
};

/// Any of Go's errors (no media type, bad parameter, duplicate parameter).
pub const Error = error{InvalidMediaType} || Allocator.Error;

fn isTSpecial(c: u8) bool {
    return switch (c) {
        '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=' => true,
        else => false,
    };
}

fn isTokenChar(c: u8) bool {
    return c > ' ' and c < 0x7f and !isTSpecial(c);
}

/// A token of at least one character, and the rest; token "" when there is none.
fn consumeToken(v: []const u8) struct { []const u8, []const u8 } {
    for (v, 0..) |c, i| {
        if (!isTokenChar(c)) return .{ v[0..i], v[i..] };
    }
    return .{ v, "" };
}

/// Go's unicode.IsSpace.
pub fn isSpaceRune(r: u21) bool {
    return switch (r) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ', 0x85, 0xa0, 0x1680, 0x2000...0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        else => false,
    };
}

/// Go's strings.TrimLeftFunc(s, unicode.IsSpace); an invalid byte is not a space.
fn trimLeftSpace(s: []const u8) []const u8 {
    var i: usize = 0;
    while (i < s.len) {
        const c = s[i];
        if (c < 0x80) {
            if (!isSpaceRune(c)) break;
            i += 1;
            continue;
        }
        const n = std.unicode.utf8ByteSequenceLength(c) catch break;
        if (i + n > s.len) break;
        const r = std.unicode.utf8Decode(s[i..][0..n]) catch break;
        if (!isSpaceRune(r)) break;
        i += n;
    }
    return s[i..];
}

/// Go's strings.TrimSpace (only the ASCII and Latin-1 spaces matter for a media type).
pub fn trimSpace(s: []const u8) []const u8 {
    var t = trimLeftSpace(s);
    while (t.len > 0) {
        const c = t[t.len - 1];
        if (c < 0x80) {
            if (!isSpaceRune(c)) break;
            t = t[0 .. t.len - 1];
            continue;
        }
        // A multi-byte space at the end: find the rune start.
        var start = t.len - 1;
        while (start > 0 and (t[start] & 0xc0) == 0x80) start -= 1;
        const n = std.unicode.utf8ByteSequenceLength(t[start]) catch break;
        if (start + n != t.len) break;
        const r = std.unicode.utf8Decode(t[start..]) catch break;
        if (!isSpaceRune(r)) break;
        t = t[0..start];
    }
    return t;
}

fn checkMediaType(s: []const u8) bool {
    const typ, var rest = consumeToken(s);
    if (typ.len == 0) return false;
    if (rest.len == 0) return true;
    if (rest[0] != '/') return false;
    rest = rest[1..];
    const sub, rest = consumeToken(rest);
    if (sub.len == 0) return false;
    return rest.len == 0;
}

const Value = struct { value: []const u8, rest: []const u8, ok: bool };

/// A token or a quoted string; not ok (and `rest` == v) on failure.
fn consumeValue(arena: Allocator, v: []const u8) Allocator.Error!Value {
    if (v.len == 0) return .{ .value = "", .rest = v, .ok = false };
    if (v[0] != '"') {
        const tok, const rest = consumeToken(v);
        return .{ .value = tok, .rest = rest, .ok = tok.len > 0 };
    }
    var buf: std.ArrayList(u8) = .empty;
    var i: usize = 1;
    while (i < v.len) : (i += 1) {
        const c = v[i];
        if (c == '"') return .{ .value = buf.items, .rest = v[i + 1 ..], .ok = true };
        // MSIE sends "C:\dev\foo.txt" without escaping the backslashes: a
        // backslash before anything but a tspecial is a literal backslash.
        if (c == '\\' and i + 1 < v.len and isTSpecial(v[i + 1])) {
            try buf.append(arena, v[i + 1]);
            i += 1;
            continue;
        }
        if (c == '\r' or c == '\n') return .{ .value = "", .rest = v, .ok = false };
        try buf.append(arena, c);
    }
    return .{ .value = "", .rest = v, .ok = false };
}

const ParamResult = struct { key: []const u8, value: []const u8, rest: []const u8 };

/// One `; key=value`; the key is "" on failure (and `rest` is `v`).
fn consumeMediaParam(arena: Allocator, v: []const u8) Allocator.Error!ParamResult {
    var rest = trimLeftSpace(v);
    if (rest.len == 0 or rest[0] != ';') return .{ .key = "", .value = "", .rest = v };
    rest = trimLeftSpace(rest[1..]);
    const tok, rest = consumeToken(rest);
    if (tok.len == 0) return .{ .key = "", .value = "", .rest = v };
    const key = try std.ascii.allocLowerString(arena, tok);
    rest = trimLeftSpace(rest);
    if (rest.len == 0 or rest[0] != '=') return .{ .key = "", .value = "", .rest = v };
    rest = trimLeftSpace(rest[1..]);
    const val = try consumeValue(arena, rest);
    // Go: `value == "" && rest2 == rest` is the failure; an empty quoted
    // string ("") succeeds because it consumed input.
    if (!val.ok) return .{ .key = "", .value = "", .rest = v };
    return .{ .key = key, .value = val.value, .rest = val.rest };
}

fn isHex(c: u8) bool {
    return std.ascii.isHex(c);
}

/// Go's percentHexUnescape, including its test `i+2 >= len(s)`.
fn percentHexUnescape(arena: Allocator, s: []const u8) Allocator.Error!?[]const u8 {
    var i: usize = 0;
    var percents: usize = 0;
    while (i < s.len) {
        if (s[i] != '%') {
            i += 1;
            continue;
        }
        percents += 1;
        if (i + 2 >= s.len or !isHex(s[i + 1]) or !isHex(s[i + 2])) return null;
        i += 3;
    }
    if (percents == 0) return s;
    const out = try arena.alloc(u8, s.len - 2 * percents);
    var j: usize = 0;
    i = 0;
    while (i < s.len) {
        if (s[i] == '%') {
            out[j] = (hexVal(s[i + 1]) << 4) | hexVal(s[i + 2]);
            i += 3;
        } else {
            out[j] = s[i];
            i += 1;
        }
        j += 1;
    }
    return out;
}

fn hexVal(c: u8) u8 {
    return std.fmt.charToDigit(c, 16) catch 0;
}

/// RFC 2231 `charset'language'percent-encoded`; only us-ascii and utf-8.
fn decode2231Enc(arena: Allocator, v: []const u8) Allocator.Error!?[]const u8 {
    const q1 = std.mem.findScalar(u8, v, '\'') orelse return null;
    const charset = v[0..q1];
    const after = v[q1 + 1 ..];
    const q2 = std.mem.findScalar(u8, after, '\'') orelse return null;
    const vals = after[q2 + 1 ..];
    if (!std.ascii.eqlIgnoreCase(charset, "us-ascii") and !std.ascii.eqlIgnoreCase(charset, "utf-8")) return null;
    return percentHexUnescape(arena, vals);
}

const Piece = struct { key: []const u8, value: []const u8 };

fn setParam(arena: Allocator, list: *std.ArrayList(Param), key: []const u8, value: []const u8) Allocator.Error!void {
    for (list.items) |*p| {
        if (std.mem.eql(u8, p.key, key)) {
            p.value = value;
            return;
        }
    }
    try list.append(arena, .{ .key = key, .value = value });
}

fn findPiece(pieces: []const Piece, key: []const u8) ?[]const u8 {
    for (pieces) |p| if (std.mem.eql(u8, p.key, key)) return p.value;
    return null;
}

/// Parses `v` like Go's mime.ParseMediaType. Any error of Go's is
/// `error.InvalidMediaType`.
pub fn parse(arena: Allocator, v_in: []const u8) Error!Media {
    const semi = std.mem.findScalar(u8, v_in, ';');
    const base = v_in[0 .. semi orelse v_in.len];
    const media_type = trimSpace(try std.ascii.allocLowerString(arena, base));
    if (!checkMediaType(media_type)) return error.InvalidMediaType;

    var params: std.ArrayList(Param) = .empty;
    // The parameters with a '*', in order: the pieces of RFC 2231 values.
    var starred: std.ArrayList(Piece) = .empty;
    var v = v_in[base.len..];
    while (v.len > 0) {
        v = trimLeftSpace(v);
        if (v.len == 0) break;
        const p = try consumeMediaParam(arena, v);
        if (p.key.len == 0) {
            // Trailing semicolons are not an error.
            if (std.mem.eql(u8, trimSpace(p.rest), ";")) break;
            return error.InvalidMediaType;
        }
        const has_star = std.mem.findScalar(u8, p.key, '*') != null;
        const existing: []const Piece = if (has_star) starred.items else &.{};
        if (has_star) {
            if (findPiece(existing, p.key)) |old| {
                // The same name twice is wrong, but allowed when equal.
                if (!std.mem.eql(u8, old, p.value)) return error.InvalidMediaType;
            } else try starred.append(arena, .{ .key = p.key, .value = p.value });
        } else {
            for (params.items) |q| {
                if (std.mem.eql(u8, q.key, p.key) and !std.mem.eql(u8, q.value, p.value)) return error.InvalidMediaType;
            }
            try setParam(arena, &params, p.key, p.value);
        }
        v = p.rest;
    }

    // Stitch the continuations ("foo*0", "foo*0*") and the "foo*" values.
    var done: std.ArrayList([]const u8) = .empty;
    for (starred.items) |first| {
        const star = std.mem.findScalar(u8, first.key, '*').?;
        const name = first.key[0..star];
        const seen = for (done.items) |d| {
            if (std.mem.eql(u8, d, name)) break true;
        } else false;
        if (seen) continue;
        try done.append(arena, name);

        const single = try std.fmt.allocPrint(arena, "{s}*", .{name});
        if (findPiece(starred.items, single)) |enc| {
            if (try decode2231Enc(arena, enc)) |dec| try setParam(arena, &params, name, dec);
            continue;
        }
        var buf: std.ArrayList(u8) = .empty;
        var valid = false;
        var n: usize = 0;
        while (true) : (n += 1) {
            const simple = try std.fmt.allocPrint(arena, "{s}*{d}", .{ name, n });
            if (findPiece(starred.items, simple)) |piece| {
                valid = true;
                try buf.appendSlice(arena, piece);
                continue;
            }
            const encoded = try std.fmt.allocPrint(arena, "{s}*", .{simple});
            const piece = findPiece(starred.items, encoded) orelse break;
            valid = true;
            if (n == 0) {
                if (try decode2231Enc(arena, piece)) |dec| try buf.appendSlice(arena, dec);
            } else if (try percentHexUnescape(arena, piece)) |dec| {
                try buf.appendSlice(arena, dec);
            }
        }
        if (valid) try setParam(arena, &params, name, buf.items);
    }
    return .{ .type = media_type, .params = params.items };
}

const testing = std.testing;

fn expectParam(m: Media, key: []const u8, want: ?[]const u8) !void {
    const got = m.get(key);
    if (want) |w| {
        try testing.expect(got != null);
        try testing.expectEqualStrings(w, got.?);
    } else try testing.expect(got == null);
}

test "a plain media type and its parameters" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try parse(arena.allocator(), "Multipart/Form-Data; Boundary=\"x y\"; charset=utf-8");
    try testing.expectEqualStrings("multipart/form-data", m.type);
    try expectParam(m, "boundary", "x y");
    try expectParam(m, "charset", "utf-8");
    try expectParam(m, "other", null);
}

test "form-data with a name and a file name" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try parse(arena.allocator(), "form-data; name=\"nzb\"; filename=\"My.Release.nzb\"");
    try testing.expectEqualStrings("form-data", m.type);
    try expectParam(m, "name", "nzb");
    try expectParam(m, "filename", "My.Release.nzb");
}

test "a quoted string takes escapes, and MSIE's bare backslashes stay" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try parse(arena.allocator(), "form-data; filename=\"a\\\"b\"; name=\"C:\\dev\\x.nzb\"");
    try expectParam(m, "filename", "a\"b");
    try expectParam(m, "name", "C:\\dev\\x.nzb");
}

test "RFC 2231 values: one encoded value, and continuations" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try expectParam(try parse(a, "attachment; filename*=utf-8''%e2%82%ac%20rates.nzb"), "filename", "\xe2\x82\xac rates.nzb");
    try expectParam(try parse(a, "attachment; filename*0=\"a\"; filename*1=\"b\"; filename*2=c"), "filename", "abc");
    try expectParam(try parse(a, "attachment; filename*0*=utf-8''a%20; filename*1*=b%20c"), "filename", "a b c");
    // Unsupported charset: the value is dropped.
    try expectParam(try parse(a, "attachment; filename*=iso-8859-1''x"), "filename", null);
    // The starred value replaces a plain one.
    try expectParam(try parse(a, "attachment; filename=plain; filename*=utf-8''fancy"), "filename", "fancy");
}

test "errors: no type, bad parameter, a parameter twice with different values" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    for ([_][]const u8{ "", ";x=y", "/json", "text/", "text/plain extra", "form-data; name", "form-data; name=", "form-data; =x", "form-data; name=\"open", "a/b; x=1; x=2" }) |bad| {
        try testing.expectError(error.InvalidMediaType, parse(a, bad));
    }
    // Equal repeats and trailing semicolons are fine.
    try expectParam(try parse(a, "a/b; x=1; x=1;"), "x", "1");
    try expectParam(try parse(a, "a/b; x=1 ; "), "x", "1");
    _ = try parse(a, "form-data");
}

test "a media type is lower case but the parameters keep their case" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const m = try parse(arena.allocator(), "  APPLICATION/JSON ; Charset=UTF-8");
    try testing.expectEqualStrings("application/json", m.type);
    try expectParam(m, "charset", "UTF-8");
}

/// A fuzz seed: the Smith stream for one slice (u32 length, then the bytes).
fn seed(comptime s: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, s.len)) ++ s.*);
}

test "fuzz: parse never crashes or leaks on any bytes" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [256]u8 = undefined;
            const n = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            _ = parse(arena.allocator(), buf[0..n]) catch {};
        }
    }.one, .{ .corpus = &.{
        seed("form-data; name=\"nzb\"; filename=\"a.nzb\""),
        seed("attachment; filename*0*=utf-8''a%20; filename*1=b"),
        seed("multipart/form-data; boundary=\"----WebKitFormBoundary\""),
    } });
}
