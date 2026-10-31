//! SABnzbd human-readable fail_message strings; mirrors Go `failureText` in
//! internal/adapter/sabnzbd/status.go, and `view.CleanDetail`.
const std = @import("std");
const download = @import("download.zig");
const Allocator = std.mem.Allocator;

/// Human-readable fail_message for a Failed Download (SAB wording + optional detail).
pub fn failureText(arena: std.mem.Allocator, reason: download.FailureReason, detail: []const u8) ![]const u8 {
    const base: []const u8 = switch (reason) {
        .missing_articles => "Aborted, cannot be completed - articles are missing and no repair data can recover them",
        .repair_insufficient => "Repair failed, not enough repair blocks",
        .archive_unsupported => "Unpacking failed, unsupported archive format",
        .archive_password => "Unpacking failed, archive requires a password",
        .archive_corrupt => "Unpacking failed, archive is damaged",
        .disk_full => "Aborted, not enough free disk space",
        .path_violation => "Aborted, the download contains an unsafe file path",
        .source_invalid => "Aborted, the NZB cannot be used",
        else => "Download failed",
    };
    if (detail.len == 0) return base;
    return std.mem.concat(arena, u8, &.{ base, ": ", detail });
}

/// Failure detail safe for UI; mirrors Go `view.CleanDetail` (URL redaction + cleanup).
/// The result lives in `arena`.
pub fn cleanDetail(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    const urls = try urlsIn(arena, s);
    const mapped = try mapDetail(arena, urls);
    const collapsed = try collapseFields(arena, mapped);
    return capDetail(arena, collapsed);
}

const max_detail_bytes: usize = 300;

// Go regexp `\s` is the ASCII class, not Unicode space.
fn isUrlStop(c: u8) bool {
    return c == ' ' or c == '\t' or c == '\n' or c == '\r' or c == 0x0c or c == '"' or c == '\'';
}

fn isSchemeChar(c: u8) bool {
    return std.ascii.isAlphanumeric(c) or c == '+' or c == '.' or c == '-';
}

/// End index of a `[A-Za-z][A-Za-z0-9+.-]*:(?://|\?)[^\s"']+` match at `i`, if any.
fn matchUrl(s: []const u8, i: usize) ?usize {
    if (i >= s.len or !std.ascii.isAlphabetic(s[i])) return null;
    var j = i + 1;
    while (j < s.len and isSchemeChar(s[j])) j += 1;
    if (j >= s.len or s[j] != ':') return null;
    j += 1;
    if (j + 1 < s.len and s[j] == '/' and s[j + 1] == '/') {
        j += 2;
    } else if (j < s.len and s[j] == '?') {
        j += 1;
    } else return null;
    if (j >= s.len or isUrlStop(s[j])) return null;
    while (j < s.len and !isUrlStop(s[j])) j += 1;
    return j;
}

fn urlsIn(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) {
        if (matchUrl(s, i)) |end| {
            const u = s[i..end];
            const rep = if (u.len >= 7 and std.ascii.eqlIgnoreCase(u[0..7], "magnet:"))
                "magnet:?[redacted]"
            else
                try redactUrl(arena, u, 0);
            try out.appendSlice(arena, rep);
            i = end;
        } else {
            try out.append(arena, s[i]);
            i += 1;
        }
    }
    return out.items;
}

// ---- URL redaction (Go net/url.Parse + redact.URL) ----
//
// std.Uri is the wrong seam: Go keeps an empty password (`user:@host`),
// lowercases the scheme, cuts the query before the path, and rejects ASCII
// controls. Those are the cases CleanDetail's later tests rely on.

const invalid_url = "[invalid URL redacted]";
const mask = "REDACTED";
const max_nesting: u8 = 4;
const hexdigits = "0123456789ABCDEF";

const Enc = enum { path, host, user, query, fragment };

const GoUrl = struct {
    scheme: []const u8 = "",
    opaque_data: []const u8 = "",
    has_user: bool = false,
    username: []const u8 = "",
    password_set: bool = false,
    host: []const u8 = "",
    omit_host: bool = false,
    path: []const u8 = "",
    force_query: bool = false,
    raw_query: []const u8 = "",
    fragment: []const u8 = "",
};

