//! The Server routes of /api/v1 (internal/api/handlers_servers.go and the
//! server DTO in dto.go). The password only ever travels inwards: responses
//! say `has_password` and `password_unavailable` instead.
//!
//! The two connection-test routes need the NNTP client (Z3); until then they
//! answer 503 `server_test_unavailable`.

const std = @import("std");
const Allocator = std.mem.Allocator;
const ex = @import("http_exchange.zig");
const respond = @import("respond.zig");
const api_json = @import("api_json.zig");
const app_error = @import("app_error.zig");
const time_fmt = @import("time_fmt.zig");
const types = @import("servers_types.zig");
const svc_mod = @import("servers_service.zig");
const ServerService = svc_mod.ServerService;

/// A Server as a response shows it (dto.go serverJSON), in the Go field order.
pub const Json = struct {
    s: types.Server,

    pub fn jsonStringify(self: Json, jw: anytype) !void {
        const s = self.s;
        try jw.beginObject();
        try jw.objectField("id");
        try jw.write(s.id);
        try jw.objectField("name");
        try jw.write(s.name);
        try jw.objectField("host");
        try jw.write(s.host);
        try jw.objectField("port");
        try jw.write(s.port);
        try jw.objectField("username");
        try jw.write(s.username);
        try jw.objectField("has_password");
        try jw.write(s.hasPassword());
        try jw.objectField("password_unavailable");
        try jw.write(s.password_unavailable);
        try jw.objectField("max_connections");
        try jw.write(s.max_connections);
        try jw.objectField("retention_days");
        try jw.write(s.retention_days);
        try jw.objectField("priority");
        try jw.write(s.priority);
        try jw.objectField("enabled");
        try jw.write(s.enabled);
        try jw.objectField("created_at");
        var buf: [40]u8 = undefined;
        var w: std.Io.Writer = .fixed(&buf);
        try time_fmt.writeRfc3339(&w, s.created_at_ms);
        try jw.write(w.buffered());
        try jw.endObject();
    }
};

const Int = api_json.Int;

fn int(v: ?Int) ?i64 {
    return if (v) |i| i.value else null;
}

/// Request body of POST /servers and PATCH /servers/{id}: the same fields. A
/// null is the same as a missing field, as in Go.
const Body = struct {
    name: ?[]const u8 = null,
    host: ?[]const u8 = null,
    port: ?Int = null,
    username: ?[]const u8 = null,
    password: ?[]const u8 = null,
    max_connections: ?Int = null,
    retention_days: ?Int = null,
    priority: ?Int = null,
    enabled: ?bool = null,

    /// For a new Server: a missing field takes the zero value, which the
    /// service turns into the default.
    fn spec(b: Body) svc_mod.Spec {
        return .{
            .name = b.name orelse "",
            .host = b.host orelse "",
            .port = int(b.port) orelse 0,
            .username = b.username orelse "",
            .password = b.password orelse "",
            .max_connections = int(b.max_connections) orelse 0,
            .retention_days = int(b.retention_days) orelse 0,
            .priority = int(b.priority) orelse 0,
            .enabled = b.enabled,
        };
    }

    /// For a change: a missing field stays as it is.
    fn patch(b: Body) svc_mod.Patch {
        return .{
            .name = b.name,
            .host = b.host,
            .port = int(b.port),
            .username = b.username,
            .password = b.password,
            .max_connections = int(b.max_connections),
            .retention_days = int(b.retention_days),
            .priority = int(b.priority),
            .enabled = b.enabled,
        };
    }
};

pub fn register(routes: *ex.Routes, svc: *ServerService) !void {
    const small = respond.small_body_limit;
    try routes.add("GET /api/v1/servers", .{ .handler = list, .state = svc, .body_limit = small });
    try routes.add("POST /api/v1/servers", .{ .handler = create, .state = svc, .body_limit = small });
    try routes.add("POST /api/v1/servers/test", .{ .handler = testUnavailable, .state = svc, .body_limit = small });
    try routes.add("GET /api/v1/servers/{id}", .{ .handler = get, .state = svc, .body_limit = small });
    try routes.add("PATCH /api/v1/servers/{id}", .{ .handler = patch, .state = svc, .body_limit = small });
    try routes.add("DELETE /api/v1/servers/{id}", .{ .handler = delete, .state = svc, .body_limit = small });
    try routes.add("POST /api/v1/servers/{id}/test", .{ .handler = testUnavailable, .state = svc, .body_limit = small });
}

