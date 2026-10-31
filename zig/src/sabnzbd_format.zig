//! SABnzbd formatting helpers (machine MB fields and display sizes).
const std = @import("std");

/// Renders `n` like Go `formatMB`: `"%.2f"` megabytes, clamping negative to zero.
pub fn formatMB(arena: std.mem.Allocator, n: i64) ![]const u8 {
    const bytes: f64 = @floatFromInt(@max(n, 0));
    return std.fmt.allocPrint(arena, "{d:.2}", .{bytes / (1 << 20)});
}

/// Renders `n` like Go `humanBytes`: SAB size strings ("0 B", "1.0 KB", …).
pub fn humanBytes(arena: std.mem.Allocator, n: i64) ![]const u8 {
    var f: f64 = @floatFromInt(@max(n, 0));
    if (f < 1024) {
        return std.fmt.allocPrint(arena, "{d:.0} B", .{f});
    }
    const units = [_][]const u8{ "KB", "MB", "GB", "TB" };
    for (units) |unit| {
        f /= 1024;
        if (f < 1024 or std.mem.eql(u8, unit, "TB")) {
            return std.fmt.allocPrint(arena, "{d:.1} {s}", .{ f, unit });
        }
    }
    return "";
}

/// Renders `bytesPerSec` like Go `humanSpeed`: SAB speed strings ("0  ", "1.0 K", …).
pub fn humanSpeed(arena: std.mem.Allocator, bytesPerSec: i64) ![]const u8 {
    var f: f64 = @floatFromInt(@max(bytesPerSec, 0));
    if (f < 1024) {
        return std.fmt.allocPrint(arena, "{d:.0}  ", .{f});
    }
    const units = [_][]const u8{ "K", "M", "G" };
    for (units) |unit| {
        f /= 1024;
        if (f < 1024 or std.mem.eql(u8, unit, "G")) {
            return std.fmt.allocPrint(arena, "{d:.1} {s}", .{ f, unit });
        }
    }
    return "";
}

/// Renders `sec` like Go `formatTimeLeft`: SAB time-left strings ("H:MM:SS" or "D:HH:MM:SS").
pub fn formatTimeLeft(arena: std.mem.Allocator, sec: i64) ![]const u8 {
    const clamped = @max(sec, 0);
    const d = @divTrunc(clamped, 86400);
    const h = @mod(@divTrunc(clamped, 3600), 24);
    const m = @mod(@divTrunc(clamped, 60), 60);
    const s = @mod(clamped, 60);
    if (d > 0) {
        return std.fmt.allocPrint(arena, "{d}:{d:0>2}:{d:0>2}:{d:0>2}", .{ d, h, m, s });
    }
    return std.fmt.allocPrint(arena, "{d}:{d:0>2}:{d:0>2}", .{ h, m, s });
}

/// Renders `sec` like Go `formatAge`: SAB age strings ("5m", "2h", "1d", …).
pub fn formatAge(arena: std.mem.Allocator, sec: f64) ![]const u8 {
    const s: i64 = @intFromFloat(@max(sec, 0));
    if (s >= 86400) {
        return std.fmt.allocPrint(arena, "{d}d", .{@divTrunc(s, 86400)});
    }
    if (s >= 3600) {
        return std.fmt.allocPrint(arena, "{d}h", .{@divTrunc(s, 3600)});
    }
    return std.fmt.allocPrint(arena, "{d}m", .{@divTrunc(s, 60)});
}

/// Renders progress like Go `percentage`: decimal string from integer done/total math.
pub fn percentage(arena: std.mem.Allocator, done: i64, total: i64) ![]const u8 {
    if (total <= 0 or done <= 0) {
        return std.fmt.allocPrint(arena, "{d}", .{0});
    }
    const d: u64 = @intCast(done);
    const t: u64 = @intCast(total);
    const pct = @min(@divTrunc(d * 100, t), 100);
    return std.fmt.allocPrint(arena, "{d}", .{pct});
}

test "formatAge matches Go formatAge" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try formatAge(arena, 300);
    try testing.expectEqualStrings("5m", got);
}

test "percentage matches Go percentage integer math" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const cases = .{
        .{ @as(i64, 0), @as(i64, 0), "0" },
        .{ @as(i64, 1), @as(i64, 3), "33" },
        .{ @as(i64, 3), @as(i64, 3), "100" },
        .{ @as(i64, 4), @as(i64, 3), "100" },
    };

    inline for (cases) |c| {
        const got = try percentage(arena, c[0], c[1]);
        try testing.expectEqualStrings(c[2], got);
    }
}

test "formatTimeLeft matches Go TestTimeLeftFormats" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const cases = .{
        .{ @as(i64, 0), "0:00:00" },
        .{ @as(i64, 59), "0:00:59" },
        .{ @as(i64, 61), "0:01:01" },
        .{ @as(i64, 3599), "0:59:59" },
        .{ @as(i64, 3600), "1:00:00" },
        .{ @as(i64, 86399), "23:59:59" },
        .{ @as(i64, 86400), "1:00:00:00" },
        .{ @as(i64, 90061), "1:01:01:01" },
        .{ @as(i64, -5), "0:00:00" },
    };

    inline for (cases) |c| {
        const got = try formatTimeLeft(arena, c[0]);
        try testing.expectEqualStrings(c[1], got);
    }
}

test "formatMB matches Go formatMB" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const mib: i64 = 1 << 20;

    const got0 = try formatMB(arena, 0);
    try testing.expectEqualStrings("0.00", got0);

    const got1 = try formatMB(arena, mib);
    try testing.expectEqualStrings("1.00", got1);

    const got_neg = try formatMB(arena, -1);
    try testing.expectEqualStrings("0.00", got_neg);
}

test "humanBytes matches Go humanBytes" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const cases = .{
        .{ @as(i64, 0), "0 B" },
        .{ @as(i64, 1023), "1023 B" },
        .{ @as(i64, 1024), "1.0 KB" },
        .{ @as(i64, 1536), "1.5 KB" },
        .{ @as(i64, 5 << 20), "5.0 MB" },
        .{ @as(i64, 3 << 30), "3.0 GB" },
        .{ @as(i64, 2 << 40), "2.0 TB" },
        .{ @as(i64, -1), "0 B" },
    };

    inline for (cases) |c| {
        const got = try humanBytes(arena, c[0]);
        try testing.expectEqualStrings(c[1], got);
    }
}

test "humanSpeed matches Go humanSpeed" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const cases = .{
        .{ @as(i64, 0), "0  " },
        .{ @as(i64, 1023), "1023  " },
        .{ @as(i64, 1024), "1.0 K" },
        .{ @as(i64, 1536), "1.5 K" },
        .{ @as(i64, 5 << 20), "5.0 M" },
        .{ @as(i64, 3 << 30), "3.0 G" },
        .{ @as(i64, -1), "0  " },
    };

    inline for (cases) |c| {
        const got = try humanSpeed(arena, c[0]);
        try testing.expectEqualStrings(c[1], got);
    }
}
