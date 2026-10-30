//! What a handler sees of one request, and the route table entry that names
//! the handler. The server fills an Exchange, runs the handler and writes
//! `res`; everything in it lives in the request's arena.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const router = @import("router.zig");
const respond = @import("respond.zig");

pub const Method = std.http.Method;

/// A handler fills `ex.res`. Returning an error turns into a generic 500 (and
/// a log line with the request ID), except `error.PayloadTooLarge`, which is
/// the 413 of the API, and `error.ReadFailed` (a body that could not be read),
/// which is a 400.
pub const Handler = *const fn (state: ?*anyopaque, ex: *Exchange) anyerror!void;

pub const Auth = enum {
    /// Anyone (health).
    none,
    /// The app key, as Bearer or X-Api-Key (auth/appkey.go KeyOr); a request
    /// without a valid key never reaches the handler.
    key,
};

pub const Route = struct {
    handler: Handler,
    state: ?*anyopaque = null,
    auth: Auth = .key,
    /// Largest request body the route accepts (Go's maxBody). Reading a body
    /// above it (declared or chunked) fails with PayloadTooLarge, which the
    /// server answers with 413; a handler that never reads the body is not
    /// affected.
    body_limit: u64 = respond.small_body_limit,
};

pub const Routes = router.Router(Route);

pub const BodyError = error{
    /// The body is larger than the route's limit.
    PayloadTooLarge,
    /// The client went away, timed out, or sent a broken chunked body.
    ReadFailed,
    /// The body was already taken.
    BodyTaken,
    OutOfMemory,
};

pub const StreamError = error{
    /// The server is shutting down hard (the task was cancelled); propagate it.
    Canceled,
    /// The client went away, or did not take a write within the stream write
    /// timeout. The stream is over; the connection closes.
    StreamClosed,
};

/// What a long-lived response should do next.
pub const StreamState = enum {
    open,
    /// The client closed its side of the connection.
    peer_gone,
    /// The server is shutting down: finish the stream.
    stopping,
};

/// The server's side of a streaming response. The server fills it in when it
/// builds an Exchange; tests of handlers may leave it null.
pub const Streamer = struct {
    ctx: *anyopaque,
    begin: *const fn (ctx: *anyopaque, res: *const respond.Response) StreamError!void,
    write: *const fn (ctx: *anyopaque, bytes: []const u8) StreamError!void,
    state: *const fn (ctx: *anyopaque) StreamError!StreamState,
};

pub const Exchange = struct {
    /// One arena per request, reset by the server when the request is done.
    arena: Allocator,
    io: Io,
    method: Method,
    /// The request path as received (still percent-encoded) and the query
    /// without its '?'.
    path: []const u8,
    query: []const u8,
    /// Wildcards of the matched pattern, percent-decoded.
    params: []const router.Param,
    /// Copy of the request head, safe to use while the body is read.
    head: []const u8,
    /// "ip:port" of the peer.
    remote: []const u8,
    request_id: []const u8,
    content_length: ?u64,
    body_limit: u64,
    res: respond.Response = .{},

    request: *std.http.Server.Request,
    body_taken: bool = false,

    streamer: ?Streamer = null,
    /// Set by `startStream`: the head is out, `res` is not written again and
    /// the connection closes when the handler returns.
    streaming: bool = false,
    stream_bytes: u64 = 0,

    /// The first value of header `name`, or null.
    pub fn header(ex: *const Exchange, name: []const u8) ?[]const u8 {
        var it = std.http.HeaderIterator.init(ex.head);
        while (it.next()) |h| {
            if (it.is_trailer) break;
            if (std.ascii.eqlIgnoreCase(h.name, name)) return h.value;
        }
        return null;
    }

    /// Wildcard `name` of the matched pattern.
    pub fn param(ex: *const Exchange, name: []const u8) ?[]const u8 {
        for (ex.params) |p| if (std.mem.eql(u8, p.name, name)) return p.value;
        return null;
    }

    /// Reads the whole body into the arena, up to the route's limit. A handler
    /// that streams its body instead (uploads) uses `bodyReader` and must stop
    /// at `body_limit` itself.
    pub fn bodyAlloc(ex: *Exchange) BodyError![]u8 {
        const r = try ex.bodyReader(try ex.arena.alloc(u8, 4096));
        return r.allocRemaining(ex.arena, .limited(@intCast(ex.body_limit))) catch |err| switch (err) {
            error.StreamTooLong => error.PayloadTooLarge,
            error.ReadFailed => error.ReadFailed,
            error.OutOfMemory => error.OutOfMemory,
        };
    }

    /// The raw request body (after the 100-continue handshake, if asked for).
    /// Fails with PayloadTooLarge when the declared length is above the limit.
    pub fn bodyReader(ex: *Exchange, buffer: []u8) BodyError!*Io.Reader {
        if (ex.body_taken) return error.BodyTaken;
        if (ex.content_length) |n| if (n > ex.body_limit) return error.PayloadTooLarge;
        ex.body_taken = true;
        return ex.request.readerExpectContinue(buffer) catch error.ReadFailed;
    }

    /// Switches to a streaming response: sends the status and headers of
    /// `res` now (chunked on HTTP/1.1, close-delimited on 1.0) and leaves the
    /// body to `streamWrite`. Instead of the server's whole-response write
    /// timeout, every write gets its own (Limits.stream_write_timeout_ms), so
    /// a client that stops reading ends the stream rather than holding it. The
    /// connection closes afterwards. Do not read the request body after this.
    pub fn startStream(ex: *Exchange) StreamError!void {
        const st = ex.streamer orelse return error.StreamClosed;
        std.debug.assert(!ex.streaming);
        ex.streaming = true;
        try st.begin(st.ctx, &ex.res);
    }

    /// Writes `bytes` to the stream and flushes them to the client.
    pub fn streamWrite(ex: *Exchange, bytes: []const u8) StreamError!void {
        const st = ex.streamer orelse return error.StreamClosed;
        std.debug.assert(ex.streaming);
        try st.write(st.ctx, bytes);
        ex.stream_bytes += bytes.len;
    }

    /// Whether the stream may go on. Cheap enough to call every few hundred
    /// milliseconds; a handler that waits calls it between waits.
    pub fn streamState(ex: *Exchange) StreamError!StreamState {
        const st = ex.streamer orelse return error.StreamClosed;
        return st.state(st.ctx);
    }
};
