//! The HTTP server of the daemon: std.http.Server on std.Io, with our own
//! router, middleware and limits (ADR-0014, M15). Ports internal/api
//! (server.go, middleware.go) for the skeleton routes.
//!
//! Memory: one arena per request (reset when the request is done); one
//! read buffer and one write buffer per connection, owned by the connection
//! task; the Server itself is heap allocated by `init` and owned by the caller
//! until `deinit`. No pointer into a growing buffer is kept: the request head
//! is copied into the arena before the body is read.
//!
//! Threads: `serve` accepts on the calling task and runs every connection as a
//! task of an Io.Group. Cancelling the `serve` task shuts the server down:
//! stop accepting, finish requests in flight for up to `shutdown_grace_ms`,
//! then cancel the rest.
//!
//! Deliberate differences from Go's net/http, as observable from outside:
//! - the errors the server itself answers (400, 431, 501, 503, 417) have the
//!   API's JSON error body, not net/http's plain text;
//! - an unknown method is a 501, not a 405/404;
//! - only origin-form request targets ("/path?query") are accepted;
//! - a declared Content-Length above the route's limit is a 413 even when the
//!   handler would not have read the body (Go only fails when it reads it);
//! - the redirect for an unclean path or "/tree" has no HTML body;
//! - a request whose method cannot carry a body but that declares one is
//!   answered and then the connection is closed.
const std = @import("std");
const Io = std.Io;
const net = Io.net;
const Allocator = std.mem.Allocator;

const common = @import("http_common.zig");
const http_listen = @import("http_listen.zig");
const ex = @import("http_exchange.zig");
const respond = @import("respond.zig");
const head_check = @import("http_head.zig");
const redact = @import("http_redact.zig");
const api = @import("api.zig");

pub const Logger = common.Logger;
pub const Level = common.Level;
pub const Attr = common.Attr;
pub const KeyChecker = common.KeyChecker;
pub const SetupGate = common.SetupGate;
pub const Version = api.Version;
pub const Exchange = ex.Exchange;
pub const Route = ex.Route;
pub const Routes = ex.Routes;

/// Limits against hostile and broken clients. The defaults are Go's server
/// settings (HTTPServer in server.go) plus the connection cap.
pub const Limits = struct {
    /// Request line plus headers. Larger is a 431.
    max_header_bytes: usize = 64 << 10,
    /// Header lines. More is a 431.
    max_headers: usize = 128,
    /// From the first byte of a request to the end of its headers
    /// (ReadHeaderTimeout; also how long a new connection may stay silent).
    read_header_timeout_ms: u32 = 10_000,
    /// From the first byte of a request to the end of its body (ReadTimeout).
    read_timeout_ms: u32 = 60_000,
    /// To write one response (WriteTimeout).
    write_timeout_ms: u32 = 60_000,
    /// To write one piece of a streaming response (an event stream); replaces
    /// `write_timeout_ms` for streams, as streamWriteTimeout does in Go.
    stream_write_timeout_ms: u32 = 30_000,
    /// How long a kept-alive connection may wait for its next request.
    idle_timeout_ms: u32 = 120_000,
    /// Connections served at once. More are answered with a 503 and closed.
    max_connections: u32 = 256,
    /// How long requests in flight get after the serving task is cancelled.
    shutdown_grace_ms: u32 = 5_000,
};

pub const Options = struct {
    logger: Logger = .discard,
    /// Checks the app key of routes that need it.
    keys: KeyChecker,
    version: Version = .{},
    limits: Limits = .{},
    /// SO_REUSEADDR, as Go sets it, so a restart does not wait for sockets in
    /// TIME_WAIT. (Zig's option also sets SO_REUSEPORT on POSIX, which lets a
    /// second process of the same user share the port; the daemon is the only
    /// one that binds it.)
    reuse_address: bool = true,
    /// While first-run setup is open the server answers 503 setup_required to
    /// every request but GET /api/v1/health, before routing (null: no gate).
    setup_gate: ?SetupGate = null,
};

pub const ListenError = net.IpAddress.ListenError || error{ InvalidAddress, AlreadyListening };

/// How often an idle connection looks at the shutdown flag.
const idle_poll_ms = 250;

