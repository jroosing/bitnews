//! IP address and prefix parsing with the rules and the messages of Go's
//! net/netip (ParseAddr, ParsePrefix), which the settings validation uses for
//! the CIDR lists and the bind addresses. Hostile text goes through here, so
//! it has a fuzz target and works on slices only.
const std = @import("std");
const Allocator = std.mem.Allocator;
const text = @import("settings_text.zig");

pub const Addr = struct {
    /// The four bytes of an IPv4 address are at the start.
    bytes: [16]u8 = @splat(0),
    is4: bool = false,
    /// The IPv6 zone, without the '%'; empty when there is none.
    zone: []const u8 = "",

    pub fn is4In6(a: Addr) bool {
        if (a.is4) return false;
        for (a.bytes[0..10]) |b| if (b != 0) return false;
        return a.bytes[10] == 0xff and a.bytes[11] == 0xff;
    }

    fn unmapped(a: Addr) Addr {
        if (!a.is4In6()) return a;
        var out: Addr = .{ .is4 = true };
        @memcpy(out.bytes[0..4], a.bytes[12..16]);
        return out;
    }

    pub fn isMulticast(a: Addr) bool {
        const u = a.unmapped();
        if (u.is4) return u.bytes[0] & 0xf0 == 0xe0;
        return u.bytes[0] == 0xff;
    }

    /// 0.0.0.0 or :: (an address with a zone is a different address).
    pub fn isUnspecified(a: Addr) bool {
        if (a.zone.len > 0) return false;
        const n: usize = if (a.is4) 4 else 16;
        for (a.bytes[0..n]) |b| if (b != 0) return false;
        return true;
    }
};

/// Why an address did not parse: a message, and the part of the input where
/// it went wrong when that is known.
pub const AddrError = struct {
    in: []const u8,
    msg: []const u8,
    at: []const u8 = "",

    /// `ParseAddr("in"): msg (at "rest")`.
    pub fn render(e: AddrError, arena: Allocator) Allocator.Error![]u8 {
        const q_in = try text.quote(arena, e.in);
        if (e.at.len == 0) return std.fmt.allocPrint(arena, "ParseAddr({s}): {s}", .{ q_in, e.msg });
        const q_at = try text.quote(arena, e.at);
        return std.fmt.allocPrint(arena, "ParseAddr({s}): {s} (at {s})", .{ q_in, e.msg, q_at });
    }
};

pub const AddrResult = union(enum) { ok: Addr, err: AddrError };

/// netip.ParseAddr. The zone of the result points into `s`.
pub fn parseAddr(s: []const u8) AddrResult {
    for (s) |c| switch (c) {
        '.' => return parseIPv4(s),
        ':' => return parseIPv6(s),
        '%' => return .{ .err = .{ .in = s, .msg = "missing IPv6 address" } },
        else => {},
    };
    return .{ .err = .{ .in = s, .msg = "unable to parse IP" } };
}

fn parseIPv4Fields(in: []const u8, off: usize, end: usize, fields: []u8) ?AddrError {
    var val: u32 = 0;
    var pos: usize = 0;
    var dig_len: usize = 0;
    const s = in[off..end];
    for (s, 0..) |c, i| {
        if (c >= '0' and c <= '9') {
            if (dig_len == 1 and val == 0) return .{ .in = in, .msg = "IPv4 field has octet with leading zero" };
            val = val * 10 + (c - '0');
            dig_len += 1;
            if (val > 255) return .{ .in = in, .msg = "IPv4 field has value >255" };
        } else if (c == '.') {
            if (i == 0 or i == s.len - 1 or s[i - 1] == '.') {
                return .{ .in = in, .msg = "IPv4 field must have at least one digit", .at = s[i..] };
            }
            if (pos == 3) return .{ .in = in, .msg = "IPv4 address too long" };
            fields[pos] = @intCast(val);
            pos += 1;
            val = 0;
            dig_len = 0;
        } else {
            return .{ .in = in, .msg = "unexpected character", .at = s[i..] };
        }
    }
    if (pos < 3) return .{ .in = in, .msg = "IPv4 address too short" };
    fields[3] = @intCast(val);
    return null;
}

