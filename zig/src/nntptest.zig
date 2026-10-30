//! A scriptable fake NNTP server for tests above the NNTP client (the
//! scheduler, the runner, the end-to-end tests), the counterpart of Go's
//! internal/usenet/nntp/nntptest. It is a real server on a loopback port that
//! speaks just enough NNTP (greeting, AUTHINFO, BODY, DATE, QUIT), so the
//! whole client stack, pool included, runs against it unchanged. It can also
//! be started on its own as a standalone helper (`Server.start`, then hand
//! `port()` to anything that dials).
//!
//!     const srv = try nntptest.Server.start(gpa, io);
//!     defer srv.stop();
//!     var fx = try nntptest.buildFixture(gpa, &.{.{ .name = "movie.mkv", .data = data }}, 700_000, .{});
//!     defer fx.deinit();
//!     try srv.installFixture(&fx);
//!     const pool = try nntp.Pool.create(gpa, io, .{ .name = "main", .config = srv.config(), .dialer = srv.dialer() });
//!
//! `fx.nzb` is a document for the NZB parser, and every message id in it is
//! served as yEnc multipart articles with correct begin, end and CRC32.
//! Install the same Fixture on a second Server to play a fill Server.
//!
//! Scripting (any time, also while connections are open):
//!   addArticle        stores body lines (`encodeArticles` makes them).
//!   missing           makes ids answer 430.
//!   corrupt           serves ids with a flipped data byte: the yEnc CRC32
//!                     check fails while the part still decodes to its length.
//!   setLatencyMs      delays every BODY and DATE answer.
//!   disconnect        drops the connection after N lines of a body.
//!   requireAuth       demands these credentials (481 otherwise, 480 for
//!                     commands before authentication).
//!   failAuth          answers every AUTHINFO USER with 481.
//!   setMaxConnections refuses extra connections with a 502 greeting.
//!   failNextDials     closes the next N connections without a greeting.
//!   setRetentionDays  only records the retention, for scheduler tests.
//!
//! Observing: requests, totalRequests, requestedIds and connRequests count
//! BODY commands per message id and per connection; dials, authAttempts,
//! openConns and peakConns describe connections, so a pool test asserts its
//! limit with peakConns. `config()` has credentials, because that is what
//! makes a client authenticate (authAttempts counts AUTHINFO USER commands).
//!
//! Plain TCP only (the dialer of this file); `startTls` serves TLS for the
//! tests of the real TLS dialer.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const scripted = @import("nntp_scripted.zig");
const fixture = @import("nntptest_fixture.zig");
const conn_mod = @import("nntp_conn.zig");
const transport = @import("nntp_transport.zig");
const tls = @import("tls.zig");

pub const encodeArticles = fixture.encodeArticles;
pub const corruptLines = fixture.corruptLines;
pub const buildFixture = fixture.buildFixture;
pub const Fixture = fixture.Fixture;
pub const FixtureFile = fixture.FixtureFile;
pub const FixtureInput = fixture.FixtureInput;
pub const FixtureOptions = fixture.FixtureOptions;
pub const default_line_length = fixture.default_line_length;
pub const test_host = scripted.test_host;

const Disconnect = struct {
    /// Lines delivered before the drop.
    after: usize,
    /// Drops left; negative means every time.
    remaining: i32,
};

