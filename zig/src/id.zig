//! IDs of Downloads, Categories, Servers and so on: 128 random bits as 26
//! lowercase base32 characters without padding, like core.NewID.
const std = @import("std");
const Io = std.Io;

pub const length = 26;
const alphabet = "abcdefghijklmnopqrstuvwxyz234567";

/// The ID for 16 given bytes (RFC 4648 base32, lowercase, no padding).
pub fn fromBytes(raw: [16]u8) [length]u8 {
    var out: [length]u8 = undefined;
    var acc: u16 = 0;
    var bits: u4 = 0;
    var n: usize = 0;
    for (raw) |b| {
        acc = (acc << 8) | b;
        bits += 8;
        while (bits >= 5) {
            bits -= 5;
            out[n] = alphabet[(acc >> bits) & 31];
            n += 1;
        }
        acc &= (@as(u16, 1) << bits) - 1;
    }
    if (bits > 0) {
        out[n] = alphabet[(acc << (5 - bits)) & 31];
        n += 1;
    }
    std.debug.assert(n == length);
    return out;
}

/// A fresh random ID.
pub fn new(io: Io) [length]u8 {
    var raw: [16]u8 = undefined;
    io.random(&raw);
    return fromBytes(raw);
}

/// Whether `s` has the shape of an ID (what the Go routes accept in `{id}`:
/// lowercase letters and digits, bounded). Used to refuse hostile path values
/// before they reach a query.
pub fn isValid(s: []const u8) bool {
    if (s.len == 0 or s.len > 64) return false;
    for (s) |c| switch (c) {
        'a'...'z', '0'...'9', '_', '-' => {},
        else => return false,
    };
    return true;
}

const testing = std.testing;

test "an ID is 26 lowercase base32 characters, the same as Go's for the same bytes" {
    // Go: strings.ToLower(base32.StdEncoding.WithPadding(NoPadding).EncodeToString(bytes))
    const raw = [16]u8{ 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15 };
    try testing.expectEqualStrings("aaaqeayeaudaocajbifqydiob4", &fromBytes(raw));
    try testing.expectEqualStrings("77777777777777777777777774", &fromBytes(@splat(0xff)));
    try testing.expectEqualStrings("aaaaaaaaaaaaaaaaaaaaaaaaaa", &fromBytes(@splat(0)));
}

test "new IDs differ and are valid" {
    const a = new(testing.io);
    const b = new(testing.io);
    try testing.expect(!std.mem.eql(u8, &a, &b));
    try testing.expect(isValid(&a));
}

test "isValid refuses anything that is not an ID" {
    for ([_][]const u8{ "", "A", "a b", "a/b", "../x", "a\x00", &@as([65]u8, @splat('a')) }) |s| try testing.expect(!isValid(s));
    try testing.expect(isValid("abc123"));
}
