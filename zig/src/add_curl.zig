//! The real fetcher: libcurl's easy API (add_fetch.zig has the seam and the
//! rules). One request per redirect hop, because the redirect policy is
//! bitnews's own and its texts are Go's: at most five redirects, only to http
//! and https ("refused a redirect to a \"ftp\" URL: ..."), a redirect to a
//! "magnet:" link ends the fetch with that link.
//!
//! What each request does: GET with the User-Agent and Accept of Go, gzip
//! accepted and decoded, no proxy from the environment (the empty proxy
//! disables it), no cookies, no netrc, strict TLS 1.2 or newer with the
//! system roots, the credentials of the URL as Basic auth and nothing else,
//! the body capped as it arrives, and a total deadline. The progress callback
//! runs about every second, so a cancelled task ends the transfer.
//!
//! libcurl runs on the calling thread and blocks it; call this from a request
//! task, which is a thread of its own in the threaded `Io`.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const c = @import("curl_c");
const fetch = @import("add_fetch.zig");

pub const fetcher: fetch.Fetcher = .{ .ctx = null, .fetch_fn = fetchImpl };

// ---- libcurl set-up ----

var global_state: std.atomic.Value(u8) = .init(0);

/// curl_global_init is not thread safe the first time: the first caller does
/// it, the others wait for it.
fn ensureGlobalInit() void {
    if (global_state.cmpxchgStrong(0, 1, .acq_rel, .acquire) == null) {
        _ = c.curl_global_init(c.CURL_GLOBAL_DEFAULT);
        global_state.store(2, .release);
        return;
    }
    while (global_state.load(.acquire) != 2) std.atomic.spinLoopHint();
}

/// What the callbacks of one hop share with the code around curl_easy_perform.
const Hop = struct {
    gpa: Allocator,
    io: Io,
    max_bytes: u64,
    body: std.ArrayList(u8) = .empty,
    status: u16 = 0,
    content_length: ?u64 = null,
    encoded: bool = false,
    location: ?[]u8 = null,
    /// The header block of the final response was read.
    headers_done: bool = false,
    /// Set by the header callback to end the transfer: the answer is not 200
    /// (the body is not wanted) or the announced length is too large.
    stop: bool = false,
    too_large: bool = false,
    canceled: bool = false,
    oom: bool = false,

    fn deinit(h: *Hop) void {
        h.body.deinit(h.gpa);
        if (h.location) |l| h.gpa.free(l);
    }
};

fn hopOf(p: ?*anyopaque) *Hop {
    return @ptrCast(@alignCast(p.?));
}

fn headerCallback(buffer: [*c]u8, size: usize, nitems: usize, userdata: ?*anyopaque) callconv(.c) usize {
    const h = hopOf(userdata);
    const total = size * nitems;
    const line = std.mem.trimEnd(u8, buffer[0..total], "\r\n");
    if (std.mem.startsWith(u8, line, "HTTP/")) {
        // A new response (a redirect's, a 100 Continue's): start over.
        h.status = 0;
        h.content_length = null;
        h.encoded = false;
        h.headers_done = false;
        if (h.location) |l| h.gpa.free(l);
        h.location = null;
        var it = std.mem.tokenizeScalar(u8, line, ' ');
        _ = it.next();
        if (it.next()) |code| h.status = std.fmt.parseInt(u16, code, 10) catch 0;
        return total;
    }
    if (line.len == 0) {
        if (h.status >= 100 and h.status < 200) return total; // another response follows
        h.headers_done = true;
        if (h.status != 200) {
            h.stop = true;
            return 0;
        }
        // Go compares the announced length before reading a body; an encoded
        // body has no usable length.
        if (!h.encoded) if (h.content_length) |n| if (n > h.max_bytes) {
            h.too_large = true;
            return 0;
        };
        return total;
    }
    const colon = std.mem.findScalar(u8, line, ':') orelse return total;
    const name = line[0..colon];
    const value = std.mem.trim(u8, line[colon + 1 ..], " \t");
    if (std.ascii.eqlIgnoreCase(name, "content-length")) {
        h.content_length = std.fmt.parseInt(u64, value, 10) catch null;
    } else if (std.ascii.eqlIgnoreCase(name, "content-encoding")) {
        h.encoded = value.len > 0 and !std.ascii.eqlIgnoreCase(value, "identity");
    } else if (std.ascii.eqlIgnoreCase(name, "location")) {
        if (h.location) |l| h.gpa.free(l);
        h.location = h.gpa.dupe(u8, value) catch {
            h.oom = true;
            return 0;
        };
    }
    return total;
}

