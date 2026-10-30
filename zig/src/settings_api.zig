//! GET and PUT /api/v1/settings (internal/api/handlers_settings.go).
//!
//! PUT replaces every setting except the authentication options: they decide
//! who may reach the UI without logging in, and an API key could otherwise
//! open the UI to the whole LAN, so they are changed in the UI. The body is
//! decoded onto the zero value, so a section left out is reset, like in Go.
//! Integer fields reject "563", 563.0 and 5e2 (the decoder in settings_json
//! does not go through std.json's lenient integers).
const std = @import("std");
const Allocator = std.mem.Allocator;
const ex = @import("http_exchange.zig");
const respond = @import("respond.zig");
const api_json = @import("api_json.zig");
const app_error = @import("app_error.zig");
const json = @import("settings_json.zig");
const service = @import("settings_service.zig");

const Service = service.Service;
const Settings = service.Settings;

pub const route = "/api/v1/settings";

/// Registers the two routes. `svc` must outlive the router.
pub fn register(routes: *ex.Routes, svc: *Service) !void {
    try routes.add("GET " ++ route, .{ .handler = getHandler, .state = svc, .body_limit = respond.small_body_limit });
    try routes.add("PUT " ++ route, .{ .handler = putHandler, .state = svc, .body_limit = respond.default_body_limit });
}

fn getHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const svc: *Service = @ptrCast(@alignCast(state.?));
    try handleGet(svc, e.arena, &e.res);
}

fn putHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const svc: *Service = @ptrCast(@alignCast(state.?));
    if (!api_json.isJsonContentType(e.header("Content-Type"))) {
        return e.res.fail(e.arena, .unsupported_media_type, "unsupported_media_type", "send the body as Content-Type: application/json");
    }
    // A body above the limit propagates as PayloadTooLarge, the server's 413.
    const body = try e.bodyAlloc();
    try handlePut(svc, e.arena, body, &e.res);
}

/// GET: the Settings in effect, with empty lists instead of nulls.
pub fn handleGet(svc: *Service, arena: Allocator, res: *respond.Response) !void {
    const snap = svc.current();
    defer snap.release();
    try res.json(arena, .ok, snap.settings);
}

/// PUT, after the Content-Type check: decode, refuse an auth change, validate
/// and store, answer with the new Settings.
pub fn handlePut(svc: *Service, arena: Allocator, body: []const u8, res: *respond.Response) !void {
    var next: Settings = .{};
    switch (try json.decode(arena, body, &next, true)) {
        .ok => {},
        .invalid => |msg| return res.fail(arena, .bad_request, "invalid_json", msg),
    }

    var fail: app_error.Failure = .{};
    // The guard compares with the stored Settings under the write lock, so the
    // auth section cannot change between the check and the write.
    svc.change(arena, &fail, .{ .ctx = &next, .run = replaceKeepingAuth }) catch |err| switch (err) {
        error.SessionRequired => return res.fail(arena, .forbidden, "session_required", "the authentication settings are changed in the UI, not through the API"),
        error.Rejected => return res.fail(arena, fail.kind.status(), fail.code, fail.message),
        else => return err,
    };
    try handleGet(svc, arena, res);
}

fn replaceKeepingAuth(ctx: ?*anyopaque, cur: *const Settings, dst: *Settings, _: Allocator, _: *app_error.Failure) anyerror!void {
    const next: *const Settings = @ptrCast(@alignCast(ctx.?));
    if (!cur.auth.eql(next.auth)) return error.SessionRequired;
    // `next` lives in the request arena, which outlives the change.
    dst.* = next.*;
}

// ---- tests ----

