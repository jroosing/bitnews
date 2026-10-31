//! SABnzbd add API: priority and related request fields.
const std = @import("std");
const download = @import("download.zig");

pub const ParsedPriority = struct {
    priority: download.Priority,
    paused: bool,
};

const normal_not_paused = ParsedPriority{ .priority = .normal, .paused = false };

/// Reads the priority parameter: SAB's number, or a name. Empty and
/// unparsable values are the default (normal, not paused).
pub fn parsePriority(s: []const u8) ParsedPriority {
    const t = std.mem.trim(u8, s, &std.ascii.whitespace);
    if (std.fmt.parseInt(i32, t, 10) catch null) |n| {
        return switch (n) {
            2 => .{ .priority = .force, .paused = false },
            1 => .{ .priority = .high, .paused = false },
            -1 => .{ .priority = .low, .paused = false },
            -2 => .{ .priority = .normal, .paused = true },
            else => normal_not_paused,
        };
    }
    if (std.ascii.eqlIgnoreCase(t, "force")) return .{ .priority = .force, .paused = false };
    if (std.ascii.eqlIgnoreCase(t, "high")) return .{ .priority = .high, .paused = false };
    if (std.ascii.eqlIgnoreCase(t, "low")) return .{ .priority = .low, .paused = false };
    if (std.ascii.eqlIgnoreCase(t, "paused")) return .{ .priority = .normal, .paused = true };
    return normal_not_paused;
}

/// Maps download.Priority to SAB numeric priority for the add API.
pub fn priorityToSAB(p: download.Priority) i32 {
    return switch (p) {
        .force => 2,
        .high => 1,
        .low => -1,
        .normal => 0,
    };
}

/// SAB addfile: no staged file is `"No file given"`; when a file was received, no error here.
pub fn addFileError(has_file: bool) ?[]const u8 {
    if (!has_file) return "No file given";
    return null;
}

/// SAB addurl: empty `name` is `"expect one parameter"`; non-empty has no URL error here.
pub fn addUrlError(name: []const u8) ?[]const u8 {
    const t = std.mem.trim(u8, name, &std.ascii.whitespace);
    if (t.len == 0) return "expect one parameter";
    return null;
}

test "addFileError no file is No file given" {
    const testing = @import("std").testing;
    const got = addFileError(false).?;
    try testing.expectEqualStrings("No file given", got);
}

test "addUrlError empty name is expect one parameter" {
    const testing = @import("std").testing;
    const got = addUrlError("").?;
    try testing.expectEqualStrings("expect one parameter", got);
}

test "parsePriority sab numeric 2 is force not paused" {
    const testing = @import("std").testing;
    const got = parsePriority("2");
    try testing.expect(got.priority == .force);
    try testing.expect(!got.paused);
}

test "priorityToSAB force is sab numeric 2" {
    const testing = @import("std").testing;
    try testing.expectEqual(@as(i32, 2), priorityToSAB(.force));
}
