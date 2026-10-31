//! SABnzbd config-mode helpers (get_config, get_cats, …).
const std = @import("std");
const categories_types = @import("categories_types.zig");

/// Dir a media manager joins to complete_dir for a Category's output.
pub fn categoryDir(root: []const u8, cat: categories_types.Category) []const u8 {
    if (cat.usenet_dir.len == 0) return cat.name;
    const dir = cat.usenet_dir;
    if (dir.len > root.len and std.mem.startsWith(u8, dir, root) and dir[root.len] == '/') {
        const rel = dir[root.len + 1 ..];
        if (rel.len != 0 and
            !std.mem.eql(u8, rel, ".") and
            !std.mem.eql(u8, rel, "..") and
            !std.mem.startsWith(u8, rel, "../"))
        {
            return rel;
        }
    }
    return dir;
}

/// Names for mode=get_cats: `"*"` first, then each category name in order.
pub fn categoryNames(arena: std.mem.Allocator, cats: []const []const u8) ![]const []const u8 {
    const n = @min(cats.len, 1024);
    const names = try arena.alloc([]const u8, n + 1);
    names[0] = "*";
    for (cats[0..n], names[1..]) |src, *dst| {
        dst.* = src;
    }
    return names;
}

pub fn gigabytes(arena: std.mem.Allocator, n: i64) ![]const u8 {
    return std.fmt.allocPrint(arena, "{d:.2}", .{@as(f64, @floatFromInt(n)) / @as(f64, 1 << 30)});
}

test "gigabytes five gibibytes formats as 5.00" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const s = try gigabytes(arena, 5 << 30);
    try testing.expectEqualStrings("5.00", s);
}

test "categoryNames prepends star then category names" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const cats = [_][]const u8{ "tv", "movies" };
    const names = try categoryNames(arena, cats[0..]);

    try testing.expectEqual(@as(usize, 3), names.len);
    try testing.expectEqualStrings("*", names[0]);
    try testing.expectEqualStrings("tv", names[1]);
    try testing.expectEqualStrings("movies", names[2]);
}

test "categoryDir empty usenet_dir returns category name" {
    const testing = std.testing;
    const cat = categories_types.Category{
        .id = "i",
        .name = "tv",
        .usenet_dir = "",
    };
    const got = categoryDir("/data/complete", cat);
    try testing.expectEqualStrings("tv", got);
}

test "categoryDir usenet_dir inside complete root returns relative posix path" {
    const testing = std.testing;
    const cat = categories_types.Category{
        .id = "i",
        .name = "movies",
        .usenet_dir = "/data/complete/films/hd",
    };
    const got = categoryDir("/data/complete", cat);
    try testing.expectEqualStrings("films/hd", got);
}