const testing = std.testing;
const golden = @import("settings_golden.zig");
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const event = @import("event.zig");

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    svc: Service,
    events: usize = 0,

    fn onEvent(ctx: *anyopaque, e: event.Event) void {
        const rig: *Rig = @ptrCast(@alignCast(ctx));
        if (e == .settings_changed) rig.events += 1;
    }

    fn open(rig: *Rig) !void {
        rig.events = 0;
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        errdefer rig.store.close();
        {
            const w = try rig.store.acquireWriter();
            defer w.release();
            var diag: migrate.Diagnostic = .{};
            try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        }
        rig.svc = try Service.init(testing.allocator, testing.io, .{ .store = rig.store }, .{ .ctx = rig, .publish_fn = onEvent }, null);
    }

    fn close(rig: *Rig) void {
        rig.svc.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }
};

const full_body =
    \\{"queues":{"usenet":2,"torrent":4,"postproc_concurrency":0,"min_free":0},
    \\"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},
    \\"paths":{"usenet":{"incomplete":"/d/u/inc","complete":"/d/u/done"},"torrent":{"incomplete":"/d/t","complete":"/d/t"}},
    \\"network":{"usenet":{"interface":"","address":"10.2.0.2"},"torrent":{"interface":"wg0","address":""}},
    \\"seeding":{"action":"pause","ratio_limit":0,"seeding_time_minutes":0},
    \\"speed":{"global":{"down_kbps":0,"up_kbps":0},"usenet":{"down_kbps":0,"up_kbps":0},"torrent":{"down_kbps":0,"up_kbps":0}},
    \\"torrent":{"listen_port":0,"disable_dht":false,"disable_pex":false,"lsd":false,"mse":""},
    \\"postprocess":{"clean_names":false,"underscores_to_dots":false}}
;

const full_answer =
    \\{"queues":{"usenet":2,"torrent":4,"postproc_concurrency":0,"min_free":0},"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},"paths":{"usenet":{"incomplete":"/d/u/inc","complete":"/d/u/done"},"torrent":{"incomplete":"/d/t","complete":"/d/t"}},"network":{"usenet":{"interface":"","address":"10.2.0.2"},"torrent":{"interface":"wg0","address":""}},"seeding":{"ratio_limit":0,"seeding_time_minutes":0,"action":"pause"},"speed":{"global":{"down_kbps":0,"up_kbps":0},"usenet":{"down_kbps":0,"up_kbps":0},"torrent":{"down_kbps":0,"up_kbps":0}},"torrent":{"listen_port":0,"disable_dht":false,"disable_pex":false,"lsd":false,"mse":""},"postprocess":{"clean_names":false,"underscores_to_dots":false}}
++ "\n";

fn expectError(res: respond.Response, status: std.http.Status, code: []const u8, message_part: []const u8) !void {
    try testing.expectEqual(status, res.status);
    const needle = try std.fmt.allocPrint(testing.allocator, "\"code\":\"{s}\"", .{code});
    defer testing.allocator.free(needle);
    try testing.expect(std.mem.indexOf(u8, res.body, needle) != null);
    try testing.expect(std.mem.indexOf(u8, res.body, message_part) != null);
}

test "GET answers the defaults with empty arrays, byte for byte like Go" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var res: respond.Response = .{};
    try handleGet(&rig.svc, arena.allocator(), &res);
    try testing.expectEqual(.ok, res.status);
    try testing.expectEqualStrings(golden.default_json, res.body);
    try testing.expectEqualStrings("no-store", res.header("Cache-Control").?);
}

test "PUT stores, publishes settings_changed and the answer equals a later GET" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    var put: respond.Response = .{};
    try handlePut(&rig.svc, a, full_body, &put);
    try testing.expectEqual(.ok, put.status);
    try testing.expectEqualStrings(full_answer, put.body);
    var get: respond.Response = .{};
    try handleGet(&rig.svc, a, &get);
    try testing.expectEqualStrings(full_answer, get.body);
    try testing.expectEqual(1, rig.events);
}

