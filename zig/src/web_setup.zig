//! GET /setup and POST /setup (Z6). GET is a fixed string: that response
//! carries no user input. POST re-renders the form and escapes the username.
const std = @import("std");
const ex = @import("http_exchange.zig");
const html = @import("html.zig");

/// Largest POST /setup body. Four short fields; anything larger is refused
/// by the route before the handler keeps it.
pub const body_limit: u64 = 64 << 10;

/// Pairs kept from one body. Further pairs are dropped, so a padded body
/// cannot push `token` out of the window by sheer count.
const max_fields: usize = 32;

/// Decoded name or value. A longer pair is skipped, not truncated: truncation
/// would turn an over-long password into one that looks short enough.
const max_field: usize = 4096;

const token_missing = "Enter the setup token from the bitnews log.";

/// Fields this slice retains. Password and confirm are parsed only far enough
/// to drop them: they are never copied and never written back into HTML.
const Form = struct {
    token: []const u8 = "",
    username: []const u8 = "",
};

const page_html =
    \\<!DOCTYPE html>
    \\<html lang="en">
    \\<head>
    \\<meta charset="utf-8">
    \\<title>Set up bitnews</title>
    \\</head>
    \\<body>
    \\<main id="main">
    \\<h1>Set up bitnews</h1>
    \\<p>Create the admin account. You will need the one-time setup token that bitnews wrote to its log when it started.</p>
    \\<form id="setup-form" method="post" action="/setup" hx-post="/setup" hx-target="this" hx-swap="outerHTML" hx-disabled-elt="find button[type=submit]" class="space-y-5" novalidate>
    \\<div>
    \\<label for="token">Setup token</label>
    \\<input id="token" name="token" type="text" autocomplete="off" autocapitalize="none" autocorrect="off" spellcheck="false" required>
    \\</div>
    \\<div>
    \\<label for="username">Username</label>
    \\<input id="username" name="username" type="text" autocomplete="username" maxlength="64" autocapitalize="none" autocorrect="off" spellcheck="false" required>
    \\</div>
    \\<div>
    \\<label for="password">Password</label>
    \\<input id="password" name="password" type="password" autocomplete="new-password" required>
    \\</div>
    \\<div>
    \\<label for="confirm">Confirm password</label>
    \\<input id="confirm" name="confirm" type="password" autocomplete="new-password" required>
    \\</div>
    \\<button type="submit">Create account</button>
    \\</form>
    \\</main>
    \\</body>
    \\</html>
;

pub fn page(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = std.http.Status.ok;
    e.res.body = page_html;
    try e.res.setHeader(e.arena, "Content-Type", "text/html; charset=utf-8");
    try e.res.setHeader(e.arena, "Cache-Control", "no-store");
}

/// POST /setup. Reads the urlencoded body only (not the query). An empty
/// token is refused with the form again. `accounts_setup.complete` is a
/// later slice: a presented token is not accepted here either.
pub fn post(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const form = try formFromBody(e);
    const token = std.mem.trim(u8, form.token, &std.ascii.whitespace);
    if (token.len == 0) {
        try render(e, form.username, token_missing);
        return;
    }
    try render(e, form.username, null);
}

/// Same document as `page_html`, plus an optional token error and the
/// username value. `{s}` is the error fragment, then the username attribute.
const form_fmt =
    \\<!DOCTYPE html>
    \\<html lang="en">
    \\<head>
    \\<meta charset="utf-8">
    \\<title>Set up bitnews</title>
    \\</head>
    \\<body>
    \\<main id="main">
    \\<h1>Set up bitnews</h1>
    \\<p>Create the admin account. You will need the one-time setup token that bitnews wrote to its log when it started.</p>
    \\<form id="setup-form" method="post" action="/setup" hx-post="/setup" hx-target="this" hx-swap="outerHTML" hx-disabled-elt="find button[type=submit]" class="space-y-5" novalidate>
    \\<div>
    \\<label for="token">Setup token</label>
    \\<input id="token" name="token" type="text" autocomplete="off" autocapitalize="none" autocorrect="off" spellcheck="false" required>
    \\{s}</div>
    \\<div>
    \\<label for="username">Username</label>
    \\<input id="username" name="username" type="text" autocomplete="username" maxlength="64" autocapitalize="none" autocorrect="off" spellcheck="false"{s} required>
    \\</div>
    \\<div>
    \\<label for="password">Password</label>
    \\<input id="password" name="password" type="password" autocomplete="new-password" required>
    \\</div>
    \\<div>
    \\<label for="confirm">Confirm password</label>
    \\<input id="confirm" name="confirm" type="password" autocomplete="new-password" required>
    \\</div>
    \\<button type="submit">Create account</button>
    \\</form>
    \\</main>
    \\</body>
    \\</html>
