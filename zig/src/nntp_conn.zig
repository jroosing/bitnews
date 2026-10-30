//! One authenticated NNTP connection (internal/usenet/nntp conn.go,
//! body.go, date.go): greeting, AUTHINFO, BODY by message id and DATE, one
//! command at a time, no pipelining. Like Go's client it never sends MODE
//! READER: providers speak reader mode on their NNTPS port.
//!
//! Every failure is an error of the class the scheduler decides by, with the
//! details in a `Diag`. Everything except `ArticleNotFound` (and a rejected
//! message id, which sends nothing) leaves the connection dead: `isDead`
//! reports true and the caller must `close` and drop it.
//!
//! Hostile input: lines are capped at `proto.max_line_length`, a body at
//! `proto.max_body_size`; a body line handed to the Sink is a slice into the
//! reader's buffer and valid only during the call, so the whole body is never
//! buffered.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const proto = @import("nntp_proto.zig");
const diag_mod = @import("nntp_diag.zig");
const transport_mod = @import("nntp_transport.zig");
const types = @import("usenet_types.zig");

const Diag = diag_mod.Diag;
const Cause = diag_mod.Cause;
const Transport = transport_mod.Transport;
const Dialer = transport_mod.Dialer;

pub const default_port = 563;
pub const default_connect_timeout_ms = 30_000;
pub const default_read_timeout_ms = 60_000;
/// How long a QUIT may take.
const quit_timeout_ms = 2_000;

/// Describes one Server endpoint. TLS is always on, certificates are always
/// verified and there is deliberately no way to change either.
pub const Config = struct {
    /// The DNS name or IP of the Server; also the name the certificate must
    /// match.
    host: []const u8,
    port: u16 = default_port,
    /// Empty for a Server that needs no authentication. Never logged or put
    /// in a Diag.
    user: []const u8 = "",
    pass: []const u8 = "",
    /// Bounds the TCP connect plus the TLS handshake.
    connect_timeout_ms: u32 = default_connect_timeout_ms,
    /// The idle timeout of each read and write.
    read_timeout_ms: u32 = default_read_timeout_ms,
    /// An overall deadline for the handshake and the commands after it (the
    /// connection test uses one); the idle timeout still applies inside it.
    deadline: ?Io.Clock.Timestamp = null,
};

pub const OpenError = error{
    /// The Config is unusable (empty host, control characters in the
    /// credentials). Nothing was sent.
    InvalidConfig,
    Connection,
    Protocol,
    Auth,
    OutOfMemory,
    Canceled,
};

pub const BodyError = error{
    ArticleNotFound,
    Connection,
    Protocol,
    InvalidMessageId,
    /// The Sink refused a line.
    Aborted,
    Canceled,
};

pub const DateError = error{ Connection, Protocol, Canceled };

/// Rejects a Config before anything is dialed.
pub fn validateConfig(cfg: Config) error{InvalidConfig}!void {
    if (cfg.host.len == 0) return error.InvalidConfig;
    if (hasControl(cfg.user) or hasControl(cfg.pass)) return error.InvalidConfig;
}

fn hasControl(s: []const u8) bool {
    return std.mem.findAny(u8, s, "\r\n\x00") != null;
}