test "PUT lists the problems of invalid settings" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const paths = "\"paths\":{\"usenet\":{\"incomplete\":\"/a\",\"complete\":\"/b\"},\"torrent\":{\"incomplete\":\"/c\",\"complete\":\"/d\"}}";

    const cases = [_]struct { body: []const u8, status: std.http.Status, code: []const u8, part: []const u8 }{
        .{ .body = "{\"queues\":{\"usenet\":-1,\"torrent\":5},\"auth\":{}," ++ paths ++ "}", .status = .bad_request, .code = "invalid_settings", .part = "usenet queue limit" },
        .{ .body = "{\"queues\":{\"usenet\":1,\"torrent\":5},\"auth\":{},\"paths\":{}}", .status = .bad_request, .code = "invalid_settings", .part = "not an absolute path" },
        .{ .body = "{\"queues\":{\"usenet\":1,\"torrent\":5}," ++ paths ++ ",\"network\":{\"torrent\":{\"interface\":\"../wg0\"}}}", .status = .bad_request, .code = "invalid_settings", .part = "network.torrent.interface" },
        .{ .body = "{\"queues\":{\"usenet\":1,\"torrent\":5}," ++ paths ++ ",\"network\":{\"usenet\":{\"address\":\"vpn.example.com\"}}}", .status = .bad_request, .code = "invalid_settings", .part = "network.usenet.address" },
        .{ .body = "{\"queues\":{},\"bogus\":1}", .status = .bad_request, .code = "invalid_json", .part = "bogus" },
        .{ .body = "[]", .status = .bad_request, .code = "invalid_json", .part = "" },
        .{ .body = "", .status = .bad_request, .code = "invalid_json", .part = "the request body is empty" },
        // Integers must be written as integers.
        .{ .body = "{\"queues\":{\"usenet\":\"563\"}}", .status = .bad_request, .code = "invalid_json", .part = "cannot unmarshal string into Go struct field Settings.queues.usenet of type int" },
        .{ .body = "{\"queues\":{\"usenet\":563.0}}", .status = .bad_request, .code = "invalid_json", .part = "cannot unmarshal number 563.0" },
        .{ .body = "{\"torrent\":{\"listen_port\":5e2}}", .status = .bad_request, .code = "invalid_json", .part = "cannot unmarshal number 5e2" },
        .{ .body = "{\"speed\":{\"global\":{\"down_kbps\":1.5}}}", .status = .bad_request, .code = "invalid_json", .part = "cannot unmarshal number 1.5" },
    };
    for (cases) |c| {
        var res: respond.Response = .{};
        try handlePut(&rig.svc, a, c.body, &res);
        try expectError(res, c.status, c.code, c.part);
    }
    try testing.expectEqual(0, rig.events);
    var get: respond.Response = .{};
    try handleGet(&rig.svc, a, &get);
    try testing.expectEqualStrings(golden.default_json, get.body);
}

test "PUT cannot change the auth section" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const open_auth =
        \\{"queues":{"usenet":1,"torrent":5},
        \\"auth":{"disable_auth":true,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":["example.com"]},
        \\"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}
    ;
    var res: respond.Response = .{};
    try handlePut(&rig.svc, a, open_auth, &res);
    try expectError(res, .forbidden, "session_required", "changed in the UI");

    const snap = rig.svc.current();
    defer snap.release();
    try testing.expect(!snap.settings.auth.disable_auth);
    try testing.expectEqual(0, rig.events);
}

test "PUT keeps the auth section when the body repeats it" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    // The UI sets the auth options through the service, not through the API.
    var fail: app_error.Failure = .{};
    var s = Settings.defaults();
    s.auth = .{ .disable_auth = true, .allowed_hosts = &.{"example.com"} };
    try rig.svc.update(a, &fail, s);

    const same =
        \\{"queues":{"usenet":3,"torrent":5},
        \\"auth":{"disable_auth":true,"allowed_hosts":["example.com"]},
        \\"paths":{"usenet":{"incomplete":"/a","complete":"/b"},"torrent":{"incomplete":"/c","complete":"/d"}}}
    ;
    var res: respond.Response = .{};
    try handlePut(&rig.svc, a, same, &res);
    try testing.expectEqual(.ok, res.status);
    const snap = rig.svc.current();
    defer snap.release();
    try testing.expectEqual(3, snap.settings.queues.usenet);
}