fn service(state: ?*anyopaque) *ServerService {
    return @ptrCast(@alignCast(state.?));
}

/// Zeroes a password the JSON parser handed out. Strings without escapes point
/// into the request body, which `bodyAlloc` put in the arena, so this wipes
/// the body's copy; one with escapes is a copy of its own.
fn wipe(secret: ?[]const u8) void {
    if (secret) |s| std.crypto.secureZero(u8, @constCast(s));
}

fn list(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const servers = service(state).list(e.arena) catch |err| return api_json.fail(e, err, &.{});
    defer types.deinitList(e.arena, servers);
    const items = try e.arena.alloc(Json, servers.len);
    for (servers, items) |s, *item| item.* = .{ .s = s };
    try e.res.json(e.arena, .ok, .{ .items = items });
}

fn get(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    var srv = service(state).get(e.arena, e.param("id").?) catch |err| return api_json.fail(e, err, &.{});
    defer srv.deinit(e.arena);
    try e.res.json(e.arena, .ok, Json{ .s = srv });
}

fn create(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const body = (try api_json.decode(Body, e)) orelse return;
    return createWith(service(state), e, body);
}

fn createWith(svc: *ServerService, e: *ex.Exchange, body: Body) anyerror!void {
    defer wipe(body.password);
    var f: app_error.Failure = .{};
    var srv = svc.create(e.arena, body.spec(), &f) catch |err| return api_json.fail(e, err, &f);
    defer srv.deinit(e.arena);
    try e.res.json(e.arena, .created, Json{ .s = srv });
}

fn patch(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const body = (try api_json.decode(Body, e)) orelse return;
    return patchWith(service(state), e, body);
}

fn patchWith(svc: *ServerService, e: *ex.Exchange, body: Body) anyerror!void {
    defer wipe(body.password);
    var f: app_error.Failure = .{};
    var srv = svc.update(e.arena, e.param("id").?, body.patch(), &f) catch |err| return api_json.fail(e, err, &f);
    defer srv.deinit(e.arena);
    try e.res.json(e.arena, .ok, Json{ .s = srv });
}

fn delete(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    service(state).delete(e.param("id").?) catch |err| return api_json.fail(e, err, &.{});
    e.res.status = .no_content;
}

/// Both test routes, until the NNTP client exists (Z3).
fn testUnavailable(_: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    var f: app_error.Failure = .{};
    const err = f.setLiteral(.unavailable, "server_test_unavailable", "testing a server connection is not available in this build yet");
    return api_json.fail(e, err, &f);
}

// ---- tests ----

const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const Key = @import("keyfile.zig").Key;
const Io = std.Io;

