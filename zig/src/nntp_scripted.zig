//! Test support: a loopback NNTP server driven by a handler function, one
//! call per accepted connection (Go: fakeserver_test.go). `nntptest.zig`'s
//! article server is a handler on top of it, and the tests of the client use
//! it directly to play out odd conversations byte by byte.
//!
//! Plain TCP, or TLS when a server context is given. Not for production: the
//! only plain-text path to a Dialer lives here.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const net = Io.net;
const transport = @import("nntp_transport.zig");
const tls = @import("tls.zig");

/// The name every test Config dials; the test Dialer ignores it and connects
/// to the loopback server, but a TLS Dialer checks the certificate against it
/// (the test certificates are valid for "localhost").
pub const test_host = "localhost";

pub const Handler = *const fn (ctx: ?*anyopaque, s: *Session) void;

pub const Options = struct {
    handler: Handler,
    ctx: ?*anyopaque = null,
    /// Serve TLS with this context instead of plain text.
    tls_context: ?tls.ServerContext = null,
};

/// One accepted client connection.
pub const Session = struct {
    script: *Script,
    reader: *Io.Reader,
    writer: *Io.Writer,
    stream: net.Stream,
    /// The number of this connection, from 0 in accept order.
    num: usize,

    /// Sends a status or data line plus CRLF.
    pub fn line(s: *Session, l: []const u8) void {
        s.writer.writeAll(l) catch return;
        s.writer.writeAll("\r\n") catch return;
        s.writer.flush() catch {};
    }

    /// Sends raw bytes as they are.
    pub fn send(s: *Session, raw: []const u8) void {
        s.writer.writeAll(raw) catch return;
        s.writer.flush() catch {};
    }

    /// Reads one command line (without CRLF) and records it. null on EOF or
    /// an error. The slice is valid until the next read.
    pub fn readCmd(s: *Session) ?[]const u8 {
        const raw = s.reader.takeDelimiterInclusive('\n') catch return null;
        var l = raw[0 .. raw.len - 1];
        if (l.len > 0 and l[l.len - 1] == '\r') l = l[0 .. l.len - 1];
        s.script.record(l);
        return l;
    }

    /// Reads one command and notes a failure if it differs from `want`.
    pub fn expect(s: *Session, want: []const u8) void {
        const got = s.readCmd() orelse {
            s.script.noteFailure();
            return;
        };
        if (!std.mem.eql(u8, got, want)) s.script.noteFailure();
    }

    /// Blocks until the client hangs up or the test ends.
    pub fn stall(s: *Session) void {
        var buf: [256]u8 = undefined;
        while (true) {
            const n = s.reader.readSliceShort(&buf) catch return;
            if (n == 0) return;
        }
    }

    /// Drops the connection abruptly.
    pub fn close(s: *Session) void {
        s.stream.close(s.script.io);
        s.script.closedByHandler(s.num);
    }
};