fn writeCallback(buffer: [*c]u8, size: usize, nmemb: usize, userdata: ?*anyopaque) callconv(.c) usize {
    const h = hopOf(userdata);
    const n = size * nmemb;
    if (h.stop or h.too_large) return 0;
    if (h.body.items.len + n > h.max_bytes) {
        h.too_large = true;
        return 0;
    }
    h.body.appendSlice(h.gpa, buffer[0..n]) catch {
        h.oom = true;
        return 0;
    };
    return n;
}

fn progressCallback(clientp: ?*anyopaque, dltotal: c.curl_off_t, dlnow: c.curl_off_t, ultotal: c.curl_off_t, ulnow: c.curl_off_t) callconv(.c) c_int {
    _ = .{ dltotal, dlnow, ultotal, ulnow };
    const h = hopOf(clientp);
    h.io.checkCancel() catch {
        h.canceled = true;
        return 1;
    };
    return 0;
}

// ---- URL parts ----

/// A part of a CURLU handle, copied into `arena`; null when it has none.
fn urlPart(arena: Allocator, u: *c.CURLU, what: c.CURLUPart, flags: c_uint) Allocator.Error!?[]const u8 {
    var out: [*c]u8 = null;
    if (c.curl_url_get(u, what, &out, flags) != c.CURLUE_OK or out == null) return null;
    defer c.curl_free(out);
    return try arena.dupe(u8, std.mem.span(out));
}

fn setUrl(u: *c.CURLU, arena: Allocator, url: []const u8) Allocator.Error!bool {
    const z = try arena.dupeSentinel(u8, url, 0);
    // URLENCODE: a space or a raw non-ASCII byte becomes a %-escape, as Go
    // does; NON_SUPPORT_SCHEME: a redirect to "ftp:" is read, to be refused
    // with its name.
    return c.curl_url_set(u, c.CURLUPART_URL, z.ptr, c.CURLU_URLENCODE | c.CURLU_NON_SUPPORT_SCHEME) == c.CURLUE_OK;
}

// ---- the transfer ----

fn setLong(easy: *c.CURL, opt: c.CURLoption, v: c_long) bool {
    return c.curl_easy_setopt(easy, opt, v) == c.CURLE_OK;
}

fn setPtr(easy: *c.CURL, opt: c.CURLoption, p: anytype) bool {
    return c.curl_easy_setopt(easy, opt, p) == c.CURLE_OK;
}

const Credentials = struct { user: []const u8, password: []const u8 };

const HopEnd = union(enum) {
    /// The answer: 200 with its body, or another status (with the Location
    /// of a redirect).
    answered: struct { status: u16, body: []u8, location: ?[]u8 },
    outcome: fetch.Outcome,
};

fn errnoText(arena: Allocator, errno_value: c_long) Allocator.Error![]const u8 {
    const text = std.mem.span(strerror(@intCast(errno_value)));
    return std.ascii.allocLowerString(arena, text);
}

extern "c" fn strerror(errnum: c_int) [*:0]const u8;