var test_now: i64 = 1_700_000_000_123;
fn testClock(_: Io) i64 {
    return test_now;
}

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    key: Key,
    svc: ServerService,
    arena_state: std.heap.ArenaAllocator,
    changes: usize = 0,

    fn notify(ctx: *anyopaque) void {
        const r: *Rig = @ptrCast(@alignCast(ctx));
        r.changes += 1;
    }

    fn open(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        const path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..n]});
        defer testing.allocator.free(path);
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        errdefer rig.store.close();
        {
            const w = try rig.store.acquireWriter();
            defer w.release();
            var diag: migrate.Diagnostic = .{};
            try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        }
        rig.key = .{ .bytes = @splat(7) };
        rig.changes = 0;
        rig.svc = ServerService.init(testing.io, rig.store, &rig.key, .{ .ctx = rig, .notify_fn = notify }, testClock);
        rig.arena_state = .init(testing.allocator);
    }

    fn close(rig: *Rig) void {
        rig.arena_state.deinit();
        rig.key.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }

    /// An Exchange with no connection behind it: enough for handlers that
    /// work on a parsed body.
    fn exchange(rig: *Rig, params: []const @import("router.zig").Param) ex.Exchange {
        return .{
            .arena = rig.arena_state.allocator(),
            .io = testing.io,
            .method = .GET,
            .path = "",
            .query = "",
            .params = params,
            .head = "",
            .remote = "",
            .request_id = "",
            .content_length = null,
            .body_limit = respond.small_body_limit,
            .request = undefined,
        };
    }

    fn create(rig: *Rig, body: []const u8) !ex.Exchange {
        var e = rig.exchange(&.{});
        const parsed = try api_json.parseBody(Body, e.arena, try e.arena.dupe(u8, body));
        try createWith(&rig.svc, &e, parsed.ok);
        return e;
    }

    fn patchServer(rig: *Rig, id: []const u8, body: []const u8) !ex.Exchange {
        const params = try rig.arena_state.allocator().dupe(@import("router.zig").Param, &.{.{ .name = "id", .value = id }});
        var e = rig.exchange(params);
        switch (try api_json.parseBody(Body, e.arena, try e.arena.dupe(u8, body))) {
            .ok => |b| try patchWith(&rig.svc, &e, b),
            .invalid => |msg| try e.res.fail(e.arena, .bad_request, "invalid_json", msg),
        }
        return e;
    }

    /// The id of the Server in a create response.
    fn idOf(rig: *Rig, e: ex.Exchange) ![]const u8 {
        const parsed = try std.json.parseFromSliceLeaky(struct { id: []const u8 }, rig.arena_state.allocator(), e.res.body, .{ .ignore_unknown_fields = true });
        return parsed.id;
    }

    fn call(rig: *Rig, handler: ex.Handler, params: []const @import("router.zig").Param) !ex.Exchange {
        var e = rig.exchange(params);
        try handler(&rig.svc, &e);
        return e;
    }
};

fn expectError(e: ex.Exchange, status: std.http.Status, code: []const u8) !void {
    try testing.expectEqual(status, e.res.status);
    var buf: [128]u8 = undefined;
    const want = try std.fmt.bufPrint(&buf, "{{\"error\":{{\"code\":\"{s}\",\"message\":\"", .{code});
    try testing.expect(std.mem.startsWith(u8, e.res.body, want));
}

test "create answers 201 with the Go JSON and no password" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const e = try rig.create("{\"name\":\"main\",\"host\":\"news.example.com\",\"username\":\"joe\",\"password\":\"pw-SECRET-4711\"}");

    try testing.expectEqual(std.http.Status.created, e.res.status);
    try testing.expectEqualStrings("application/json", e.res.header("Content-Type").?);
    const id = try rig.idOf(e);
    // The expected text is what encoding/json writes for dto.go's serverJSON.
    const want = try std.fmt.allocPrint(e.arena, "{{\"id\":\"{s}\",\"name\":\"main\",\"host\":\"news.example.com\",\"port\":563,\"username\":\"joe\"," ++
        "\"has_password\":true,\"password_unavailable\":false,\"max_connections\":8,\"retention_days\":0,\"priority\":0,\"enabled\":true," ++
        "\"created_at\":\"2023-11-14T22:13:20.123Z\"}}\n", .{id});
    try testing.expectEqualStrings(want, e.res.body);
    try testing.expect(std.mem.indexOf(u8, e.res.body, "SECRET") == null);
}

