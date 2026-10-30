//! TLS for the NNTP client: an OpenSSL client over a `std.Io.net.Stream`.
//!
//! Always on and always verified: certificate chain and host name (or IP
//! address) are checked, TLS 1.2 is the minimum, SNI is the host name. There
//! is no option that weakens any of this. The trust store is OpenSSL's default
//! one (SSL_CERT_FILE and SSL_CERT_DIR work) plus, for tests,
//! `extra_roots_pem`. It mirrors internal/usenet/nntp/conn.go.
//!
//! How it works: OpenSSL runs on memory BIOs (tls_conn.zig). The engine moves
//! ciphertext between the BIOs and the stream through `std.Io`, so a blocked
//! read, write or handshake is an Io operation and `Future.cancel` ends it
//! promptly with `error.Canceled`. Nothing blocks on a raw fd.
//!
//! Lifetimes and rules:
//!  - The caller owns the stream. `connect` never closes it (also on failure);
//!    `close` / `abort` free the TLS state and leave the stream open.
//!  - The `*Client` is heap allocated with the allocator given to `connect`
//!    and stays valid until `close` / `abort`. `reader()` and `writer()` point
//!    into it, and so do slices their buffers return.
//!  - One task may read while another writes. Two readers or two writers at
//!    once are not supported (the std Reader and Writer are not either).
//!  - `reader()` fails with `error.ReadFailed`; the cause is `readError()`. A
//!    close_notify from the peer is `error.EndOfStream` (orderly, nothing is
//!    lost); a transport close without it is `ReadFailed` with
//!    `error.ConnectionClosed` (possible truncation). Same for `writer()`
//!    with `writeError()`. A cancelled read can be retried; after a cancelled
//!    or failed write the connection is unusable: `close` it.
//!  - Close the writer's buffer with `writer().flush()` before `close()`;
//!    `close()` sends close_notify, does not wait for the peer's, and frees.
//!  - No task may use a Client while `close` runs.
//!  - OpenSSL's per-thread error queue is cleared around every call, and
//!    plaintext buffers are wiped before they are freed. Nothing here logs.
//!
//! Test data (zig/testdata/tls, made once by testdata/tls/gen.sh with the
//! OpenSSL 3.4+ CLI, 100 year validity): ca.pem is the test CA (its key was
//! discarded); server.pem/.key is a P-256 leaf for localhost, 127.0.0.1 and
//! ::1; expired.* is the same names with a validity in January 2020;
//! wronghost.* is for other.example.test; other_ca.pem and untrusted.* are a
//! second unrelated CA and a localhost leaf under it. The server side is
//! test support (tls_server.zig, re-exported below).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const c = @import("openssl_c");
const conn_mod = @import("tls_conn.zig");
const server_mod = @import("tls_server.zig");

pub const Error = conn_mod.Error;
pub const Failure = conn_mod.Failure;
pub const Diagnostic = conn_mod.Diagnostic;
pub const Conn = conn_mod.Conn;

// Test support: the server side and the test certificates.
pub const Server = server_mod.Server;
pub const ServerContext = server_mod.ServerContext;
pub const ServerOptions = server_mod.ServerOptions;
pub const Version = server_mod.Version;
pub const testContext = server_mod.testContext;
pub const testRootsPem = server_mod.testRootsPem;

pub const ContextOptions = struct {
    /// PEM certificates trusted in addition to OpenSSL's default CA locations.
    /// For tests; production passes null.
    extra_roots_pem: ?[]const u8 = null,
};

/// A client-side OpenSSL context (trust store, protocol limits) that many
/// connections can share. It is reference counted by OpenSSL: connections made
/// from it stay valid after `deinit`. Building one per connection is cheap
/// enough for tests but loads the system CA store each time; the daemon makes
/// one per server.
pub const Context = struct {
    ctx: *c.SSL_CTX,

    pub fn init(options: ContextOptions) Error!Context {
        const ctx = c.SSL_CTX_new(c.TLS_client_method()) orelse return error.OutOfMemory;
        errdefer c.SSL_CTX_free(ctx);
        defer c.ERR_clear_error();
        if (c.SSL_CTX_set_min_proto_version(ctx, c.TLS1_2_VERSION) != 1) return error.HandshakeFailed;
        c.SSL_CTX_set_verify(ctx, c.SSL_VERIFY_PEER, null);
        // Renegotiation is not needed by NNTP and is a known attack surface.
        // SSL_OP_NO_RENEGOTIATION is 1 << 30 (the macro does not translate).
        _ = c.SSL_CTX_set_options(ctx, @as(u64, 1) << 30);
        if (c.SSL_CTX_set_default_verify_paths(ctx) != 1) return error.HandshakeFailed;
        if (options.extra_roots_pem) |pem| try addRoots(ctx, pem);
        return .{ .ctx = ctx };
    }

    pub fn deinit(self: Context) void {
        c.SSL_CTX_free(self.ctx);
    }
};

fn addRoots(ctx: *c.SSL_CTX, pem: []const u8) Error!void {
    if (pem.len == 0 or pem.len > std.math.maxInt(c_int)) return error.InvalidRootCertificates;
    const store = c.SSL_CTX_get_cert_store(ctx) orelse return error.OutOfMemory;
    const bio = c.BIO_new_mem_buf(pem.ptr, @intCast(pem.len)) orelse return error.OutOfMemory;
    defer _ = c.BIO_free(bio);
    var count: usize = 0;
    while (c.PEM_read_bio_X509(bio, null, null, null)) |cert| {
        defer c.X509_free(cert);
        if (c.X509_STORE_add_cert(store, cert) != 1) {
            // A duplicate is fine; anything else is not.
            const e = c.ERR_peek_last_error();
            if ((e & 0x7FFFFF) != c.X509_R_CERT_ALREADY_IN_HASH_TABLE) return error.InvalidRootCertificates;
        }
        count += 1;
    }
    if (count == 0) return error.InvalidRootCertificates;
}