/// Go's error text for a failed transfer. Nothing in it comes from the URL
/// but the host name.
fn failureText(arena: Allocator, easy: *c.CURL, rc: c.CURLcode, errbuf: []const u8, host: []const u8, port: []const u8) Allocator.Error![]const u8 {
    switch (@as(c_int, @intCast(rc))) {
        c.CURLE_COULDNT_RESOLVE_HOST, c.CURLE_COULDNT_RESOLVE_PROXY => return std.fmt.allocPrint(arena, "dial tcp: lookup {s}: no such host", .{host}),
        c.CURLE_COULDNT_CONNECT => {
            var errno_value: c_long = 0;
            _ = c.curl_easy_getinfo(easy, c.CURLINFO_OS_ERRNO, &errno_value);
            const why = if (errno_value != 0) try errnoText(arena, errno_value) else "connection refused";
            return std.fmt.allocPrint(arena, "dial tcp {s}:{s}: connect: {s}", .{ host, port, why });
        },
        c.CURLE_GOT_NOTHING => return "EOF",
        c.CURLE_PARTIAL_FILE => return "unexpected EOF",
        c.CURLE_PEER_FAILED_VERIFICATION => return std.fmt.allocPrint(arena, "tls: failed to verify certificate: {s}", .{try tidy(arena, errbuf)}),
        c.CURLE_SSL_CONNECT_ERROR, c.CURLE_SSL_CERTPROBLEM, c.CURLE_SSL_CIPHER, c.CURLE_SSL_CACERT_BADFILE, c.CURLE_SSL_ISSUER_ERROR => return std.fmt.allocPrint(arena, "tls: {s}", .{try tidy(arena, errbuf)}),
        c.CURLE_UNSUPPORTED_PROTOCOL => return "unsupported protocol scheme",
        c.CURLE_URL_MALFORMAT => return "invalid URL",
        else => {},
    }
    if (errbuf.len > 0) return tidy(arena, errbuf);
    return tidy(arena, std.mem.span(c.curl_easy_strerror(rc)));
}

/// An error text for the user: lower case first letter, and nothing that
/// looks like a URL (it could hold an API key).
fn tidy(arena: Allocator, text: []const u8) Allocator.Error![]const u8 {
    const t = std.mem.trim(u8, text, " \t\r\n");
    if (std.mem.find(u8, t, "://") != null or t.len == 0) return "request failed";
    const out = try arena.dupe(u8, t);
    out[0] = std.ascii.toLower(out[0]);
    return out;
}

