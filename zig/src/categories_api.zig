//! The Category routes (internal/api/handlers_categories.go):
//! GET and POST /api/v1/categories, PATCH and DELETE /api/v1/categories/{id}.
//! Bodies are small (16 KiB) like in routes.go.
const std = @import("std");
const api_json = @import("api_json.zig");
const app_error = @import("app_error.zig");
const ex = @import("http_exchange.zig");
const respond = @import("respond.zig");
const types = @import("categories_types.zig");
const svc_mod = @import("categories_service.zig");
const Service = svc_mod.Service;

/// Registers the routes. `svc` must outlive the router.
pub fn register(routes: *ex.Routes, svc: *Service) !void {
    const limit = respond.small_body_limit;
    try routes.add("GET /api/v1/categories", .{ .handler = listHandler, .state = svc, .body_limit = limit });
    try routes.add("POST /api/v1/categories", .{ .handler = createHandler, .state = svc, .body_limit = limit });
    try routes.add("PATCH /api/v1/categories/{id}", .{ .handler = patchHandler, .state = svc, .body_limit = limit });
    try routes.add("DELETE /api/v1/categories/{id}", .{ .handler = deleteHandler, .state = svc, .body_limit = limit });
}

/// A Category as responses show it: a folder override is null when unset.
pub const Json = struct {
    c: types.Category,

    pub fn jsonStringify(self: Json, jw: anytype) !void {
        try jw.beginObject();
        try jw.objectField("id");
        try jw.write(self.c.id);
        try jw.objectField("name");
        try jw.write(self.c.name);
        try jw.objectField("usenet_dir");
        try jw.write(if (self.c.usenet_dir.len == 0) null else self.c.usenet_dir);
        try jw.objectField("torrent_dir");
        try jw.write(if (self.c.torrent_dir.len == 0) null else self.c.torrent_dir);
        try jw.endObject();
    }
};

// Go decodes null into a plain string as "no value", so these accept null too.
pub const CreateBody = struct {
    name: ?[]const u8 = null,
    usenet_dir: ?[]const u8 = null,
    torrent_dir: ?[]const u8 = null,
};

pub const PatchBody = struct {
    name: ?[]const u8 = null,
    usenet_dir: api_json.Nullable([]const u8) = .{},
    torrent_dir: api_json.Nullable([]const u8) = .{},
};

fn service(state: ?*anyopaque) *Service {
    return @ptrCast(@alignCast(state.?));
}

fn listHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try list(service(state), e);
}

fn createHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const body = (try api_json.decode(CreateBody, e)) orelse return;
    try create(service(state), e, body);
}

fn patchHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const body = (try api_json.decode(PatchBody, e)) orelse return;
    try patch(service(state), e, body);
}

fn deleteHandler(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    try delete(service(state), e);
}

// The handlers proper take the decoded body, so tests need no socket.

fn list(svc: *Service, e: *ex.Exchange) !void {
    const cats = svc.list(e.arena) catch |err| return api_json.fail(e, err, &.{});
    const items = try e.arena.alloc(Json, cats.len);
    for (cats, items) |c, *out| out.* = .{ .c = c };
    try e.res.json(e.arena, .ok, .{ .items = items });
}

fn create(svc: *Service, e: *ex.Exchange, body: CreateBody) !void {
    var f: app_error.Failure = .{};
    const c = svc.create(e.arena, &f, body.name orelse "", body.usenet_dir orelse "", body.torrent_dir orelse "") catch |err|
        return api_json.fail(e, err, &f);
    try e.res.json(e.arena, .created, Json{ .c = c });
}

fn patch(svc: *Service, e: *ex.Exchange, body: PatchBody) !void {
    var f: app_error.Failure = .{};
    // null clears an override, like the empty string.
    const p: svc_mod.CategoryPatch = .{
        .name = body.name,
        .usenet_dir = if (body.usenet_dir.set) body.usenet_dir.value orelse "" else null,
        .torrent_dir = if (body.torrent_dir.set) body.torrent_dir.value orelse "" else null,
    };
    const c = svc.update(e.arena, &f, e.param("id") orelse "", p) catch |err| return api_json.fail(e, err, &f);
    try e.res.json(e.arena, .ok, Json{ .c = c });
}

fn delete(svc: *Service, e: *ex.Exchange) !void {
    svc.delete(e.arena, e.param("id") orelse "") catch |err| return api_json.fail(e, err, &.{});
    e.res.status = .no_content;
}

// ---- tests ----

const testing = std.testing;
const router = @import("router.zig");

fn fakeExchange(arena: std.mem.Allocator, params: []const router.Param) ex.Exchange {
    return .{
        .arena = arena,
        .io = testing.io,
        .method = .GET,
        .path = "/api/v1/categories",
        .query = "",
        .params = params,
        .head = "",
        .remote = "127.0.0.1:1",
        .request_id = "t",
        .content_length = null,
        .body_limit = respond.small_body_limit,
        .request = undefined,
    };
}

