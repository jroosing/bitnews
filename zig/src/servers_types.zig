//! A Usenet Server and the rules for its fields (internal/core/server.go).
//!
//! TLS is always on and certificates are always verified: the type has no
//! field to change either, on purpose.
//!
//! Memory: a `Server` owns its strings and its password, all allocated by the
//! allocator given to `deinit`. Servers handed out by the repo and the service
//! live in a request arena; `deinit` still wipes the password first, so it
//! does not linger in memory the arena reuses.

const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Secret = @import("secret.zig").Secret;
const log_text = @import("log_text.zig");

/// The NNTP-over-TLS port (RFC 8143) a Server gets when none is given.
pub const default_port = 563;
/// What a Server gets when the client leaves max_connections out.
pub const default_max_connections = 8;

pub const max_name_bytes = 64;
pub const max_host_bytes = 253;
pub const max_label_bytes = 63;
pub const max_credential_bytes = 256;
pub const max_connections_limit = 100;

pub const Server = struct {
    id: []const u8 = "",
    /// A unique label shown to the user.
    name: []const u8 = "",
    /// A bare host name or IP address; also the name the certificate must match.
    host: []const u8 = "",
    port: i64 = default_port,
    /// Empty for a Server that needs no authentication.
    username: []const u8 = "",
    password: ?Secret = null,
    max_connections: i64 = default_max_connections,
    /// 0 means unlimited.
    retention_days: i64 = 0,
    /// Servers with the same value share load; a higher number is used only
    /// for Articles the lower ones do not have.
    priority: i64 = 0,
    enabled: bool = true,
    created_at_ms: i64 = 0,
    /// Set by the repo when the stored password could not be opened (the key
    /// changed or is missing). `password` is null then. Saving the Server with
    /// this flag set leaves the stored password untouched.
    password_unavailable: bool = false,

    pub fn hasPassword(self: Server) bool {
        return self.password_unavailable or (self.password != null and self.password.?.len > 0);
    }

    /// The password, or "" when there is none.
    pub fn passwordBytes(self: Server) []const u8 {
        return if (self.password) |p| p.reveal() else "";
    }

    pub fn deinit(self: *Server, alloc: Allocator) void {
        if (self.password) |*p| p.deinit();
        alloc.free(self.id);
        alloc.free(self.name);
        alloc.free(self.host);
        alloc.free(self.username);
        self.* = undefined;
    }
};

pub fn deinitList(alloc: Allocator, servers: []Server) void {
    for (servers) |*s| s.deinit(alloc);
    alloc.free(servers);
}

/// Which rules a Server breaks. The order of the fields is the order in which
/// the API reports them.
pub const Problems = struct {
    name: bool = false,
    host: bool = false,
    port: bool = false,
    max_connections: bool = false,
    retention_days: bool = false,
    priority: bool = false,
    credentials: bool = false,

    pub fn any(p: Problems) bool {
        inline for (comptime std.meta.fieldNames(Problems)) |name| {
            if (@field(p, name)) return true;
        }
        return false;
    }

    /// The API code of the first problem.
    pub fn firstCode(p: Problems) []const u8 {
        if (p.name) return "invalid_name";
        if (p.host) return "invalid_host";
        if (p.port) return "invalid_port";
        if (p.max_connections) return "invalid_max_connections";
        if (p.retention_days) return "invalid_retention_days";
        if (p.priority) return "invalid_priority";
        return "invalid_credentials";
    }
};

/// Checks every rule. Credentials are never part of a message.
pub fn check(s: Server) Problems {
    return .{
        .name = trimSpace(s.name).len == 0 or s.name.len > max_name_bytes or hasControl(s.name),
        .host = !validHost(s.host),
        .port = s.port < 1 or s.port > 65535,
        .max_connections = s.max_connections < 1 or s.max_connections > max_connections_limit,
        .retention_days = s.retention_days < 0,
        .priority = s.priority < 0,
        .credentials = !validCredential(s.username) or !validCredential(s.passwordBytes()),
    };
}

