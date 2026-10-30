//! A real HTTP server on loopback for the tests of the add group: the curl
//! fetcher is tested against it, and the add route is called through it with
//! a real client. Only the tests import this file.
const std = @import("std");
const Io = std.Io;
const testing = std.testing;
const srv = @import("http_server.zig");
const common = @import("http_common.zig");
const ex = @import("http_exchange.zig");

pub const test_key = "bitnews_testkey0123456789";

const TestKeys = struct {
    fn check(_: *anyopaque, presented: []const u8) bool {
        return std.mem.eql(u8, presented, test_key);
    }
    var dummy: u8 = 0;
    const checker: common.KeyChecker = .{ .ctx = &dummy, .check = check };
};

pub const Setup = *const fn (routes: *ex.Routes) anyerror!void;

pub const Server = struct {
    server: *srv.Server,
    fut: Io.Future(error{NotListening}!void),
    port: u16,

    /// Starts a server on a free loopback port; `setup` registers the routes.
    pub fn start(s: *Server, setup: Setup) !void {
        s.server = try srv.Server.init(testing.allocator, testing.io, .{ .keys = TestKeys.checker });
        errdefer s.server.deinit();
        try setup(&s.server.routes);
        s.port = try s.server.listen("127.0.0.1:0");
        s.fut = try testing.io.concurrent(srv.Server.serve, .{s.server});
    }

    pub fn stop(s: *Server) void {
        s.fut.cancel(testing.io) catch {};
        s.server.deinit();
    }

    /// "http://127.0.0.1:<port><path>" in `a`.
    pub fn url(s: *const Server, a: std.mem.Allocator, path: []const u8) ![]u8 {
        return std.fmt.allocPrint(a, "http://127.0.0.1:{d}{s}", .{ s.port, path });
    }
};