pub const Server = struct {
    gpa: Allocator,
    io: Io,
    script: *scripted.Script,
    mutex: Io.Mutex = .init,
    /// Article lines and ids live here until `stop`.
    store: std.heap.ArenaAllocator,
    tls_context: ?tls.ServerContext = null,

    // Guarded by `mutex`.
    articles: std.StringHashMapUnmanaged([]const []const u8) = .empty,
    missing_ids: std.StringHashMapUnmanaged(void) = .empty,
    corrupt_ids: std.StringHashMapUnmanaged(void) = .empty,
    disconnects: std.StringHashMapUnmanaged(Disconnect) = .empty,
    requested: std.StringHashMapUnmanaged(usize) = .empty,
    conn_reqs: std.ArrayList(usize) = .empty,
    total: usize = 0,
    dial_count: usize = 0,
    retention_days: i64 = 0,
    fail_auth: bool = false,
    required_user: ?[]const u8 = null,
    required_pass: ?[]const u8 = null,
    max_conns: usize = 0,
    fail_dials: usize = 0,
    open: usize = 0,
    peak: usize = 0,
    auth_attempts: usize = 0,
    latency_ms: u32 = 0,

    /// Default credentials of `config()` when none are required.
    pub const default_user = "user";
    pub const default_pass = "pass";

    pub fn start(gpa: Allocator, io: Io) !*Server {
        return startWith(gpa, io, null);
    }

    /// Serves TLS with `context`; the Server owns it from a successful return
    /// on and frees it in `stop`.
    pub fn startTls(gpa: Allocator, io: Io, context: tls.ServerContext) !*Server {
        const self = try startWith(gpa, io, context);
        self.tls_context = context;
        return self;
    }

    fn startWith(gpa: Allocator, io: Io, context: ?tls.ServerContext) !*Server {
        const self = try gpa.create(Server);
        errdefer gpa.destroy(self);
        self.* = .{ .gpa = gpa, .io = io, .script = undefined, .store = .init(gpa) };
        errdefer self.store.deinit();
        self.script = try scripted.Script.start(gpa, io, .{ .handler = handle, .ctx = self, .tls_context = context });
        return self;
    }

    pub fn stop(self: *Server) void {
        self.script.stop();
        const gpa = self.gpa;
        self.articles.deinit(gpa);
        self.missing_ids.deinit(gpa);
        self.corrupt_ids.deinit(gpa);
        self.disconnects.deinit(gpa);
        self.requested.deinit(gpa);
        self.conn_reqs.deinit(gpa);
        self.store.deinit();
        if (self.tls_context) |c| c.deinit();
        gpa.destroy(self);
    }

    pub fn port(self: *const Server) u16 {
        return self.script.port();
    }

    /// A client Config that reaches this server (with credentials, so the
    /// client authenticates) and short timeouts.
    pub fn config(self: *Server) conn_mod.Config {
        self.lock();
        defer self.unlock();
        return .{
            .host = test_host,
            .port = self.port(),
            .user = self.required_user orelse default_user,
            .pass = self.required_pass orelse default_pass,
            .connect_timeout_ms = 5000,
            .read_timeout_ms = 5000,
        };
    }

    /// A Dialer to this server in plain text, whatever host is asked for.
    pub fn dialer(self: *Server) transport.Dialer {
        return self.script.dialer();
    }

    // ---- scripting ------------------------------------------------------------

    /// Stores the body lines of the Article with this message id (no angle
    /// brackets): dot-unstuffed, without line terminators, as the client hands
    /// them to its Sink. The lines are copied.
    pub fn addArticle(self: *Server, id: []const u8, lines: []const []const u8) Allocator.Error!void {
        const a = self.store.allocator();
        const copy = try a.alloc([]const u8, lines.len);
        for (lines, copy) |l, *c| c.* = try a.dupe(u8, l);
        const key = try a.dupe(u8, id);
        self.lock();
        defer self.unlock();
        try self.articles.put(self.gpa, key, copy);
    }

    /// Adds all Articles of the fixture.
    pub fn installFixture(self: *Server, fx: *const Fixture) Allocator.Error!void {
        for (fx.articles) |a| try self.addArticle(a.id, a.lines);
    }

    /// Makes the server answer 430 for these ids, added or not.
    pub fn missing(self: *Server, ids: []const []const u8) Allocator.Error!void {
        const a = self.store.allocator();
        self.lock();
        defer self.unlock();
        for (ids) |id| try self.missing_ids.put(self.gpa, try a.dupe(u8, id), {});
    }

    /// Serves these Articles with one data byte changed (the stored lines are
    /// not modified).
    pub fn corrupt(self: *Server, ids: []const []const u8) Allocator.Error!void {
        const a = self.store.allocator();
        self.lock();
        defer self.unlock();
        for (ids) |id| try self.corrupt_ids.put(self.gpa, try a.dupe(u8, id), {});
    }

    /// Delays every BODY and DATE answer by `ms`.
    pub fn setLatencyMs(self: *Server, ms: u32) void {
        self.lock();
        defer self.unlock();
        self.latency_ms = ms;
    }

    /// Drops the connection after `after_lines` lines of the body of `id`
    /// were delivered, for the next `times` requests of it (0: every time).
    pub fn disconnect(self: *Server, id: []const u8, after_lines: usize, times: u32) Allocator.Error!void {
        const key = try self.store.allocator().dupe(u8, id);
        self.lock();
        defer self.unlock();
        try self.disconnects.put(self.gpa, key, .{ .after = after_lines, .remaining = if (times == 0) -1 else @intCast(times) });
    }

    /// Every AUTHINFO USER is answered with 481 until called again with false.
    pub fn failAuth(self: *Server, fail: bool) void {
        self.lock();
        defer self.unlock();
        self.fail_auth = fail;
    }

    /// Accepts only these credentials (`config()` then carries them).
    pub fn requireAuth(self: *Server, user: []const u8, pass: []const u8) Allocator.Error!void {
        const a = self.store.allocator();
        const u = try a.dupe(u8, user);
        const p = try a.dupe(u8, pass);
        self.lock();
        defer self.unlock();
        self.required_user = u;
        self.required_pass = p;
    }

    /// Limits concurrently open connections; a connect beyond the limit gets
    /// a "502 Too many connections" greeting, as providers do. 0: no limit.
    pub fn setMaxConnections(self: *Server, n: usize) void {
        self.lock();
        defer self.unlock();
        self.max_conns = n;
    }

    /// The next `n` connections are closed without a greeting.
    pub fn failNextDials(self: *Server, n: usize) void {
        self.lock();
        defer self.unlock();
        self.fail_dials = n;
    }

    pub fn setRetentionDays(self: *Server, days: i64) void {
        self.lock();
        defer self.unlock();
        self.retention_days = days;
    }

    pub fn retentionDays(self: *Server) i64 {
        self.lock();
        defer self.unlock();
        return self.retention_days;
    }

    // ---- observing ----------------------------------------------------------------

    /// How many BODY commands asked for `id`, whatever the answer.
    pub fn requests(self: *Server, id: []const u8) usize {
        self.lock();
        defer self.unlock();
        return self.requested.get(id) orelse 0;
    }

    pub fn totalRequests(self: *Server) usize {
        self.lock();
        defer self.unlock();
        return self.total;
    }

    /// Every message id that was asked for at least once, sorted. Caller
    /// frees the slice (not the ids).
    pub fn requestedIds(self: *Server, gpa: Allocator) Allocator.Error![][]const u8 {
        self.lock();
        defer self.unlock();
        var ids = try gpa.alloc([]const u8, self.requested.count());
        var i: usize = 0;
        var it = self.requested.keyIterator();
        while (it.next()) |k| : (i += 1) ids[i] = k.*;
        std.mem.sort([]const u8, ids, {}, struct {
            fn less(_: void, a: []const u8, b: []const u8) bool {
                return std.mem.lessThan(u8, a, b);
            }
        }.less);
        return ids;
    }

    /// BODY commands received per connection, in the order the connections
    /// were opened. Caller frees.
    pub fn connRequests(self: *Server, gpa: Allocator) Allocator.Error![]usize {
        self.lock();
        defer self.unlock();
        return gpa.dupe(usize, self.conn_reqs.items);
    }

    /// Connections accepted, including refused ones.
    pub fn dials(self: *Server) usize {
        self.lock();
        defer self.unlock();
        return self.dial_count;
    }

    /// Connections that got as far as AUTHINFO USER.
    pub fn authAttempts(self: *Server) usize {
        self.lock();
        defer self.unlock();
        return self.auth_attempts;
    }

    pub fn openConns(self: *Server) usize {
        self.lock();
        defer self.unlock();
        return self.open;
    }

    /// The highest `openConns` has been.
    pub fn peakConns(self: *Server) usize {
        self.lock();
        defer self.unlock();
        return self.peak;
    }

    /// Waits until `openConns` is `n`; false after `timeout_ms`. A connection
    /// the client closed is only noticed when the server reads the EOF.
    pub fn waitOpen(self: *Server, n: usize, timeout_ms: u32) bool {
        var waited: u32 = 0;
        while (self.openConns() != n) {
            if (waited >= timeout_ms) return false;
            self.io.sleep(.fromMilliseconds(5), .awake) catch return false;
            waited += 5;
        }
        return true;
    }

    /// The command lines received so far, over all connections.
    pub fn commands(self: *Server) []const []const u8 {
        return self.script.receivedNoWait();
    }

    fn lock(self: *Server) void {
        self.mutex.lockUncancelable(self.io);
    }

    fn unlock(self: *Server) void {
        self.mutex.unlock(self.io);
    }

    // ---- the conversation --------------------------------------------------------------

    fn handle(ctx: ?*anyopaque, s: *scripted.Session) void {
        const self: *Server = @ptrCast(@alignCast(ctx.?));
        self.lock();
        self.dial_count += 1;
        if (self.fail_dials > 0) {
            self.fail_dials -= 1;
            self.unlock();
            s.close();
            return;
        }
        if (self.max_conns > 0 and self.open >= self.max_conns) {
            self.unlock();
            s.line("502 Too many connections");
            return;
        }
        self.open += 1;
        self.peak = @max(self.peak, self.open);
        self.conn_reqs.append(self.gpa, 0) catch {};
        const num = self.conn_reqs.items.len -| 1;
        self.unlock();
        defer {
            self.lock();
            self.open -= 1;
            self.unlock();
        }

        s.line("200 Fake NNTP server ready");
        var pending_user: [300]u8 = undefined;
        var pending_len: usize = 0;
        var authed = false;
        while (s.readCmd()) |cmd| {
            if (std.ascii.startsWithIgnoreCase(cmd, "AUTHINFO USER ")) {
                const user = cmd["AUTHINFO USER ".len..];
                self.lock();
                self.auth_attempts += 1;
                const fail = self.fail_auth;
                self.unlock();
                if (fail) {
                    s.line("481 Authentication failed");
                    continue;
                }
                pending_len = @min(user.len, pending_user.len);
                @memcpy(pending_user[0..pending_len], user[0..pending_len]);
                s.line("381 Password required");
            } else if (std.ascii.startsWithIgnoreCase(cmd, "AUTHINFO PASS ")) {
                const pass = cmd["AUTHINFO PASS ".len..];
                self.lock();
                const ok = !self.fail_auth and (self.required_user == null or
                    (std.mem.eql(u8, self.required_user.?, pending_user[0..pending_len]) and std.mem.eql(u8, self.required_pass.?, pass)));
                self.unlock();
                if (ok) {
                    authed = true;
                    s.line("281 Authentication accepted");
                } else s.line("481 Authentication failed");
            } else if (std.ascii.eqlIgnoreCase(cmd, "QUIT")) {
                s.line("205 Goodbye");
                return;
            } else if (std.ascii.eqlIgnoreCase(cmd, "DATE")) {
                if (!self.mayCommand(s, authed)) continue;
                if (!self.pause(s)) return;
                self.sendDate(s);
            } else if (std.ascii.startsWithIgnoreCase(cmd, "BODY <") and cmd[cmd.len - 1] == '>') {
                if (!self.mayCommand(s, authed)) continue;
                var id_buf: [300]u8 = undefined;
                const id_raw = cmd["BODY <".len .. cmd.len - 1];
                const id = id_buf[0..@min(id_raw.len, id_buf.len)];
                @memcpy(id, id_raw[0..id.len]);
                if (!self.sendBody(s, num, id)) return;
            } else s.line("500 Unknown command");
        }
    }

    /// 480 for a command before authentication on a server that wants it.
    fn mayCommand(self: *Server, s: *scripted.Session, authed: bool) bool {
        self.lock();
        const need = self.required_user != null and !authed;
        self.unlock();
        if (need) s.line("480 Authentication required");
        return !need;
    }

    /// The scripted latency; false when cancelled.
    fn pause(self: *Server, s: *scripted.Session) bool {
        self.lock();
        const ms = self.latency_ms;
        self.unlock();
        if (ms == 0) return true;
        s.script.io.sleep(.fromMilliseconds(ms), .awake) catch return false;
        return true;
    }

    fn sendDate(self: *Server, s: *scripted.Session) void {
        const now = Io.Clock.now(.real, self.io);
        const secs: u64 = @intCast(@max(0, now.toSeconds()));
        const es: std.time.epoch.EpochSeconds = .{ .secs = secs };
        const yd = es.getEpochDay().calculateYearDay();
        const md = yd.calculateMonthDay();
        const ds = es.getDaySeconds();
        var buf: [32]u8 = undefined;
        s.line(std.fmt.bufPrint(&buf, "111 {d:0>4}{d:0>2}{d:0>2}{d:0>2}{d:0>2}{d:0>2}", .{
            yd.year,              md.month.numeric(),      md.day_index + 1,
            ds.getHoursIntoDay(), ds.getMinutesIntoHour(), ds.getSecondsIntoMinute(),
        }) catch return);
    }

    /// Answers one BODY. False when the connection is over.
    fn sendBody(self: *Server, s: *scripted.Session, num: usize, id: []const u8) bool {
        self.lock();
        self.total += 1;
        if (self.requested.getPtr(id)) |n| {
            n.* += 1;
        } else if (self.store.allocator().dupe(u8, id)) |key| {
            self.requested.put(self.gpa, key, 1) catch {};
        } else |_| {}
        if (num < self.conn_reqs.items.len) self.conn_reqs.items[num] += 1;
        const found: ?[]const []const u8 = if (self.missing_ids.contains(id)) null else self.articles.get(id);
        const corrupt_it = self.corrupt_ids.contains(id);
        var cut: ?usize = null;
        if (self.disconnects.getPtr(id)) |d| if (d.remaining != 0) {
            cut = d.after;
            if (d.remaining > 0) d.remaining -= 1;
        };
        self.unlock();

        if (!self.pause(s)) return false;
        const lines = found orelse {
            s.line("430 No such article");
            return true;
        };
        var corrupted: ?[][]u8 = null;
        defer if (corrupted) |c| {
            for (c) |l| self.gpa.free(l);
            self.gpa.free(c);
        };
        var shown = lines;
        if (corrupt_it) {
            corrupted = fixture.corruptLines(self.gpa, lines) catch return false;
            shown = @ptrCast(corrupted.?);
        }

        var head: [320]u8 = undefined;
        s.line(std.fmt.bufPrint(&head, "222 0 <{s}>", .{id}) catch return false);
        for (shown, 0..) |l, i| {
            if (cut == i) {
                s.writer.flush() catch {};
                s.close();
                return false;
            }
            // Dot-stuffing: a line that starts with a dot gets another.
            if (l.len > 0 and l[0] == '.') s.writer.writeByte('.') catch return false;
            s.writer.writeAll(l) catch return false;
            s.writer.writeAll("\r\n") catch return false;
        }
        if (cut != null and cut.? >= shown.len) {
            s.writer.flush() catch {};
            s.close();
            return false;
        }
        s.writer.writeAll(".\r\n") catch return false;
        s.writer.flush() catch return false;
        return true;
    }
};