fn shouldEscape(c: u8, mode: Enc) bool {
    if (std.ascii.isAlphanumeric(c)) return false;
    if (mode == .host) {
        switch (c) {
            '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '[', ']', '<', '>', '"' => return false,
            else => {},
        }
    }
    switch (c) {
        '-', '_', '.', '~' => return false,
        '$', '&', '+', ',', '/', ':', ';', '=', '?', '@' => switch (mode) {
            .path => return c == '?',
            .user => return c == '@' or c == '/' or c == '?' or c == ':',
            .query => return true,
            .fragment => return false,
            .host => {},
        },
        else => {},
    }
    if (mode == .fragment) {
        switch (c) {
            '!', '(', ')', '*' => return false,
            else => {},
        }
    }
    return true;
}

fn unhex(c: u8) u8 {
    return switch (c) {
        '0'...'9' => c - '0',
        'a'...'f' => c - 'a' + 10,
        'A'...'F' => c - 'A' + 10,
        else => 0,
    };
}

fn encodingOk(s: []const u8, mode: Enc) bool {
    var i: usize = 0;
    while (i < s.len) {
        if (s[i] == '%') {
            if (i + 2 >= s.len or !std.ascii.isHex(s[i + 1]) or !std.ascii.isHex(s[i + 2])) return false;
            if (mode == .host and unhex(s[i + 1]) < 8 and !(s[i + 1] == '2' and s[i + 2] == '5')) return false;
            i += 3;
        } else {
            if (mode == .host and s[i] < 0x80 and shouldEscape(s[i], .host)) return false;
            i += 1;
        }
    }
    return true;
}

/// Go's unescape. Null is a bad escape (the caller turns that into invalid_url).
fn unescape(arena: Allocator, s: []const u8, mode: Enc) Allocator.Error!?[]const u8 {
    if (!encodingOk(s, mode)) return null;
    var n: usize = 0;
    var has_plus = false;
    var i: usize = 0;
    while (i < s.len) : (i += 1) {
        if (s[i] == '%') {
            n += 1;
            i += 2;
        } else if (s[i] == '+' and mode == .query) has_plus = true;
    }
    if (n == 0 and !has_plus) return s;
    const out = try arena.alloc(u8, s.len - 2 * n);
    var j: usize = 0;
    i = 0;
    while (i < s.len) {
        if (s[i] == '%') {
            out[j] = (unhex(s[i + 1]) << 4) | unhex(s[i + 2]);
            j += 1;
            i += 3;
        } else {
            out[j] = if (s[i] == '+' and mode == .query) ' ' else s[i];
            j += 1;
            i += 1;
        }
    }
    return out;
}

fn escape(arena: Allocator, s: []const u8, mode: Enc) Allocator.Error![]const u8 {
    var hexs: usize = 0;
    for (s) |c| {
        if (shouldEscape(c, mode) and !(c == ' ' and mode == .query)) hexs += 1;
    }
    if (hexs == 0) {
        if (mode != .query or std.mem.indexOfScalar(u8, s, ' ') == null) return s;
    }
    const out = try arena.alloc(u8, s.len + 2 * hexs);
    var j: usize = 0;
    for (s) |c| {
        if (c == ' ' and mode == .query) {
            out[j] = '+';
            j += 1;
        } else if (shouldEscape(c, mode)) {
            out[j] = '%';
            out[j + 1] = hexdigits[c >> 4];
            out[j + 2] = hexdigits[c & 15];
            j += 3;
        } else {
            out[j] = c;
            j += 1;
        }
    }
    return out;
}

fn validEncoded(s: []const u8, mode: Enc) bool {
    for (s) |c| {
        switch (c) {
            '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@', '[', ']', '%' => {},
            else => if (shouldEscape(c, mode)) return false,
        }
    }
    return true;
}

fn componentEscaped(arena: Allocator, raw: []const u8, mode: Enc) Allocator.Error!?[]const u8 {
    const unesc = (try unescape(arena, raw, mode)) orelse return null;
    const def = try escape(arena, unesc, mode);
    if (std.mem.eql(u8, raw, def) or validEncoded(raw, mode)) return raw;
    return def;
}

fn containsCtl(s: []const u8) bool {
    for (s) |b| if (b < ' ' or b == 0x7f) return true;
    return false;
}

fn lowerAscii(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    for (s) |c| {
        if (c >= 'A' and c <= 'Z') {
            const out = try arena.alloc(u8, s.len);
            for (s, out) |b, *o| o.* = if (b >= 'A' and b <= 'Z') b + ('a' - 'A') else b;
            return out;
        }
    }
    return s;
}