pub const Server = struct {
    gpa: Allocator,
    io: Io,
    options: Options,
    /// The routes. `init` registers the skeleton's; add more before `serve`.
    routes: Routes,
    api_state: api.Api,

    listener: ?net.Server = null,
    group: Io.Group = .init,
    stopping: std.atomic.Value(bool) = .init(false),
    mutex: Io.Mutex = .init,
    idle: Io.Condition = .init,
    /// Connection tasks running, guarded by `mutex`.
    active: u32 = 0,

    /// `gpa` must be thread-safe (the connection tasks allocate from it).
    pub fn init(gpa: Allocator, io: Io, options: Options) !*Server {
        const s = try gpa.create(Server);
        errdefer gpa.destroy(s);
        s.* = .{
            .gpa = gpa,
            .io = io,
            .options = options,
            .routes = .init(gpa),
            .api_state = .{ .version = options.version },
        };
        errdefer s.routes.deinit();
        try s.api_state.register(&s.routes);
        return s;
    }

    /// Only after `serve` has returned.
    pub fn deinit(s: *Server) void {
        if (s.listener) |*l| l.deinit(s.io);
        s.routes.deinit();
        s.gpa.destroy(s);
    }

    /// Binds "host:port" ("127.0.0.1:9494", ":9494" for all IPv4 interfaces,
    /// "[::1]:9494", port 0 for any free port) and returns the port bound.
    /// Host names other than "localhost" are not resolved: pass an IP address.
    pub fn listen(s: *Server, address: []const u8) ListenError!u16 {
        if (s.listener != null) return error.AlreadyListening;
        const addr = try parseAddress(address);
        s.listener = if (s.options.reuse_address)
            try http_listen.listen(s.io, addr, net.default_kernel_backlog)
        else
            try addr.listen(s.io, .{});
        return s.listener.?.socket.address.getPort();
    }

    /// Serves until the calling task is cancelled, then shuts down and returns.
    /// Start it with `io.concurrent` and stop it with `Future.cancel`.
    pub fn serve(s: *Server) error{NotListening}!void {
        const io = s.io;
        const listener = if (s.listener) |*l| l else return error.NotListening;
        const log = s.options.logger;
        var backoff_ms: u32 = 50;
        while (true) {
            const stream = listener.accept(io) catch |err| switch (err) {
                error.Canceled => break,
                // The listener was shut down under us.
                error.SocketNotListening => break,
                // Out of descriptors or memory, or a client that went away
                // before we got to it: back off and go on.
                else => {
                    log.write(.warn, "accept failed", &.{Attr.str("err", @errorName(err))});
                    io.sleep(.fromMilliseconds(backoff_ms), .awake) catch break;
                    backoff_ms = @min(backoff_ms * 2, 1000);
                    continue;
                },
            };
            backoff_ms = 50;
            s.admit(stream);
        }
        s.shutdown();
    }

    fn shutdown(s: *Server) void {
        const io = s.io;
        s.stopping.store(true, .release);
        const deadline = Io.Clock.Timestamp.now(io, .awake).addDuration(.{
            .raw = .fromMilliseconds(s.options.limits.shutdown_grace_ms),
            .clock = .awake,
        });
        s.mutex.lockUncancelable(io);
        while (s.active > 0) {
            s.idle.waitTimeout(io, &s.mutex, .{ .deadline = deadline }) catch break;
        }
        s.mutex.unlock(io);
        s.group.cancel(io);
    }

    fn admit(s: *Server, stream: net.Stream) void {
        const io = s.io;
        s.mutex.lockUncancelable(io);
        const room = s.active < s.options.limits.max_connections;
        if (room) s.active += 1;
        s.mutex.unlock(io);
        if (!room) {
            s.options.logger.write(.debug, "too many connections", &.{});
            rejectBusy(s, stream);
            return;
        }
        s.group.concurrent(io, serveConnection, .{ s, stream }) catch {
            s.release();
            rejectBusy(s, stream);
        };
    }

    fn release(s: *Server) void {
        const io = s.io;
        s.mutex.lockUncancelable(io);
        s.active -= 1;
        if (s.active == 0) s.idle.broadcast(io);
        s.mutex.unlock(io);
    }

    /// 503 and close, written without parsing anything: it is a few hundred
    /// bytes into a fresh socket buffer, so it cannot block.
    fn rejectBusy(s: *Server, stream: net.Stream) void {
        const io = s.io;
        defer stream.close(io);
        const body = "{\"error\":{\"code\":\"unavailable\",\"message\":\"too many connections\"}}\n";
        var buf: [512]u8 = undefined;
        var w = stream.writer(io, &buf);
        w.interface.print("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nCache-Control: no-store\r\n" ++
            "Retry-After: 1\r\nConnection: close\r\nContent-Length: {d}\r\n\r\n{s}", .{ body.len, body }) catch return;
        w.interface.flush() catch return;
    }
};

pub fn parseAddress(address: []const u8) ListenError!net.IpAddress {
    const colon = std.mem.findScalarLast(u8, address, ':') orelse return error.InvalidAddress;
    const port = std.fmt.parseInt(u16, address[colon + 1 ..], 10) catch return error.InvalidAddress;
    var host = address[0..colon];
    if (host.len >= 2 and host[0] == '[' and host[host.len - 1] == ']') host = host[1 .. host.len - 1];
    if (host.len == 0) host = "0.0.0.0";
    if (std.mem.eql(u8, host, "localhost")) host = "127.0.0.1";
    return net.IpAddress.parse(host, port) catch error.InvalidAddress;
}