// ---- tests ------------------------------------------------------------------------------

const testing = std.testing;
const Conn = conn_mod.Conn;
const Diag = @import("nntp_diag.zig").Diag;
const types = @import("usenet_types.zig");

const tio = testing.io;

const Collect = struct {
    lines: std.ArrayList([]u8) = .empty,

    fn deinit(c: *Collect) void {
        for (c.lines.items) |l| testing.allocator.free(l);
        c.lines.deinit(testing.allocator);
    }

    fn sink(c: *Collect) types.Sink {
        return .{ .ctx = c, .line_fn = take };
    }

    fn take(ctx: *anyopaque, line: []const u8) error{Abort}!void {
        const c: *Collect = @ptrCast(@alignCast(ctx));
        const copy = testing.allocator.dupe(u8, line) catch return error.Abort;
        c.lines.append(testing.allocator, copy) catch return error.Abort;
    }
};

fn dial(srv: *Server) !*Conn {
    var d: Diag = .{};
    return Conn.connect(testing.allocator, tio, srv.dialer(), srv.config(), &d);
}

test "serves stored articles, dot-stuffed on the wire and unstuffed for the sink" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.addArticle("a@x", &.{ "=ybegin", ".dot", "..", "", "x" });
    const c = try dial(srv);
    defer c.close(tio);
    var got: Collect = .{};
    defer got.deinit();
    try c.body(tio, "a@x", got.sink());
    try testing.expectEqual(@as(usize, 5), got.lines.items.len);
    try testing.expectEqualStrings(".dot", got.lines.items[1]);
    try testing.expectEqualStrings("..", got.lines.items[2]);
    try testing.expectEqualStrings("", got.lines.items[3]);
    try testing.expectEqual(@as(usize, 1), srv.requests("a@x"));
    try testing.expectEqual(@as(usize, 1), srv.totalRequests());
}