/// One GET of `url` (no credentials in it).
fn performHop(gpa: Allocator, arena: Allocator, io: Io, url: []const u8, creds: ?Credentials, req: fetch.Request, remaining_ms: u32, host: []const u8, port: []const u8) (Allocator.Error || Io.Cancelable)!HopEnd {
    const easy = c.curl_easy_init() orelse return error.OutOfMemory;
    defer c.curl_easy_cleanup(easy);

    var hop: Hop = .{ .gpa = gpa, .io = io, .max_bytes = req.max_bytes };
    defer hop.deinit();
    var errbuf: [c.CURL_ERROR_SIZE]u8 = @splat(0);

    const url_z = try arena.dupeSentinel(u8, url, 0);
    const agent_z = try arena.dupeSentinel(u8, fetch.user_agent, 0);
    const accept_line = try std.fmt.allocPrintSentinel(arena, "Accept: {s}", .{req.accept}, 0);
    const headers = c.curl_slist_append(null, accept_line.ptr);
    defer c.curl_slist_free_all(headers);

    var ok = true;
    ok = ok and setPtr(easy, c.CURLOPT_URL, url_z.ptr);
    ok = ok and setLong(easy, c.CURLOPT_HTTPGET, 1);
    ok = ok and setPtr(easy, c.CURLOPT_USERAGENT, agent_z.ptr);
    ok = ok and setPtr(easy, c.CURLOPT_HTTPHEADER, headers);
    // The same as Go's transport: gzip asked for and decoded.
    ok = ok and setPtr(easy, c.CURLOPT_ACCEPT_ENCODING, @as([*:0]const u8, "gzip"));
    // We follow redirects ourselves; no proxy, from the environment either;
    // only http and https.
    ok = ok and setLong(easy, c.CURLOPT_FOLLOWLOCATION, 0);
    ok = ok and setPtr(easy, c.CURLOPT_PROXY, @as([*:0]const u8, ""));
    ok = ok and setPtr(easy, c.CURLOPT_PROTOCOLS_STR, @as([*:0]const u8, "http,https"));
    // Strict TLS, 1.2 or newer.
    ok = ok and setLong(easy, c.CURLOPT_SSL_VERIFYPEER, 1);
    ok = ok and setLong(easy, c.CURLOPT_SSL_VERIFYHOST, 2);
    ok = ok and setLong(easy, c.CURLOPT_SSLVERSION, c.CURL_SSLVERSION_TLSv1_2);
    ok = ok and setLong(easy, c.CURLOPT_NOSIGNAL, 1);
    ok = ok and setLong(easy, c.CURLOPT_TIMEOUT_MS, remaining_ms);
    ok = ok and setLong(easy, c.CURLOPT_CONNECTTIMEOUT_MS, @min(remaining_ms, 30_000));
    ok = ok and setPtr(easy, c.CURLOPT_ERRORBUFFER, &errbuf);
    ok = ok and setPtr(easy, c.CURLOPT_HEADERFUNCTION, &headerCallback);
    ok = ok and setPtr(easy, c.CURLOPT_HEADERDATA, &hop);
    ok = ok and setPtr(easy, c.CURLOPT_WRITEFUNCTION, &writeCallback);
    ok = ok and setPtr(easy, c.CURLOPT_WRITEDATA, &hop);
    ok = ok and setLong(easy, c.CURLOPT_NOPROGRESS, 0);
    ok = ok and setPtr(easy, c.CURLOPT_XFERINFOFUNCTION, &progressCallback);
    ok = ok and setPtr(easy, c.CURLOPT_XFERINFODATA, &hop);
    if (creds) |cr| {
        const user_z = try arena.dupeSentinel(u8, cr.user, 0);
        const pass_z = try arena.dupeSentinel(u8, cr.password, 0);
        ok = ok and setLong(easy, c.CURLOPT_HTTPAUTH, @intCast(c.CURLAUTH_BASIC));
        ok = ok and setPtr(easy, c.CURLOPT_USERNAME, user_z.ptr);
        ok = ok and setPtr(easy, c.CURLOPT_PASSWORD, pass_z.ptr);
    }
    if (!ok) return .{ .outcome = .{ .failed = try gpa.dupe(u8, "could not set up the request") } };

    const rc = c.curl_easy_perform(easy);

    if (hop.oom) return error.OutOfMemory;
    if (hop.canceled) return error.Canceled;
    if (hop.too_large) return .{ .outcome = .too_large };
    if (rc != c.CURLE_OK and !(hop.stop and rc == c.CURLE_WRITE_ERROR)) {
        if (rc == c.CURLE_OPERATION_TIMEDOUT) return .{ .outcome = .timeout };
        const text = try failureText(arena, easy, rc, std.mem.sliceTo(&errbuf, 0), host, port);
        return .{ .outcome = .{ .failed = try gpa.dupe(u8, text) } };
    }
    var code: c_long = 0;
    _ = c.curl_easy_getinfo(easy, c.CURLINFO_RESPONSE_CODE, &code);
    const location = hop.location;
    hop.location = null;
    return .{ .answered = .{ .status = @intCast(code), .body = try hop.body.toOwnedSlice(gpa), .location = location } };
}

fn isRedirect(status: u16) bool {
    return switch (status) {
        301, 302, 303, 307, 308 => true,
        else => false,
    };
}

/// Whether a Location has neither a scheme ("ftp:") nor a host ("//host").
fn isRelativeReference(loc: []const u8) bool {
    if (std.mem.startsWith(u8, loc, "//")) return false;
    for (loc, 0..) |ch, i| {
        switch (ch) {
            'a'...'z', 'A'...'Z' => {},
            '0'...'9', '+', '-', '.' => if (i == 0) return true,
            ':' => return i == 0,
            else => return true,
        }
    }
    return true;
}

fn isMagnet(s: []const u8) bool {
    return s.len >= 7 and std.ascii.eqlIgnoreCase(s[0..7], "magnet:");
}

fn failed(gpa: Allocator, comptime fmt: []const u8, args: anytype) Allocator.Error!fetch.Outcome {
    return .{ .failed = try std.fmt.allocPrint(gpa, fmt, args) };
}