// ---- timed I/O -------------------------------------------------------------

pub fn deadlineIn(io: Io, ms: u32) Io.Clock.Timestamp {
    return Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(ms), .clock = .awake });
}

fn expired(io: Io, deadline: Io.Clock.Timestamp) bool {
    return Io.Clock.Timestamp.now(io, .awake).raw.nanoseconds >= deadline.raw.nanoseconds;
}

/// One socket operation that gives up at `deadline` with error.Timeout.
fn timedOperate(io: Io, op: Io.Operation, deadline: Io.Clock.Timestamp) !Io.Operation.Result {
    var storage: [1]Io.Operation.Storage = undefined;
    var batch: Io.Batch = .init(&storage);
    defer batch.cancel(io);
    batch.addAt(0, op);
    try batch.awaitConcurrent(io, .{ .deadline = deadline });
    return batch.next().?.result;
}

/// A socket reader with a deadline, which net.Stream.Reader lacks. The
/// connection task moves `deadline` as the request advances (idle wait,
/// headers, body). A failed read reports error.ReadFailed with the cause
/// (error.Timeout, error.Canceled, a socket error) in `err`.
pub const TimedReader = struct {
    io: Io,
    stream: net.Stream,
    deadline: Io.Clock.Timestamp,
    err: ?anyerror = null,
    interface: Io.Reader,

    pub fn init(io: Io, stream: net.Stream, buffer: []u8) TimedReader {
        return .{
            .io = io,
            .stream = stream,
            .deadline = Io.Clock.Timestamp.now(io, .awake),
            .interface = .{
                .vtable = &.{ .stream = streamImpl, .readVec = readVec },
                .buffer = buffer,
                .seek = 0,
                .end = 0,
            },
        };
    }

    fn streamImpl(io_r: *Io.Reader, io_w: *Io.Writer, limit: Io.Limit) Io.Reader.StreamError!usize {
        const dest = limit.slice(try io_w.writableSliceGreedy(1));
        var data: [1][]u8 = .{dest};
        const n = try readVec(io_r, &data);
        io_w.advance(n);
        return n;
    }

    fn readVec(io_r: *Io.Reader, data: [][]u8) Io.Reader.Error!usize {
        const r: *TimedReader = @alignCast(@fieldParentPtr("interface", io_r));
        var iovecs: [8][]u8 = undefined;
        const dest_n, const data_size = try io_r.writableVector(&iovecs, data);
        const op: Io.Operation = .{ .net_read = .{ .socket_handle = r.stream.socket.handle, .data = iovecs[0..dest_n] } };
        const result = timedOperate(r.io, op, r.deadline) catch |err| {
            r.err = err;
            return error.ReadFailed;
        };
        const read = result.net_read catch |err| {
            r.err = err;
            return error.ReadFailed;
        };
        if (read.data_len == 0) return error.EndOfStream;
        if (read.data_len > data_size) {
            r.interface.end += read.data_len - data_size;
            return data_size;
        }
        return read.data_len;
    }
};

/// The writing counterpart: a response that the client does not read gives up
/// at `deadline`.
const TimedWriter = struct {
    io: Io,
    stream: net.Stream,
    deadline: Io.Clock.Timestamp,
    err: ?anyerror = null,
    interface: Io.Writer,

    fn init(io: Io, stream: net.Stream, buffer: []u8) TimedWriter {
        return .{
            .io = io,
            .stream = stream,
            .deadline = Io.Clock.Timestamp.now(io, .awake),
            .interface = .{ .vtable = &.{ .drain = drain }, .buffer = buffer },
        };
    }

    fn drain(io_w: *Io.Writer, data: []const []const u8, splat: usize) Io.Writer.Error!usize {
        const w: *TimedWriter = @alignCast(@fieldParentPtr("interface", io_w));
        const op: Io.Operation = .{ .net_write = .{
            .socket_handle = w.stream.socket.handle,
            .header = io_w.buffered(),
            .data = data,
            .splat = splat,
        } };
        const result = timedOperate(w.io, op, w.deadline) catch |err| {
            w.err = err;
            return error.WriteFailed;
        };
        const n = result.net_write catch |err| {
            w.err = err;
            return error.WriteFailed;
        };
        return io_w.consume(n);
    }
};

// ---- one connection --------------------------------------------------------

/// Everything a connection task owns. Created in place: the std server keeps
/// pointers to the reader and writer inside it.
const Conn = struct {
    server: *Server,
    reader: TimedReader,
    writer: TimedWriter,
    http: std.http.Server,
    remote_buf: [64]u8 = undefined,
    remote: []const u8 = "",
};

