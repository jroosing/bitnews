//! Fetching an NZB from a URL (internal/app/fetch.go): the seam the add
//! service calls (`Fetcher`, a function pointer plus context, so tests plug in
//! a fake), the URL check that decides "invalid_url", and the texts a failed
//! fetch shows. The real fetcher is add_curl.zig.
//!
//! The rules every fetcher keeps (Go's newFetchClient): http and https only,
//! also across redirects; at most five redirects; no cookies, no proxy from
//! the environment, no netrc; credentials only when the URL itself has them;
//! strict TLS; and a URL is never logged or put in an error text, because it
//! may carry an API key.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;

/// The total time a fetch may take, redirects and body included.
pub const default_timeout_ms: u32 = 30_000;
/// How many redirects a fetch follows.
pub const max_redirects: u8 = 5;
/// Identifies bitnews to indexers.
pub const user_agent = "bitnews (+https://github.com/jroosing/bitnews)";
/// The Accept header of a link that may be an NZB or a .torrent.
pub const accept_any = "application/x-bittorrent, application/x-nzb, text/xml;q=0.9, */*;q=0.1";
/// The Accept header of a link that must be an NZB.
pub const accept_nzb = "application/x-nzb, text/xml;q=0.9, */*;q=0.1";

pub const Request = struct {
    /// Passed `parseHttpUrl`.
    url: []const u8,
    accept: []const u8 = accept_any,
    /// The body may be at most this long (Content-Length or counted); more is
    /// `Outcome.too_large`.
    max_bytes: u64,
    timeout_ms: u32 = default_timeout_ms,
};

/// How a fetch ended. Memory in a result is allocated with the `gpa` given to
/// the fetcher; `deinit` frees it.
pub const Outcome = union(enum) {
    /// The server answered 200 with this body.
    body: []u8,
    /// A redirect led to a "magnet:" link, which is in the Location header
    /// (Go: indexer proxies answer a torrent download that way). The fetcher
    /// does not look inside it.
    magnet: []u8,
    /// Any other answer, not followed (a 3xx without Location included).
    status: u16,
    /// Content-Length or the body is over `max_bytes`.
    too_large,
    /// The server could not be used; the text is what Go's transport says
    /// ("dial tcp ...: connect: connection refused", "stopped after 5
    /// redirects", ...) and holds no URL.
    failed: []u8,
    /// The timeout passed.
    timeout,

    pub fn deinit(o: Outcome, gpa: Allocator) void {
        switch (o) {
            .body, .magnet, .failed => |bytes| gpa.free(bytes),
            else => {},
        }
    }
};

pub const FetchError = Allocator.Error || Io.Cancelable;

/// Fetches `req.url`. May block for the timeout: run it where blocking is
/// fine (a request task), and it ends early with `error.Canceled` when the
/// task is cancelled.
pub const Fetcher = struct {
    ctx: ?*anyopaque = null,
    fetch_fn: *const fn (ctx: ?*anyopaque, gpa: Allocator, io: Io, req: Request) FetchError!Outcome,

    pub fn fetch(f: Fetcher, gpa: Allocator, io: Io, req: Request) FetchError!Outcome {
        return f.fetch_fn(f.ctx, gpa, io, req);
    }
};

/// Go's http.StatusText: "" for a code it does not know.
pub fn statusText(code: u16) []const u8 {
    const table = [_]struct { u16, []const u8 }{
        .{ 100, "Continue" },                        .{ 101, "Switching Protocols" },
        .{ 102, "Processing" },                      .{ 103, "Early Hints" },
        .{ 200, "OK" },                              .{ 201, "Created" },
        .{ 202, "Accepted" },                        .{ 203, "Non-Authoritative Information" },
        .{ 204, "No Content" },                      .{ 205, "Reset Content" },
        .{ 206, "Partial Content" },                 .{ 207, "Multi-Status" },
        .{ 208, "Already Reported" },                .{ 226, "IM Used" },
        .{ 300, "Multiple Choices" },                .{ 301, "Moved Permanently" },
        .{ 302, "Found" },                           .{ 303, "See Other" },
        .{ 304, "Not Modified" },                    .{ 305, "Use Proxy" },
        .{ 307, "Temporary Redirect" },              .{ 308, "Permanent Redirect" },
        .{ 400, "Bad Request" },                     .{ 401, "Unauthorized" },
        .{ 402, "Payment Required" },                .{ 403, "Forbidden" },
        .{ 404, "Not Found" },                       .{ 405, "Method Not Allowed" },
        .{ 406, "Not Acceptable" },                  .{ 407, "Proxy Authentication Required" },
        .{ 408, "Request Timeout" },                 .{ 409, "Conflict" },
        .{ 410, "Gone" },                            .{ 411, "Length Required" },
        .{ 412, "Precondition Failed" },             .{ 413, "Request Entity Too Large" },
        .{ 414, "Request URI Too Long" },            .{ 415, "Unsupported Media Type" },
        .{ 416, "Requested Range Not Satisfiable" }, .{ 417, "Expectation Failed" },
        .{ 418, "I'm a teapot" },                    .{ 421, "Misdirected Request" },
        .{ 422, "Unprocessable Entity" },            .{ 423, "Locked" },
        .{ 424, "Failed Dependency" },               .{ 425, "Too Early" },
        .{ 426, "Upgrade Required" },                .{ 428, "Precondition Required" },
        .{ 429, "Too Many Requests" },               .{ 431, "Request Header Fields Too Large" },
        .{ 451, "Unavailable For Legal Reasons" },   .{ 500, "Internal Server Error" },
        .{ 501, "Not Implemented" },                 .{ 502, "Bad Gateway" },
        .{ 503, "Service Unavailable" },             .{ 504, "Gateway Timeout" },
        .{ 505, "HTTP Version Not Supported" },      .{ 506, "Variant Also Negotiates" },
        .{ 507, "Insufficient Storage" },            .{ 508, "Loop Detected" },
        .{ 510, "Not Extended" },                    .{ 511, "Network Authentication Required" },
    };
    for (table) |entry| if (entry[0] == code) return entry[1];
    return "";
}

