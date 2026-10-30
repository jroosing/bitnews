//! What went wrong in the NNTP client, beyond the error class (Go: the
//! `*Error` with Kind, Op, Code, Text and Err). Zig errors carry no payload,
//! so a failing call fills a `Diag` next to the error it returns.
//!
//! A Diag never holds credentials: server text goes through
//! `nntp_proto.sanitize` with the secrets redacted.
const std = @import("std");
const Io = std.Io;
const proto = @import("nntp_proto.zig");

/// The four classes of Go's sentinel errors.
pub const Kind = enum {
    /// The Server does not have the Article; the connection is still usable.
    article_not_found,
    /// The Server rejected the credentials.
    auth,
    /// The connection failed, or the Server reported a condition that makes
    /// it unusable.
    connection,
    /// The Server broke the protocol or exceeded a limit.
    protocol,
};

/// The step that failed.
pub const Op = enum { none, dial, tls, greeting, auth, body, date };

/// The underlying cause, as far as the probe and the logs need it.
pub const Cause = enum {
    none,
    timeout,
    canceled,
    /// The peer closed the connection.
    eof,
    /// Any other socket error.
    io,
    /// The command was sent to a connection that is already dead.
    conn_dead,
    line_too_long,
    body_too_large,
    malformed_response,
    dns_not_found,
    dns_timeout,
    dns_other,
    tcp_refused,
    tcp_timeout,
    tcp_unreachable,
    tcp_other,
    tls_cert_untrusted,
    tls_cert_hostname,
    tls_cert_expired,
    tls_cert_invalid,
    tls_not_tls,
    tls_timeout,
    tls_handshake,

    pub fn isTimeout(c: Cause) bool {
        return switch (c) {
            .timeout, .dns_timeout, .tcp_timeout, .tls_timeout => true,
            else => false,
        };
    }
};

pub const Diag = struct {
    kind: Kind = .connection,
    op: Op = .none,
    /// The three-digit response code, or 0 when there was none.
    code: u16 = 0,
    cause: Cause = .none,
    text_buf: [proto.max_error_text]u8 = undefined,
    text_len: u8 = 0,

    pub fn text(d: *const Diag) []const u8 {
        return d.text_buf[0..d.text_len];
    }

    pub fn reset(d: *Diag) void {
        d.* = .{};
    }

    pub fn set(d: *Diag, kind: Kind, op: Op, cause: Cause) void {
        d.kind = kind;
        d.op = op;
        d.code = 0;
        d.cause = cause;
        d.text_len = 0;
    }

    /// Stores the (sanitised) server text and code of a response.
    pub fn setResponse(d: *Diag, kind: Kind, op: Op, code: u16, raw_text: []const u8, secrets: []const []const u8) void {
        d.set(kind, op, .none);
        d.code = code;
        d.setText(raw_text, secrets);
    }

    pub fn setText(d: *Diag, raw: []const u8, secrets: []const []const u8) void {
        // The sanitised text may start after leading spaces were trimmed: move
        // it to the front so text() is a plain prefix.
        const clean = proto.sanitize(&d.text_buf, raw, secrets);
        std.mem.copyForwards(u8, d.text_buf[0..clean.len], clean);
        d.text_len = @intCast(clean.len);
    }

    /// "nntp: connection error during body: 502 text: cause", for logs.
    pub fn describe(d: *const Diag, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeAll(switch (d.kind) {
            .article_not_found => "nntp: article not found",
            .auth => "nntp: authentication failed",
            .connection => "nntp: connection error",
            .protocol => "nntp: protocol error",
        });
        if (d.op != .none) try w.print(" during {s}", .{@tagName(d.op)});
        if (d.code != 0) {
            try w.print(": {d}", .{d.code});
            if (d.text_len > 0) try w.print(" {s}", .{d.text()});
        }
        if (d.cause != .none) try w.print(": {s}", .{@tagName(d.cause)});
    }
};

test "describe reads like Go's Error and never shows more than was stored" {
    var d: Diag = .{};
    d.setResponse(.connection, .body, 502, "  too many \xff connections  ", &.{});
    var buf: [200]u8 = undefined;
    var w: Io.Writer = .fixed(&buf);
    try d.describe(&w);
    try std.testing.expectEqualStrings("nntp: connection error during body: 502 too many ? connections", w.buffered());
}

test "credentials echoed by the server are redacted" {
    var d: Diag = .{};
    d.setResponse(.auth, .auth, 481, "wrong password hunter2 for alice", &.{ "alice", "hunter2" });
    var buf: [200]u8 = undefined;
    var w: Io.Writer = .fixed(&buf);
    try d.describe(&w);
    try std.testing.expect(std.mem.indexOf(u8, w.buffered(), "hunter2") == null);
    try std.testing.expect(std.mem.indexOf(u8, w.buffered(), "alice") == null);
    try std.testing.expectEqualStrings("nntp: authentication failed during auth: 481 wrong password *** for ***", w.buffered());
}