fn serveConnection(s: *Server, stream: net.Stream) void {
    defer s.release();
    defer stream.close(s.io);
    runConnection(s, stream) catch |err| switch (err) {
        error.OutOfMemory => s.options.logger.write(.warn, "connection dropped: out of memory", &.{}),
        error.Canceled => {},
    };
}

fn runConnection(s: *Server, stream: net.Stream) error{ OutOfMemory, Canceled }!void {
    const io = s.io;
    const rbuf = try s.gpa.alloc(u8, s.options.limits.max_header_bytes);
    defer s.gpa.free(rbuf);
    var wbuf: [4096]u8 = undefined;

    var conn: Conn = .{
        .server = s,
        .reader = .init(io, stream, rbuf),
        .writer = .init(io, stream, &wbuf),
        .http = undefined,
    };
    conn.http = .init(&conn.reader.interface, &conn.writer.interface);
    var fbs: Io.Writer = .fixed(&conn.remote_buf);
    stream.socket.address.format(&fbs) catch {};
    conn.remote = fbs.buffered();

    var first = true;
    while (try waitForRequest(&conn, first)) {
        first = false;
        var arena_state: std.heap.ArenaAllocator = .init(s.gpa);
        defer arena_state.deinit();
        const again = serveRequest(&conn, arena_state.allocator()) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.Canceled => return error.Canceled,
            // The peer went away or stalled; nothing more to say to it.
            error.ConnectionLost => false,
        };
        if (!again) {
            lingerClose(&conn, stream);
            return;
        }
    }
}

/// How long and how much a closing connection still reads (Go does the same:
/// closeWriteAndWait).
const linger_ms = 500;
const linger_max_bytes = 256 << 10;

/// Closing a socket that still has unread request bytes makes the kernel
/// reset the connection, which can destroy a response the client has not read
/// yet (a 413 for a body we stopped reading). So: send FIN, then discard what
/// arrives until the client closes, a short deadline or a size cap.
fn lingerClose(conn: *Conn, stream: net.Stream) void {
    const io = conn.server.io;
    if (conn.server.stopping.load(.acquire)) return;
    stream.shutdown(io, .send) catch return;
    conn.reader.deadline = Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(linger_ms), .clock = .awake });
    _ = conn.reader.interface.discard(.limited(linger_max_bytes)) catch {};
}

/// Waits for the first byte of the next request. A new connection gets the
/// header timeout, a kept-alive one the idle timeout; both look at the
/// shutdown flag every `idle_poll_ms`. False when the connection should end.
fn waitForRequest(c: *Conn, first: bool) error{Canceled}!bool {
    const s = c.server;
    const io = s.io;
    const limits = s.options.limits;
    const hard = deadlineIn(io, if (first) limits.read_header_timeout_ms else limits.idle_timeout_ms);
    while (true) {
        if (s.stopping.load(.acquire)) return false;
        var slice = deadlineIn(io, idle_poll_ms);
        if (hard.raw.nanoseconds < slice.raw.nanoseconds) slice = hard;
        c.reader.deadline = slice;
        c.reader.err = null;
        c.reader.interface.fill(1) catch |err| switch (err) {
            error.EndOfStream => return false,
            error.ReadFailed => {
                const cause = c.reader.err orelse return false;
                if (cause == error.Canceled) return error.Canceled;
                if (cause == error.Timeout and !expired(io, hard)) continue;
                return false;
            },
        };
        return true;
    }
}

const RequestError = error{ OutOfMemory, Canceled, ConnectionLost };

const request_id_header = "X-Request-ID";