/// "30s", the way Go prints a Duration, for "timed out after 30s".
pub fn durationText(buf: []u8, ms: u32) []const u8 {
    const secs = ms / 1000;
    const frac = ms % 1000;
    if (ms == 0) return "0s";
    if (ms < 1000) return std.fmt.bufPrint(buf, "{d}ms", .{ms}) catch "?";
    var w: Io.Writer = .fixed(buf);
    const mins = secs / 60;
    const s = secs % 60;
    if (mins >= 60) w.print("{d}h", .{mins / 60}) catch return "?";
    if (mins >= 1) w.print("{d}m", .{mins % 60}) catch return "?";
    if (frac == 0) {
        w.print("{d}s", .{s}) catch return "?";
    } else {
        w.print("{d}.{d:0>3}", .{ s, frac }) catch return "?";
        // Go trims the trailing zeros of the fraction.
        var out = w.buffered();
        while (out[out.len - 1] == '0') out = out[0 .. out.len - 1];
        w.end = out.len;
        w.writeAll("s") catch return "?";
    }
    return w.buffered();
}

/// What `parseHttpUrl` learns.
pub const Parsed = struct {
    /// The host name, for logs (no port, no brackets, no user information).
    host: []const u8,
};

/// Go's `url.Parse` followed by the scheme and host checks of fetchNZB: null
/// unless `raw` is an absolute http or https URL with a host.
pub fn parseHttpUrl(raw: []const u8) ?Parsed {
    for (raw) |c| if (c < 0x20 or c == 0x7f) return null;
    // Scheme (Go's getScheme).
    var colon: usize = 0;
    const scheme_end: ?usize = for (raw, 0..) |c, i| {
        switch (c) {
            'a'...'z', 'A'...'Z' => {},
            '0'...'9', '+', '-', '.' => if (i == 0) break null,
            ':' => if (i == 0) return null else break i,
            else => break null,
        }
    } else null;
    colon = scheme_end orelse return null;
    const scheme = raw[0..colon];
    if (!std.ascii.eqlIgnoreCase(scheme, "http") and !std.ascii.eqlIgnoreCase(scheme, "https")) return null;

    var rest = raw[colon + 1 ..];
    // A fragment must be escaped properly; a query is kept as it is.
    if (std.mem.findScalar(u8, rest, '#')) |i| {
        if (!escapesOk(rest[i + 1 ..])) return null;
        rest = rest[0..i];
    }
    if (std.mem.findScalar(u8, rest, '?')) |i| rest = rest[0..i];
    if (!std.mem.startsWith(u8, rest, "//")) return null; // opaque, or no host
    rest = rest[2..];
    const slash = std.mem.findScalar(u8, rest, '/') orelse rest.len;
    const authority = rest[0..slash];
    if (!escapesOk(rest[slash..])) return null;

    var host_port = authority;
    if (std.mem.findScalarLast(u8, authority, '@')) |at| {
        if (!validUserinfo(authority[0..at])) return null;
        host_port = authority[at + 1 ..];
    }
    return parseHost(host_port);
}

fn parseHost(host: []const u8) ?Parsed {
    if (std.mem.findScalarLast(u8, host, '[')) |open| {
        if (open > 0) return null; // "invalid IP-literal"
        const close = std.mem.findScalarLast(u8, host, ']') orelse return null;
        if (!validOptionalPort(host[close + 1 ..])) return null;
        const name = host[1..close];
        if (name.len == 0 or !hostCharsOk(name)) return null;
        return .{ .host = name };
    }
    var name = host;
    if (std.mem.findScalar(u8, host, ':')) |i| {
        // http and https hosts have no second colon.
        if (!validOptionalPort(host[i..])) return null;
        name = host[0..i];
    }
    if (!hostCharsOk(host)) return null;
    if (name.len == 0) return null;
    return .{ .host = name };
}