fn parsed(comptime T: type, arena: std.mem.Allocator, json: []const u8) !T {
    return (try api_json.parseBody(T, arena, json)).ok;
}

fn expectInvalidJson(comptime T: type, arena: std.mem.Allocator, json: []const u8) !void {
    try testing.expect((try api_json.parseBody(T, arena, json)) == .invalid);
}

fn expectError(e: *const ex.Exchange, status: u16, code: []const u8) !void {
    try testing.expectEqual(status, @backingInt(e.res.status));
    const want = try std.fmt.allocPrint(e.arena, "{{\"error\":{{\"code\":\"{s}\",", .{code});
    try testing.expect(std.mem.startsWith(u8, e.res.body, want));
}

test "create answers 201 with the stored category" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    var e = fakeExchange(rig.a(), &.{});

    try create(&rig.svc, &e, try parsed(CreateBody, rig.a(), "{\"name\":\"tv\",\"usenet_dir\":\"/data/tv\"}"));

    try testing.expectEqual(201, @backingInt(e.res.status));
    try testing.expectEqualStrings("application/json", e.res.header("Content-Type").?);
    const want = try std.fmt.allocPrint(rig.a(), "{{\"id\":\"{s}\",\"name\":\"tv\",\"usenet_dir\":\"/data/tv\",\"torrent_dir\":null}}\n", .{e.res.body[7..33]});
    try testing.expectEqualStrings(want, e.res.body);
    const stored = try rig.svc.repo.get(rig.a(), e.res.body[7..33]);
    try testing.expectEqualStrings("/data/tv", stored.usenet_dir);
}

test "list shows null for missing overrides and an empty list as items []" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    var e = fakeExchange(rig.a(), &.{});

    try list(&rig.svc, &e);
    try testing.expectEqualStrings("{\"items\":[]}\n", e.res.body);

    const tv = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    const movies = try rig.svc.create(rig.a(), &rig.f, "movies", "", "/data/torrents");
    try list(&rig.svc, &e);
    const want = try std.fmt.allocPrint(
        rig.a(),
        "{{\"items\":[{{\"id\":\"{s}\",\"name\":\"movies\",\"usenet_dir\":null,\"torrent_dir\":\"/data/torrents\"}},{{\"id\":\"{s}\",\"name\":\"tv\",\"usenet_dir\":null,\"torrent_dir\":null}}]}}\n",
        .{ movies.id, tv.id },
    );
    try testing.expectEqualStrings(want, e.res.body);
}

test "create refuses invalid input with the Go codes and statuses" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    _ = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");

    const cases = [_]struct { json: []const u8, status: u16, code: []const u8 }{
        .{ .json = "{\"name\":\"TV Shows\"}", .status = 400, .code = "invalid_name" },
        .{ .json = "{\"name\":\"..\"}", .status = 400, .code = "invalid_name" },
        .{ .json = "{}", .status = 400, .code = "invalid_name" },
        .{ .json = "{\"name\":null}", .status = 400, .code = "invalid_name" },
        .{ .json = "{\"name\":\"x\",\"torrent_dir\":\"rel/dir\"}", .status = 400, .code = "invalid_dir_override" },
        .{ .json = "{\"name\":\"tv\"}", .status = 409, .code = "name_taken" },
    };
    for (cases) |tc| {
        var e = fakeExchange(rig.a(), &.{});
        try create(&rig.svc, &e, try parsed(CreateBody, rig.a(), tc.json));
        try expectError(&e, tc.status, tc.code);
    }
}

test "create refuses the old dir_override field, unknown fields and wrong types" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for ([_][]const u8{
        "{\"name\":\"x\",\"dir_override\":\"/data/x\"}",
        "{\"name\":\"x\",\"id\":\"abc\"}",
        "{\"name\":5}",
        "{\"name\":\"x\",\"usenet_dir\":1}",
        "",
        "[]",
    }) |json| try expectInvalidJson(CreateBody, arena.allocator(), json);
    for ([_][]const u8{ "{\"dir_override\":null}", "{\"name\":5}", "{\"usenet_dir\":5}" }) |json| try expectInvalidJson(PatchBody, arena.allocator(), json);
}