/// Handles one request whose first byte has arrived. Returns whether the
/// connection can serve another.
fn serveRequest(c: *Conn, arena: Allocator) RequestError!bool {
    const s = c.server;
    const io = s.io;
    const limits = s.options.limits;
    const t0 = Io.Clock.Timestamp.now(io, .awake);
    const header_deadline = t0.addDuration(.{ .raw = .fromMilliseconds(limits.read_header_timeout_ms), .clock = .awake });
    const request_deadline = t0.addDuration(.{ .raw = .fromMilliseconds(limits.read_timeout_ms), .clock = .awake });

    c.reader.deadline = header_deadline;
    c.reader.err = null;
    const head_buffer = c.http.reader.receiveHead() catch |err| switch (err) {
        error.HttpHeadersOversize => {
            try rejectEarly(c, arena, .request_header_fields_too_large, "request_header_too_large", "the request header is too large");
            return false;
        },
        error.HttpRequestTruncated, error.HttpConnectionClosing => return false,
        error.ReadFailed => {
            if (c.reader.err) |cause| {
                if (cause == error.Canceled) return error.Canceled;
                if (cause == error.Timeout) s.options.logger.write(.debug, "request header timed out", &.{Attr.str("remote", c.remote)});
            }
            return false;
        },
    };
    // From here the head lives in the arena; the read buffer may be reused.
    const head_copy = try arena.dupe(u8, head_buffer);
    c.reader.deadline = request_deadline;

    const info = head_check.check(head_copy, limits.max_headers) catch |err| {
        switch (err) {
            error.TooManyHeaders => try rejectEarly(c, arena, .request_header_fields_too_large, "request_header_too_large", "the request has too many headers"),
            error.MalformedHead, error.BadTarget => try rejectEarly(c, arena, .bad_request, "bad_request", "malformed request"),
        }
        return false;
    };
    const std_head = std.http.Server.Request.Head.parse(head_copy) catch |err| {
        switch (err) {
            error.UnknownHttpMethod, error.HttpTransferEncodingUnsupported, error.CompressionUnsupported => try rejectEarly(c, arena, .not_implemented, "not_implemented", "not implemented"),
            else => try rejectEarly(c, arena, .bad_request, "bad_request", "malformed request"),
        }
        return false;
    };
    // HTTP/1.1 requires exactly one Host (Go answers 400 otherwise).
    if (std_head.version == .@"HTTP/1.1" and countHeader(head_copy, "Host") != 1) {
        try rejectEarly(c, arena, .bad_request, "bad_request", "exactly one Host header is required");
        return false;
    }
    // Ambiguous framing is how requests get smuggled past a proxy.
    if (std_head.content_length != null and std_head.transfer_encoding != .none) {
        try rejectEarly(c, arena, .bad_request, "bad_request", "both Content-Length and Transfer-Encoding");
        return false;
    }
    if (std_head.expect) |e| if (!std.ascii.eqlIgnoreCase(e, "100-continue")) {
        try rejectEarly(c, arena, .expectation_failed, "expectation_failed", "unsupported Expect");
        return false;
    };

    var request: std.http.Server.Request = .{ .server = &c.http, .head = std_head, .head_buffer = head_copy };
    const started = Io.Clock.Timestamp.now(io, .awake);

    var e: Exchange = .{
        .arena = arena,
        .io = io,
        .method = std_head.method,
        .path = info.path,
        .query = info.query,
        .params = &.{},
        .head = head_copy,
        .remote = c.remote,
        .request_id = "",
        .content_length = std_head.content_length,
        .body_limit = respond.small_body_limit,
        .request = &request,
    };
    var stream: StreamCtx = .{ .c = c, .chunked = std_head.version == .@"HTTP/1.1", .version = std_head.version };
    e.streamer = stream.streamer();
    e.request_id = try requestId(&e);
    try applyBaseHeaders(&e.res, arena, e.request_id);

    // Ahead of the routes: the declared body must be one we can account for.
    var force_close = false;
    const declares_body = (std_head.content_length orelse 0) > 0 or std_head.transfer_encoding != .none;
    if (declares_body and !std_head.method.requestHasBody()) force_close = true;

    try dispatch(s, &e);

    // A stream wrote its own head and body; the connection is spent.
    if (e.streaming) {
        try stream.finish();
        logRequest(c, &e, started.durationTo(Io.Clock.Timestamp.now(io, .awake)));
        return false;
    }

    // The body is owed to the framing: whatever the handler did not read must
    // be discarded (bounded by the route's limit) or the connection closed.
    var keep = std_head.keep_alive and !force_close and !s.stopping.load(.acquire);
    if (!e.body_taken and declares_body) {
        if (std_head.transfer_encoding != .none) keep = false;
        if (std_head.content_length) |n| if (n > e.body_limit) {
            keep = false;
        };
    }
    // An unfinished body (limit hit, client stalled) leaves the stream
    // mid-message.
    if (e.body_taken and c.http.reader.state != .ready) keep = false;
    // We never promised the client a 100 Continue for a body we did not read.
    if (!e.body_taken) request.head.expect = null;

    try writeResponse(c, arena, &request, &e.res, keep);

    const finished = Io.Clock.Timestamp.now(io, .awake);
    logRequest(c, &e, started.durationTo(finished));

    return keep and c.http.reader.state == .ready;
}

const setup_required_message = "first-run setup is required: create the admin account with the setup token from the server log";

/// Standalone page for a path the web UI does not have. Fixed text, so it
/// needs no escaping. It must not contain the API error code `not_found`.
const page_not_found_html = "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>Page not found</title></head><body><h1>Page not found</h1></body></html>";

/// What the gate lets through while setup is open: GET /api/v1/health, GET
/// and HEAD / (the front door, which redirects to /setup), GET and HEAD
/// /login (which redirects to /setup while setup is open), and GET, HEAD and
/// POST /setup. Static files join it with the rest of the web UI.
/// Exact paths only, so unclean forms stay gated. /api is not exempt.
fn setupExempt(e: *const Exchange) bool {
    if (e.method == .GET and std.mem.eql(u8, e.path, "/api/v1/health")) return true;
    // Exact path only. Unclean forms ("//", "/./", "/login/", "/setup/") stay gated, as in Go.
    if (std.mem.eql(u8, e.path, "/") or std.mem.eql(u8, e.path, "/login")) return e.method == .GET or e.method == .HEAD;
    if (std.mem.eql(u8, e.path, "/setup")) return e.method == .GET or e.method == .HEAD or e.method == .POST;
    return false;
}