test "unknown and missing ids answer 430 and the connection stays usable" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.addArticle("here@x", &.{"data"});
    try srv.addArticle("gone@x", &.{"data"});
    try srv.missing(&.{"gone@x"});
    const c = try dial(srv);
    defer c.close(tio);
    var got: Collect = .{};
    defer got.deinit();
    try testing.expectError(error.ArticleNotFound, c.body(tio, "nobody@x", got.sink()));
    try testing.expectError(error.ArticleNotFound, c.body(tio, "gone@x", got.sink()));
    try c.body(tio, "here@x", got.sink());
    try testing.expectEqual(@as(usize, 1), got.lines.items.len);
    try testing.expectEqual(@as(usize, 3), srv.totalRequests());
    const ids = try srv.requestedIds(testing.allocator);
    defer testing.allocator.free(ids);
    try testing.expectEqual(@as(usize, 3), ids.len);
    try testing.expectEqualStrings("gone@x", ids[0]);
    const per_conn = try srv.connRequests(testing.allocator);
    defer testing.allocator.free(per_conn);
    try testing.expectEqualSlices(usize, &.{3}, per_conn);
}

test "a fixture installs and every message id of its NZB is served" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    var fx = try buildFixture(testing.allocator, &.{.{ .name = "a.bin", .data = "0123456789abcdef" }}, 5, .{});
    defer fx.deinit();
    try srv.installFixture(&fx);
    const c = try dial(srv);
    defer c.close(tio);
    for (fx.files[0].message_ids) |id| {
        var got: Collect = .{};
        defer got.deinit();
        try c.body(tio, id, got.sink());
        try testing.expect(std.mem.startsWith(u8, got.lines.items[0], "=ybegin part="));
        try testing.expect(std.mem.startsWith(u8, got.lines.items[got.lines.items.len - 1], "=yend "));
    }
}

