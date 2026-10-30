//! The NNTP client (internal/usenet/nntp): a strict NNTP-over-TLS client with
//! one connection pool per Server. Import this file; the pieces are:
//!
//!   Pool       up to N connections to one Server, `fetcher()` for the
//!              scheduler (`usenet_types.Fetcher`).
//!   Conn       one authenticated connection (greeting, AUTHINFO, BODY, DATE).
//!   Dialer     the seam that opens secure transports: `TlsDialer` is the real
//!              one (strict TLS, tls.zig); the network-interface binding
//!              (VPN kill switch) plugs in there.
//!   probe      the connection test of a Server.
//!
//! Errors are classes (`usenet_types.FetchError`), details are in a `Diag`.
//! Test support is `nntptest.zig`.
const std = @import("std");
const servers_types = @import("servers_types.zig");

pub const proto = @import("nntp_proto.zig");
pub const diag = @import("nntp_diag.zig");
pub const transport = @import("nntp_transport.zig");
pub const conn = @import("nntp_conn.zig");
pub const pool = @import("nntp_pool.zig");
pub const probe_mod = @import("nntp_probe.zig");
pub const tls_dialer = @import("nntp_tls.zig");

pub const Diag = diag.Diag;
pub const Transport = transport.Transport;
pub const Dialer = transport.Dialer;
pub const DialRequest = transport.DialRequest;
pub const DialError = transport.DialError;
pub const Conn = conn.Conn;
pub const Config = conn.Config;
pub const Pool = pool.Pool;
pub const PoolOptions = pool.Options;
pub const TlsDialer = tls_dialer.TlsDialer;
pub const probe = probe_mod.probe;
pub const ProbeResult = probe_mod.Result;
pub const ProbeTarget = probe_mod.Target;

/// The client Config of a configured Server (already validated by
/// `servers_types.check`: the port fits, the credentials are clean). The
/// returned Config borrows the Server's strings.
pub fn configFromServer(s: servers_types.Server) Config {
    return .{
        .host = s.host,
        .port = @intCast(std.math.clamp(s.port, 1, 65535)),
        .user = s.username,
        .pass = s.passwordBytes(),
    };
}

/// The pool options of a configured Server: its name, connection limit and
/// credentials, dialed through `dialer`.
pub fn poolOptionsFromServer(s: servers_types.Server, dialer: Dialer) PoolOptions {
    return .{
        .name = s.name,
        .max_connections = s.max_connections,
        .config = configFromServer(s),
        .dialer = dialer,
    };
}

test "a Server becomes a client config" {
    var pw = try @import("secret.zig").Secret.init(std.testing.allocator, "pw");
    defer pw.deinit();
    const s: servers_types.Server = .{ .name = "main", .host = "news.example", .port = 443, .username = "u", .password = pw, .max_connections = 12 };
    const c = configFromServer(s);
    try std.testing.expectEqualStrings("news.example", c.host);
    try std.testing.expectEqual(@as(u16, 443), c.port);
    try std.testing.expectEqualStrings("pw", c.pass);
    const o = poolOptionsFromServer(s, undefined);
    try std.testing.expectEqual(@as(i64, 12), o.max_connections);
    try std.testing.expectEqualStrings("main", o.name);
}
