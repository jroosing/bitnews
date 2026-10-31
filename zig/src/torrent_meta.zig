//! The .torrent file saved next to a magnet Download once metadata arrives.
//! Mode 0600: the file can carry tracker passkeys. The name is the display
//! name plus `.torrent`, made safe; a name that cannot be used falls back to
//! `.bitnews-metainfo.torrent`.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const fsutil = @import("fsutil.zig");
const fsutil_name = @import("fsutil_name.zig");

pub const reserved_name = ".bitnews-metainfo.torrent";

pub const Error = error{WriteFailed} || Allocator.Error || Io.Cancelable;

pub fn fileName(alloc: Allocator, display: []const u8) Allocator.Error![]u8 {
    if (display.len == 0) return alloc.dupe(u8, reserved_name);
    const raw = try std.fmt.allocPrint(alloc, "{s}.torrent", .{display});
    defer alloc.free(raw);
    return fsutil_name.safeName(alloc, raw) catch alloc.dupe(u8, reserved_name);
}

/// Writes `bytes` to `dir`/`fileName(display)` atomically. Returns the path.
pub fn write(alloc: Allocator, io: Io, dir: []const u8, display: []const u8, bytes: []const u8) Error![]u8 {
    const name = try fileName(alloc, display);
    defer alloc.free(name);
    const path = try fsutil.join(alloc, &.{ dir, name });
    errdefer alloc.free(path);
    var atomic = Io.Dir.cwd().createFileAtomic(io, path, .{ .permissions = .fromMode(0o600) }) catch return error.WriteFailed;
    defer atomic.deinit(io);
    atomic.file.setPermissions(io, .fromMode(0o600)) catch return error.WriteFailed;
    atomic.file.writeStreamingAll(io, bytes) catch return error.WriteFailed;
    atomic.file.sync(io) catch return error.WriteFailed;
    atomic.link(io) catch |err| switch (err) {
        error.PathAlreadyExists => return path,
        else => return error.WriteFailed,
    };
    return path;
}

test "fileName keeps a plain title and rejects an empty one" {
    const testing = std.testing;
    const plain = try fileName(testing.allocator, "Show.S01");
    defer testing.allocator.free(plain);
    try testing.expectEqualStrings("Show.S01.torrent", plain);
    const slash = try fileName(testing.allocator, "Show/S01");
    defer testing.allocator.free(slash);
    try testing.expectEqualStrings("Show_S01.torrent", slash);
    const empty = try fileName(testing.allocator, "");
    defer testing.allocator.free(empty);
    try testing.expectEqualStrings(reserved_name, empty);
}

test "write stores the bytes in the download directory" {
    const testing = std.testing;
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    const path = try write(testing.allocator, testing.io, buf[0..n], "Show.S01", "d7:comment4:bite");
    defer testing.allocator.free(path);
    var file = try Io.Dir.cwd().openFile(testing.io, path, .{});
    defer file.close(testing.io);
    var got: [32]u8 = undefined;
    const read = try file.readPositionalAll(testing.io, &got, 0);
    try testing.expectEqualStrings("d7:comment4:bite", got[0..read]);
}