const SchemeSplit = struct { scheme: []const u8, rest: []const u8, bad: bool };

fn getScheme(raw: []const u8) SchemeSplit {
    for (raw, 0..) |c, i| {
        switch (c) {
            'a'...'z', 'A'...'Z' => {},
            '0'...'9', '+', '-', '.' => if (i == 0) return .{ .scheme = "", .rest = "", .bad = true },
            ':' => {
                if (i == 0) return .{ .scheme = "", .rest = "", .bad = true };
                return .{ .scheme = raw[0..i], .rest = raw[i + 1 ..], .bad = false };
            },
            else => return .{ .scheme = "", .rest = raw, .bad = false },
        }
    }
    return .{ .scheme = "", .rest = raw, .bad = false };
}

fn countScalar(s: []const u8, c: u8) usize {
    var n: usize = 0;
    for (s) |b| {
        if (b == c) n += 1;
    }
    return n;
}

fn validOptionalPort(port: []const u8) bool {
    if (port.len == 0) return true;
    if (port[0] != ':') return false;
    for (port[1..]) |b| if (b < '0' or b > '9') return false;
    return true;
}

fn validUserinfo(s: []const u8) bool {
    for (s) |c| {
        if (std.ascii.isAlphanumeric(c)) continue;
        switch (c) {
            '-', '.', '_', ':', '~', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', '%', '@' => {},
            else => return false,
        }
    }
    return true;
}

/// Enough of an IPv6 literal for Go's netip check. Zone identifiers (`%25`) are
/// rejected: Go accepts them, and no CleanDetail test uses one.
fn looksLikeIpv6(s: []const u8) bool {
    if (s.len == 0 or std.mem.indexOfScalar(u8, s, ':') == null) return false;
    for (s) |c| if (!std.ascii.isHex(c) and c != ':' and c != '.') return false;
    return true;
}

fn parseHost(arena: Allocator, scheme: []const u8, host: []const u8) Allocator.Error!?[]const u8 {
    if (std.mem.lastIndexOfScalar(u8, host, '[')) |open| {
        if (open != 0) return null;
        const close = std.mem.lastIndexOfScalar(u8, host, ']') orelse return null;
        if (close < 1) return null;
        const colon_port = host[close + 1 ..];
        if (!validOptionalPort(colon_port)) return null;
        const colon_unesc = (try unescape(arena, colon_port, .host)) orelse return null;
        const hostname = host[1..close];
        if (std.mem.indexOfScalar(u8, hostname, '%') != null) return null;
        const name = (try unescape(arena, hostname, .host)) orelse return null;
        if (!looksLikeIpv6(name)) return null;
        return try std.mem.concat(arena, u8, &.{ "[", name, "]", colon_unesc });
    }
    if (std.mem.indexOfScalar(u8, host, ':')) |first| {
        const last = std.mem.lastIndexOfScalar(u8, host, ':').?;
        const at = if (last != first and !std.mem.eql(u8, scheme, "http") and !std.mem.eql(u8, scheme, "https")) last else first;
        if (!validOptionalPort(host[at..])) return null;
    }
    return try unescape(arena, host, .host);
}

const Authority = struct {
    has_user: bool,
    username: []const u8,
    password_set: bool,
    host: []const u8,
};

fn parseAuthority(arena: Allocator, scheme: []const u8, authority: []const u8) Allocator.Error!?Authority {
    const at = std.mem.lastIndexOfScalar(u8, authority, '@');
    const host_raw = if (at) |i| authority[i + 1 ..] else authority;
    const host = (try parseHost(arena, scheme, host_raw)) orelse return null;
    const userinfo = if (at) |i| authority[0..i] else return .{
        .has_user = false,
        .username = "",
        .password_set = false,
        .host = host,
    };
    if (!validUserinfo(userinfo)) return null;
    if (std.mem.indexOfScalar(u8, userinfo, ':')) |colon| {
        const name = (try unescape(arena, userinfo[0..colon], .user)) orelse return null;
        if (!encodingOk(userinfo[colon + 1 ..], .user)) return null;
        return .{ .has_user = true, .username = name, .password_set = true, .host = host };
    }
    const name = (try unescape(arena, userinfo, .user)) orelse return null;
    return .{ .has_user = true, .username = name, .password_set = false, .host = host };
}

fn segmentHasColon(rest: []const u8) bool {
    const seg = if (std.mem.indexOfScalar(u8, rest, '/')) |i| rest[0..i] else rest;
    return std.mem.indexOfScalar(u8, seg, ':') != null;
}

