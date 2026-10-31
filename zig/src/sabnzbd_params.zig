//! SABnzbd request parameters: a query string plus an optional
//! `application/x-www-form-urlencoded` body. Multipart is not parsed here;
//! the caller passes only the urlencoded bytes.
const std = @import("std");

/// Names kept from the query and the form together. Further pairs are dropped.
const max_keys = 256;
/// Decoded name or value. A longer pair is skipped, and `parse` still returns.
const max_field = 4096;

const Pair = struct {
    name: []const u8,
    value: []const u8,
};

pub const Params = struct {
    /// First accepted value per name. Bytes live in the arena passed to `parse`.
    pairs: []const Pair = &.{},

    /// Trimmed first value for `name`, or "" when the name was not accepted.
    /// The match is case-sensitive, as in Go's `url.Values`.
    pub fn get(self: *const Params, name: []const u8) []const u8 {
        for (self.pairs) |p| {
            if (std.mem.eql(u8, p.name, name)) return trimSpace(p.value);
        }
        return "";
    }
};

/// Parses `query` and optional `form_body`. Each source is split on `&` and
/// `;` on its own. Query names win; inside one source the first accepted
/// value of a name wins. `get` trims that value.
///
/// A pair is skipped when the name is empty, either side is not valid
/// percent-encoding (`+` is a space), or a decoded name or value is longer
/// than 4096 bytes. At most 256 names are kept. Only `arena` is used for
/// storage; the returned slices do not alias `query` or `form_body`.
pub fn parse(arena: std.mem.Allocator, query: []const u8, form_body: []const u8) !Params {
    // One block for the whole table. The list is not deinited: the slice
    // below is the stored table, and the arena frees it with the request.
    var list: std.ArrayList(Pair) = try .initCapacity(arena, max_keys);
    try ingest(arena, &list, query);
    try ingest(arena, &list, form_body);
    return .{ .pairs = list.items };
}

fn ingest(arena: std.mem.Allocator, list: *std.ArrayList(Pair), raw: []const u8) !void {
    var name_buf: [max_field]u8 = undefined;
    var value_buf: [max_field]u8 = undefined;
    var it = std.mem.splitAny(u8, raw, "&;");
    while (it.next()) |part| {
        if (list.items.len >= max_keys) break;
        if (part.len == 0) continue;
        const eq = std.mem.findScalar(u8, part, '=');
        const name_raw = if (eq) |i| part[0..i] else part;
        const value_raw = if (eq) |i| part[i + 1 ..] else "";
        if (name_raw.len == 0) continue;

        const name_tmp = decodeInto(&name_buf, name_raw) orelse continue;
        if (name_tmp.len == 0 or has(list.items, name_tmp)) continue;
        const value_tmp = decodeInto(&value_buf, value_raw) orelse continue;
        try list.append(arena, .{
            .name = try arena.dupe(u8, name_tmp),
            .value = try arena.dupe(u8, value_tmp),
        });
    }
}

fn has(pairs: []const Pair, name: []const u8) bool {
    for (pairs) |p| if (std.mem.eql(u8, p.name, name)) return true;
    return false;
}

/// `application/x-www-form-urlencoded` decode into `buf`.
/// Returns a slice of `buf`, or of `s` when `s` has no `+` or `%`.
/// Null when an escape is invalid or the decoded length would exceed `buf`.
fn decodeInto(buf: *[max_field]u8, s: []const u8) ?[]const u8 {
    if (std.mem.indexOfAny(u8, s, "%+") == null) {
        if (s.len > max_field) return null;
        return s;
    }
    var n: usize = 0;
    var i: usize = 0;
    while (i < s.len) {
        if (n == buf.len) return null;
        if (s[i] == '+') {
            buf[n] = ' ';
            n += 1;
            i += 1;
        } else if (s[i] == '%') {
            if (i + 2 >= s.len) return null;
            const hi = std.fmt.charToDigit(s[i + 1], 16) catch return null;
            const lo = std.fmt.charToDigit(s[i + 2], 16) catch return null;
            buf[n] = (hi << 4) | lo;
            n += 1;
            i += 3;
        } else {
            buf[n] = s[i];
            n += 1;
            i += 1;
        }
    }
    return buf[0..n];
}

/// Go's `strings.TrimSpace`. Invalid UTF-8 is not white space.
fn trimSpace(s: []const u8) []const u8 {
    var start: usize = 0;
    while (start < s.len) {
        const sp = spaceLen(s[start..]);
        if (sp == 0) break;
        start += sp;
    }
    var end = start;
    var i = start;
    while (i < s.len) {
        const sp = spaceLen(s[i..]);
        if (sp == 0) {
            i += 1;
            end = i;
        } else i += sp;
    }
    return s[start..end];
}

/// Length of the Unicode white space (Go's `unicode.IsSpace`) at the start of
/// `s`, or 0.
fn spaceLen(s: []const u8) usize {
    if (s.len == 0) return 0;
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

test "parse merges query then form with query winning duplicate keys" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const query = "mode=queue&apikey=fromquery";
    const form = "apikey=fromform&cat=movies";

    const params = try parse(arena, query, form);
    try testing.expectEqualStrings("fromquery", params.get("apikey"));
    try testing.expectEqualStrings("queue", params.get("mode"));
    try testing.expectEqualStrings("movies", params.get("cat"));
}