fn parseIPv4(s: []const u8) AddrResult {
    var a: Addr = .{ .is4 = true };
    if (parseIPv4Fields(s, 0, s.len, a.bytes[0..4])) |e| return .{ .err = e };
    return .{ .ok = a };
}

fn hexValue(c: u8) ?u32 {
    return switch (c) {
        '0'...'9' => c - '0',
        'a'...'f' => c - 'a' + 10,
        'A'...'F' => c - 'A' + 10,
        else => null,
    };
}

fn parseIPv6(in: []const u8) AddrResult {
    var s = in;
    var zone: []const u8 = "";
    if (std.mem.indexOfScalar(u8, s, '%')) |i| {
        zone = s[i + 1 ..];
        s = s[0..i];
        if (zone.len == 0) return .{ .err = .{ .in = in, .msg = "zone must be a non-empty string" } };
    }
    var ip: [16]u8 = @splat(0);
    var ellipsis: ?usize = null;

    if (s.len >= 2 and s[0] == ':' and s[1] == ':') {
        ellipsis = 0;
        s = s[2..];
        if (s.len == 0) return .{ .ok = .{ .zone = zone } };
    }

    var i: usize = 0;
    while (i < 16) {
        var off: usize = 0;
        var acc: u32 = 0;
        while (off < s.len) : (off += 1) {
            const d = hexValue(s[off]) orelse break;
            acc = (acc << 4) + d;
            if (off > 3) return .{ .err = .{ .in = in, .msg = "each group must have 4 or less digits", .at = s } };
            if (acc > 0xffff) return .{ .err = .{ .in = in, .msg = "IPv6 field has value >=2^16", .at = s } };
        }
        if (off == 0) {
            return .{ .err = .{ .in = in, .msg = "each colon-separated field must have at least one digit", .at = s } };
        }

        if (off < s.len and s[off] == '.') {
            if (ellipsis == null and i != 12) {
                return .{ .err = .{ .in = in, .msg = "embedded IPv4 address must replace the final 2 fields of the address", .at = s } };
            }
            if (i + 4 > 16) {
                return .{ .err = .{ .in = in, .msg = "too many hex fields to fit an embedded IPv4 at the end of the address", .at = s } };
            }
            const end = in.len - (if (zone.len > 0) zone.len + 1 else 0);
            if (parseIPv4Fields(in, end - s.len, end, ip[i .. i + 4])) |e| return .{ .err = e };
            s = "";
            i += 4;
            break;
        }

        ip[i] = @intCast(acc >> 8);
        ip[i + 1] = @intCast(acc & 0xff);
        i += 2;

        s = s[off..];
        if (s.len == 0) break;

        if (s[0] != ':') {
            return .{ .err = .{ .in = in, .msg = "unexpected character, want colon", .at = s } };
        } else if (s.len == 1) {
            return .{ .err = .{ .in = in, .msg = "colon must be followed by more characters", .at = s } };
        }
        s = s[1..];

        if (s[0] == ':') {
            if (ellipsis != null) return .{ .err = .{ .in = in, .msg = "multiple :: in address", .at = s } };
            ellipsis = i;
            s = s[1..];
            if (s.len == 0) break;
        }
    }

    if (s.len != 0) return .{ .err = .{ .in = in, .msg = "trailing garbage after address", .at = s } };

    if (i < 16) {
        const e = ellipsis orelse return .{ .err = .{ .in = in, .msg = "address string too short" } };
        const n = 16 - i;
        var j = i;
        while (j > e) {
            j -= 1;
            ip[j + n] = ip[j];
        }
        @memset(ip[e .. e + n], 0);
    } else if (ellipsis != null) {
        return .{ .err = .{ .in = in, .msg = "the :: must expand to at least one field of zeros" } };
    }
    return .{ .ok = .{ .bytes = ip, .zone = zone } };
}

/// netip.ParsePrefix: null when `s` is a valid prefix, else the error text
/// (`netip.ParsePrefix("s"): why`), in the arena.
pub fn prefixError(arena: Allocator, s: []const u8) Allocator.Error!?[]u8 {
    const why = (try prefixProblem(arena, s)) orelse return null;
    const q = try text.quote(arena, s);
    return try std.fmt.allocPrint(arena, "netip.ParsePrefix({s}): {s}", .{ q, why });
}