test "create takes nulls for missing fields and refuses bad input like Go" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const ok = try rig.create("{\"name\":\"a\",\"host\":\"h\",\"port\":null,\"enabled\":null,\"password\":null}");
    try testing.expectEqual(std.http.Status.created, ok.res.status);

    const cases = [_]struct { body: []const u8, status: std.http.Status, code: []const u8 }{
        .{ .body = "{\"host\":\"h.example.com\"}", .status = .bad_request, .code = "invalid_name" },
        .{ .body = "{\"name\":\"x\"}", .status = .bad_request, .code = "invalid_host" },
        .{ .body = "{\"name\":\"x\",\"host\":\"https://h.example.com\"}", .status = .bad_request, .code = "invalid_host" },
        .{ .body = "{\"name\":\"x\",\"host\":\"h\",\"port\":99999}", .status = .bad_request, .code = "invalid_port" },
        .{ .body = "{\"name\":\"x\",\"host\":\"h\",\"max_connections\":500}", .status = .bad_request, .code = "invalid_max_connections" },
        .{ .body = "{\"name\":\"x\",\"host\":\"h\",\"retention_days\":-5}", .status = .bad_request, .code = "invalid_retention_days" },
        .{ .body = "{\"name\":\"x\",\"host\":\"h\",\"priority\":-1}", .status = .bad_request, .code = "invalid_priority" },
        .{ .body = "{\"name\":\"a\",\"host\":\"h\"}", .status = .conflict, .code = "name_taken" },
    };
    for (cases) |c| try expectError(try rig.create(c.body), c.status, c.code);

    // Shape errors are the body parser's: invalid_json, with no echo of values.
    for ([_][]const u8{
        "{\"name\":\"x\",\"host\":\"h\",\"port\":\"563\"}",
        "{\"name\":\"x\",\"host\":\"h\",\"port\":563.0}",
        "{\"name\":\"x\",\"host\":\"h\",\"port\":5e2}",
        "{\"name\":\"x\",\"host\":\"h\",\"port\":99999999999999999999}",
        "{\"name\":\"x\",\"host\":\"h\",\"password\":5}",
        "{\"name\":\"x\",\"host\":\"h\",\"id\":\"abc\"}",
        "{\"name\":\"x\",\"host\":\"h\",\"insecure\":true}",
    }) |body| {
        const parsed = try api_json.parseBody(Body, rig.arena_state.allocator(), body);
        try testing.expect(parsed == .invalid);
    }
}

test "list is ordered, wrapped in items and never null" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const empty = try rig.call(list, &.{});
    try testing.expectEqualStrings("{\"items\":[]}\n", empty.res.body);

    _ = try rig.create("{\"name\":\"zeta\",\"host\":\"h\",\"priority\":0}");
    _ = try rig.create("{\"name\":\"fill\",\"host\":\"h\",\"priority\":1}");
    _ = try rig.create("{\"name\":\"alpha\",\"host\":\"h\",\"priority\":0}");

    const e = try rig.call(list, &.{});
    const Item = struct { name: []const u8, has_password: bool };
    const out = try std.json.parseFromSliceLeaky(struct { items: []Item }, e.arena, e.res.body, .{ .ignore_unknown_fields = true });
    try testing.expectEqual(@as(usize, 3), out.items.len);
    try testing.expectEqualStrings("alpha", out.items[0].name);
    try testing.expectEqualStrings("zeta", out.items[1].name);
    try testing.expectEqualStrings("fill", out.items[2].name);
    try testing.expect(!out.items[2].has_password);
}

test "get, patch and delete" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const created = try rig.create("{\"name\":\"main\",\"host\":\"h.example.com\",\"password\":\"pw\"}");
    const id = try rig.idOf(created);
    const params = [_]@import("router.zig").Param{.{ .name = "id", .value = id }};
    const missing = [_]@import("router.zig").Param{.{ .name = "id", .value = "nope" }};

    const got = try rig.call(get, &params);
    try testing.expectEqualStrings(created.res.body, got.res.body);
    try expectError(try rig.call(get, &missing), .not_found, "not_found");

    const renamed = try rig.patchServer(id, "{\"name\":\"renamed\",\"port\":443,\"enabled\":false,\"priority\":2,\"max_connections\":3,\"retention_days\":100}");
    try testing.expectEqual(std.http.Status.ok, renamed.res.status);
    try testing.expect(std.mem.indexOf(u8, renamed.res.body, "\"name\":\"renamed\",\"host\":\"h.example.com\",\"port\":443") != null);
    try testing.expect(std.mem.indexOf(u8, renamed.res.body, "\"has_password\":true") != null);
    try testing.expect(std.mem.indexOf(u8, renamed.res.body, "\"enabled\":false") != null);

    try expectError(try rig.patchServer(id, "{}"), .bad_request, "empty_patch");
    try expectError(try rig.patchServer(id, "{\"name\":null}"), .bad_request, "empty_patch");
    try expectError(try rig.patchServer(id, "{\"host\":\"a b\"}"), .bad_request, "invalid_host");
    try expectError(try rig.patchServer(id, "{\"port\":0}"), .bad_request, "invalid_port");
    try expectError(try rig.patchServer("nope", "{\"name\":\"x\"}"), .not_found, "not_found");
    try expectError(try rig.patchServer(id, "{\"insecure\":true}"), .bad_request, "invalid_json");
    _ = try rig.create("{\"name\":\"b\",\"host\":\"h\"}");
    try expectError(try rig.patchServer(id, "{\"name\":\"b\"}"), .conflict, "name_taken");

    const gone = try rig.call(delete, &params);
    try testing.expectEqual(std.http.Status.no_content, gone.res.status);
    try testing.expectEqualStrings("", gone.res.body);
    try expectError(try rig.call(delete, &params), .not_found, "not_found");
}