;

fn render(e: *ex.Exchange, username: []const u8, token_error: ?[]const u8) !void {
    const arena = e.arena;
    const user_attr = try html.attr(arena, "value", username);
    const err_html: []const u8 = if (token_error) |msg| blk: {
        const safe = try html.escape(arena, msg);
        break :blk try std.fmt.allocPrint(arena, "<p id=\"token-error\">{s}</p>\n", .{safe});
    } else "";
    const body = try std.fmt.allocPrint(arena, form_fmt, .{ err_html, user_attr });
    e.res.status = std.http.Status.unprocessable_entity;
    e.res.body = body;
    try e.res.setHeader(arena, "Content-Type", "text/html; charset=utf-8");
    try e.res.setHeader(arena, "Cache-Control", "no-store");
}

fn formFromBody(e: *ex.Exchange) !Form {
    if (!isUrlencoded(e.header("Content-Type"))) return .{};
    const raw = try e.bodyAlloc();
    return parseForm(e.arena, raw);
}

/// Media type only; parameters after ';' are ignored.
fn isUrlencoded(header: ?[]const u8) bool {
    const h = header orelse return false;
    const end = std.mem.indexOfScalar(u8, h, ';') orelse h.len;
    return std.ascii.eqlIgnoreCase(std.mem.trim(u8, h[0..end], &std.ascii.whitespace), "application/x-www-form-urlencoded");
}

/// `application/x-www-form-urlencoded` body. Splits on `&` only. A pair that
/// contains `;`, an invalid `%` escape, an empty name, or a decoded side
/// longer than `max_field` is skipped. First accepted `token` and `username`
/// win. Other names, including password and confirm, are not stored.
fn parseForm(arena: std.mem.Allocator, raw: []const u8) !Form {
    var form: Form = .{};
    var seen_token = false;
    var seen_username = false;
    var count: usize = 0;
    var it = std.mem.splitScalar(u8, raw, '&');
    while (it.next()) |part| {
        if (count >= max_fields) break;
        if (part.len == 0) continue;
        count += 1;
        if (std.mem.indexOfScalar(u8, part, ';') != null) continue;
        const eq = std.mem.indexOfScalar(u8, part, '=');
        const name_raw = if (eq) |i| part[0..i] else part;
        const value_raw = if (eq) |i| part[i + 1 ..] else "";
        var name_buf: [max_field]u8 = undefined;
        const name = decodeInto(&name_buf, name_raw) orelse continue;
        if (name.len == 0) continue;
        if (!seen_token and std.mem.eql(u8, name, "token")) {
            const value = (try dupeField(arena, value_raw)) orelse continue;
            seen_token = true;
            form.token = value;
        } else if (!seen_username and std.mem.eql(u8, name, "username")) {
            const value = (try dupeField(arena, value_raw)) orelse continue;
            seen_username = true;
            form.username = value;
        }
    }
    return form;
}

fn dupeField(arena: std.mem.Allocator, raw: []const u8) !?[]const u8 {
    var buf: [max_field]u8 = undefined;
    const decoded = decodeInto(&buf, raw) orelse return null;
    if (decoded.len == 0) return "";
    return try arena.dupe(u8, decoded);
}

/// Decode into `buf`. Null when an escape is invalid or the decoded length
/// would exceed `buf`. The returned slice is `buf`, never `s`.
fn decodeInto(buf: *[max_field]u8, s: []const u8) ?[]const u8 {
    if (std.mem.indexOfAny(u8, s, "%+") == null) {
        if (s.len > max_field) return null;
        @memcpy(buf[0..s.len], s);
        return buf[0..s.len];
    }
    var n: usize = 0;
    var i: usize = 0;
    while (i < s.len) {
        if (n == buf.len) return null;
        if (s[i] == '+') {
            buf[n] = ' ';
            n += 1;
            i += 1;
        } else if (s[i] == '%') {
            if (i + 2 >= s.len) return null;
            const hi = std.fmt.charToDigit(s[i + 1], 16) catch return null;
            const lo = std.fmt.charToDigit(s[i + 2], 16) catch return null;
            buf[n] = (hi << 4) | lo;
            n += 1;
            i += 3;
        } else {
            buf[n] = s[i];
            n += 1;
            i += 1;
        }
    }
    return buf[0..n];
}