/// The text of every problem, joined with "; ", as app.validateServer builds
/// it. The host is quoted like Go's %q.
pub fn writeProblems(w: *Io.Writer, s: Server, p: Problems) Io.Writer.Error!void {
    var first = true;
    inline for (.{ "name", "host", "port", "max_connections", "retention_days", "priority", "credentials" }) |field| {
        if (@field(p, field)) {
            if (!first) try w.writeAll("; ");
            first = false;
            if (comptime std.mem.eql(u8, field, "name")) {
                try w.writeAll("server name must be 1 to 64 characters without control characters");
            } else if (comptime std.mem.eql(u8, field, "host")) {
                try w.writeAll("server host must be a host name or IP address, without scheme, port or path: ");
                try log_text.writeQuoted(w, s.host);
            } else if (comptime std.mem.eql(u8, field, "port")) {
                try w.print("server port must be 1 to 65535: {d}", .{s.port});
            } else if (comptime std.mem.eql(u8, field, "max_connections")) {
                try w.print("server max connections must be 1 to 100: {d}", .{s.max_connections});
            } else if (comptime std.mem.eql(u8, field, "retention_days")) {
                try w.writeAll("server retention days must not be negative");
            } else if (comptime std.mem.eql(u8, field, "priority")) {
                try w.writeAll("server priority must not be negative");
            } else {
                try w.writeAll("server username and password must be at most 256 bytes without control characters");
            }
        }
    }
}

/// unicode.IsControl: U+0000 to U+001F and U+007F to U+009F. Bytes that are
/// not valid UTF-8 are not control characters (Go decodes them as U+FFFD), and
/// a control byte can never be part of a longer sequence, so a byte scan does.
pub fn hasControl(s: []const u8) bool {
    for (s, 0..) |b, i| {
        if (b < 0x20 or b == 0x7f) return true;
        // U+0080 to U+009F are C2 80 to C2 9F.
        if (b == 0xc2 and i + 1 < s.len and s[i + 1] >= 0x80 and s[i + 1] <= 0x9f) return true;
    }
    return false;
}

fn validCredential(s: []const u8) bool {
    return s.len <= max_credential_bytes and !hasControl(s);
}

fn isSpaceRune(cp: u21) bool {
    return switch (cp) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        0x2000...0x200a => true,
        else => false,
    };
}

/// strings.TrimSpace: Unicode white space at both ends. Invalid UTF-8 is
/// never white space.
pub fn trimSpace(s: []const u8) []const u8 {
    var start: usize = 0;
    while (start < s.len) {
        const n = std.unicode.utf8ByteSequenceLength(s[start]) catch break;
        if (start + n > s.len) break;
        const cp = std.unicode.utf8Decode(s[start..][0..n]) catch break;
        if (!isSpaceRune(cp)) break;
        start += n;
    }
    var end = s.len;
    while (end > start) {
        // The last rune begins at most 3 continuation bytes back.
        var b = end - 1;
        const lim = end -| 4;
        while (b > lim and s[b] & 0xc0 == 0x80) b -= 1;
        const n = std.unicode.utf8ByteSequenceLength(s[b]) catch break;
        if (b + n != end or b < start) break;
        const cp = std.unicode.utf8Decode(s[b..end]) catch break;
        if (!isSpaceRune(cp)) break;
        end = b;
    }
    return s[start..end];
}

/// An IP address (without brackets or zone) or a DNS name of ASCII letters,
/// digits, '-' and '_' (Docker service names use it). Internationalized names
/// must be given in their punycode form.
pub fn validHost(h: []const u8) bool {
    if (h.len == 0 or h.len > max_host_bytes) return false;
    if (std.mem.findScalar(u8, h, ':') != null) return validIpv6(h);
    var labels = std.mem.splitScalar(u8, h, '.');
    while (labels.next()) |label| {
        if (label.len == 0 or label.len > max_label_bytes or label[0] == '-' or label[label.len - 1] == '-') return false;
        for (label) |c| {
            switch (c) {
                'a'...'z', 'A'...'Z', '0'...'9', '-', '_' => {},
                else => return false,
            }
        }
    }
    return true;
}

/// netip.ParseAddr for a string with a colon, without zone: the same
/// strictness (one "::", at most 4 hex digits of value per group, an embedded
/// IPv4 address only as the last 32 bits).
fn validIpv6(str: []const u8) bool {
    var s = str;
    var ellipsis: ?usize = null;
    var i: usize = 0;
    if (s.len >= 2 and s[0] == ':' and s[1] == ':') {
        ellipsis = 0;
        s = s[2..];
        if (s.len == 0) return true;
    }
    while (i < 16) {
        var off: usize = 0;
        var acc: u32 = 0;
        while (off < s.len) : (off += 1) {
            const d = std.fmt.charToDigit(s[off], 16) catch break;
            acc = (acc << 4) + d;
            if (acc > 0xffff) return false;
        }
        if (off == 0 or off > 4) return false;
        if (off < s.len and s[off] == '.') {
            if (ellipsis == null and i != 12) return false;
            if (i + 4 > 16) return false;
            if (!validIpv4(s)) return false;
            s = s[s.len..];
            i += 4;
            break;
        }
        i += 2;
        s = s[off..];
        if (s.len == 0) break;
        if (s[0] != ':' or s.len == 1) return false;
        s = s[1..];
        if (s[0] == ':') {
            if (ellipsis != null) return false;
            ellipsis = i;
            s = s[1..];
            if (s.len == 0) break;
        }
    }
    if (s.len != 0) return false;
    if (i < 16) return ellipsis != null;
    return ellipsis == null;
}