fn prefixProblem(arena: Allocator, s: []const u8) Allocator.Error!?[]const u8 {
    const slash = std.mem.lastIndexOfScalar(u8, s, '/') orelse return "no '/'";
    const ip = switch (parseAddr(s[0..slash])) {
        .ok => |a| a,
        .err => |e| return try e.render(arena),
    };
    if (!ip.is4 and ip.zone.len > 0) return "IPv6 zones cannot be present in a prefix";

    const bits_str = s[slash + 1 ..];
    const bad = try std.fmt.allocPrint(arena, "bad bits after slash: {s}", .{try text.quote(arena, bits_str)});
    // strconv.Atoi accepts a sign and leading zeros; a prefix length does not.
    if (bits_str.len > 1 and (bits_str[0] < '1' or bits_str[0] > '9')) return bad;
    if (bits_str.len == 0) return bad;
    var bits: u64 = 0;
    for (bits_str) |c| {
        if (c < '0' or c > '9') return bad;
        const mul = @mulWithOverflow(bits, 10);
        const add = @addWithOverflow(mul[0], c - '0');
        // Atoi fails above the int range.
        if (mul[1] != 0 or add[1] != 0 or add[0] > std.math.maxInt(i64)) return bad;
        bits = add[0];
    }
    const max_bits: u64 = if (ip.is4) 32 else 128;
    if (bits > max_bits) return "prefix length out of range";
    return null;
}

/// ValidBindAddress: a plain IP address, not unspecified, not multicast and
/// without a zone. Returns the reason, or null when the address is fine.
pub fn bindAddressProblem(s: []const u8) ?[]const u8 {
    const a = switch (parseAddr(s)) {
        .ok => |a| a,
        .err => return "not an IP address",
    };
    if (a.zone.len > 0) return "must not have a zone";
    if (a.isUnspecified()) return "must not be the unspecified address";
    if (a.isMulticast()) return "must not be a multicast address";
    return null;
}

const testing = std.testing;

fn expectAddrError(in: []const u8, want: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    switch (parseAddr(in)) {
        .ok => return error.TestExpectedError,
        .err => |e| try testing.expectEqualStrings(want, try e.render(arena.allocator())),
    }
}

test "valid addresses" {
    const v4 = parseAddr("192.168.1.10").ok;
    try testing.expect(v4.is4);
    try testing.expectEqualSlices(u8, &.{ 192, 168, 1, 10 }, v4.bytes[0..4]);

    const v6 = parseAddr("2001:db8::68").ok;
    try testing.expect(!v6.is4);
    try testing.expectEqualSlices(u8, &.{ 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x68 }, &v6.bytes);

    const mapped = parseAddr("::ffff:10.0.0.1").ok;
    try testing.expect(mapped.is4In6());
    try testing.expectEqualStrings("eth0", parseAddr("fe80::1%eth0").ok.zone);
    try testing.expect(parseAddr("::").ok.isUnspecified());
    try testing.expect(parseAddr("0.0.0.0").ok.isUnspecified());
    try testing.expect(!parseAddr("::ffff:0.0.0.0").ok.isUnspecified());
    try testing.expect(parseAddr("224.0.0.1").ok.isMulticast());
    try testing.expect(parseAddr("::ffff:224.0.0.1").ok.isMulticast());
    try testing.expect(parseAddr("ff02::1").ok.isMulticast());
    try testing.expect(!parseAddr("10.0.0.1").ok.isMulticast());
}