test "corrupt serves a damaged copy and leaves the stored lines alone" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const good = (try encodeArticles(arena.allocator(), "f.bin", "some payload bytes", 100, 128))[0];
    try srv.addArticle("a@x", good);
    try srv.addArticle("b@x", good);
    try srv.corrupt(&.{"a@x"});
    const c = try dial(srv);
    defer c.close(tio);
    var bad: Collect = .{};
    defer bad.deinit();
    var clean: Collect = .{};
    defer clean.deinit();
    try c.body(tio, "a@x", bad.sink());
    try c.body(tio, "b@x", clean.sink());
    try testing.expectEqual(good.len, clean.lines.items.len);
    for (good, clean.lines.items) |w, g| try testing.expectEqualStrings(w, g);
    var differs = false;
    for (good, bad.lines.items) |w, g| differs = differs or !std.mem.eql(u8, w, g);
    try testing.expect(differs);
}

test "disconnect drops the connection mid-body, the scripted number of times" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.addArticle("a@x", &.{ "one", "two", "three" });
    try srv.disconnect("a@x", 2, 1);
    var got: Collect = .{};
    defer got.deinit();
    {
        const c = try dial(srv);
        defer c.close(tio);
        try testing.expectError(error.Connection, c.body(tio, "a@x", got.sink()));
        try testing.expect(c.isDead());
        try testing.expectEqual(@as(usize, 2), got.lines.items.len);
    }
    {
        const c = try dial(srv);
        defer c.close(tio);
        var again: Collect = .{};
        defer again.deinit();
        try c.body(tio, "a@x", again.sink());
        try testing.expectEqual(@as(usize, 3), again.lines.items.len);
    }
    try testing.expectEqual(@as(usize, 2), srv.dials());
}

