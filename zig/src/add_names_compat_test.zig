//! Differential test of add_names.zig against the Go functions it ports: the
//! cases of add_names_cases.zig were run through Go's cleanDisplayName and
//! splitPasswordSuffix.
const std = @import("std");
const testing = std.testing;
const names = @import("add_names.zig");
const cases = @import("add_names_cases.zig");

test "display names and password suffixes match Go" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expect(cases.cases.len > 200);
    for (cases.cases, 0..) |case, n| {
        const clean = try names.cleanDisplayName(arena.allocator(), case.input);
        testing.expectEqualStrings(case.clean, clean) catch |err| {
            std.debug.print("case {d} {f}: clean\n", .{ n, std.zig.fmtString(case.input) });
            return err;
        };
        const split = names.splitPasswordSuffix(case.input);
        testing.expectEqualStrings(case.split_name, split.name) catch |err| {
            std.debug.print("case {d} {f}: split name\n", .{ n, std.zig.fmtString(case.input) });
            return err;
        };
        testing.expectEqualStrings(case.split_password, split.password) catch |err| {
            std.debug.print("case {d} {f}: split password\n", .{ n, std.zig.fmtString(case.input) });
            return err;
        };
    }
}
