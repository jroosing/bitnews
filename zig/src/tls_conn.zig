//! The TLS engine behind `tls.Client` and `tls.Server`: one OpenSSL `SSL`
//! object wired to two memory BIOs. OpenSSL never touches the socket. The
//! engine reads ciphertext from the `std.Io.net.Stream` through Io and feeds
//! it to the read BIO, and sends what OpenSSL leaves in the write BIO through
//! Io. Every blocking operation is therefore an Io operation that can be
//! cancelled.
//!
//! Locks (all `Io.Mutex`):
//!  - `ssl_mutex` guards every OpenSSL call on this connection and both BIOs.
//!    It is held only for non-blocking OpenSSL calls, never across Io, so a
//!    task blocked in a socket read does not hold it.
//!  - `rx_mutex` serialises socket reads and the shared `cipher_in` buffer.
//!  - `tx_mutex` serialises socket writes, so records of two writers never
//!    interleave.
//! Lock order is tx or rx first, then ssl. This makes one reading task and one
//! writing task safe at the same time.
//!
//! The OpenSSL error queue is thread local and Io tasks hop between threads,
//! so the queue is cleared before and after every OpenSSL call, inside the
//! `ssl_mutex` section (which contains no suspension point).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const c = @import("openssl_c");

/// Errors of a TLS connection. `Canceled` is `std.Io`'s: the task was cancelled.
pub const Error = error{
    /// The peer's certificate was refused (see `Diagnostic.failure` for why).
    CertificateInvalid,
    /// The handshake failed for another reason: no TLS on the port, no common
    /// protocol version (TLS 1.2 is the minimum), a fatal alert, a bad message.
    HandshakeFailed,
    /// The peer closed the transport without a TLS close_notify, or reset it.
    /// An orderly close_notify is `error.EndOfStream` on the reader instead.
    ConnectionClosed,
    /// The TLS stream is broken after the handshake (bad record, fatal alert).
    TlsFailure,
    /// Another transport error (see `Conn.io_error`).
    NetworkFailure,
    Canceled,
    OutOfMemory,
    /// The host name is empty, too long or has control characters.
    InvalidHost,
    /// `extra_roots_pem` has no certificate.
    InvalidRootCertificates,
    /// Certificate or key of a server context could not be loaded.
    InvalidServerCredentials,
};

/// Why a handshake failed, in the terms of the connection test
/// (internal/app/prober.go): `detail()` is the stable machine-readable token
/// and `message()` the sentence shown to the user. Neither carries anything
/// from the peer.
pub const Failure = enum {
    none,
    certificate_untrusted,
    certificate_hostname,
    certificate_expired,
    certificate_invalid,
    not_tls,
    handshake,

    pub fn detail(f: Failure) []const u8 {
        return @tagName(f);
    }

    pub fn message(f: Failure) []const u8 {
        return switch (f) {
            .none => "",
            .certificate_untrusted => "the server's certificate is not trusted",
            .certificate_hostname => "the server's certificate is not valid for this host name",
            .certificate_expired => "the server's certificate has expired or is not valid yet",
            .certificate_invalid => "the server's certificate is not valid",
            .not_tls => "the server does not speak TLS on this port: bitnews only connects over TLS",
            .handshake => "the TLS handshake failed",
        };
    }

    fn isCertificate(f: Failure) bool {
        return switch (f) {
            .certificate_untrusted, .certificate_hostname, .certificate_expired, .certificate_invalid => true,
            else => false,
        };
    }
};

/// Filled by a failed `Client.connect` when `ClientOptions.diagnostic` is set.
pub const Diagnostic = struct {
    failure: Failure = .none,
};

const Kind = enum { handshake, read, write };

const Outcome = union(enum) {
    done: usize,
    want_read,
    want_write,
    /// The peer sent close_notify.
    closed,
    failed: Failure,
    syscall,
};

pub const Role = enum { client, server };

/// Plaintext and ciphertext chunk size: one TLS record.
const record_size = 16 * 1024;