fn fetchImpl(_: ?*anyopaque, gpa: Allocator, io: Io, req: fetch.Request) fetch.FetchError!fetch.Outcome {
    ensureGlobalInit();
    var scratch: std.heap.ArenaAllocator = .init(gpa);
    defer scratch.deinit();
    const arena = scratch.allocator();

    const started = Io.Clock.now(.awake, io).toMilliseconds();
    const u = c.curl_url() orelse return error.OutOfMemory;
    defer c.curl_url_cleanup(u);
    if (!try setUrl(u, arena, req.url)) return .{ .failed = try gpa.dupe(u8, "invalid URL") };

    var redirects: u8 = 0;
    while (true) {
        const elapsed: i64 = Io.Clock.now(.awake, io).toMilliseconds() - started;
        if (elapsed >= req.timeout_ms) return .timeout;
        const remaining: u32 = @intCast(@as(i64, req.timeout_ms) - elapsed);

        // The URL of this hop without its credentials, which go in Basic auth.
        const host = (try urlPart(arena, u, c.CURLUPART_HOST, 0)) orelse "";
        const port = (try urlPart(arena, u, c.CURLUPART_PORT, c.CURLU_DEFAULT_PORT)) orelse "";
        const user = try urlPart(arena, u, c.CURLUPART_USER, c.CURLU_URLDECODE);
        const password = try urlPart(arena, u, c.CURLUPART_PASSWORD, c.CURLU_URLDECODE);
        const bare = c.curl_url_dup(u) orelse return error.OutOfMemory;
        defer c.curl_url_cleanup(bare);
        _ = c.curl_url_set(bare, c.CURLUPART_USER, null, 0);
        _ = c.curl_url_set(bare, c.CURLUPART_PASSWORD, null, 0);
        const hop_url = (try urlPart(arena, bare, c.CURLUPART_URL, 0)) orelse return .{ .failed = try gpa.dupe(u8, "invalid URL") };
        const creds: ?Credentials = if (user) |name| .{ .user = name, .password = password orelse "" } else null;

        const end = try performHop(gpa, arena, io, hop_url, creds, req, remaining, host, port);
        const answered = switch (end) {
            .outcome => |o| return o,
            .answered => |a| a,
        };
        var keep_body = true;
        defer if (keep_body) gpa.free(answered.body);
        defer if (answered.location) |l| gpa.free(l);

        if (answered.status == 200) {
            keep_body = false;
            return .{ .body = answered.body };
        }
        const location = answered.location orelse "";
        if (!isRedirect(answered.status) or location.len == 0) return .{ .status = answered.status };

        // Indexer proxies answer a torrent download that has only a magnet
        // link with a redirect to it.
        if (isMagnet(location)) return .{ .magnet = try gpa.dupe(u8, location) };
        if (redirects >= fetch.max_redirects) return failed(gpa, "stopped after {d} redirects", .{fetch.max_redirects});
        if (!try setUrl(u, arena, location)) return .{ .failed = try gpa.dupe(u8, "failed to parse Location header") };
        // A reference without a scheme or a host stays on the host and keeps
        // its credentials, as Go's URL resolution does; whether libcurl keeps
        // them differs between versions, so say so.
        if (isRelativeReference(location)) if (user) |name| {
            if ((try urlPart(arena, u, c.CURLUPART_USER, 0)) == null) {
                const name_z = try arena.dupeSentinel(u8, name, 0);
                _ = c.curl_url_set(u, c.CURLUPART_USER, name_z.ptr, c.CURLU_URLENCODE);
                const pass_z = try arena.dupeSentinel(u8, password orelse "", 0);
                _ = c.curl_url_set(u, c.CURLUPART_PASSWORD, pass_z.ptr, c.CURLU_URLENCODE);
            }
        };
        const scheme = (try urlPart(arena, u, c.CURLUPART_SCHEME, 0)) orelse "";
        if (!std.ascii.eqlIgnoreCase(scheme, "http") and !std.ascii.eqlIgnoreCase(scheme, "https")) {
            const lower = try std.ascii.allocLowerString(arena, scheme);
            return failed(gpa, "refused a redirect to a \"{s}\" URL: only http and https are allowed", .{lower});
        }
        redirects += 1;
    }
}

