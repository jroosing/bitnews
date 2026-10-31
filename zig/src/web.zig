//! Web UI routes (Z6). `register` adds handlers before `listen`.
const std = @import("std");
const ex = @import("http_exchange.zig");

const setup = @import("web_setup.zig");

pub fn register(routes: *ex.Routes) !void {
    // This slice always sends the front door to /setup. Queue and login
    // redirects come with the rest of getRoot. "{$}" is the exact root, so
    // "GET /" does not swallow every other GET.
    try routes.add("GET /{$}", .{ .handler = getRoot, .auth = .none });
    // A path with a segment and no trailing "/" is exact, not a subtree.
    // "GET /" would be: that pattern is only "/".
    try routes.add("GET /login", .{ .handler = getLogin, .auth = .none });
    try routes.add("GET /setup", .{ .handler = setup.page, .auth = .none });
    try routes.add("POST /setup", .{ .handler = setup.post, .auth = .none, .body_limit = setup.body_limit });
}

fn getRoot(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = std.http.Status.see_other;
    try e.res.setHeader(e.arena, "Location", "/setup");
}

/// This slice always sends /login to /setup. The form and the signed-in
/// redirect come with the rest of getLogin. GET also answers HEAD.
fn getLogin(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    e.res.status = std.http.Status.see_other;
    try e.res.setHeader(e.arena, "Location", "/setup");
}
