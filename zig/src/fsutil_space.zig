//! Free space of the filesystem holding a path (internal/fsutil FreeBytes and
//! DiskSpace). The disk guard of the Queue asks for it before it starts a
//! Download. std has no wrapper for statfs, so the two libc structs are
//! declared here with only the leading fields we read, followed by padding
//! larger than the rest of the real struct (the kernel or libc writes it, we
//! ignore it).
//!
//! Darwin uses statfs, whose counts are 64 bit (statvfs there has 32 bit
//! counts); Linux uses statvfs, whose counts are in units of f_frsize. Other
//! platforms, and 32 bit targets, report `error.Unsupported`, which callers
//! treat as "unknown".
const std = @import("std");
const builtin = @import("builtin");

pub const Error = error{
    /// This platform has no probe: the space is unknown.
    Unsupported,
    FileNotFound,
    AccessDenied,
    NameTooLong,
    /// The path holds a NUL byte or is empty.
    InvalidPath,
    Unexpected,
};

pub const Space = struct {
    /// Bytes available to an unprivileged process.
    free: u64,
    total: u64,
};

const DarwinStatfs = extern struct {
    bsize: u32,
    iosize: i32,
    blocks: u64,
    bfree: u64,
    bavail: u64,
    rest: [4096]u8,
};

const LinuxStatvfs = extern struct {
    bsize: c_ulong,
    frsize: c_ulong,
    blocks: u64,
    bfree: u64,
    bavail: u64,
    rest: [512]u8,
};

const probe_exists = builtin.link_libc and @sizeOf(usize) == 8 and
    (builtin.os.tag == .macos or builtin.os.tag == .linux);

/// The bytes available to an unprivileged process on the filesystem holding
/// `path`.
pub fn freeBytes(path: []const u8) Error!u64 {
    return (try diskSpace(path)).free;
}

pub fn diskSpace(path: []const u8) Error!Space {
    if (comptime !probe_exists) return error.Unsupported;
    if (path.len == 0 or std.mem.indexOfScalar(u8, path, 0) != null) return error.InvalidPath;
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    if (path.len >= buf.len) return error.NameTooLong;
    @memcpy(buf[0..path.len], path);
    buf[path.len] = 0;
    const z: [*:0]const u8 = @ptrCast(&buf);

    var unit: u64 = undefined;
    var blocks: u64 = undefined;
    var avail: u64 = undefined;
    if (comptime builtin.os.tag == .macos) {
        const statfs = @extern(*const fn ([*:0]const u8, *DarwinStatfs) callconv(.c) c_int, .{
            .name = if (builtin.cpu.arch == .x86_64) "statfs$INODE64" else "statfs",
        });
        var st: DarwinStatfs = undefined;
        try check(statfs(z, &st));
        unit, blocks, avail = .{ st.bsize, st.blocks, st.bavail };
    } else {
        const statvfs = @extern(*const fn ([*:0]const u8, *LinuxStatvfs) callconv(.c) c_int, .{ .name = "statvfs" });
        var st: LinuxStatvfs = undefined;
        try check(statvfs(z, &st));
        unit = if (st.frsize != 0) st.frsize else st.bsize;
        blocks, avail = .{ st.blocks, st.bavail };
    }
    return .{
        .free = std.math.mul(u64, avail, unit) catch std.math.maxInt(u64),
        .total = std.math.mul(u64, blocks, unit) catch std.math.maxInt(u64),
    };
}

fn check(rc: c_int) Error!void {
    return switch (std.c.errno(rc)) {
        .SUCCESS => {},
        .NOENT, .NOTDIR => error.FileNotFound,
        .ACCES, .PERM => error.AccessDenied,
        .NAMETOOLONG => error.NameTooLong,
        .NOSYS => error.Unsupported,
        else => error.Unexpected,
    };
}

const testing = std.testing;

fn tmpPath(tmp: *testing.TmpDir, buf: []u8) ![]u8 {
    const n = try tmp.dir.realPath(testing.io, buf);
    return buf[0..n];
}

test "the space of a temp directory is positive, and free never exceeds total" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const path = try tmpPath(&tmp, &buf);
    const s = diskSpace(path) catch |err| switch (err) {
        error.Unsupported => return error.SkipZigTest,
        else => return err,
    };
    try testing.expect(s.total > 0);
    try testing.expect(s.free <= s.total);
    try testing.expect(try freeBytes(path) <= s.total);
}

test "a missing path is not found" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir = try tmpPath(&tmp, &buf);
    var full_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
    const full = try std.fmt.bufPrint(&full_buf, "{s}/missing", .{dir});
    if (freeBytes(full)) |_| {
        return error.TestUnexpectedResult;
    } else |err| switch (err) {
        error.Unsupported => return error.SkipZigTest,
        else => try testing.expectEqual(error.FileNotFound, err),
    }
}

test "an empty path, a NUL byte and a path too long are refused" {
    if (!probe_exists) return error.SkipZigTest;
    try testing.expectError(error.InvalidPath, freeBytes(""));
    try testing.expectError(error.InvalidPath, freeBytes("/tm\x00p"));
    const long: [std.fs.max_path_bytes + 1]u8 = @splat('a');
    try testing.expectError(error.NameTooLong, freeBytes(&long));
}