// ---- tests: the fetcher against a real server on loopback ----

const testing = std.testing;
const ex = @import("http_exchange.zig");
const ts = @import("add_testserver.zig");

/// What the test server saw of the last request to /ok.
const Seen = struct {
    user_agent: []const u8 = "",
    accept: []const u8 = "",
    cookie: []const u8 = "",
    authorization: []const u8 = "",
    accept_encoding: []const u8 = "",
    hits: usize = 0,
};
var seen: Seen = .{};
var seen_arena: std.heap.ArenaAllocator = undefined;

fn header(e: *ex.Exchange, name: []const u8) []const u8 {
    return seen_arena.allocator().dupe(u8, e.header(name) orelse "") catch "";
}

fn okHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    seen.hits += 1;
    seen.user_agent = header(e, "User-Agent");
    seen.accept = header(e, "Accept");
    seen.cookie = header(e, "Cookie");
    seen.authorization = header(e, "Authorization");
    seen.accept_encoding = header(e, "Accept-Encoding");
    e.res.body = "hello nzb";
    try e.res.setHeader(e.arena, "Content-Type", "application/x-nzb");
}

fn redirect(e: *ex.Exchange, status: std.http.Status, location: []const u8) !void {
    e.res.status = status;
    try e.res.setHeader(e.arena, "Location", location);
}

fn hopHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const n = try std.fmt.parseInt(u32, e.param("n").?, 10);
    if (n >= 5) {
        e.res.body = "arrived";
        return;
    }
    return redirect(e, .found, try std.fmt.allocPrint(e.arena, "/hop/{d}", .{n + 1}));
}

fn loopHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    return redirect(e, .moved_permanently, "/loop");
}
fn relativeHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    return redirect(e, .see_other, "ok?from=relative");
}
fn ftpHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    return redirect(e, .found, "FTP://files.example/x.nzb?apikey=SECRETKEY");
}
fn magnetHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    return redirect(e, .found, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x");
}
fn noLocationHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = .found;
}
fn missingHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = .not_found;
    e.res.body = "nope SECRETKEY";
}
fn brokenHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = .internal_server_error;
}
const big_body: [1000]u8 = @splat('x');

fn bigHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.body = &big_body;
}
fn endlessHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = .ok;
    try e.startStream();
    var chunk: [100]u8 = @splat(' ');
    for (0..5) |_| try e.streamWrite(&chunk);
}
fn slowHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try e.io.sleep(.fromMilliseconds(3000), .awake);
    e.res.body = "late";
}
fn gzipHandler(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const plain = "<nzb>gzip body</nzb>";
    // A gzip member with one stored deflate block.
    var out: std.ArrayList(u8) = .empty;
    try out.appendSlice(e.arena, &.{ 0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3 });
    try out.appendSlice(e.arena, &.{ 1, plain.len, 0, ~@as(u8, plain.len), 0xff });
    try out.appendSlice(e.arena, plain);
    var crc = std.hash.Crc32.init();
    crc.update(plain);
    try out.appendSlice(e.arena, &std.mem.toBytes(std.mem.nativeToLittle(u32, crc.final())));
    try out.appendSlice(e.arena, &std.mem.toBytes(std.mem.nativeToLittle(u32, plain.len)));
    e.res.body = out.items;
    try e.res.setHeader(e.arena, "Content-Encoding", "gzip");
}

fn setup(routes: *ex.Routes) anyerror!void {
    const r = .{
        .{ "GET /ok", okHandler },
        .{ "GET /hop/{n}", hopHandler },
        .{ "GET /loop", loopHandler },
        .{ "GET /relative", relativeHandler },
        .{ "GET /ftp", ftpHandler },
        .{ "GET /magnet", magnetHandler },
        .{ "GET /nolocation", noLocationHandler },
        .{ "GET /missing", missingHandler },
        .{ "GET /broken", brokenHandler },
        .{ "GET /big", bigHandler },
        .{ "GET /endless", endlessHandler },
        .{ "GET /slow", slowHandler },
        .{ "GET /gzip", gzipHandler },
    };
    inline for (r) |entry| try routes.add(entry[0], .{ .handler = entry[1], .auth = .none });
}