/// Four decimal fields of at most 255 without leading zeros.
fn validIpv4(s: []const u8) bool {
    var fields: usize = 0;
    var parts = std.mem.splitScalar(u8, s, '.');
    while (parts.next()) |p| {
        fields += 1;
        if (fields > 4 or p.len == 0 or p.len > 3) return false;
        if (p.len > 1 and p[0] == '0') return false;
        var v: u32 = 0;
        for (p) |c| {
            if (c < '0' or c > '9') return false;
            v = v * 10 + (c - '0');
        }
        if (v > 255) return false;
    }
    return fields == 4;
}

// ---- tests ----

const testing = std.testing;

fn rep(comptime c: u8, comptime n: usize) *const [n]u8 {
    const a: [n]u8 = @splat(c);
    return &a;
}

fn valid() Server {
    return .{ .id = "id", .name = "Newshosting", .host = "news.example.com", .port = 563, .username = "joe", .max_connections = 20 };
}

test "a valid server has no problems" {
    try testing.expect(!check(valid()).any());
}

test "hosts: names and IP addresses are accepted" {
    // The list of core/server_test.go, plus netip edge cases.
    for ([_][]const u8{
        "news.example.com", "localhost", "a-b.c", "usenet_1",       "10.0.0.5",        "::1",                 "2001:db8::1",
        "NEWS.Example.COM", "::",        "1::",   "::ffff:1.2.3.4", "1:2:3:4:5:6:7:8", "1:2:3:4:5:6:1.2.3.4", "1:2:3:4:5:6:7::",
    }) |h| try testing.expect(validHost(h));
    var long_label: [67]u8 = undefined;
    @memset(&long_label, 'a');
    @memcpy(long_label[63..], ".com");
    try testing.expect(validHost(long_label[0..67]));
}

test "hosts: anything that is not a bare host is refused" {
    for ([_][]const u8{
        "",                           " ",                "nntps://news.example.com", "news.example.com/path", "news.example.com:563",
        "user@news.example.com",      "news example.com", "-bad.example.com",         "bad-.example.com",      "a..b",
        ".a",                         "a.",               "[::1]",                    "news.example.com?x=1",  "new\x00s",
        "\xc3\xbcnicode.example.com", "fe80::1%eth0",     "fe80::1%",                 ":::",                   "1::2::3",
        "1:2:3:4:5:6:7:8:9",          "1:2:3:4:5:6:7",    "12345::1",                 "::1.2.3",               "::01.2.3.4",
        "::1.2.3.256",                "00000001::",       "1:2:3:4:5:6:7:8::",        "::1:2:3:4:5:6:7:8",     "1.2.3.4:5",
        "1:2:3:4:5:6:7::8",           ":1::2",            "1::2:",
    }) |h| try testing.expect(!validHost(h));
    var big: [64 + 4]u8 = undefined;
    @memset(&big, 'a');
    @memcpy(big[64..], ".com");
    try testing.expect(!validHost(&big));
    var dots: [260]u8 = undefined;
    for (&dots, 0..) |*c, i| c.* = if (i % 2 == 0) 'a' else '.';
    try testing.expect(!validHost(&dots));
}