test "patch renames, sets and clears the folder overrides" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    const c = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    const params = [_]router.Param{.{ .name = "id", .value = c.id }};

    const steps = [_]struct { json: []const u8, usenet: []const u8, torrent: []const u8, name: []const u8 }{
        .{ .json = "{\"name\":\"series\"}", .usenet = "null", .torrent = "null", .name = "series" },
        .{ .json = "{\"torrent_dir\":\"/data/series\"}", .usenet = "null", .torrent = "\"/data/series\"", .name = "series" },
        .{ .json = "{\"usenet_dir\":\"/data/usenet/series\"}", .usenet = "\"/data/usenet/series\"", .torrent = "\"/data/series\"", .name = "series" },
        .{ .json = "{\"usenet_dir\":null,\"torrent_dir\":null}", .usenet = "null", .torrent = "null", .name = "series" },
        .{ .json = "{\"name\":null,\"usenet_dir\":\"/x\"}", .usenet = "\"/x\"", .torrent = "null", .name = "series" },
    };
    for (steps) |s| {
        var e = fakeExchange(rig.a(), &params);
        try patch(&rig.svc, &e, try parsed(PatchBody, rig.a(), s.json));
        const want = try std.fmt.allocPrint(rig.a(), "{{\"id\":\"{s}\",\"name\":\"{s}\",\"usenet_dir\":{s},\"torrent_dir\":{s}}}\n", .{ c.id, s.name, s.usenet, s.torrent });
        try testing.expectEqual(200, @backingInt(e.res.status));
        try testing.expectEqualStrings(want, e.res.body);
    }
}

test "patch refuses invalid changes and unknown IDs" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    const c = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    _ = try rig.svc.create(rig.a(), &rig.f, "movies", "", "");
    const params = [_]router.Param{.{ .name = "id", .value = c.id }};
    const missing = [_]router.Param{.{ .name = "id", .value = "nope" }};

    const cases = [_]struct { json: []const u8, params: []const router.Param, status: u16, code: []const u8 }{
        .{ .json = "{\"name\":\"Bad\"}", .params = &params, .status = 400, .code = "invalid_name" },
        .{ .json = "{\"name\":\"movies\"}", .params = &params, .status = 409, .code = "name_taken" },
        .{ .json = "{}", .params = &params, .status = 400, .code = "empty_patch" },
        .{ .json = "{\"name\":null}", .params = &params, .status = 400, .code = "empty_patch" },
        .{ .json = "{\"usenet_dir\":\"rel\"}", .params = &params, .status = 400, .code = "invalid_dir_override" },
        .{ .json = "{\"name\":\"x\"}", .params = &missing, .status = 404, .code = "not_found" },
    };
    for (cases) |tc| {
        var e = fakeExchange(rig.a(), tc.params);
        try patch(&rig.svc, &e, try parsed(PatchBody, rig.a(), tc.json));
        try expectError(&e, tc.status, tc.code);
    }
}

test "delete answers 204 once, then 404, and announces the downloads" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    const c = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    var rows = [_]svc_mod.FakeDownloads.Row{.{ .id = "d1", .category_id = c.id }};
    rig.downloads.rows = &rows;
    rig.downloads.repo = rig.svc.repo;
    const params = [_]router.Param{.{ .name = "id", .value = c.id }};

    var e = fakeExchange(rig.a(), &params);
    try delete(&rig.svc, &e);
    try testing.expectEqual(204, @backingInt(e.res.status));
    try testing.expectEqualStrings("", e.res.body);
    try testing.expectEqual(1, rig.recorder.seen.items.len);

    var again = fakeExchange(rig.a(), &params);
    try delete(&rig.svc, &again);
    try expectError(&again, 404, "not_found");
}

test "the four routes are registered with the small body limit and the key" {
    var rig: svc_mod.Rig = undefined;
    try rig.open();
    defer rig.close();
    var routes = ex.Routes.init(testing.allocator);
    defer routes.deinit();
    try register(&routes, &rig.svc);

    const cases = [_]struct { method: ex.Method, path: []const u8 }{
        .{ .method = .GET, .path = "/api/v1/categories" },
        .{ .method = .POST, .path = "/api/v1/categories" },
        .{ .method = .PATCH, .path = "/api/v1/categories/abc" },
        .{ .method = .DELETE, .path = "/api/v1/categories/abc" },
    };
    for (cases) |tc| {
        const m = try routes.lookup(rig.a(), tc.method, tc.path);
        try testing.expect(m == .found);
        try testing.expectEqual(respond.small_body_limit, m.found.value.body_limit);
        try testing.expectEqual(ex.Auth.key, m.found.value.auth);
    }
    const m = try routes.lookup(rig.a(), .PATCH, "/api/v1/categories/abc");
    try testing.expectEqualStrings("abc", m.found.params[0].value);
    try testing.expect((try routes.lookup(rig.a(), .PUT, "/api/v1/categories")) == .method_not_allowed);
}

fn fuzzPatchBody(_: void, smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    _ = try api_json.parseBody(PatchBody, arena.allocator(), buf[0..n]);
    _ = try api_json.parseBody(CreateBody, arena.allocator(), buf[0..n]);
}

test "fuzz: request bodies never crash or leak" {
    try testing.fuzz({}, fuzzPatchBody, .{ .corpus = &.{
        "\x1b\x00\x00\x00{\"usenet_dir\":null,\"name\":\"a\"}",
        "\x10\x00\x00\x00{\"torrent_dir\":5}",
    } });
}