const Rig = struct {
    server: ts.Server,
    arena: std.heap.ArenaAllocator,

    fn open(rig: *Rig) !void {
        seen = .{};
        seen_arena = .init(testing.allocator);
        rig.arena = .init(testing.allocator);
        try rig.server.start(setup);
    }

    fn close(rig: *Rig) void {
        rig.server.stop();
        rig.arena.deinit();
        seen_arena.deinit();
    }

    fn get(rig: *Rig, path: []const u8) !fetch.Outcome {
        return rig.getWith(path, .{ .url = "", .max_bytes = 1 << 20 });
    }

    fn getWith(rig: *Rig, path: []const u8, req: fetch.Request) !fetch.Outcome {
        var r = req;
        r.url = try rig.server.url(rig.arena.allocator(), path);
        return fetcher.fetch(testing.allocator, testing.io, r);
    }
};

fn expectFailed(want: []const u8, out: fetch.Outcome) !void {
    defer out.deinit(testing.allocator);
    try testing.expect(out == .failed);
    try testing.expectEqualStrings(want, out.failed);
}

test "a 200 answer gives its body, with the request Go sends" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const out = try rig.get("/ok?apikey=SECRETKEY");
    defer out.deinit(testing.allocator);

    try testing.expect(out == .body);
    try testing.expectEqualStrings("hello nzb", out.body);
    try testing.expectEqualStrings("bitnews (+https://github.com/jroosing/bitnews)", seen.user_agent);
    try testing.expectEqualStrings("application/x-bittorrent, application/x-nzb, text/xml;q=0.9, */*;q=0.1", seen.accept);
    try testing.expectEqualStrings("", seen.cookie);
    try testing.expectEqualStrings("", seen.authorization);
    try testing.expect(std.mem.indexOf(u8, seen.accept_encoding, "gzip") != null);
}

test "credentials in the URL become Basic auth, and only then" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena.allocator();

    const url = try std.fmt.allocPrint(a, "http://user:p%40ss@127.0.0.1:{d}/ok", .{rig.server.port});
    const out = try fetcher.fetch(testing.allocator, testing.io, .{ .url = url, .max_bytes = 1 << 20 });
    out.deinit(testing.allocator);
    // base64("user:p@ss")
    try testing.expectEqualStrings("Basic dXNlcjpwQHNz", seen.authorization);

    // A relative redirect stays on the host and keeps the credentials, like Go.
    seen = .{};
    const rurl = try std.fmt.allocPrint(a, "http://user:p%40ss@127.0.0.1:{d}/relative", .{rig.server.port});
    const out2 = try fetcher.fetch(testing.allocator, testing.io, .{ .url = rurl, .max_bytes = 1 << 20 });
    out2.deinit(testing.allocator);
    try testing.expectEqualStrings("Basic dXNlcjpwQHNz", seen.authorization);
}

test "five redirects are followed and a sixth is not" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const ok = try rig.get("/hop/0");
    defer ok.deinit(testing.allocator);
    try testing.expect(ok == .body);
    try testing.expectEqualStrings("arrived", ok.body);

    try expectFailed("stopped after 5 redirects", try rig.get("/loop"));
}

test "a relative Location is resolved against the URL" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const out = try rig.get("/relative");
    defer out.deinit(testing.allocator);
    try testing.expectEqualStrings("hello nzb", out.body);
    try testing.expectEqual(@as(usize, 1), seen.hits);
}

test "a redirect to another scheme is refused by name, a magnet link ends the fetch" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    try expectFailed("refused a redirect to a \"ftp\" URL: only http and https are allowed", try rig.get("/ftp"));

    const m = try rig.get("/magnet");
    defer m.deinit(testing.allocator);
    try testing.expect(m == .magnet);
    try testing.expectEqualStrings("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x", m.magnet);
}

