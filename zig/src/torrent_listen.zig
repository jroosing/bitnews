//! Listen interface string for `bnlt_new`: `iface:port`, else `address:port`,
//! else `0.0.0.0:port`.
const std = @import("std");

pub fn format(buf: *[72]u8, address: []const u8, iface: []const u8, port: u16) [:0]const u8 {
    const host = if (iface.len > 0 and iface.len <= 15) iface else if (address.len > 0 and address.len < 46) address else "0.0.0.0";
    const written = std.fmt.bufPrint(buf[0 .. buf.len - 1], "{s}:{d}", .{ host, port }) catch {
        const fallback = std.fmt.bufPrint(buf[0 .. buf.len - 1], "0.0.0.0:{d}", .{port}) catch unreachable;
        buf[fallback.len] = 0;
        return buf[0..fallback.len :0];
    };
    buf[written.len] = 0;
    return buf[0..written.len :0];
}

test "format prefers the interface, then the address" {
    const testing = std.testing;
    var buf: [72]u8 = undefined;
    try testing.expectEqualStrings("eth0:6881", format(&buf, "10.1.0.1", "eth0", 6881));
    try testing.expectEqualStrings("10.1.0.1:51413", format(&buf, "10.1.0.1", "", 51413));
    try testing.expectEqualStrings("0.0.0.0:51413", format(&buf, "", "", 51413));
}