test "a Server with an unreadable password is listed with both flags" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    _ = try rig.create("{\"name\":\"main\",\"host\":\"h\",\"password\":\"pw\"}");
    var other: Key = .{ .bytes = @splat(9) };
    defer other.deinit();
    rig.svc.repo.key = &other;

    const e = try rig.call(list, &.{});

    try testing.expect(std.mem.indexOf(u8, e.res.body, "\"has_password\":true,\"password_unavailable\":true") != null);
}

test "no response or error holds the password" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const secret = "pw-SECRET-4711";
    const created = try rig.create("{\"name\":\"main\",\"host\":\"h\",\"password\":\"" ++ secret ++ "\"}");
    const id = try rig.idOf(created);
    const bodies = [_]ex.Exchange{
        created,
        try rig.call(list, &.{}),
        try rig.call(get, &.{.{ .name = "id", .value = id }}),
        try rig.patchServer(id, "{\"password\":\"" ++ secret ++ "-2\",\"host\":\"x.example.com\"}"),
        try rig.create("{\"name\":\"y\",\"host\":\"\",\"port\":0,\"password\":\"" ++ secret ++ "\"}"),
    };
    for (bodies) |b| try testing.expect(std.mem.indexOf(u8, b.res.body, "SECRET") == null);
}

test "the test routes answer 503 server_test_unavailable" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const e = try rig.call(testUnavailable, &.{});
    try expectError(e, .service_unavailable, "server_test_unavailable");
}

test "the routes are registered with the Go patterns" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var routes = ex.Routes.init(testing.allocator);
    defer routes.deinit();
    try register(&routes, &rig.svc);
    for ([_]struct { method: std.http.Method, path: []const u8 }{
        .{ .method = .GET, .path = "/api/v1/servers" },
        .{ .method = .POST, .path = "/api/v1/servers" },
        .{ .method = .POST, .path = "/api/v1/servers/test" },
        .{ .method = .GET, .path = "/api/v1/servers/abc" },
        .{ .method = .PATCH, .path = "/api/v1/servers/abc" },
        .{ .method = .DELETE, .path = "/api/v1/servers/abc" },
        .{ .method = .POST, .path = "/api/v1/servers/abc/test" },
    }) |r| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const m = try routes.lookup(arena.allocator(), r.method, r.path);
        try testing.expect(m == .found);
        try testing.expectEqual(respond.small_body_limit, m.found.value.body_limit);
        try testing.expectEqual(ex.Auth.key, m.found.value.auth);
    }
}

fn fuzzBody(_: void, smith: *testing.Smith) !void {
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    switch (try api_json.parseBody(Body, arena.allocator(), buf[0..n])) {
        .ok => |b| {
            // Whatever parsed must also be checkable without crashing.
            var srv: types.Server = .{ .name = b.name orelse "", .host = b.host orelse "" };
            srv.port = int(b.port) orelse 0;
            _ = types.check(srv);
        },
        .invalid => {},
    }
}

fn seed(comptime x: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.*);
}

test "fuzz: request bodies never crash" {
    try testing.fuzz({}, fuzzBody, .{ .corpus = &.{
        seed("{\"name\":\"main\",\"host\":\"news.example.com\",\"port\":563,\"password\":\"x\"}"),
        seed("{\"port\":\"563\",\"enabled\":null}"),
        seed("{\"port\":99999999999999999999}"),
        seed("{\"port\":1e2,\"name\":\"\\u0000\"}"),
        seed("[]"),
    } });
}