pub const Script = struct {
    gpa: Allocator,
    io: Io,
    options: Options,
    listener: net.Server,
    accept_future: ?Io.Future(void) = null,
    group: Io.Group = .init,
    mutex: Io.Mutex = .init,
    idle: Io.Condition = .init,
    active: usize = 0,
    accepted: usize = 0,
    failures: usize = 0,
    commands: std.ArrayList([]u8) = .empty,
    /// Connections whose handler closed the socket itself.
    handler_closed: std.ArrayList(usize) = .empty,

    pub fn start(gpa: Allocator, io: Io, options: Options) !*Script {
        const self = try gpa.create(Script);
        errdefer gpa.destroy(self);
        const any: net.IpAddress = try .parse("127.0.0.1", 0);
        self.* = .{ .gpa = gpa, .io = io, .options = options, .listener = try any.listen(io, .{}) };
        errdefer self.listener.deinit(io);
        self.accept_future = try io.concurrent(acceptLoop, .{self});
        return self;
    }

    /// Stops accepting, cancels the sessions still running and frees
    /// everything.
    pub fn stop(self: *Script) void {
        const io = self.io;
        if (self.accept_future) |*f| _ = f.cancel(io);
        self.group.cancel(io);
        self.listener.deinit(io);
        for (self.commands.items) |c| self.gpa.free(c);
        self.commands.deinit(self.gpa);
        self.handler_closed.deinit(self.gpa);
        self.gpa.destroy(self);
    }

    pub fn port(self: *const Script) u16 {
        return self.listener.socket.address.getPort();
    }

    /// Waits until every handler has returned, at most `timeout_ms`.
    pub fn waitSessions(self: *Script, timeout_ms: u32) void {
        const io = self.io;
        const deadline = Io.Clock.Timestamp.now(io, .awake).addDuration(.{ .raw = .fromMilliseconds(timeout_ms), .clock = .awake });
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        while (self.active > 0) {
            self.idle.waitTimeout(io, &self.mutex, .{ .deadline = deadline }) catch return;
        }
    }

    /// Every command line read so far, in order, after waiting for the
    /// sessions to end. The strings live until `stop`.
    pub fn received(self: *Script) []const []const u8 {
        self.waitSessions(5000);
        return self.receivedNoWait();
    }

    pub fn receivedNoWait(self: *Script) []const []const u8 {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        return @ptrCast(self.commands.items);
    }

    pub fn failureCount(self: *Script) usize {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        return self.failures;
    }

    pub fn acceptedCount(self: *Script) usize {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        return self.accepted;
    }

    /// The test Dialer: connects to this server whatever host is asked for.
    /// Plain text (or the TLS of `tls_dialer` when the server is a TLS one is
    /// the caller's choice; see nntp_tls.zig).
    pub fn dialer(self: *Script) transport.Dialer {
        return .{ .ctx = self, .dial_fn = dialPlain };
    }

    fn dialPlain(ctx: *anyopaque, gpa: Allocator, io: Io, req: transport.DialRequest) transport.DialError!transport.Transport {
        const self: *Script = @ptrCast(@alignCast(ctx));
        const stream = try transport.connectTcp(io, "127.0.0.1", self.port(), req.connect_timeout_ms);
        return transport.TimedStream.create(gpa, io, stream, req.read_timeout_ms) catch error.OutOfMemory;
    }

    // ---- internals ----------------------------------------------------------

    fn record(self: *Script, l: []const u8) void {
        const io = self.io;
        const copy = self.gpa.dupe(u8, l) catch return;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        self.commands.append(self.gpa, copy) catch self.gpa.free(copy);
    }

    fn noteFailure(self: *Script) void {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        self.failures += 1;
    }

    fn closedByHandler(self: *Script, num: usize) void {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        self.handler_closed.append(self.gpa, num) catch {};
    }

    fn wasClosed(self: *Script, num: usize) bool {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);
        return std.mem.findScalar(usize, self.handler_closed.items, num) != null;
    }

    fn acceptLoop(self: *Script) void {
        const io = self.io;
        while (true) {
            const stream = self.listener.accept(io) catch |err| switch (err) {
                error.Canceled, error.SocketNotListening => return,
                else => {
                    io.sleep(.fromMilliseconds(20), .awake) catch return;
                    continue;
                },
            };
            self.mutex.lockUncancelable(io);
            const num = self.accepted;
            self.accepted += 1;
            self.active += 1;
            self.mutex.unlock(io);
            self.group.concurrent(io, serve, .{ self, stream, num }) catch {
                stream.close(io);
                self.finished();
            };
        }
    }

    fn finished(self: *Script) void {
        const io = self.io;
        self.mutex.lockUncancelable(io);
        self.active -= 1;
        if (self.active == 0) self.idle.broadcast(io);
        self.mutex.unlock(io);
    }

    fn serve(self: *Script, stream: net.Stream, num: usize) void {
        const io = self.io;
        defer self.finished();
        var session: Session = .{ .script = self, .reader = undefined, .writer = undefined, .stream = stream, .num = num };
        if (self.options.tls_context) |ctx| {
            const t = tls.Server.accept(self.gpa, io, stream, ctx) catch {
                stream.close(io);
                return;
            };
            session.reader = t.reader();
            session.writer = t.writer();
            self.options.handler(self.options.ctx, &session);
            if (!self.wasClosed(num)) {
                session.writer.flush() catch {};
                t.close();
            } else t.abort();
            if (!self.wasClosed(num)) stream.close(io);
            return;
        }
        var rbuf: [16 * 1024]u8 = undefined;
        var wbuf: [4 * 1024]u8 = undefined;
        var r = stream.reader(io, &rbuf);
        var w = stream.writer(io, &wbuf);
        session.reader = &r.interface;
        session.writer = &w.interface;
        self.options.handler(self.options.ctx, &session);
        if (!self.wasClosed(num)) stream.close(io);
    }
};