fn fragmentOf(arena: Allocator, frag: ?[]const u8) Allocator.Error!?[]const u8 {
    const raw = frag orelse return "";
    if (raw.len == 0) return "";
    return componentEscaped(arena, raw, .fragment);
}

fn parseGo(arena: Allocator, raw: []const u8) Allocator.Error!?GoUrl {
    const hash = std.mem.indexOfScalar(u8, raw, '#');
    const before = if (hash) |h| raw[0..h] else raw;
    if (containsCtl(before)) return null;
    const frag_raw: ?[]const u8 = if (hash) |h| raw[h + 1 ..] else null;
    const fragment = (try fragmentOf(arena, frag_raw)) orelse return null;

    const split = getScheme(before);
    if (split.bad) return null;
    const scheme = try lowerAscii(arena, split.scheme);
    var rest = split.rest;

    var force_query = false;
    var raw_query: []const u8 = "";
    if (rest.len > 0 and rest[rest.len - 1] == '?' and countScalar(rest, '?') == 1) {
        force_query = true;
        rest = rest[0 .. rest.len - 1];
    } else if (std.mem.indexOfScalar(u8, rest, '?')) |q| {
        raw_query = rest[q + 1 ..];
        rest = rest[0..q];
    }

    var url: GoUrl = .{
        .scheme = scheme,
        .force_query = force_query,
        .raw_query = raw_query,
        .fragment = fragment,
    };

    if (!std.mem.startsWith(u8, rest, "/")) {
        if (scheme.len != 0) {
            url.opaque_data = rest;
            return url;
        }
        if (segmentHasColon(rest)) return null;
    }

    const triple = std.mem.startsWith(u8, rest, "///");
    var path_raw = rest;
    if ((scheme.len != 0 or !triple) and std.mem.startsWith(u8, rest, "//")) {
        var authority = rest[2..];
        path_raw = "";
        if (std.mem.indexOfScalar(u8, authority, '/')) |i| {
            path_raw = authority[i..];
            authority = authority[0..i];
        }
        const auth = (try parseAuthority(arena, scheme, authority)) orelse return null;
        url.has_user = auth.has_user;
        url.username = auth.username;
        url.password_set = auth.password_set;
        url.host = auth.host;
    } else if (scheme.len != 0 and std.mem.startsWith(u8, rest, "/")) {
        url.omit_host = true;
    }

    url.path = (try componentEscaped(arena, path_raw, .path)) orelse return null;
    return url;
}

fn formatUrl(arena: Allocator, u: GoUrl) Allocator.Error![]const u8 {
    var buf: std.ArrayList(u8) = .empty;
    if (u.scheme.len != 0) {
        try buf.appendSlice(arena, u.scheme);
        try buf.append(arena, ':');
    }
    if (u.opaque_data.len != 0) {
        try buf.appendSlice(arena, u.opaque_data);
    } else {
        if (u.scheme.len != 0 or u.host.len != 0 or u.has_user) {
            if (!(u.omit_host and u.host.len == 0 and !u.has_user)) {
                if (u.host.len != 0 or u.path.len != 0 or u.has_user) try buf.appendSlice(arena, "//");
                if (u.has_user) {
                    try buf.appendSlice(arena, try escape(arena, u.username, .user));
                    if (u.password_set) try buf.appendSlice(arena, ":" ++ mask);
                    try buf.append(arena, '@');
                }
                if (u.host.len != 0) try buf.appendSlice(arena, try escape(arena, u.host, .host));
            }
        }
        var path = u.path;
        if (u.omit_host and u.host.len == 0 and !u.has_user and std.mem.startsWith(u8, path, "//")) {
            try buf.appendSlice(arena, "%2F");
            path = path[1..];
        }
        if (path.len != 0 and path[0] != '/' and u.host.len != 0) try buf.append(arena, '/');
        if (buf.items.len == 0 and segmentHasColon(path)) try buf.appendSlice(arena, "./");
        try buf.appendSlice(arena, path);
    }
    if (u.force_query or u.raw_query.len != 0) {
        try buf.append(arena, '?');
        try buf.appendSlice(arena, u.raw_query);
    }
    return buf.items;
}