pub const Conn = struct {
    gpa: Allocator,
    transport: Transport,
    dead: std.atomic.Value(bool) = .init(false),
    /// What the last failing call found out.
    diag: Diag = .{},
    /// An overall deadline for the commands that follow (the connection test
    /// sets one); the idle timeout still applies inside it.
    command_deadline: ?Io.Clock.Timestamp = null,
    /// Scrubbed from any server text kept in `diag`, because servers
    /// sometimes echo credentials back. Owned, wiped on close.
    user: []u8,
    pass: []u8,
    secret_list: [2][]const u8,

    /// Dials with `dialer`, reads the greeting and authenticates when
    /// `cfg.user` is set (Go: Dial). On failure `diag` says why.
    pub fn connect(gpa: Allocator, io: Io, dialer: Dialer, cfg: Config, diag: *Diag) OpenError!*Conn {
        diag.reset();
        validateConfig(cfg) catch return error.InvalidConfig;
        const transport = dialer.dial(gpa, io, .{
            .host = cfg.host,
            .port = cfg.port,
            .connect_timeout_ms = cfg.connect_timeout_ms,
            .read_timeout_ms = cfg.read_timeout_ms,
        }) catch |err| return dialFailure(diag, err);
        return open(gpa, io, transport, cfg, diag);
    }

    /// Takes over `transport` (destroyed on failure), reads the greeting and
    /// authenticates.
    pub fn open(gpa: Allocator, io: Io, transport: Transport, cfg: Config, diag: *Diag) OpenError!*Conn {
        errdefer transport.destroy(io);
        diag.reset();
        std.debug.assert(transport.reader.buffer.len >= proto.min_read_buffer);
        validateConfig(cfg) catch return error.InvalidConfig;
        const user = try gpa.dupe(u8, cfg.user);
        errdefer freeSecret(gpa, user);
        const pass = try gpa.dupe(u8, cfg.pass);
        errdefer freeSecret(gpa, pass);
        const c = try gpa.create(Conn);
        errdefer gpa.destroy(c);
        c.* = .{ .gpa = gpa, .transport = transport, .user = user, .pass = pass, .secret_list = .{ user, pass }, .command_deadline = cfg.deadline };
        c.handshake(io) catch |err| {
            diag.* = c.diag;
            // The socket is closed; the memory goes with the errdefers.
            return err;
        };
        return c;
    }

    fn wipe(c: *Conn) void {
        freeSecret(c.gpa, c.user);
        freeSecret(c.gpa, c.pass);
    }

    fn freeSecret(gpa: Allocator, buf: []u8) void {
        std.crypto.secureZero(u8, buf);
        gpa.free(buf);
    }

    pub fn isDead(c: *const Conn) bool {
        return c.dead.load(.acquire);
    }

    /// Sends QUIT on a best-effort basis (only when the connection is still
    /// alive), closes it and frees the Conn.
    pub fn close(c: *Conn, io: Io) void {
        if (!c.dead.swap(true, .acq_rel)) {
            c.transport.begin(Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(quit_timeout_ms), .clock = .awake }));
            c.transport.writer.writeAll("QUIT\r\n") catch {};
            c.transport.writer.flush() catch {};
            c.transport.end();
        }
        c.transport.destroy(io);
        c.wipe();
        const gpa = c.gpa;
        gpa.destroy(c);
    }

    // ---- commands ----------------------------------------------------------

    /// Streams the body of the article with `id` (no angle brackets) to
    /// `sink`: dot-unstuffed once, no CR/LF terminator, each line valid only
    /// during the call.
    ///
    ///   InvalidMessageId  rejected before anything was sent.
    ///   ArticleNotFound   430, 411, 412, 420, 423; the connection stays usable.
    ///   Connection        400, 502, any other 4xx/5xx, an I/O error or timeout.
    ///   Protocol          a malformed or unexpected response, a line over
    ///                     the limit, a body over the limit.
    ///   Aborted           the Sink refused a line; the connection is dead
    ///                     because the rest of the body is still in flight.
    ///   Canceled          the task was cancelled mid-command; dead as well.
    pub fn body(c: *Conn, io: Io, id: []const u8, sink: types.Sink) BodyError!void {
        if (!proto.validMessageId(id)) return error.InvalidMessageId;
        try c.begin(io, .body);
        defer c.end();

        const w = c.transport.writer;
        w.writeAll("BODY <") catch |e| return c.ioFailure(io, .body, e);
        w.writeAll(id) catch |e| return c.ioFailure(io, .body, e);
        w.writeAll(">\r\n") catch |e| return c.ioFailure(io, .body, e);
        w.flush() catch |e| return c.ioFailure(io, .body, e);

        const resp = try c.readResponse(io, .body);
        switch (resp.code) {
            222 => {},
            430, 411, 412, 420, 423 => {
                c.diag.setResponse(.article_not_found, .body, resp.code, resp.text, c.secrets());
                return error.ArticleNotFound;
            },
            else => return c.refuse(io, .body, resp, unexpectedKind(resp.code)),
        }
        return c.readBody(io, sink);
    }

    fn readBody(c: *Conn, io: Io, sink: types.Sink) BodyError!void {
        var total: usize = 0;
        while (true) {
            const raw = c.transport.reader.takeDelimiterInclusive('\n') catch |e| return c.ioFailure(io, .body, e);
            c.transport.touch();
            total += raw.len;
            if (total > proto.max_body_size) return c.protocolFailure(io, .body, .body_too_large, "body too large");
            var line = raw[0 .. raw.len - 1];
            if (line.len > 0 and line[line.len - 1] == '\r') line = line[0 .. line.len - 1];
            if (line.len > proto.max_line_length) return c.protocolFailure(io, .body, .line_too_long, "line too long");
            if (line.len > 0 and line[0] == '.') {
                if (line.len == 1) return;
                line = line[1..]; // dot-unstuffing, once
            }
            sink.line(line) catch {
                c.kill(io);
                c.diag.set(.connection, .body, .none);
                return error.Aborted;
            };
        }
    }

    /// Asks the Server for its clock (DATE) as Unix seconds: a cheap health
    /// check. Any answer but 111 with a valid timestamp kills the connection.
    pub fn date(c: *Conn, io: Io) DateError!i64 {
        try c.begin(io, .date);
        defer c.end();
        const resp = try c.roundTrip(io, .date, "DATE");
        if (resp.code != 111) return c.refuse(io, .date, resp, unexpectedKind(resp.code));
        return proto.parseDate(resp.text) orelse c.refuse(io, .date, resp, .protocol);
    }

    // ---- the handshake ---------------------------------------------------------

    fn handshake(c: *Conn, io: Io) OpenError!void {
        errdefer c.transport.shutdown(io);
        try c.begin(io, .greeting);
        defer c.end();
        const greeting = c.readResponse(io, .greeting) catch |e| return e;
        if (greeting.code != 200 and greeting.code != 201) {
            return c.refuseOpen(io, .greeting, greeting, unexpectedKind(greeting.code));
        }
        if (c.user.len == 0) return;

        // A server may accept the user name alone (281) or ask for a password
        // (381). 481, 482 and 452 are terminal for this Server until its
        // config changes. 502 is deliberately not one of them: providers also
        // use it for "too many connections".
        var resp = try c.roundTripOpen(io, .auth, "AUTHINFO USER ", c.user);
        if (resp.code == 381) resp = try c.roundTripOpen(io, .auth, "AUTHINFO PASS ", c.pass);
        switch (resp.code) {
            281 => {},
            481, 482, 452 => return c.refuseOpen(io, .auth, resp, .auth),
            else => return c.refuseOpen(io, .auth, resp, unexpectedKind(resp.code)),
        }
    }

    fn roundTripOpen(c: *Conn, io: Io, op: diag_mod.Op, cmd: []const u8, arg: []const u8) OpenError!proto.Response {
        const w = c.transport.writer;
        w.writeAll(cmd) catch |e| return c.openErr(c.ioFailure(io, op, e));
        w.writeAll(arg) catch |e| return c.openErr(c.ioFailure(io, op, e));
        w.writeAll("\r\n") catch |e| return c.openErr(c.ioFailure(io, op, e));
        w.flush() catch |e| return c.openErr(c.ioFailure(io, op, e));
        return c.readResponse(io, op) catch |e| return c.openErr(e);
    }

    fn openErr(c: *Conn, e: anyerror) OpenError {
        _ = c;
        return switch (e) {
            error.Protocol => error.Protocol,
            error.Canceled => error.Canceled,
            else => error.Connection,
        };
    }

    // ---- plumbing -----------------------------------------------------------------

    fn secrets(c: *const Conn) []const []const u8 {
        return &c.secret_list;
    }

    /// Fails fast on a dead connection and arms the command deadline.
    fn begin(c: *Conn, io: Io, op: diag_mod.Op) error{Connection}!void {
        _ = io;
        if (c.isDead()) {
            c.diag.set(.connection, op, .conn_dead);
            return error.Connection;
        }
        c.transport.begin(c.command_deadline);
    }

    fn end(c: *Conn) void {
        c.transport.end();
    }

    /// Marks the connection dead and closes the socket without a QUIT.
    fn kill(c: *Conn, io: Io) void {
        c.dead.store(true, .release);
        c.transport.shutdown(io);
    }

    /// Turns a reader or writer error into the class and cause, and kills the
    /// connection.
    fn ioFailure(c: *Conn, io: Io, op: diag_mod.Op, err: anyerror) (error{ Connection, Protocol, Canceled }) {
        c.kill(io);
        switch (err) {
            error.StreamTooLong => {
                c.diag.set(.protocol, op, .line_too_long);
                c.diag.setText("line too long", &.{});
                return error.Protocol;
            },
            error.EndOfStream => {
                c.diag.set(.connection, op, .eof);
                return error.Connection;
            },
            else => switch (c.transport.failure()) {
                .canceled => {
                    c.diag.set(.connection, op, .canceled);
                    return error.Canceled;
                },
                .timeout => {
                    c.diag.set(.connection, op, .timeout);
                    return error.Connection;
                },
                else => {
                    c.diag.set(.connection, op, .io);
                    return error.Connection;
                },
            },
        }
    }

    fn protocolFailure(c: *Conn, io: Io, op: diag_mod.Op, cause: Cause, text: []const u8) error{Protocol} {
        c.kill(io);
        c.diag.set(.protocol, op, cause);
        c.diag.setText(text, &.{});
        return error.Protocol;
    }

    /// Reads one status line.
    fn readResponse(c: *Conn, io: Io, op: diag_mod.Op) error{ Connection, Protocol, Canceled }!proto.Response {
        const raw = c.transport.reader.takeDelimiterInclusive('\n') catch |e| return c.ioFailure(io, op, e);
        c.transport.touch();
        var line = raw[0 .. raw.len - 1];
        if (line.len > 0 and line[line.len - 1] == '\r') line = line[0 .. line.len - 1];
        if (line.len > proto.max_line_length) return c.protocolFailure(io, op, .line_too_long, "line too long");
        return proto.parseResponse(line) catch {
            c.kill(io);
            c.diag.set(.protocol, op, .malformed_response);
            // The line is sanitised (and redacted) as it is stored.
            var buf: [96]u8 = undefined;
            const text = std.fmt.bufPrint(&buf, "malformed response: {s}", .{line[0..@min(line.len, 60)]}) catch buf[0..0];
            c.diag.setText(text, c.secrets());
            return error.Protocol;
        };
    }

    fn roundTrip(c: *Conn, io: Io, op: diag_mod.Op, cmd: []const u8) error{ Connection, Protocol, Canceled }!proto.Response {
        c.transport.writer.writeAll(cmd) catch |e| return c.ioFailure(io, op, e);
        c.transport.writer.writeAll("\r\n") catch |e| return c.ioFailure(io, op, e);
        c.transport.writer.flush() catch |e| return c.ioFailure(io, op, e);
        return c.readResponse(io, op);
    }

    /// Kills the connection and reports an unexpected response. The server
    /// text is sanitised; credentials are never part of it.
    fn refuse(c: *Conn, io: Io, op: diag_mod.Op, resp: proto.Response, kind: diag_mod.Kind) error{ Connection, Protocol } {
        c.kill(io);
        c.diag.setResponse(kind, op, resp.code, resp.text, c.secrets());
        return if (kind == .protocol) error.Protocol else error.Connection;
    }

    fn refuseOpen(c: *Conn, io: Io, op: diag_mod.Op, resp: proto.Response, kind: diag_mod.Kind) OpenError {
        c.kill(io);
        c.diag.setResponse(kind, op, resp.code, resp.text, c.secrets());
        return switch (kind) {
            .auth => error.Auth,
            .protocol => error.Protocol,
            else => error.Connection,
        };
    }
};