fn validOptionalPort(p: []const u8) bool {
    if (p.len == 0) return true;
    if (p[0] != ':') return false;
    for (p[1..]) |c| if (c < '0' or c > '9') return false;
    return true;
}

/// Every % is followed by two hex digits (Go: `i+2 >= len(s)` also refuses a
/// % in the last two positions).
fn escapesOk(s: []const u8) bool {
    var i: usize = 0;
    while (i < s.len) : (i += 1) {
        if (s[i] != '%') continue;
        if (i + 2 >= s.len or !std.ascii.isHex(s[i + 1]) or !std.ascii.isHex(s[i + 2])) return false;
        i += 2;
    }
    return true;
}

/// The bytes Go lets into a host: unreserved, sub-delimiters, ':', brackets,
/// '<', '>', '"', non-ASCII, and %-escapes of non-ASCII bytes (or %25).
fn hostCharsOk(s: []const u8) bool {
    var i: usize = 0;
    while (i < s.len) {
        const c = s[i];
        if (c == '%') {
            if (i + 2 >= s.len or !std.ascii.isHex(s[i + 1]) or !std.ascii.isHex(s[i + 2])) return false;
            const first = std.fmt.charToDigit(s[i + 1], 16) catch return false;
            if (first < 8 and !std.mem.eql(u8, s[i .. i + 3], "%25")) return false;
            i += 3;
            continue;
        }
        if (c < 0x80) {
            switch (c) {
                'a'...'z', 'A'...'Z', '0'...'9', '-', '_', '.', '~', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '[', ']', '<', '>', '"' => {},
                else => return false,
            }
        }
        i += 1;
    }
    return true;
}

/// Go's validUserinfo.
fn validUserinfo(s: []const u8) bool {
    for (s) |c| {
        switch (c) {
            'a'...'z', 'A'...'Z', '0'...'9', '-', '.', '_', ':', '~', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', '%', '@' => {},
            else => return false,
        }
    }
    return escapesOk(s);
}

const testing = std.testing;

test "only absolute http and https URLs with a host pass" {
    const good = [_][]const u8{
        "http://host/a.nzb",
        "https://host.example:8443/get/x.nzb?apikey=abc&t=get",
        "HTTP://HOST/x",
        "http://user:p%40ss@host/x",
        "http://127.0.0.1:9696/3/download?link=aHR0cHM6Ly9leGFtcGxl&file=x",
        "http://[::1]:9494/x",
        "http://host",
        "http://host?x=1",
        "http://host/a%20b",
        "http://prowlarr:9696/x#frag",
        "http://host/a b", // Go accepts a space in the path and escapes it
    };
    for (good) |u| try testing.expect(parseHttpUrl(u) != null);
    const bad = [_][]const u8{
        "",
        "ftp://host.example/a.nzb",
        "file:///etc/passwd",
        "not a url",
        "/relative/a.nzb",
        "http://",
        "//host.example/a.nzb",
        "javascript:alert(1)",
        "http:///nohost",
        "http:/nohost",
        "http:host",
        "http://:80/x",
        "http://host:port/x",
        "http://host:80:90/x",
        "http://ho st/x",
        "http://host/%zz",
        "http://host/x#%zz",
        "http://host/%4",
        "http://host/\x01",
        "http://[::1/x",
        "http://a[::1]/x",
        "http://us er@host/x",
        "http://%41bc/x",
        "1http://host/x",
        ":http://host/x",
        "magnet:?xt=urn:btih:abc",
    };
    for (bad) |u| try testing.expect(parseHttpUrl(u) == null);
    try testing.expectEqualStrings("host.example", parseHttpUrl("https://u:p@host.example:8443/x").?.host);
    try testing.expectEqualStrings("::1", parseHttpUrl("http://[::1]:9494/x").?.host);
}

test "status texts are Go's" {
    try testing.expectEqualStrings("Not Found", statusText(404));
    try testing.expectEqualStrings("Request Entity Too Large", statusText(413));
    try testing.expectEqualStrings("Internal Server Error", statusText(500));
    try testing.expectEqualStrings("", statusText(299));
}

test "a duration prints like Go's" {
    var buf: [32]u8 = undefined;
    try testing.expectEqualStrings("30s", durationText(&buf, 30_000));
    try testing.expectEqualStrings("1m0s", durationText(&buf, 60_000));
    try testing.expectEqualStrings("1m30s", durationText(&buf, 90_000));
    try testing.expectEqualStrings("1.5s", durationText(&buf, 1_500));
    try testing.expectEqualStrings("250ms", durationText(&buf, 250));
    try testing.expectEqualStrings("1h0m0s", durationText(&buf, 3_600_000));
}

test "fuzz: parseHttpUrl never crashes" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [300]u8 = undefined;
            const n = smith.slice(&buf);
            _ = parseHttpUrl(buf[0..n]);
        }
    }.one, .{ .corpus = &.{
        "\x16\x00\x00\x00http://u:p@host:80/x?y#z",
        "\x0c\x00\x00\x00http://[::1]",
    } });
}
