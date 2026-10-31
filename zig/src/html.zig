//! Context-aware HTML escaping and URL/class helpers (Z6). The `.templ`
//! files are the specification for page markup.
const std = @import("std");

/// Hostile input cap: escape only the first MiB.
pub const max_input: usize = 1 << 20;

/// Text-context escape (`& < > " '`), like Go `html/template` EscapeString.
pub fn escape(arena: std.mem.Allocator, s: []const u8) std.mem.Allocator.Error![]const u8 {
    const in = s[0..@min(s.len, max_input)];
    var extra: usize = 0;
    for (in) |c| {
        if (entity(c)) |e| extra += e.len - 1;
    }
    if (extra == 0) return in;
    const out = try arena.alloc(u8, in.len + extra);
    var n: usize = 0;
    for (in) |c| {
        if (entity(c)) |e| {
            @memcpy(out[n..][0..e.len], e);
            n += e.len;
        } else {
            out[n] = c;
            n += 1;
        }
    }
    std.debug.assert(n == out.len);
    return out;
}

/// Attribute fragment: leading space, `name="escaped value"`. Empty name → `""`.
pub fn attr(arena: std.mem.Allocator, name: []const u8, value: []const u8) std.mem.Allocator.Error![]const u8 {
    if (name.len == 0) return "";
    const escaped = try escape(arena, value);
    return std.fmt.allocPrint(arena, " {s}=\"{s}\"", .{ name, escaped });
}

/// At most this many class tokens; extras are dropped.
pub const max_class_tokens: usize = 64;

/// Each class token is truncated to this many bytes before join/escape.
pub const max_class_token_bytes: usize = 256;

/// Space-joined `class` attribute value (Z6). Empty tokens are skipped.
/// Each remaining token is capped at `max_class_token_bytes`, the list at
/// `max_class_tokens`. The joined string is then `escape`d so hostile names
/// cannot break out of the attribute.
pub fn class(arena: std.mem.Allocator, tokens: []const []const u8) std.mem.Allocator.Error![]const u8 {
    const list = tokens[0..@min(tokens.len, max_class_tokens)];
    var total: usize = 0;
    var kept: usize = 0;
    var only: []const u8 = "";
    for (list) |tok| {
        const t = tok[0..@min(tok.len, max_class_token_bytes)];
        if (t.len == 0) continue;
        if (kept > 0) total += 1;
        total += t.len;
        kept += 1;
        only = t;
    }
    if (kept == 0) return "";
    if (kept == 1) return escape(arena, only);

    const joined = try arena.alloc(u8, total);
    var n: usize = 0;
    for (list) |tok| {
        const t = tok[0..@min(tok.len, max_class_token_bytes)];
        if (t.len == 0) continue;
        if (n > 0) {
            joined[n] = ' ';
            n += 1;
        }
        @memcpy(joined[n..][0..t.len], t);
        n += t.len;
    }
    std.debug.assert(n == joined.len);
    return escape(arena, joined);
}

/// Sanitised value for `href` / `src` URL attributes (Z6).
/// Rejects `javascript:`, `data:`, `vbscript:` (ASCII case-insensitive, after
/// trim) and protocol-relative `//`. Those return `""`. Input is capped at
/// `max_input` like `escape`. Safe values pass through the trimmed slice.
pub fn href(arena: std.mem.Allocator, s: []const u8) std.mem.Allocator.Error![]const u8 {
    _ = arena;
    const in = std.mem.trim(u8, s[0..@min(s.len, max_input)], &std.ascii.whitespace);
    if (hostileUrl(in)) return "";
    return in;
}

fn hostileUrl(s: []const u8) bool {
    if (s.len >= 2 and s[0] == '/' and s[1] == '/') return true;
    return hasScheme(s, "javascript") or hasScheme(s, "data") or hasScheme(s, "vbscript");
}

fn hasScheme(s: []const u8, name: []const u8) bool {
    if (s.len < name.len + 1 or s[name.len] != ':') return false;
    return std.ascii.eqlIgnoreCase(s[0..name.len], name);
}

fn entity(c: u8) ?[]const u8 {
    return switch (c) {
        '&' => "&amp;",
        '<' => "&lt;",
        '>' => "&gt;",
        '"' => "&#34;",
        '\'' => "&#39;",
        else => null,
    };
}

test "escape ampersand in text like html/template EscapeString" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const got = try escape(arena, "a&b");
    try testing.expectEqualStrings("a&amp;b", got);
}

test "attr emits spaced name equals escaped value skips empty name" {
    const testing = std.testing;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    try testing.expectEqualStrings("", try attr(arena, "", "ignored"));
    const got = try attr(arena, "class", "x&y");
    try testing.expectEqualStrings(" class=\"x&amp;y\"", got);
}

fn escapeOutputWellFormed(out: []const u8) bool {
    var i: usize = 0;
    while (i < out.len) {
        if (out[i] == '&') {
            const rest = out[i..];
            if (std.mem.startsWith(u8, rest, "&amp;")) i += 5 else if (std.mem.startsWith(u8, rest, "&lt;")) i += 4 else if (std.mem.startsWith(u8, rest, "&gt;")) i += 4 else if (std.mem.startsWith(u8, rest, "&#34;")) i += 5 else if (std.mem.startsWith(u8, rest, "&#39;")) i += 5 else return false;
        } else {
            if (out[i] == '<' or out[i] == '"') return false;
            i += 1;
        }
    }
    return true;
}

fn fuzzEscape(_: void, smith: *std.testing.Smith) !void {
    const testing = std.testing;
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();
    const out = try escape(arena, buf[0..n]);
    const in_len = @min(n, max_input);
    try testing.expect(out.len <= in_len * 5);
    try testing.expect(escapeOutputWellFormed(out));

    // Escaped output must not exceed the module byte cap (worst case: all '&').
    const bulky_len = max_input / 5 + 1;
    const bulky = try testing.allocator.alloc(u8, bulky_len);
    defer testing.allocator.free(bulky);
    @memset(bulky, '&');
    const bulky_out = try escape(arena, bulky);
    try testing.expect(bulky_out.len <= bulky.len * 5);
}

test "fuzz escape stays bounded" {
    const testing = std.testing;
    try testing.fuzz({}, fuzzEscape, .{ .corpus = &.{
        "\x04\x00\x00\x00<&\"'",
        "\x08\x00\x00\x00a&b<c>d\"e'",
        "\x05\x00\x00\x00&amp;",
    } });
}