/// 4xx and 5xx are connection-level refusals, anything else is a protocol
/// violation.
fn unexpectedKind(code: u16) diag_mod.Kind {
    return if (code >= 400) .connection else .protocol;
}

fn dialFailure(diag: *Diag, err: transport_mod.DialError) OpenError {
    const op: diag_mod.Op, const cause: Cause = switch (err) {
        error.Canceled => return error.Canceled,
        error.OutOfMemory => return error.OutOfMemory,
        error.DnsNotFound => .{ .dial, .dns_not_found },
        error.DnsOther => .{ .dial, .dns_other },
        error.TcpRefused => .{ .dial, .tcp_refused },
        error.TcpTimeout => .{ .dial, .tcp_timeout },
        error.TcpUnreachable => .{ .dial, .tcp_unreachable },
        error.TcpOther => .{ .dial, .tcp_other },
        error.TlsCertUntrusted => .{ .tls, .tls_cert_untrusted },
        error.TlsCertHostname => .{ .tls, .tls_cert_hostname },
        error.TlsCertExpired => .{ .tls, .tls_cert_expired },
        error.TlsCertInvalid => .{ .tls, .tls_cert_invalid },
        error.TlsNotTls => .{ .tls, .tls_not_tls },
        error.TlsTimeout => .{ .tls, .tls_timeout },
        error.TlsHandshake => .{ .tls, .tls_handshake },
    };
    diag.set(.connection, op, cause);
    return error.Connection;
}