test "other answers are reported by status, a redirect without Location included" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    for ([_]struct { []const u8, u16 }{ .{ "/missing", 404 }, .{ "/broken", 500 }, .{ "/nolocation", 302 } }) |c_| {
        const out = try rig.get(c_[0]);
        defer out.deinit(testing.allocator);
        try testing.expect(out == .status);
        try testing.expectEqual(c_[1], out.status);
    }
}

test "a body over the cap is too large, announced or counted" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const announced = try rig.getWith("/big", .{ .url = "", .max_bytes = 999 });
    try testing.expect(announced == .too_large);
    const exact = try rig.getWith("/big", .{ .url = "", .max_bytes = 1000 });
    defer exact.deinit(testing.allocator);
    try testing.expect(exact == .body);
    try testing.expectEqual(@as(usize, 1000), exact.body.len);

    // No Content-Length: counted as it arrives.
    const counted = try rig.getWith("/endless", .{ .url = "", .max_bytes = 499 });
    try testing.expect(counted == .too_large);
    const fits = try rig.getWith("/endless", .{ .url = "", .max_bytes = 500 });
    defer fits.deinit(testing.allocator);
    try testing.expect(fits == .body);
    try testing.expectEqual(@as(usize, 500), fits.body.len);
}

test "gzip is decoded and the cap applies to the decoded size" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const out = try rig.get("/gzip");
    defer out.deinit(testing.allocator);
    try testing.expect(out == .body);
    try testing.expectEqualStrings("<nzb>gzip body</nzb>", out.body);

    const capped = try rig.getWith("/gzip", .{ .url = "", .max_bytes = 5 });
    try testing.expect(capped == .too_large);
}

test "a server that cannot be reached, and one that is too slow" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    // Nothing listens on port 1.
    const refused = try fetcher.fetch(testing.allocator, testing.io, .{ .url = "http://127.0.0.1:1/x.nzb?apikey=SECRETKEY", .max_bytes = 100 });
    defer refused.deinit(testing.allocator);
    try testing.expect(refused == .failed);
    try testing.expectEqualStrings("dial tcp 127.0.0.1:1: connect: connection refused", refused.failed);

    const slow = try rig.getWith("/slow", .{ .url = "", .max_bytes = 100, .timeout_ms = 300 });
    try testing.expect(slow == .timeout);
}

test "TLS is spoken and verified: a plain server is not a TLS server" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const url = try std.fmt.allocPrint(rig.arena.allocator(), "https://127.0.0.1:{d}/ok", .{rig.server.port});
    const out = try fetcher.fetch(testing.allocator, testing.io, .{ .url = url, .max_bytes = 100 });
    defer out.deinit(testing.allocator);
    try testing.expect(out == .failed);
    try testing.expect(std.mem.startsWith(u8, out.failed, "tls: "));
    try testing.expectEqual(@as(usize, 0), seen.hits);
}

test "cancelling the task ends a transfer that waits" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const url = try rig.server.url(rig.arena.allocator(), "/slow");
    var fut = try testing.io.concurrent(fetchImpl, .{ null, testing.allocator, testing.io, fetch.Request{ .url = url, .max_bytes = 100, .timeout_ms = 20_000 } });
    try testing.io.sleep(.fromMilliseconds(200), .awake);
    const started = Io.Clock.now(.awake, testing.io).toMilliseconds();
    const result = fut.cancel(testing.io);
    const took = Io.Clock.now(.awake, testing.io).toMilliseconds() - started;
    try testing.expectError(error.Canceled, result);
    try testing.expect(took < 2500);
}

test "a Location is relative when it has no scheme and no host" {
    for ([_][]const u8{ "/x", "x", "ok?from=relative", "../x", "?q=1", "", "#frag", "1http:x", ":x" }) |loc| try testing.expect(isRelativeReference(loc));
    for ([_][]const u8{ "http://h/x", "ftp://h/x", "//host/x", "magnet:?xt=1", "a+b-c.d:x" }) |loc| try testing.expect(!isRelativeReference(loc));
}