fn splitSlash(arena: Allocator, s: []const u8) Allocator.Error![]const []const u8 {
    var parts: std.ArrayList([]const u8) = .empty;
    var start: usize = 0;
    for (s, 0..) |c, i| if (c == '/') {
        try parts.append(arena, s[start..i]);
        start = i + 1;
    };
    try parts.append(arena, s[start..]);
    return parts.items;
}

fn allHex(s: []const u8) bool {
    for (s) |c| if (!std.ascii.isHex(c)) return false;
    return true;
}

fn allToken(s: []const u8) bool {
    for (s) |c| if (!std.ascii.isAlphanumeric(c) and c != '_' and c != '-') return false;
    return true;
}

fn isPasskeyMarker(segs: []const []const u8, i: usize, delta: isize) bool {
    const j = @as(isize, @intCast(i)) + delta;
    if (j < 0 or j >= @as(isize, @intCast(segs.len))) return false;
    const seg = segs[@intCast(j)];
    return std.ascii.eqlIgnoreCase(seg, "announce") or std.ascii.eqlIgnoreCase(seg, "passkey");
}

fn redactPath(arena: Allocator, escaped: []const u8) Allocator.Error![]const u8 {
    const segs = try splitSlash(arena, escaped);
    var out: std.ArrayList(u8) = .empty;
    for (segs, 0..) |seg, i| {
        if (i != 0) try out.append(arena, '/');
        if (seg.len >= 32 and allHex(seg)) {
            try out.appendSlice(arena, mask);
        } else if (seg.len >= 20 and allToken(seg) and (isPasskeyMarker(segs, i, -1) or isPasskeyMarker(segs, i, 1))) {
            try out.appendSlice(arena, mask);
        } else try out.appendSlice(arena, seg);
    }
    return out.items;
}

fn isSecretName(name: []const u8) bool {
    const names = [_][]const u8{
        "apikey",   "api_key",      "key",     "token",   "password",     "pass",
        "nzbkey",   "r",            "passkey", "authkey", "torrent_pass", "rsskey",
        "apitoken", "access_token",
    };
    for (names) |n| if (std.mem.eql(u8, name, n)) return true;
    return false;
}

fn isSecretParam(arena: Allocator, raw_name: []const u8) Allocator.Error!bool {
    const name = (try unescape(arena, raw_name, .query)) orelse raw_name;
    for (name) |c| if (c >= 0x80) return false;
    return isSecretName(try lowerAscii(arena, name));
}

fn nestedHttp(arena: Allocator, escaped: []const u8) Allocator.Error!?[]const u8 {
    const value = (try unescape(arena, escaped, .query)) orelse return null;
    if (std.ascii.startsWithIgnoreCase(value, "http://") or std.ascii.startsWithIgnoreCase(value, "https://")) return value;
    return null;
}

fn redactQuery(arena: Allocator, raw: []const u8, seps: []const u8, depth: u8) Allocator.Error![]const u8 {
    if (raw.len == 0) return "";
    var out: std.ArrayList(u8) = .empty;
    var rest = raw;
    while (rest.len > 0) {
        const end = std.mem.indexOfAny(u8, rest, seps) orelse rest.len;
        const part = rest[0..end];
        if (std.mem.indexOfScalar(u8, part, '=')) |eq| {
            const name = part[0..eq];
            const value = part[eq + 1 ..];
            if (try isSecretParam(arena, name)) {
                try out.appendSlice(arena, name);
                try out.appendSlice(arena, "=" ++ mask);
            } else if (depth < max_nesting) {
                if (try nestedHttp(arena, value)) |nested| {
                    const inner = try redactUrl(arena, nested, depth + 1);
                    try out.appendSlice(arena, name);
                    try out.append(arena, '=');
                    try out.appendSlice(arena, try escape(arena, inner, .query));
                } else try out.appendSlice(arena, part);
            } else try out.appendSlice(arena, part);
        } else try out.appendSlice(arena, part);
        if (end < rest.len) {
            try out.append(arena, rest[end]);
            rest = rest[end + 1 ..];
        } else break;
    }
    return out.items;
}

fn redactUrl(arena: Allocator, raw: []const u8, depth: u8) Allocator.Error![]const u8 {
    var u = (try parseGo(arena, raw)) orelse return invalid_url;
    if (u.opaque_data.len == 0) {
        const redacted = try redactPath(arena, u.path);
        const unesc = (try unescape(arena, redacted, .path)) orelse return invalid_url;
        u.path = if (validEncoded(redacted, .path)) redacted else try escape(arena, unesc, .path);
    }
    u.raw_query = try redactQuery(arena, u.raw_query, "&;", depth);
    const frag = u.fragment;
    const body = try formatUrl(arena, u);
    if (frag.len == 0) return body;
    return try std.mem.concat(arena, u8, &.{ body, "#", try redactQuery(arena, frag, "&;?", depth) });
}