/// `/api` and `/sabnzbd` keep the JSON error. Every other miss is an HTML page.
fn notFound(e: *Exchange) Allocator.Error!void {
    if (jsonNotFound(e.path)) return e.res.fail(e.arena, .not_found, "not_found", "no such route");
    e.res.status = .not_found;
    e.res.body = page_not_found_html;
    try e.res.setHeader(e.arena, "Content-Type", "text/html; charset=utf-8");
    try e.res.setHeader(e.arena, "Cache-Control", "no-store");
}

fn jsonNotFound(path: []const u8) bool {
    if (std.mem.eql(u8, path, "/api") or std.mem.startsWith(u8, path, "/api/")) return true;
    if (std.mem.eql(u8, path, "/sabnzbd") or std.mem.startsWith(u8, path, "/sabnzbd/")) return true;
    return false;
}

/// Routes the request and runs the handler, leaving the response in `e.res`.
fn dispatch(s: *Server, e: *Exchange) RequestError!void {
    const arena = e.arena;
    if (s.options.setup_gate) |gate| if (gate.required(gate.ctx) and !setupExempt(e)) {
        try e.res.fail(arena, .service_unavailable, "setup_required", setup_required_message);
        return;
    };
    const m = try s.routes.lookup(arena, e.method, e.path);
    switch (m) {
        .bad_path => try e.res.fail(arena, .bad_request, "bad_request", "invalid request path"),
        .not_found => try notFound(e),
        .method_not_allowed => |allow| {
            try e.res.setHeader(arena, "Allow", allow);
            try e.res.fail(arena, .method_not_allowed, "method_not_allowed", "this method is not allowed for this path");
        },
        .redirect => |loc| {
            const location = if (e.query.len > 0) try std.mem.concat(arena, u8, &.{ loc, "?", e.query }) else loc;
            e.res.status = .temporary_redirect;
            try e.res.setHeader(arena, "Location", location);
        },
        .found => |f| {
            e.params = f.params;
            e.body_limit = f.value.body_limit;
            if (f.value.auth == .key and !api.authorized(s.options.keys, e.header("Authorization"), e.header("X-Api-Key"))) {
                try e.res.setHeader(arena, "WWW-Authenticate", "Bearer");
                try e.res.fail(arena, .unauthorized, "unauthorized", "a valid API key is required");
                return;
            }
            // A declared body above the limit is refused when the handler reads
            // it (Exchange.bodyReader), as http.MaxBytesReader does in Go: a
            // handler that never reads one, like health, still answers.
            f.value.handler(f.value.state, e) catch |err| switch (err) {
                error.Canceled => return error.Canceled,
                error.OutOfMemory => return error.OutOfMemory,
                else => return handlerFailed(s, e, err),
            };
        },
    }
}

/// Turns an error of a handler into its response (generic, no detail).
fn handlerFailed(s: *Server, e: *Exchange, err: anyerror) RequestError!void {
    const arena = e.arena;
    // The head of a stream is out: there is no response left to replace. A
    // stream that ends badly just ends.
    if (e.streaming) {
        if (err != error.StreamClosed) s.options.logger.write(.warn, "stream failed", &.{
            Attr.str("request_id", e.request_id),
            Attr.str("err", @errorName(err)),
        });
        return;
    }
    e.res = .{};
    try applyBaseHeaders(&e.res, arena, e.request_id);
    switch (err) {
        error.PayloadTooLarge => try e.res.fail(arena, .payload_too_large, "request_too_large", "request body is too large"),
        // The client stalled, hung up or sent a broken chunked body.
        error.ReadFailed => try e.res.fail(arena, .bad_request, "bad_request", "the request body could not be read"),
        else => {
            s.options.logger.write(.err, "request failed", &.{
                Attr.str("request_id", e.request_id),
                Attr.str("err", @errorName(err)),
            });
            try e.res.fail(arena, .internal_server_error, "internal", "internal error");
        },
    }
}

fn countHeader(head: []const u8, name: []const u8) usize {
    var n: usize = 0;
    var it = std.http.HeaderIterator.init(head);
    while (it.next()) |h| {
        if (it.is_trailer) break;
        if (std.ascii.eqlIgnoreCase(h.name, name)) n += 1;
    }
    return n;
}