test "disconnect with times 0 drops every time, and also after the last line" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.addArticle("a@x", &.{ "one", "two" });
    try srv.disconnect("a@x", 2, 0);
    for (0..2) |_| {
        const c = try dial(srv);
        defer c.close(tio);
        var got: Collect = .{};
        defer got.deinit();
        try testing.expectError(error.Connection, c.body(tio, "a@x", got.sink()));
        try testing.expectEqual(@as(usize, 2), got.lines.items.len);
    }
}

test "authentication: attempts are counted, wrong credentials and failAuth are 481" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    var d: Diag = .{};
    {
        const c = try dial(srv);
        c.close(tio);
        try testing.expectEqual(@as(usize, 1), srv.authAttempts());
    }
    try srv.requireAuth("alice", "s3cret");
    {
        const c = try dial(srv);
        c.close(tio);
    }
    var cfg = srv.config();
    cfg.pass = "wrong";
    try testing.expectError(error.Auth, Conn.connect(testing.allocator, tio, srv.dialer(), cfg, &d));
    srv.failAuth(true);
    try testing.expectError(error.Auth, Conn.connect(testing.allocator, tio, srv.dialer(), srv.config(), &d));
    srv.failAuth(false);
    {
        const c = try dial(srv);
        c.close(tio);
    }
    try testing.expectEqual(@as(usize, 5), srv.authAttempts());
}

