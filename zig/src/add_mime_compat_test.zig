//! Differential test of add_mime.zig against Go's mime.ParseMediaType: the
//! cases of add_mime_cases.zig were run through Go, and ours must refuse the
//! same inputs and give the same media type and parameters for the others.
const std = @import("std");
const testing = std.testing;
const mime = @import("add_mime.zig");
const cases = @import("add_mime_cases.zig");

test "media types and parameters match Go's mime.ParseMediaType" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expect(cases.cases.len > 200);
    for (cases.cases, 0..) |case, n| {
        const got = mime.parse(arena.allocator(), case.input) catch |err| {
            if (!case.is_error) {
                std.debug.print("case {d} {f}: Go accepted it, we gave {t}\n", .{ n, std.zig.fmtString(case.input), err });
                return error.RefusedWhatGoAccepts;
            }
            continue;
        };
        if (case.is_error) {
            std.debug.print("case {d} {f}: Go refused it, we gave a type {s}\n", .{ n, std.zig.fmtString(case.input), got.type });
            return error.AcceptedWhatGoRefuses;
        }
        testing.expectEqualStrings(case.media_type, got.type) catch |err| {
            std.debug.print("case {d} {f}\n", .{ n, std.zig.fmtString(case.input) });
            return err;
        };
        testing.expectEqual(case.params.len, got.params.len) catch |err| {
            std.debug.print("case {d} {f}: {d} params in Go, {d} here\n", .{ n, std.zig.fmtString(case.input), case.params.len, got.params.len });
            return err;
        };
        for (case.params) |want| {
            const value = got.get(want.key) orelse {
                std.debug.print("case {d} {f}: no parameter {s}\n", .{ n, std.zig.fmtString(case.input), want.key });
                return error.MissingParameter;
            };
            testing.expectEqualStrings(want.value, value) catch |err| {
                std.debug.print("case {d} {f}: parameter {s}\n", .{ n, std.zig.fmtString(case.input), want.key });
                return err;
            };
        }
    }
}
