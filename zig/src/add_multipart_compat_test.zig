//! Differential test of add_multipart.zig against Go's mime/multipart: the
//! cases of add_multipart_cases.zig were run through Go's reader (parts, form
//! names, file names, bodies, and whether it ended in an error), and ours must
//! give the same, for every chunking of the input.
const std = @import("std");
const Io = std.Io;
const testing = std.testing;
const multipart = @import("add_multipart.zig");
const cases = @import("add_multipart_cases.zig");

/// A source that gives at most `chunk` bytes per fill.
const Chunked = struct {
    data: []const u8,
    pos: usize = 0,
    chunk: usize,
    buf: [16]u8 = undefined,
    interface: Io.Reader,

    fn init(c: *Chunked, data: []const u8, chunk: usize) void {
        c.* = .{ .data = data, .chunk = chunk, .interface = .{ .vtable = &.{ .stream = stream }, .buffer = &c.buf, .seek = 0, .end = 0 } };
        c.interface.buffer = &c.buf;
    }

    fn stream(r: *Io.Reader, w: *Io.Writer, limit: Io.Limit) Io.Reader.StreamError!usize {
        const c: *Chunked = @alignCast(@fieldParentPtr("interface", r));
        if (c.pos >= c.data.len) return error.EndOfStream;
        const n = @min(c.chunk, c.data.len - c.pos, @backingInt(limit));
        try w.writeAll(c.data[c.pos..][0..n]);
        c.pos += n;
        return n;
    }
};

fn runCase(case: cases.Case, chunk: usize) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var c: Chunked = undefined;
    c.init(case.body, chunk);
    const rd = try multipart.Reader.init(a, &c.interface, case.boundary, 1 << 24);

    var i: usize = 0;
    var errored = false;
    while (true) {
        const maybe = rd.next(a) catch {
            errored = true;
            break;
        };
        const part = maybe orelse break;
        const body = rd.readUpTo(a, 1 << 20) catch {
            errored = true;
            break;
        };
        // Go gives the parts it read completely, then the error.
        if (i >= case.parts.len) return error.MorePartsThanGo;
        const want = case.parts[i];
        try testing.expectEqualStrings(want.name, part.name);
        try testing.expectEqualStrings(want.file_name, part.file_name);
        try testing.expectEqualStrings(want.body, body);
        i += 1;
    }
    try testing.expectEqual(case.parts.len, i);
    try testing.expectEqual(case.error_at_end, errored);
}

test "parts, names, file names, bodies and errors match Go's mime/multipart" {
    for (cases.cases, 0..) |case, n| {
        for ([_]usize{ 1, 5, 4096, 1 << 20 }) |chunk| {
            runCase(case, chunk) catch |err| {
                std.debug.print("case {d}, chunk {d}: {t}\n  boundary {s}\n  body {f}\n", .{ n, chunk, err, case.boundary, std.zig.fmtString(case.body) });
                return err;
            };
        }
    }
}