/// X-Request-ID: the client's when it is safe to echo and log (1 to 64 of
/// [A-Za-z0-9._-]), otherwise 32 random hex characters.
fn requestId(e: *Exchange) Allocator.Error![]const u8 {
    if (e.header(request_id_header)) |id| if (validRequestId(id)) return id;
    var raw: [16]u8 = undefined;
    e.io.random(&raw);
    return std.fmt.allocPrint(e.arena, "{x}", .{&raw});
}

fn validRequestId(id: []const u8) bool {
    if (id.len == 0 or id.len > 64) return false;
    for (id) |c| switch (c) {
        'a'...'z', 'A'...'Z', '0'...'9', '.', '_', '-' => {},
        else => return false,
    };
    return true;
}

/// The headers every response carries (securityHeaders in middleware.go), set
/// before the handler runs so it can override them.
fn applyBaseHeaders(res: *respond.Response, arena: Allocator, request_id: []const u8) Allocator.Error!void {
    try res.setHeader(arena, request_id_header, request_id);
    try res.setHeader(arena, "Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'");
    try res.setHeader(arena, "X-Content-Type-Options", "nosniff");
    try res.setHeader(arena, "Referrer-Policy", "same-origin");
}

/// The streaming half of a connection (Exchange.startStream). It lives on the
/// stack of `serveRequest`, next to the Exchange that points to it.
const StreamCtx = struct {
    c: *Conn,
    version: std.http.Version,
    /// HTTP/1.1 frames the body in chunks; 1.0 has no chunking and ends the
    /// body by closing the connection.
    chunked: bool,
    failed: bool = false,

    fn streamer(sc: *StreamCtx) ex.Streamer {
        return .{ .ctx = sc, .begin = begin, .write = write, .state = state };
    }

    fn begin(ctx: *anyopaque, res: *const respond.Response) ex.StreamError!void {
        const sc: *StreamCtx = @ptrCast(@alignCast(ctx));
        const io = sc.c.server.io;
        var date_buf: [29]u8 = undefined;
        const date = respond.httpDate(&date_buf, Io.Clock.real.now(io).toSeconds());
        sc.arm();
        const out = &sc.c.writer.interface;
        sc.writeHead(out, res, date) catch return sc.fail();
    }

    fn writeHead(sc: *StreamCtx, out: *Io.Writer, res: *const respond.Response, date: []const u8) Io.Writer.Error!void {
        try out.print("{s} {d} {s}\r\n", .{ @tagName(sc.version), @backingInt(res.status), res.status.phrase() orelse "" });
        for (res.headers.items) |h| try out.print("{s}: {s}\r\n", .{ h.name, h.value });
        try out.print("Date: {s}\r\nConnection: close\r\n", .{date});
        if (sc.chunked) try out.writeAll("Transfer-Encoding: chunked\r\n");
        try out.writeAll("\r\n");
        try out.flush();
    }

    fn write(ctx: *anyopaque, bytes: []const u8) ex.StreamError!void {
        const sc: *StreamCtx = @ptrCast(@alignCast(ctx));
        if (sc.failed) return error.StreamClosed;
        if (bytes.len == 0) return;
        sc.arm();
        const out = &sc.c.writer.interface;
        sc.writeChunk(out, bytes) catch return sc.fail();
    }

    /// A write on a blocking socket that was reported writable still blocks
    /// until all of it is taken, and the write deadline is only looked at
    /// between writes. So a payload goes out in pieces the socket can take
    /// at once, and a client that stopped reading is cut at the deadline
    /// instead of holding the handler until it reads.
    const piece_len = 2048;

    fn writeChunk(sc: *StreamCtx, out: *Io.Writer, bytes: []const u8) Io.Writer.Error!void {
        if (sc.chunked) try out.print("{x}\r\n", .{bytes.len});
        var rest = bytes;
        while (rest.len > 0) {
            const n = @min(rest.len, piece_len);
            try out.writeAll(rest[0..n]);
            try out.flush();
            rest = rest[n..];
        }
        if (sc.chunked) try out.writeAll("\r\n");
        try out.flush();
    }

    /// The stream is over for good once a write failed or timed out.
    fn fail(sc: *StreamCtx) ex.StreamError {
        sc.failed = true;
        if (sc.c.writer.err) |cause| if (cause == error.Canceled) return error.Canceled;
        return error.StreamClosed;
    }

    fn arm(sc: *StreamCtx) void {
        sc.c.writer.deadline = deadlineIn(sc.c.server.io, sc.c.server.options.limits.stream_write_timeout_ms);
        sc.c.writer.err = null;
    }

    /// Looks at the shutdown flag, then at the socket: nothing is expected
    /// from the client, so anything that arrives is thrown away, and the end
    /// of the stream or a reset means it left.
    fn state(ctx: *anyopaque) ex.StreamError!ex.StreamState {
        const sc: *StreamCtx = @ptrCast(@alignCast(ctx));
        const c = sc.c;
        if (c.server.stopping.load(.acquire)) return .stopping;
        if (sc.failed) return .peer_gone;
        c.reader.interface.tossBuffered();
        c.reader.deadline = deadlineIn(c.server.io, 1);
        c.reader.err = null;
        c.reader.interface.fill(1) catch |err| switch (err) {
            error.EndOfStream => return .peer_gone,
            error.ReadFailed => {
                const cause = c.reader.err orelse return .peer_gone;
                if (cause == error.Canceled) return error.Canceled;
                return if (cause == error.Timeout) .open else .peer_gone;
            },
        };
        c.reader.interface.tossBuffered();
        return .open;
    }

    /// Ends the body (the last chunk) if the stream is still healthy.
    fn finish(sc: *StreamCtx) error{Canceled}!void {
        if (sc.failed or !sc.chunked) return;
        sc.arm();
        const out = &sc.c.writer.interface;
        out.writeAll("0\r\n\r\n") catch {
            return if (sc.fail() == error.Canceled) error.Canceled else {};
        };
        out.flush() catch {
            return if (sc.fail() == error.Canceled) error.Canceled else {};
        };
    }
};

