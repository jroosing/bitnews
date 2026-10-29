//! Secret: a value that must never reach logs or API responses, such as a
//! Server password (the core.Secret rule of the Go version).
//!
//! Formatting with `{f}` prints `[redacted]`. The bytes sit behind a
//! many-item pointer and a length rather than a slice, because `{any}` prints
//! every field of a struct and would print a slice's contents but only the
//! address of a many-item pointer. JSON stringification is a compile error
//! instead of a placeholder: the Go version writes "[redacted]", but a Secret
//! in a response is a bug and should never build (std.json.Stringify cannot
//! return a custom error, so a run-time refusal is not possible). `reveal` is
//! the only way to read the value.
const std = @import("std");

/// What a Secret shows in place of its value.
pub const redacted = "[redacted]";

pub const Secret = struct {
    ptr: [*]u8,
    len: usize,
    allocator: std.mem.Allocator,

    /// Copies `value`. The Secret owns the copy until `deinit`.
    pub fn init(allocator: std.mem.Allocator, value: []const u8) std.mem.Allocator.Error!Secret {
        const copy = try allocator.alloc(u8, value.len);
        @memcpy(copy, value);
        return .{ .ptr = copy.ptr, .len = copy.len, .allocator = allocator };
    }

    /// Zeroes the bytes. The write is not elided by the optimizer, so the
    /// value does not linger in memory that is freed afterwards.
    pub fn wipe(self: *Secret) void {
        std.crypto.secureZero(u8, self.ptr[0..self.len]);
    }

    /// Wipes the bytes, then frees them.
    pub fn deinit(self: *Secret) void {
        self.wipe();
        self.allocator.free(self.ptr[0..self.len]);
        self.* = undefined;
    }

    /// Returns the value. Call it only where the value is used, for example
    /// to authenticate to a Server; the slice is valid until `deinit`.
    pub fn reveal(self: Secret) []const u8 {
        return self.ptr[0..self.len];
    }

    /// Used by `{f}`.
    pub fn format(self: Secret, w: *std.Io.Writer) std.Io.Writer.Error!void {
        _ = self;
        try w.writeAll(redacted);
    }

    /// Makes std.json.Stringify, which would otherwise write the fields, fail
    /// to compile. `jw` is `anytype` so the body is only analyzed when a
    /// Secret is actually stringified.
    pub fn jsonStringify(self: Secret, jw: anytype) !void {
        _ = self;
        _ = jw;
        @compileError("a Secret must never be serialized to JSON; reveal() it where it is used");
    }
};

const testing = std.testing;
const pw = "hunter2-very-secret";

test "reveal returns the value" {
    var s = try Secret.init(testing.allocator, pw);
    defer s.deinit();
    try testing.expectEqualStrings(pw, s.reveal());
}

test "the value is a copy" {
    var buf = pw.*;
    var s = try Secret.init(testing.allocator, &buf);
    defer s.deinit();
    buf[0] = 'X';
    try testing.expectEqualStrings(pw, s.reveal());
}

test "format prints redacted" {
    var s = try Secret.init(testing.allocator, pw);
    defer s.deinit();
    const out = try std.fmt.allocPrint(testing.allocator, "pw={f}", .{s});
    defer testing.allocator.free(out);
    try testing.expectEqualStrings("pw=[redacted]", out);
}

test "any-formatting of a Secret does not show the bytes" {
    var s = try Secret.init(testing.allocator, pw);
    defer s.deinit();
    const Server = struct { host: []const u8, password: Secret };
    const out = try std.fmt.allocPrint(testing.allocator, "{any}", .{Server{ .host = "news.example.com", .password = s }});
    defer testing.allocator.free(out);
    try testing.expect(std.mem.indexOf(u8, out, ".password = ") != null);
    try testing.expect(std.mem.indexOf(u8, out, pw) == null);
    // Nor as decimal byte values.
    try testing.expect(std.mem.indexOf(u8, out, "104, 117, 110") == null);
}

test "JSON stringification is a compile error" {
    // The refusal itself cannot be tested (a compile error ends the build).
    // Check that the hook std.json.Stringify dispatches on is present, so a
    // Secret field never falls back to the default struct serialization.
    try testing.expect(std.meta.hasFn(Secret, "jsonStringify"));
}

test "wipe zeroes the bytes" {
    var s = try Secret.init(testing.allocator, pw);
    defer s.deinit();
    s.wipe();
    for (s.reveal()) |b| try testing.expectEqual(@as(u8, 0), b);
}
