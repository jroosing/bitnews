//! TEST SUPPORT: the server side of TLS, used by tests (the fake NNTP server
//! of the nntp group and the tls tests). bitnews never serves TLS (ADR-0014);
//! nothing outside tests may import this file.
//!
//! The same engine as the client (tls_conn.zig). A server does not verify
//! client certificates. The test certificates live in testdata/tls (see the
//! header of tls.zig); `testContext` loads one of them, `testRootsPem` the CA
//! a client must trust. Tests run with zig/ as the working directory.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const c = @import("openssl_c");
const conn_mod = @import("tls_conn.zig");

const Conn = conn_mod.Conn;
const Error = conn_mod.Error;

pub const Version = enum {
    tls1_0,
    tls1_1,
    tls1_2,
    tls1_3,

    fn code(v: Version) c_int {
        return switch (v) {
            .tls1_0 => c.TLS1_VERSION,
            .tls1_1 => c.TLS1_1_VERSION,
            .tls1_2 => c.TLS1_2_VERSION,
            .tls1_3 => c.TLS1_3_VERSION,
        };
    }
};

pub const ServerOptions = struct {
    min_version: Version = .tls1_2,
    /// null: the newest the library has.
    max_version: ?Version = null,
};

/// A reference-counted OpenSSL server context (certificate and key).
pub const ServerContext = struct {
    ctx: *c.SSL_CTX,

    /// `cert_pem`: the certificate (a chain's first entry is the leaf);
    /// `key_pem`: its unencrypted private key.
    pub fn init(cert_pem: []const u8, key_pem: []const u8, options: ServerOptions) Error!ServerContext {
        const ctx = c.SSL_CTX_new(c.TLS_server_method()) orelse return error.OutOfMemory;
        errdefer c.SSL_CTX_free(ctx);
        defer c.ERR_clear_error();
        if (c.SSL_CTX_set_min_proto_version(ctx, options.min_version.code()) != 1) return error.InvalidServerCredentials;
        if (options.max_version) |v| {
            if (c.SSL_CTX_set_max_proto_version(ctx, v.code()) != 1) return error.InvalidServerCredentials;
        }
        if (options.min_version == .tls1_0 or options.min_version == .tls1_1) {
            // Old protocol versions are only for the test that proves the
            // client refuses them; they need the lowest security level.
            if (c.SSL_CTX_set_cipher_list(ctx, "ALL:@SECLEVEL=0") != 1) return error.InvalidServerCredentials;
            c.SSL_CTX_set_security_level(ctx, 0);
        }

        if (cert_pem.len > std.math.maxInt(c_int) or key_pem.len > std.math.maxInt(c_int)) return error.InvalidServerCredentials;
        const cbio = c.BIO_new_mem_buf(cert_pem.ptr, @intCast(cert_pem.len)) orelse return error.OutOfMemory;
        defer _ = c.BIO_free(cbio);
        const cert = c.PEM_read_bio_X509(cbio, null, null, null) orelse return error.InvalidServerCredentials;
        defer c.X509_free(cert);
        if (c.SSL_CTX_use_certificate(ctx, cert) != 1) return error.InvalidServerCredentials;

        const kbio = c.BIO_new_mem_buf(key_pem.ptr, @intCast(key_pem.len)) orelse return error.OutOfMemory;
        defer _ = c.BIO_free(kbio);
        const key = c.PEM_read_bio_PrivateKey(kbio, null, null, null) orelse return error.InvalidServerCredentials;
        defer c.EVP_PKEY_free(key);
        if (c.SSL_CTX_use_PrivateKey(ctx, key) != 1) return error.InvalidServerCredentials;
        if (c.SSL_CTX_check_private_key(ctx) != 1) return error.InvalidServerCredentials;
        return .{ .ctx = ctx };
    }

    /// Connections made from it stay valid after this.
    pub fn deinit(self: ServerContext) void {
        c.SSL_CTX_free(self.ctx);
    }
};

/// One accepted TLS connection. Same reader/writer rules as `tls.Client`.
pub const Server = struct {
    conn: Conn,

    /// Runs the server handshake on `stream`. The caller keeps the stream and
    /// closes it after `close` / `abort`.
    pub fn accept(allocator: Allocator, io: Io, stream: Io.net.Stream, context: ServerContext) Error!*Server {
        const self = try allocator.create(Server);
        errdefer allocator.destroy(self);
        try self.conn.init(allocator, io, stream, context.ctx, .server);
        errdefer self.conn.deinit();
        try self.conn.handshake();
        return self;
    }

    pub fn reader(self: *Server) *Io.Reader {
        return &self.conn.reader;
    }

    pub fn writer(self: *Server) *Io.Writer {
        return &self.conn.writer;
    }

    /// The SNI host name the client sent, valid until close.
    pub fn serverName(self: *Server) ?[]const u8 {
        const p = c.SSL_get_servername(self.conn.ssl, c.TLSEXT_NAMETYPE_host_name) orelse return null;
        return std.mem.span(p);
    }

    pub fn readError(self: *const Server) ?Error {
        return self.conn.read_error;
    }

    pub fn writeError(self: *const Server) ?Error {
        return self.conn.write_error;
    }

    /// Sends close_notify (after flushing the writer yourself), frees the connection.
    pub fn close(self: *Server) void {
        self.conn.shutdown();
        self.abort();
    }

    /// Frees the connection without close_notify: to the peer it looks like
    /// an abrupt close once the stream is closed.
    pub fn abort(self: *Server) void {
        const allocator = self.conn.allocator;
        self.conn.deinit();
        allocator.destroy(self);
    }
};

/// A server context from testdata/tls/<name>.pem and <name>.key ("server",
/// "expired", "wronghost", "untrusted").
pub fn testContext(allocator: Allocator, io: Io, name: []const u8, options: ServerOptions) !ServerContext {
    var path_buf: [96]u8 = undefined;
    const cert = try readTestFile(allocator, io, try std.fmt.bufPrint(&path_buf, "testdata/tls/{s}.pem", .{name}));
    defer allocator.free(cert);
    const key = try readTestFile(allocator, io, try std.fmt.bufPrint(&path_buf, "testdata/tls/{s}.key", .{name}));
    defer {
        std.crypto.secureZero(u8, key);
        allocator.free(key);
    }
    return ServerContext.init(cert, key, options);
}

/// The PEM of the test CA that signed "server", "expired" and "wronghost":
/// pass it as `ClientOptions.extra_roots_pem`. Caller frees.
pub fn testRootsPem(allocator: Allocator, io: Io) ![]u8 {
    return readTestFile(allocator, io, "testdata/tls/ca.pem");
}

fn readTestFile(allocator: Allocator, io: Io, path: []const u8) ![]u8 {
    return Io.Dir.cwd().readFileAlloc(io, path, allocator, .limited(1 << 16));
}
