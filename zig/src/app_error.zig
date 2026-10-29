//! How services report failures a client can act on (internal/app/errors.go).
//!
//! Zig errors carry no data, so a service that refuses a request fills a
//! `Failure` (the caller passes a pointer) and returns `error.Rejected`; the
//! handler turns the pair into the JSON error with `api_json.fail`. The
//! strings are string literals or live in the request's arena.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// The class of a Failure, and with it the HTTP status.
pub const Kind = enum {
    /// Input that is not acceptable: 400.
    invalid_input,
    not_found,
    conflict,
    /// Input over a size limit: 413.
    too_large,
    /// Something bitnews called failed (the server an NZB URL points at): 502.
    upstream,
    /// A feature this instance is not set up for: 503.
    unavailable,

    pub fn status(k: Kind) std.http.Status {
        return switch (k) {
            .invalid_input => .bad_request,
            .not_found => .not_found,
            .conflict => .conflict,
            .too_large => .payload_too_large,
            .upstream => .bad_gateway,
            .unavailable => .service_unavailable,
        };
    }
};

pub const Failure = struct {
    kind: Kind = .invalid_input,
    /// A stable snake_case identifier.
    code: []const u8 = "",
    /// Safe to show to the user.
    message: []const u8 = "",

    /// Records the failure and returns `error.Rejected`, so a service writes
    /// `return fail.set(arena, .invalid_input, "invalid_name", "...", .{});`.
    pub fn set(f: *Failure, arena: Allocator, kind: Kind, code: []const u8, comptime fmt: []const u8, args: anytype) error{ Rejected, OutOfMemory } {
        const message = std.fmt.allocPrint(arena, fmt, args) catch return error.OutOfMemory;
        f.* = .{ .kind = kind, .code = code, .message = message };
        return error.Rejected;
    }

    /// The same for a message that is a literal.
    pub fn setLiteral(f: *Failure, kind: Kind, code: []const u8, message: []const u8) error{Rejected} {
        f.* = .{ .kind = kind, .code = code, .message = message };
        return error.Rejected;
    }
};

/// The errors of the services and repositories that have a meaning for the
/// client; anything else is a 500.
pub const ServiceError = error{
    /// The details are in the Failure.
    Rejected,
    NotFound,
    Conflict,
};

const testing = std.testing;

test "kinds map to the Go statuses" {
    try testing.expectEqual(std.http.Status.bad_request, Kind.invalid_input.status());
    try testing.expectEqual(std.http.Status.not_found, Kind.not_found.status());
    try testing.expectEqual(std.http.Status.conflict, Kind.conflict.status());
    try testing.expectEqual(std.http.Status.payload_too_large, Kind.too_large.status());
    try testing.expectEqual(std.http.Status.bad_gateway, Kind.upstream.status());
    try testing.expectEqual(std.http.Status.service_unavailable, Kind.unavailable.status());
}

test "set records the failure and rejects" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var f: Failure = .{};
    const err = f.set(arena.allocator(), .conflict, "name_taken", "name {s} is taken", .{"tv"});
    try testing.expectEqual(error.Rejected, err);
    try testing.expectEqual(Kind.conflict, f.kind);
    try testing.expectEqualStrings("name_taken", f.code);
    try testing.expectEqualStrings("name tv is taken", f.message);
}