test "field limits" {
    const Case = struct { mut: *const fn (*Server) void, field: []const u8 };
    const cases = [_]Case{
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.name = "";
            }
        }.f, .field = "name" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.name = "  ";
            }
        }.f, .field = "name" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.name = rep('a', 65);
            }
        }.f, .field = "name" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.name = "a\nb";
            }
        }.f, .field = "name" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.port = 0;
            }
        }.f, .field = "port" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.port = 65536;
            }
        }.f, .field = "port" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.max_connections = 0;
            }
        }.f, .field = "max_connections" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.max_connections = 101;
            }
        }.f, .field = "max_connections" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.retention_days = -1;
            }
        }.f, .field = "retention_days" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.priority = -1;
            }
        }.f, .field = "priority" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.username = "a\r\nQUIT";
            }
        }.f, .field = "credentials" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.username = rep('u', 257);
            }
        }.f, .field = "credentials" },
        .{ .mut = struct {
            fn f(s: *Server) void {
                s.username = "a\xc2\x85b";
            }
        }.f, .field = "credentials" },
    };
    for (cases) |c| {
        var s = valid();
        c.mut(&s);
        const p = check(s);
        try testing.expect(p.any());
        inline for (comptime std.meta.fieldNames(Problems)) |name| {
            try testing.expectEqual(std.mem.eql(u8, name, c.field), @field(p, name));
        }
    }
    for ([_]Server{
        .{ .name = "n", .host = "h", .port = 1, .max_connections = 1 },
        .{ .name = "n", .host = "h", .port = 65535, .max_connections = 100, .retention_days = 5000 },
    }) |s| try testing.expect(!check(s).any());
}

test "a password with a control character is refused without echoing it" {
    var pw = try Secret.init(testing.allocator, "a\x00b");
    defer pw.deinit();
    var s = valid();
    s.password = pw;
    const p = check(s);
    try testing.expect(p.credentials);
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    try writeProblems(&aw.writer, s, p);
    try testing.expectEqualStrings("server username and password must be at most 256 bytes without control characters", aw.written());
}

test "hasControl follows unicode.IsControl" {
    try testing.expect(hasControl("a\x7fb"));
    try testing.expect(hasControl("a\xc2\x80"));
    try testing.expect(hasControl("\xc2\x9f"));
    try testing.expect(!hasControl("\xc2\xa0")); // no-break space is not a control character
    try testing.expect(!hasControl("\xc2")); // a lone lead byte is invalid, not control
    try testing.expect(!hasControl("\xff\xfe"));
    try testing.expect(!hasControl("p\xc3\xa4ssw\xc3\xb6rd"));
}

test "trimSpace follows strings.TrimSpace" {
    try testing.expectEqualStrings("a b", trimSpace(" \t a b\r\n"));
    try testing.expectEqualStrings("a", trimSpace("\xc2\xa0a\xe3\x80\x80")); // NBSP, ideographic space
    try testing.expectEqualStrings("", trimSpace(" \xe2\x80\x83 "));
    try testing.expectEqualStrings("a\xff", trimSpace(" a\xff "));
    try testing.expectEqualStrings("\xc2", trimSpace(" \xc2"));
    try testing.expectEqualStrings("\xe2\x80", trimSpace("\xe2\x80 "));
    try testing.expectEqualStrings("", trimSpace(""));
}

test "problem messages are the Go messages, joined with a semicolon" {
    var s = valid();
    s.name = "";
    s.host = "a b\n";
    s.port = 70000;
    s.max_connections = 0;
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    const p = check(s);
    try writeProblems(&aw.writer, s, p);
    try testing.expectEqualStrings(
        "server name must be 1 to 64 characters without control characters; " ++
            "server host must be a host name or IP address, without scheme, port or path: \"a b\\n\"; " ++
            "server port must be 1 to 65535: 70000; server max connections must be 1 to 100: 0",
        aw.written(),
    );
    try testing.expectEqualStrings("invalid_name", p.firstCode());
}

fn fuzzHost(_: void, smith: *testing.Smith) !void {
    var buf: [300]u8 = undefined;
    const n = smith.slice(&buf);
    const h = buf[0..n];
    _ = trimSpace(h);
    _ = hasControl(h);
    if (!validHost(h)) return;
    // An accepted host is short and holds only what a host name or an IPv6
    // address can hold.
    try testing.expect(h.len >= 1 and h.len <= max_host_bytes);
    for (h) |c| try testing.expect(std.ascii.isAlphanumeric(c) or c == '-' or c == '_' or c == '.' or c == ':');
}

fn seed(comptime x: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.*);
}

test "fuzz: host validation and trimming never crash" {
    try testing.fuzz({}, fuzzHost, .{ .corpus = &.{
        seed("news.example.com"),
        seed("::ffff:1.2.3.4"),
        seed("1:2:3:4:5:6:7:8"),
        seed("fe80::1%eth0"),
        seed("a..b"),
        seed(" \xc2\xa0x\xe3\x80\x80"),
        seed("\xff\xfe:::"),
    } });
}