test "address errors carry Go's text" {
    try expectAddrError("1.2.3", "ParseAddr(\"1.2.3\"): IPv4 address too short");
    try expectAddrError("01.2.3.4", "ParseAddr(\"01.2.3.4\"): IPv4 field has octet with leading zero");
    try expectAddrError("1.2.3.-4", "ParseAddr(\"1.2.3.-4\"): unexpected character (at \"-4\")");
    try expectAddrError("1::2::3", "ParseAddr(\"1::2::3\"): multiple :: in address (at \":3\")");
    try expectAddrError("%x", "ParseAddr(\"%x\"): missing IPv6 address");
    try expectAddrError("", "ParseAddr(\"\"): unable to parse IP");
    try expectAddrError("1:2:3:4:5:6:7", "ParseAddr(\"1:2:3:4:5:6:7\"): address string too short");
    try expectAddrError("1:2:3:4:5:6:7:1.2.3.4", "ParseAddr(\"1:2:3:4:5:6:7:1.2.3.4\"): embedded IPv4 address must replace the final 2 fields of the address (at \"1.2.3.4\")");
    try expectAddrError("fe80::%", "ParseAddr(\"fe80::%\"): zone must be a non-empty string");
}

test "prefixes" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    for ([_][]const u8{ "192.168.1.0/24", "10.0.0.0/8", "fd00::/8", "127.0.0.1/32", "::/0", "1.2.3.4/0", "::ffff:1.2.3.4/120" }) |ok| {
        try testing.expectEqual(null, try prefixError(a, ok));
    }
    const cases = [_][2][]const u8{
        .{ "", "netip.ParsePrefix(\"\"): no '/'" },
        .{ "192.168.1.0/33", "netip.ParsePrefix(\"192.168.1.0/33\"): prefix length out of range" },
        .{ "10.0.0.0/8 ", "netip.ParsePrefix(\"10.0.0.0/8 \"): bad bits after slash: \"8 \"" },
        .{ "1.2.3.4/+8", "netip.ParsePrefix(\"1.2.3.4/+8\"): bad bits after slash: \"+8\"" },
        .{ "1.2.3.4/08", "netip.ParsePrefix(\"1.2.3.4/08\"): bad bits after slash: \"08\"" },
        .{ "1.2.3.4/", "netip.ParsePrefix(\"1.2.3.4/\"): bad bits after slash: \"\"" },
        .{ "fe80::1%eth0/64", "netip.ParsePrefix(\"fe80::1%eth0/64\"): IPv6 zones cannot be present in a prefix" },
        .{ "/8", "netip.ParsePrefix(\"/8\"): ParseAddr(\"\"): unable to parse IP" },
        .{ "1.2.3.4/99999999999999999999999", "netip.ParsePrefix(\"1.2.3.4/99999999999999999999999\"): bad bits after slash: \"99999999999999999999999\"" },
    };
    for (cases) |c| try testing.expectEqualStrings(c[1], (try prefixError(a, c[0])).?);
}

test "bind addresses" {
    try testing.expectEqual(null, bindAddressProblem("10.2.0.2"));
    try testing.expectEqual(null, bindAddressProblem("fd00::2"));
    try testing.expectEqualStrings("not an IP address", bindAddressProblem("localhost").?);
    try testing.expectEqualStrings("not an IP address", bindAddressProblem(" 10.0.0.1").?);
    try testing.expectEqualStrings("must not have a zone", bindAddressProblem("fe80::1%eth0").?);
    try testing.expectEqualStrings("must not be the unspecified address", bindAddressProblem("::").?);
    try testing.expectEqualStrings("must not be a multicast address", bindAddressProblem("ff02::1").?);
}

fn fuzzParse(_: void, smith: *testing.Smith) !void {
    var buf: [96]u8 = undefined;
    const n = smith.slice(&buf);
    const s = buf[0..n];
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    switch (parseAddr(s)) {
        .ok => |a| {
            // The zone is a piece of the input.
            if (a.zone.len > 0) try testing.expect(@intFromPtr(a.zone.ptr) >= @intFromPtr(s.ptr));
            _ = bindAddressProblem(s);
        },
        .err => |e| _ = try e.render(arena.allocator()),
    }
    _ = try prefixError(arena.allocator(), s);
}

test "fuzz: parsing any bytes never crashes" {
    try testing.fuzz({}, fuzzParse, .{ .corpus = &.{
        "\x0e\x00\x00\x00192.168.1.0/24",
        "\x0a\x00\x00\x00fe80::1%en0",
        "\x1e\x00\x00\x001:2:3:4:5:6:7:1.2.3.4/999999",
        "\x03\x00\x00\x00::/",
    } });
}
