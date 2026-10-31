//! Test helpers of the tidy and finalize groups: a temp tree with files made
//! from strings, and a listing of a tree to compare as text. Test code only.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;

pub const io = testing.io;

pub const Tree = struct {
    tmp: testing.TmpDir,
    /// The real path of the temp dir (no symlinks on the way), owned.
    path: []u8,

    pub fn init() !Tree {
        var t: Tree = .{ .tmp = testing.tmpDir(.{}), .path = undefined };
        errdefer t.tmp.cleanup();
        var buf: [Io.Dir.max_path_bytes]u8 = undefined;
        const n = try t.tmp.dir.realPath(io, &buf);
        t.path = try testing.allocator.dupe(u8, buf[0..n]);
        return t;
    }

    pub fn deinit(t: *Tree) void {
        testing.allocator.free(t.path);
        t.tmp.cleanup();
    }

    /// `rel` below the temp dir, allocated in `a`.
    pub fn abs(t: *const Tree, a: Allocator, rel: []const u8) ![]u8 {
        return std.fs.path.join(a, &.{ t.path, rel });
    }

    pub fn mkdir(t: *Tree, rel: []const u8) !void {
        try t.tmp.dir.createDirPath(io, rel);
    }

    /// Writes a file, making its parents.
    pub fn write(t: *Tree, rel: []const u8, content: []const u8) !void {
        if (std.fs.path.dirname(rel)) |parent| try t.tmp.dir.createDirPath(io, parent);
        try t.tmp.dir.writeFile(io, .{ .sub_path = rel, .data = content });
    }

    /// Writes `size` bytes that start with `head` and are zero after.
    pub fn writeHead(t: *Tree, rel: []const u8, head: []const u8, size: usize) !void {
        const buf = try testing.allocator.alloc(u8, @max(size, head.len));
        defer testing.allocator.free(buf);
        @memset(buf, 0);
        @memcpy(buf[0..head.len], head);
        try t.write(rel, buf);
    }

    pub fn symlink(t: *Tree, target: []const u8, rel: []const u8) !void {
        if (std.fs.path.dirname(rel)) |parent| try t.tmp.dir.createDirPath(io, parent);
        try t.tmp.dir.symLink(io, target, rel, .{});
    }

    pub fn has(t: *const Tree, rel: []const u8) bool {
        _ = t.tmp.dir.statFile(io, rel, .{ .follow_symlinks = false }) catch return false;
        return true;
    }

    pub fn read(t: *const Tree, a: Allocator, rel: []const u8) ![]u8 {
        return t.tmp.dir.readFileAlloc(io, rel, a, .limited(1 << 20));
    }

    /// The tree below `rel` ("" for all of it) as text: one sorted line
    /// "path = content" per entry, "<dir>" for a directory and
    /// "<symlink:target>" for a link; links are not followed.
    pub fn listing(t: *const Tree, a: Allocator, rel: []const u8) ![]u8 {
        var d = if (rel.len == 0) try t.tmp.dir.openDir(io, ".", .{ .iterate = true }) else try t.tmp.dir.openDir(io, rel, .{ .iterate = true });
        defer d.close(io);
        return listDir(a, d);
    }
};

/// `listing` of an absolute path.
pub fn listingOf(a: Allocator, abs_path: []const u8) ![]u8 {
    var d = try Io.Dir.cwd().openDir(io, abs_path, .{ .iterate = true });
    defer d.close(io);
    return listDir(a, d);
}

fn listDir(a: Allocator, d: Io.Dir) ![]u8 {
    var lines: std.ArrayList([]const u8) = .empty;
    defer lines.deinit(a);
    try collect(a, d, "", &lines);
    std.mem.sort([]const u8, lines.items, {}, struct {
        fn lt(_: void, x: []const u8, y: []const u8) bool {
            return std.mem.lessThan(u8, x, y);
        }
    }.lt);
    var out: std.ArrayList(u8) = .empty;
    errdefer out.deinit(a);
    for (lines.items) |l| {
        try out.appendSlice(a, l);
        try out.append(a, '\n');
        a.free(l);
    }
    return out.toOwnedSlice(a);
}

fn collect(a: Allocator, d: Io.Dir, prefix: []const u8, lines: *std.ArrayList([]const u8)) !void {
    var it = d.iterate();
    while (try it.next(io)) |e| {
        const rel = if (prefix.len == 0) try a.dupe(u8, e.name) else try std.fmt.allocPrint(a, "{s}/{s}", .{ prefix, e.name });
        defer a.free(rel);
        const st = try d.statFile(io, e.name, .{ .follow_symlinks = false });
        switch (st.kind) {
            .directory => {
                try lines.append(a, try std.fmt.allocPrint(a, "{s} = <dir>", .{rel}));
                var sub = try d.openDir(io, e.name, .{ .iterate = true, .follow_symlinks = false });
                defer sub.close(io);
                try collect(a, sub, rel, lines);
            },
            .sym_link => {
                var buf: [Io.Dir.max_path_bytes]u8 = undefined;
                const n = try d.readLink(io, e.name, &buf);
                try lines.append(a, try std.fmt.allocPrint(a, "{s} = <symlink:{s}>", .{ rel, buf[0..n] }));
            },
            else => {
                const content = try d.readFileAlloc(io, e.name, a, .limited(1 << 20));
                defer a.free(content);
                try lines.append(a, try std.fmt.allocPrint(a, "{s} = {s}", .{ rel, content }));
            },
        }
    }
}

/// Compares a listing with the expected entries, in any order.
pub fn expectListing(actual: []const u8, expected: []const [2][]const u8) !void {
    var lines: std.ArrayList([]u8) = .empty;
    defer {
        for (lines.items) |l| testing.allocator.free(l);
        lines.deinit(testing.allocator);
    }
    for (expected) |e| try lines.append(testing.allocator, try std.fmt.allocPrint(testing.allocator, "{s} = {s}", .{ e[0], e[1] }));
    std.mem.sort([]u8, lines.items, {}, struct {
        fn lt(_: void, x: []u8, y: []u8) bool {
            return std.mem.lessThan(u8, x, y);
        }
    }.lt);
    var out: std.ArrayList(u8) = .empty;
    defer out.deinit(testing.allocator);
    for (lines.items) |l| {
        try out.appendSlice(testing.allocator, l);
        try out.append(testing.allocator, '\n');
    }
    try testing.expectEqualStrings(out.items, actual);
}

pub const dir_mark = "<dir>";