// ---- rune cleanup (Go strings.Map + Fields + byte cap) ----

const Rune = struct { cp: ?u21, size: usize };

fn decodeRune(s: []const u8, i: usize) Rune {
    const b = s[i];
    if (b < 0x80) return .{ .cp = b, .size = 1 };
    const n = std.unicode.utf8ByteSequenceLength(b) catch return .{ .cp = null, .size = 1 };
    if (i + n > s.len) return .{ .cp = null, .size = 1 };
    const cp = std.unicode.utf8Decode(s[i..][0..n]) catch return .{ .cp = null, .size = 1 };
    return .{ .cp = cp, .size = n };
}

/// unicode.Cf from Go 1.27 (same table as add_names.zig). Cc is handled apart.
const format_ranges = [_][2]u21{
    .{ 0xad, 0xad },       .{ 0x600, 0x605 },     .{ 0x61c, 0x61c },     .{ 0x6dd, 0x6dd },
    .{ 0x70f, 0x70f },     .{ 0x890, 0x891 },     .{ 0x8e2, 0x8e2 },     .{ 0x180e, 0x180e },
    .{ 0x200b, 0x200f },   .{ 0x202a, 0x202e },   .{ 0x2060, 0x2064 },   .{ 0x2066, 0x206f },
    .{ 0xfeff, 0xfeff },   .{ 0xfff9, 0xfffb },   .{ 0x110bd, 0x110bd }, .{ 0x110cd, 0x110cd },
    .{ 0x13430, 0x1343f }, .{ 0x1bca0, 0x1bca3 }, .{ 0x1d173, 0x1d17a }, .{ 0xe0001, 0xe0001 },
    .{ 0xe0020, 0xe007f },
};

fn isControl(cp: u21) bool {
    return cp <= 0x1f or (cp >= 0x7f and cp <= 0x9f);
}

fn isFormat(cp: u21) bool {
    for (format_ranges) |range| if (cp >= range[0] and cp <= range[1]) return true;
    return false;
}

/// Go's unicode.IsSpace.
fn isSpace(cp: u21) bool {
    return switch (cp) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        0x2000...0x200a => true,
        else => false,
    };
}

/// ToValidUTF8(s, "") then strings.Map: controls become a space, Cf is dropped.
fn mapDetail(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) {
        const r = decodeRune(s, i);
        i += r.size;
        const cp = r.cp orelse continue;
        if (isControl(cp)) {
            try out.append(arena, ' ');
        } else if (!isFormat(cp)) try out.appendSlice(arena, s[i - r.size ..][0..r.size]);
    }
    return out.items;
}

/// strings.Join(strings.Fields(s), " ").
fn collapseFields(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    var in_space = true;
    while (i < s.len) {
        const r = decodeRune(s, i);
        const cp = r.cp orelse unreachable;
        if (isSpace(cp)) {
            in_space = true;
        } else {
            if (in_space and out.items.len != 0) try out.append(arena, ' ');
            in_space = false;
            try out.appendSlice(arena, s[i..][0..r.size]);
        }
        i += r.size;
    }
    return out.items;
}

fn capDetail(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    if (s.len <= max_detail_bytes) return s;
    var end: usize = max_detail_bytes;
    while (end > 0 and s[end - 1] & 0xC0 == 0x80) end -= 1;
    var valid = s[0..end];
    while (valid.len > 0 and !std.unicode.utf8ValidateSlice(valid)) valid = valid[0 .. valid.len - 1];
    return try std.mem.concat(arena, u8, &.{ valid, "..." });
}

test "cleanDetail redacts magnet URLs like Go CleanDetail" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try cleanDetail(arena, "see magnet:?xt=urn:btih:abc now");
    try testing.expect(std.mem.indexOf(u8, got, "magnet:?[redacted]") != null);
    try testing.expect(std.mem.indexOf(u8, got, "btih") == null);
}

test "failureText missing_articles empty detail matches Go" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try failureText(arena, .missing_articles, "");
    try testing.expectEqualStrings(
        "Aborted, cannot be completed - articles are missing and no repair data can recover them",
        got,
    );
}