test "connection limit: a greeting of 502 beyond it, and the peak is recorded" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.setMaxConnections(2);
    const a = try dial(srv);
    const b = try dial(srv);
    var d: Diag = .{};
    try testing.expectError(error.Connection, Conn.connect(testing.allocator, tio, srv.dialer(), srv.config(), &d));
    try testing.expectEqual(@as(u16, 502), d.code);
    try testing.expectEqual(@as(usize, 2), srv.openConns());
    a.close(tio);
    try testing.expect(srv.waitOpen(1, 2000));
    const c = try dial(srv);
    b.close(tio);
    c.close(tio);
    try testing.expect(srv.waitOpen(0, 2000));
    try testing.expectEqual(@as(usize, 2), srv.peakConns());
    try testing.expectEqual(@as(usize, 4), srv.dials());
}

test "failNextDials closes connections without a greeting" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    srv.failNextDials(2);
    var d: Diag = .{};
    for (0..2) |_| try testing.expectError(error.Connection, Conn.connect(testing.allocator, tio, srv.dialer(), srv.config(), &d));
    const c = try dial(srv);
    c.close(tio);
}

test "latency delays BODY and DATE; DATE returns the clock" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.addArticle("a@x", &.{"x"});
    srv.setLatencyMs(150);
    const c = try dial(srv);
    defer c.close(tio);
    const t0 = Io.Clock.now(.awake, tio);
    const now = try c.date(tio);
    try testing.expect(t0.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() >= 140);
    const real = Io.Clock.now(.real, tio).toSeconds();
    try testing.expect(@abs(now - real) < 5);
    var got: Collect = .{};
    defer got.deinit();
    const t1 = Io.Clock.now(.awake, tio);
    try c.body(tio, "a@x", got.sink());
    try testing.expect(t1.durationTo(Io.Clock.now(.awake, tio)).toMilliseconds() >= 140);
}

test "retention is only recorded" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try testing.expectEqual(@as(i64, 0), srv.retentionDays());
    srv.setRetentionDays(1200);
    try testing.expectEqual(@as(i64, 1200), srv.retentionDays());
}

test "a server that requires credentials refuses commands before authentication" {
    const srv = try Server.start(testing.allocator, tio);
    defer srv.stop();
    try srv.requireAuth("alice", "pw");
    var d: Diag = .{};
    var cfg = srv.config();
    cfg.user = "";
    cfg.pass = "";
    const c = try Conn.connect(testing.allocator, tio, srv.dialer(), cfg, &d);
    defer c.close(tio);
    var got: Collect = .{};
    defer got.deinit();
    try testing.expectError(error.Connection, c.body(tio, "a@x", got.sink()));
    try testing.expectEqual(@as(u16, 480), c.diag.code);
}