pub const Conn = struct {
    allocator: Allocator,
    io: Io,
    /// Owned by the caller of connect/accept; the engine never closes it.
    stream: Io.net.Stream,
    role: Role,
    ssl: *c.SSL,
    /// Owned by `ssl` (SSL_set_bio); kept to feed and drain them.
    rbio: *c.BIO,
    wbio: *c.BIO,
    ssl_mutex: Io.Mutex = .init,
    rx_mutex: Io.Mutex = .init,
    tx_mutex: Io.Mutex = .init,
    peer_eof: std.atomic.Value(bool) = .init(false),
    /// A socket write failed half way: the record stream is damaged.
    tx_broken: std.atomic.Value(bool) = .init(false),
    established: bool = false,
    cipher_in: []u8,
    cipher_out: []u8,
    plain_in: []u8,
    plain_out: []u8,
    reader: Io.Reader,
    writer: Io.Writer,
    /// Why the reader / writer last returned ReadFailed / WriteFailed.
    read_error: ?Error = null,
    write_error: ?Error = null,
    /// The raw transport error behind `NetworkFailure`, for logs.
    io_error: ?anyerror = null,
    failure: Failure = .none,

    /// Builds the engine in place (`conn` must not move afterwards: the
    /// reader and writer find it through their addresses). Takes a reference
    /// on `ctx`; the caller keeps its own.
    pub fn init(conn: *Conn, allocator: Allocator, io: Io, stream: Io.net.Stream, ctx: *c.SSL_CTX, role: Role) Error!void {
        const bufs = try allocator.alloc(u8, record_size * 4);
        errdefer allocator.free(bufs);
        const ssl = c.SSL_new(ctx) orelse return error.OutOfMemory;
        errdefer c.SSL_free(ssl);
        const rbio = c.BIO_new(c.BIO_s_mem()) orelse return error.OutOfMemory;
        const wbio = c.BIO_new(c.BIO_s_mem()) orelse {
            _ = c.BIO_free(rbio);
            return error.OutOfMemory;
        };
        // From here the BIOs belong to ssl.
        c.SSL_set_bio(ssl, rbio, wbio);
        _ = c.SSL_ctrl(ssl, c.SSL_CTRL_MODE, c.SSL_MODE_ENABLE_PARTIAL_WRITE | c.SSL_MODE_ACCEPT_MOVING_WRITE_BUFFER, null);
        switch (role) {
            .client => c.SSL_set_connect_state(ssl),
            .server => c.SSL_set_accept_state(ssl),
        }
        conn.* = .{
            .allocator = allocator,
            .io = io,
            .stream = stream,
            .role = role,
            .ssl = ssl,
            .rbio = rbio,
            .wbio = wbio,
            .cipher_in = bufs[0..record_size],
            .cipher_out = bufs[record_size .. 2 * record_size],
            .plain_in = bufs[2 * record_size .. 3 * record_size],
            .plain_out = bufs[3 * record_size ..],
            .reader = .{
                .vtable = &.{ .stream = readStream },
                .buffer = bufs[2 * record_size .. 3 * record_size],
                .seek = 0,
                .end = 0,
            },
            .writer = .{
                .vtable = &.{ .drain = writeDrain },
                .buffer = bufs[3 * record_size ..],
            },
        };
    }

    /// Frees everything. Does not send close_notify (see `shutdown`) and does
    /// not close the stream. No task may still use the connection.
    pub fn deinit(conn: *Conn) void {
        c.SSL_free(conn.ssl); // frees both BIOs
        c.ERR_clear_error();
        // Plaintext (passwords, article text) and key-derived ciphertext
        // buffers are wiped before they go back to the allocator.
        const all = conn.cipher_in.ptr[0 .. record_size * 4];
        std.crypto.secureZero(u8, all);
        conn.allocator.free(all);
        conn.* = undefined;
    }

    /// Sends close_notify (best effort, errors ignored). The peer's reply is
    /// not waited for.
    pub fn shutdown(conn: *Conn) void {
        if (!conn.established or conn.tx_broken.load(.acquire)) return;
        conn.ssl_mutex.lockUncancelable(conn.io);
        c.ERR_clear_error();
        _ = c.SSL_shutdown(conn.ssl);
        c.ERR_clear_error();
        conn.ssl_mutex.unlock(conn.io);
        conn.flushOut() catch {};
    }

    /// Runs the TLS handshake to completion.
    pub fn handshake(conn: *Conn) Error!void {
        _ = conn.run(.handshake, null, 0) catch |err| switch (err) {
            error.EndOfStream => return error.ConnectionClosed,
            else => |e| return e,
        };
        conn.established = true;
    }

    // ---- one OpenSSL step ------------------------------------------------

    fn attempt(conn: *Conn, kind: Kind, ptr: ?*anyopaque, len: usize) Outcome {
        conn.ssl_mutex.lockUncancelable(conn.io);
        defer conn.ssl_mutex.unlock(conn.io);
        c.ERR_clear_error();
        defer c.ERR_clear_error();
        const n: c_int = switch (kind) {
            .handshake => c.SSL_do_handshake(conn.ssl),
            .read => c.SSL_read(conn.ssl, ptr, @intCast(@min(len, record_size))),
            .write => c.SSL_write(conn.ssl, ptr, @intCast(@min(len, record_size))),
        };
        if (n > 0) return .{ .done = if (kind == .handshake) 0 else @intCast(n) };
        switch (c.SSL_get_error(conn.ssl, n)) {
            c.SSL_ERROR_WANT_READ => return .want_read,
            c.SSL_ERROR_WANT_WRITE => return .want_write,
            c.SSL_ERROR_ZERO_RETURN => return .closed,
            c.SSL_ERROR_SSL => return .{ .failed = conn.classify(kind) },
            else => return .syscall,
        }
    }

    /// Called with the error queue still filled and `ssl_mutex` held.
    fn classify(conn: *Conn, kind: Kind) Failure {
        if (kind != .handshake) return .handshake;
        if (conn.role == .client) {
            switch (c.SSL_get_verify_result(conn.ssl)) {
                c.X509_V_OK => {},
                c.X509_V_ERR_CERT_HAS_EXPIRED, c.X509_V_ERR_CERT_NOT_YET_VALID => return .certificate_expired,
                c.X509_V_ERR_HOSTNAME_MISMATCH, c.X509_V_ERR_IP_ADDRESS_MISMATCH => return .certificate_hostname,
                c.X509_V_ERR_UNABLE_TO_GET_ISSUER_CERT,
                c.X509_V_ERR_UNABLE_TO_GET_ISSUER_CERT_LOCALLY,
                c.X509_V_ERR_UNABLE_TO_VERIFY_LEAF_SIGNATURE,
                c.X509_V_ERR_DEPTH_ZERO_SELF_SIGNED_CERT,
                c.X509_V_ERR_SELF_SIGNED_CERT_IN_CHAIN,
                => return .certificate_untrusted,
                else => return .certificate_invalid,
            }
        }
        // The first queued error is the cause. ERR_GET_REASON in OpenSSL 3.
        const e = c.ERR_peek_error();
        const reason: c_ulong = e & 0x7FFFFF;
        const lib: c_ulong = (e >> 23) & 0xFF;
        if (lib == c.ERR_LIB_SSL) switch (reason) {
            c.SSL_R_WRONG_VERSION_NUMBER,
            c.SSL_R_HTTP_REQUEST,
            c.SSL_R_HTTPS_PROXY_REQUEST,
            c.SSL_R_PACKET_LENGTH_TOO_LONG,
            => return .not_tls,
            else => {},
        };
        return .handshake;
    }

    // ---- driving Io -------------------------------------------------------

    const RunError = Error || error{EndOfStream};

    fn run(conn: *Conn, kind: Kind, ptr: ?*anyopaque, len: usize) RunError!usize {
        while (true) {
            const out = conn.attempt(kind, ptr, len);
            // Whatever OpenSSL produced goes out first: handshake flights,
            // alerts, key updates, the record just written.
            const sent = conn.flushOut();
            switch (out) {
                .done => |n| {
                    try sent;
                    return n;
                },
                .want_write => try sent,
                .want_read => {
                    try sent;
                    if (conn.peer_eof.load(.acquire)) return error.ConnectionClosed;
                    try conn.fillIn();
                },
                .closed => return if (kind == .read) error.EndOfStream else error.ConnectionClosed,
                .failed => |f| {
                    conn.failure = f;
                    if (kind != .handshake) return error.TlsFailure;
                    return if (f.isCertificate()) error.CertificateInvalid else error.HandshakeFailed;
                },
                .syscall => return error.ConnectionClosed,
            }
        }
    }

    fn mapIo(conn: *Conn, err: anyerror) Error {
        return switch (err) {
            error.Canceled => error.Canceled,
            error.ConnectionResetByPeer, error.SocketUnconnected, error.BrokenPipe => error.ConnectionClosed,
            else => {
                conn.io_error = err;
                return error.NetworkFailure;
            },
        };
    }

    /// Reads one chunk of ciphertext into the read BIO. A zero-length read
    /// sets `peer_eof`.
    fn fillIn(conn: *Conn) Error!void {
        conn.rx_mutex.lock(conn.io) catch return error.Canceled;
        defer conn.rx_mutex.unlock(conn.io);
        // Another task may have fed the BIO while this one waited for the lock.
        conn.ssl_mutex.lockUncancelable(conn.io);
        const pending = c.BIO_ctrl_pending(conn.rbio);
        conn.ssl_mutex.unlock(conn.io);
        if (pending > 0 or conn.peer_eof.load(.acquire)) return;

        var bufs = [_][]u8{conn.cipher_in};
        const res = conn.stream.readWithControl(conn.io, &bufs, &.{}) catch |err| return conn.mapIo(err);
        const n = res.data_len;
        if (n == 0) {
            conn.peer_eof.store(true, .release);
            return;
        }
        conn.ssl_mutex.lockUncancelable(conn.io);
        defer conn.ssl_mutex.unlock(conn.io);
        if (c.BIO_write(conn.rbio, conn.cipher_in.ptr, @intCast(n)) != @as(c_int, @intCast(n))) return error.OutOfMemory;
    }

    /// Sends everything OpenSSL left in the write BIO.
    fn flushOut(conn: *Conn) Error!void {
        if (conn.tx_broken.load(.acquire)) return error.ConnectionClosed;
        conn.ssl_mutex.lockUncancelable(conn.io);
        const pending = c.BIO_ctrl_pending(conn.wbio);
        conn.ssl_mutex.unlock(conn.io);
        if (pending == 0) return;

        conn.tx_mutex.lock(conn.io) catch return error.Canceled;
        defer conn.tx_mutex.unlock(conn.io);
        while (true) {
            conn.ssl_mutex.lockUncancelable(conn.io);
            const n = c.BIO_read(conn.wbio, conn.cipher_out.ptr, @intCast(conn.cipher_out.len));
            conn.ssl_mutex.unlock(conn.io);
            if (n <= 0) return;
            var w = conn.stream.writer(conn.io, &.{});
            w.interface.writeAll(conn.cipher_out[0..@intCast(n)]) catch {
                conn.tx_broken.store(true, .release);
                return conn.mapIo(w.err orelse error.WriteFailed);
            };
        }
    }

    // ---- std.Io.Reader / Writer -------------------------------------------

    fn readStream(r: *Io.Reader, w: *Io.Writer, limit: Io.Limit) Io.Reader.StreamError!usize {
        const conn: *Conn = @alignCast(@fieldParentPtr("reader", r));
        const dest = limit.slice(try w.writableSliceGreedy(1));
        if (dest.len == 0) return 0;
        const n = conn.run(.read, dest.ptr, dest.len) catch |err| switch (err) {
            error.EndOfStream => return error.EndOfStream,
            else => |e| {
                conn.read_error = e;
                return error.ReadFailed;
            },
        };
        w.advance(n);
        return n;
    }

    fn writeDrain(w: *Io.Writer, data: []const []const u8, splat: usize) Io.Writer.Error!usize {
        const conn: *Conn = @alignCast(@fieldParentPtr("writer", w));
        conn.sendAll(w.buffered()) catch |err| {
            conn.write_error = err;
            return error.WriteFailed;
        };
        w.end = 0;
        var total: usize = 0;
        for (data, 0..) |bytes, i| {
            const reps = if (i == data.len - 1) splat else 1;
            for (0..reps) |_| {
                conn.sendAll(bytes) catch |err| {
                    conn.write_error = err;
                    return error.WriteFailed;
                };
                total += bytes.len;
            }
        }
        return total;
    }

    fn sendAll(conn: *Conn, bytes: []const u8) Error!void {
        var rest = bytes;
        while (rest.len > 0) {
            const n = conn.run(.write, @constCast(rest.ptr), rest.len) catch |err| switch (err) {
                error.EndOfStream => return error.ConnectionClosed,
                else => |e| return e,
            };
            rest = rest[n..];
        }
    }
};