pub const ClientOptions = struct {
    /// Extra trusted roots (PEM), for tests. Ignored when `context` is set.
    extra_roots_pem: ?[]const u8 = null,
    /// A shared context. null: a private one is made for this connection.
    context: ?Context = null,
    /// Filled on failure, so a connection test can say what was wrong.
    diagnostic: ?*Diagnostic = null,
};

pub const Client = struct {
    conn: Conn,

    /// TLS handshake as a client on `stream`, verifying the server against
    /// `host` (a DNS name, sent as SNI and matched against the certificate,
    /// or an IP literal, matched against the certificate's IP addresses).
    /// `host` is only copied during the call.
    ///
    /// Errors: `CertificateInvalid` (`options.diagnostic` says unknown
    /// authority, host name, expired or other), `HandshakeFailed` (no TLS,
    /// no common version, alert), `ConnectionClosed`, `NetworkFailure`,
    /// `Canceled` (the task was cancelled mid handshake), `OutOfMemory`,
    /// `InvalidHost`, `InvalidRootCertificates`. On any error everything is
    /// freed and the stream is untouched.
    pub fn connect(allocator: Allocator, io: Io, stream: Io.net.Stream, host: []const u8, options: ClientOptions) Error!*Client {
        if (host.len == 0 or host.len > 253) return error.InvalidHost;
        for (host) |ch| if (ch <= 0x20 or ch == 0x7f) return error.InvalidHost;

        var own: ?Context = null;
        defer if (own) |o| o.deinit();
        const context = options.context orelse blk: {
            own = try Context.init(.{ .extra_roots_pem = options.extra_roots_pem });
            break :blk own.?;
        };

        const self = try allocator.create(Client);
        errdefer allocator.destroy(self);
        try self.conn.init(allocator, io, stream, context.ctx, .client);
        errdefer self.conn.deinit();
        try setHost(self.conn.ssl, host);

        self.conn.handshake() catch |err| {
            if (options.diagnostic) |d| d.failure = switch (err) {
                error.CertificateInvalid, error.HandshakeFailed => self.conn.failure,
                error.Canceled, error.OutOfMemory => .none,
                else => .handshake,
            };
            return err;
        };
        // SSL_VERIFY_PEER already fails the handshake; this is the belt on
        // top of the braces.
        if (c.SSL_get_verify_result(self.conn.ssl) != c.X509_V_OK or c.SSL_get0_peer_certificate(self.conn.ssl) == null) {
            if (options.diagnostic) |d| d.failure = .certificate_invalid;
            return error.CertificateInvalid;
        }
        return self;
    }

    pub fn reader(self: *Client) *Io.Reader {
        return &self.conn.reader;
    }

    /// Buffered; `flush()` sends. Each flush makes at least one TLS record.
    pub fn writer(self: *Client) *Io.Writer {
        return &self.conn.writer;
    }

    /// Why the reader last returned `error.ReadFailed`.
    pub fn readError(self: *const Client) ?Error {
        return self.conn.read_error;
    }

    /// Why the writer last returned `error.WriteFailed`.
    pub fn writeError(self: *const Client) ?Error {
        return self.conn.write_error;
    }

    /// The raw transport error behind `NetworkFailure`, for logs only.
    pub fn transportError(self: *const Client) ?anyerror {
        return self.conn.io_error;
    }

    /// "TLSv1.3" or "TLSv1.2".
    pub fn protocolVersion(self: *Client) []const u8 {
        return std.mem.span(c.SSL_get_version(self.conn.ssl));
    }

    /// Sends close_notify, frees the connection. The stream stays open: the
    /// caller closes it.
    pub fn close(self: *Client) void {
        self.conn.shutdown();
        self.abort();
    }

    /// Frees the connection without close_notify (the transport is already
    /// dead, or the task was cancelled).
    pub fn abort(self: *Client) void {
        const allocator = self.conn.allocator;
        self.conn.deinit();
        allocator.destroy(self);
    }
};

fn setHost(ssl: *c.SSL, host: []const u8) Error!void {
    var z: [254]u8 = undefined;
    @memcpy(z[0..host.len], host);
    z[host.len] = 0;
    const param = c.SSL_get0_param(ssl) orelse return error.HandshakeFailed;
    defer c.ERR_clear_error();
    const is_ip = if (Io.net.IpAddress.parse(host, 0)) |_| true else |_| false;
    if (is_ip) {
        // No SNI for IP literals (RFC 6066); the certificate must list the IP.
        if (c.X509_VERIFY_PARAM_set1_ip_asc(param, &z) != 1) return error.InvalidHost;
    } else {
        if (c.SSL_ctrl(ssl, c.SSL_CTRL_SET_TLSEXT_HOSTNAME, c.TLSEXT_NAMETYPE_host_name, &z) != 1) return error.InvalidHost;
        c.X509_VERIFY_PARAM_set_hostflags(param, c.X509_CHECK_FLAG_NO_PARTIAL_WILDCARDS);
        if (c.X509_VERIFY_PARAM_set1_host(param, &z, host.len) != 1) return error.InvalidHost;
    }
}
