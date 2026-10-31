//! `x.pe` peer hints on a magnet link (BEP 9). At most 8, each at most 128
//! bytes, each containing a colon. Slices point into `magnet`.
const std = @import("std");

pub const max_peers = 8;

pub fn hints(magnet: []const u8, out: [][]const u8) usize {
    var n: usize = 0;
    var rest = magnet;
    const key = "x.pe=";
    while (n < out.len) {
        const at = std.mem.indexOf(u8, rest, key) orelse break;
        const tail = rest[at + key.len ..];
        const end = std.mem.indexOfAny(u8, tail, "&#") orelse tail.len;
        const addr = tail[0..end];
        rest = if (end < tail.len) tail[end..] else tail[end..end];
        if (addr.len == 0 or addr.len > 128) continue;
        if (std.mem.indexOfScalar(u8, addr, ':') == null) continue;
        out[n] = addr;
        n += 1;
    }
    return n;
}

test "hints reads x.pe and skips a bare host" {
    const testing = std.testing;
    const magnet = "magnet:?xt=urn:btih:ab&x.pe=127.0.0.1:6881&x.pe=notahost&x.pe=[::1]:9";
    var out: [max_peers][]const u8 = undefined;
    const n = hints(magnet, &out);
    try testing.expectEqual(@as(usize, 2), n);
    try testing.expectEqualStrings("127.0.0.1:6881", out[0]);
    try testing.expectEqualStrings("[::1]:9", out[1]);
}
