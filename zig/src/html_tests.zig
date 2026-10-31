//! Tests of the html group: import the files of the group here.
const html = @import("html.zig");
const std = @import("std");

test {
    _ = html;
}

test "href rejects javascript scheme in URL attributes" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try html.href(arena, "javascript:alert(1)");
    try testing.expectEqualStrings("", got);
}

test "href keeps same-origin relative path" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try html.href(arena, "/queue");
    try testing.expectEqualStrings("/queue", got);
}

test "class skips empty tokens and joins with space" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try html.class(arena, &.{ "", "btn", "" });
    try testing.expectEqualStrings("btn", got);
}