fn writeResponse(c: *Conn, arena: Allocator, request: *std.http.Server.Request, res: *const respond.Response, keep: bool) RequestError!void {
    const io = c.server.io;
    var date_buf: [29]u8 = undefined;
    const date = respond.httpDate(&date_buf, Io.Clock.real.now(io).toSeconds());
    const headers = try arena.alloc(std.http.Header, res.headers.items.len + 1);
    @memcpy(headers[0..res.headers.items.len], res.headers.items);
    headers[res.headers.items.len] = .{ .name = "Date", .value = date };

    c.writer.deadline = deadlineIn(io, c.server.options.limits.write_timeout_ms);
    c.writer.err = null;
    request.respond(res.body, .{ .version = request.head.version, .status = res.status, .extra_headers = headers, .keep_alive = keep }) catch {
        if (c.writer.err) |cause| if (cause == error.Canceled) return error.Canceled;
        return error.ConnectionLost;
    };
}

/// Answers a request that never became an Exchange (oversized or malformed
/// head) and tells the client the connection is over.
fn rejectEarly(c: *Conn, arena: Allocator, status: std.http.Status, code: []const u8, message: []const u8) RequestError!void {
    const io = c.server.io;
    var res: respond.Response = .{};
    var raw: [16]u8 = undefined;
    io.random(&raw);
    try applyBaseHeaders(&res, arena, try std.fmt.allocPrint(arena, "{x}", .{&raw}));
    try res.fail(arena, status, code, message);
    try res.setHeader(arena, "Connection", "close");
    try res.setHeader(arena, "Content-Length", try std.fmt.allocPrint(arena, "{d}", .{res.body.len}));
    var date_buf: [29]u8 = undefined;
    try res.setHeader(arena, "Date", respond.httpDate(&date_buf, Io.Clock.real.now(io).toSeconds()));

    c.server.options.logger.write(.debug, "request rejected", &.{
        Attr.str("remote", c.remote),
        Attr.int("status", @backingInt(status)),
        Attr.str("code", code),
    });

    c.writer.deadline = deadlineIn(io, c.server.options.limits.write_timeout_ms);
    c.writer.err = null;
    const out = &c.writer.interface;
    writeEarlyHead(out, &res) catch {
        if (c.writer.err) |cause| if (cause == error.Canceled) return error.Canceled;
        return;
    };
}

fn writeEarlyHead(out: *Io.Writer, res: *const respond.Response) Io.Writer.Error!void {
    try out.print("HTTP/1.1 {d} {s}\r\n", .{ @backingInt(res.status), res.status.phrase() orelse "" });
    for (res.headers.items) |h| try out.print("{s}: {s}\r\n", .{ h.name, h.value });
    try out.writeAll("\r\n");
    try out.writeAll(res.body);
    try out.flush();
}

/// The access log (accessLog in middleware.go): one line per request, health
/// checks at debug level so they stay out of the default log.
fn logRequest(c: *Conn, e: *const Exchange, took: Io.Clock.Duration) void {
    const s = c.server;
    const status: i64 = @backingInt(e.res.status);
    const level: Level = if (std.mem.eql(u8, e.path, "/api/v1/health") and e.res.status == .ok) .debug else .info;
    const target = redact.loggedTarget(e.arena, e.path, e.query) catch "[unloggable]";
    s.options.logger.write(level, "request", &.{
        Attr.str("request_id", e.request_id),
        Attr.str("method", @tagName(e.method)),
        Attr.str("url", target),
        Attr.int("status", status),
        Attr.int("bytes", @intCast(e.res.body.len + e.stream_bytes)),
        Attr.duration("duration", @intCast(@max(took.raw.nanoseconds, 0))),
        Attr.str("remote", e.remote),
    });
}

test {
    _ = @import("http_server_test.zig");
}