test "parse decodes urlencoding and splits on semicolons" {
    var arena_state: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const query = "a=b%3Bc;d=x+y&a=later&eq=b=c&a+b=v&sp=%20z%09&nbsp=%C2%A0z%C2%A0&bin=%FFhi&a%62=1&ab=2&nameonly";
    const params = try parse(arena, query, "a=form&g=fromform");
    try std.testing.expectEqualStrings("b;c", params.get("a"));
    try std.testing.expectEqualStrings("x y", params.get("d"));
    try std.testing.expectEqualStrings("b=c", params.get("eq"));
    try std.testing.expectEqualStrings("v", params.get("a b"));
    try std.testing.expectEqualStrings("z", params.get("sp"));
    try std.testing.expectEqualStrings("z", params.get("nbsp"));
    try std.testing.expectEqualSlices(u8, "\xffhi", params.get("bin"));
    try std.testing.expectEqualStrings("1", params.get("ab"));
    try std.testing.expectEqualStrings("", params.get("nameonly"));
    try std.testing.expectEqualStrings("fromform", params.get("g"));
    try std.testing.expectEqualStrings("", params.get("missing"));
}

test "parse skips empty names and bad escapes and lets an empty query value block the form" {
    var arena_state: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const query = "=novalue&&;%zz=bad&a=%2&a=ok&b=&c";
    const params = try parse(arena, query, "a=form&b=fromform&c=fromform&d=4");
    try std.testing.expectEqualStrings("ok", params.get("a"));
    try std.testing.expectEqualStrings("", params.get("b"));
    try std.testing.expectEqualStrings("", params.get("c"));
    try std.testing.expectEqualStrings("4", params.get("d"));
    try std.testing.expectEqualStrings("", params.get(""));
}

test "parse keeps at most 256 names and 4096 decoded bytes" {
    var arena_state: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    // Percent-encoding shrinks, so the cap is on the decoded length.
    {
        const raw = try arena.alloc(u8, 2 + 3 * 4096);
        raw[0] = 'k';
        raw[1] = '=';
        for (0..4096) |i| @memcpy(raw[2 + i * 3 ..][0..3], "%41");
        const params = try parse(arena, raw, "");
        try std.testing.expectEqual(4096, params.get("k").len);
        try std.testing.expect(std.mem.allEqual(u8, params.get("k"), 'A'));
    }

    // One byte over the cap: that pair is skipped, a later short one is kept.
    {
        const raw = try arena.alloc(u8, 2 + 4097 + 5);
        @memcpy(raw[0..2], "k=");
        @memset(raw[2..][0..4097], 'Z');
        @memcpy(raw[2 + 4097 ..][0..5], "&k=ok");
        const params = try parse(arena, raw, "");
        try std.testing.expectEqualStrings("ok", params.get("k"));
    }

    // A too-long name is skipped; the next pair is still read.
    {
        const raw = try arena.alloc(u8, 4097 + 6);
        @memset(raw[0..4097], 'n');
        @memcpy(raw[4097..][0..6], "=z&a=1");
        const params = try parse(arena, raw, "");
        try std.testing.expectEqualStrings("1", params.get("a"));
        try std.testing.expectEqualStrings("", params.get(raw[0..4097]));
    }

    // 256 query names fill the table. A 257th query name and a form-only name drop.
    {
        var buf: std.ArrayList(u8) = .empty;
        for (0..256) |i| {
            var piece: [32]u8 = undefined;
            const p = try std.fmt.bufPrint(&piece, "{s}n{d}=q", .{ if (i == 0) "" else "&", i });
            try buf.appendSlice(arena, p);
        }
        try buf.appendSlice(arena, "&overflow=q");
        const params = try parse(arena, buf.items, "n0=form&extra=1");
        try std.testing.expectEqualStrings("q", params.get("n0"));
        try std.testing.expectEqualStrings("q", params.get("n255"));
        try std.testing.expectEqualStrings("", params.get("overflow"));
        try std.testing.expectEqualStrings("", params.get("extra"));
    }

    // 255 query names leave one slot. The form fills it and nothing past it.
    {
        var buf: std.ArrayList(u8) = .empty;
        for (0..255) |i| {
            var piece: [32]u8 = undefined;
            const p = try std.fmt.bufPrint(&piece, "{s}n{d}=q", .{ if (i == 0) "" else "&", i });
            try buf.appendSlice(arena, p);
        }
        const params = try parse(arena, buf.items, "extra=1&another=2");
        try std.testing.expectEqualStrings("q", params.get("n0"));
        try std.testing.expectEqualStrings("q", params.get("n254"));
        try std.testing.expectEqualStrings("", params.get("n255"));
        try std.testing.expectEqualStrings("1", params.get("extra"));
        try std.testing.expectEqualStrings("", params.get("another"));
    }
}

fn fuzzParse(_: void, smith: *std.testing.Smith) !void {
    var qbuf: [1024]u8 = undefined;
    var fbuf: [1024]u8 = undefined;
    const qn = smith.slice(&qbuf);
    const fn_len = smith.slice(&fbuf);
    var arena_state: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena_state.deinit();
    const params = try parse(arena_state.allocator(), qbuf[0..qn], fbuf[0..fn_len]);
    const names = [_][]const u8{ "", "apikey", "mode", "cat", "a" };
    for (names) |name| {
        const v = params.get(name);
        try std.testing.expect(v.len <= max_field);
        try std.testing.expectEqualSlices(u8, trimSpace(v), v);
    }
}

/// A Smith stream of two slices: a u32 little-endian length and the bytes, twice.
fn seed2(comptime q: []const u8, comptime form: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, q.len)) ++ q.* ++ std.mem.toBytes(@as(u32, form.len)) ++ form.*);
}

test "fuzz parse does not crash on garbage" {
    try std.testing.fuzz({}, fuzzParse, .{ .corpus = &.{
        seed2("mode=queue&apikey=fromquery", "apikey=fromform&cat=movies"),
        seed2("", ""),
        seed2("a=%zz&b=%41;c=+d&%&;=&=x", ";;;%2&=&+"),
        seed2("%", "&&&&"),
    } });
}
