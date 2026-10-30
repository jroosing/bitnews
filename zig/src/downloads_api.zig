//! The Downloads routes (internal/api/handlers_downloads.go):
//!
//!   GET    /api/v1/downloads            list, with filters and a cursor
//!   GET    /api/v1/downloads/{id}
//!   POST   /api/v1/downloads            adding (NZB or torrent): not in this build yet
//!   POST   /api/v1/downloads/{id}/pause
//!   POST   /api/v1/downloads/{id}/resume
//!   PATCH  /api/v1/downloads/{id}       priority, category, queue position
//!   DELETE /api/v1/downloads/{id}       ?delete_data=true removes the files too
//!
//! Every route needs the app key (the server's `auth = .key`).
const std = @import("std");
const Allocator = std.mem.Allocator;
const ex = @import("http_exchange.zig");
const Exchange = ex.Exchange;
const respond = @import("respond.zig");
const router = @import("router.zig");
const api_json = @import("api_json.zig");
const app_error = @import("app_error.zig");
const download_json = @import("download_json.zig");
const repo_mod = @import("downloads_repo.zig");
const svc_mod = @import("downloads_service.zig");
const Service = svc_mod.Service;

/// Room for an NZB of up to 64 MiB plus the multipart framing and form fields
/// (addBodyLimit in routes.go).
const add_body_limit: u64 = (64 << 20) + (1 << 20);

/// Registers the routes. `svc` must outlive the router.
pub fn register(routes: *ex.Routes, svc: *Service) !void {
    const small = respond.small_body_limit;
    try routes.add("GET /api/v1/downloads", .{ .handler = listDownloads, .state = svc, .body_limit = small });
    try routes.add("GET /api/v1/downloads/{id}", .{ .handler = getDownload, .state = svc, .body_limit = small });
    // The lead replaces this handler when adding exists (milestones Z3 and Z7).
    try routes.add("POST /api/v1/downloads", .{ .handler = addUnavailable, .state = svc, .body_limit = add_body_limit });
    try routes.add("POST /api/v1/downloads/{id}/pause", .{ .handler = pauseDownload, .state = svc, .body_limit = small });
    try routes.add("POST /api/v1/downloads/{id}/resume", .{ .handler = resumeDownload, .state = svc, .body_limit = small });
    try routes.add("PATCH /api/v1/downloads/{id}", .{ .handler = patchDownload, .state = svc, .body_limit = small });
    try routes.add("DELETE /api/v1/downloads/{id}", .{ .handler = deleteDownload, .state = svc, .body_limit = small });
}

fn service(state: ?*anyopaque) *Service {
    return @ptrCast(@alignCast(state.?));
}

/// Adding needs the NZB parser and the staging area (Z3) or the torrent engine
/// (Z7); until then the route exists, so clients get a clear answer.
fn addUnavailable(_: ?*anyopaque, e: *Exchange) anyerror!void {
    var f: app_error.Failure = .{};
    return api_json.fail(e, f.setLiteral(.unavailable, "add_unavailable", "adding downloads is not available in this build yet"), &f);
}

// ---- query string ----

const Pair = struct { name: []const u8, value: []const u8 };

/// The pairs of a query string like Go's url.Query(): pairs with a ';' or a bad
/// escape are dropped, '+' is a space.
fn parseQuery(arena: Allocator, raw: []const u8) Allocator.Error![]const Pair {
    var out: std.ArrayList(Pair) = .empty;
    var it = std.mem.splitScalar(u8, raw, '&');
    while (it.next()) |item| {
        if (item.len == 0 or std.mem.indexOfScalar(u8, item, ';') != null) continue;
        const eq = std.mem.indexOfScalar(u8, item, '=');
        const name = try unescape(arena, item[0 .. eq orelse item.len]) orelse continue;
        const value = if (eq) |i| try unescape(arena, item[i + 1 ..]) orelse continue else "";
        try out.append(arena, .{ .name = name, .value = value });
    }
    return out.items;
}

/// Go's url.QueryUnescape; null for a bad escape.
fn unescape(arena: Allocator, s: []const u8) Allocator.Error!?[]const u8 {
    if (std.mem.indexOfAny(u8, s, "%+") == null) return s;
    if (!router.validEscapes(s)) return null;
    const spaced = try arena.dupe(u8, s);
    for (spaced) |*c| {
        if (c.* == '+') c.* = ' ';
    }
    return try router.percentDecode(arena, spaced);
}

/// The first value of `name`, or "" (Go: url.Values.Get).
fn queryGet(pairs: []const Pair, name: []const u8) []const u8 {
    for (pairs) |p| if (std.mem.eql(u8, p.name, name)) return p.value;
    return "";
}

/// Answers 400 for a parameter outside `allowed`, so a typo such as "stat=ready"
/// does not silently return everything. True when the request was refused.
fn rejectUnknownParams(e: *Exchange, pairs: []const Pair, allowed: []const []const u8) !bool {
    for (pairs) |p| {
        const known = for (allowed) |a| {
            if (std.mem.eql(u8, a, p.name)) break true;
        } else false;
        if (known) continue;
        var f: app_error.Failure = .{};
        const quoted = try svc_mod.quote(e.arena, p.name);
        const err = f.set(e.arena, .invalid_input, "unknown_parameter", "unknown query parameter {s}", .{quoted});
        try api_json.fail(e, err, &f);
        return true;
    }
    return false;
}

/// Go's strconv.ParseBool.
fn parseBool(s: []const u8) ?bool {
    const yes = [_][]const u8{ "1", "t", "T", "TRUE", "true", "True" };
    const no = [_][]const u8{ "0", "f", "F", "FALSE", "false", "False" };
    for (yes) |y| if (std.mem.eql(u8, s, y)) return true;
    for (no) |n| if (std.mem.eql(u8, s, n)) return false;
    return null;
}

// ---- handlers ----

fn listDownloads(state: ?*anyopaque, e: *Exchange) anyerror!void {
    const svc = service(state);
    const pairs = try parseQuery(e.arena, e.query);
    if (try rejectUnknownParams(e, pairs, &.{ "state", "category_id", "kind", "after", "limit" })) return;
    var filter: svc_mod.ListFilter = .{
        .state = queryGet(pairs, "state"),
        .category_id = queryGet(pairs, "category_id"),
        .kind = queryGet(pairs, "kind"),
        .after = queryGet(pairs, "after"),
    };
    const raw_limit = queryGet(pairs, "limit");
    if (raw_limit.len > 0) {
        const n = repo_mod.parseDecimal(raw_limit) orelse 0;
        if (n < 1) return e.res.fail(e.arena, .bad_request, "invalid_limit", "limit must be a positive integer");
        filter.limit = n;
    }
    var failure: app_error.Failure = .{};
    const page = svc.list(e.arena, &failure, filter) catch |err| return api_json.fail(e, err, &failure);
    const items = try e.arena.alloc(download_json.Json, page.items.len);
    for (page.items, items) |d, *out| out.* = .{ .d = d };
    try e.res.json(e.arena, .ok, .{ .items = items, .next = page.next });
}

fn getDownload(state: ?*anyopaque, e: *Exchange) anyerror!void {
    const d = service(state).get(e.arena, e.param("id").?) catch |err| {
        var failure: app_error.Failure = .{};
        return api_json.fail(e, err, &failure);
    };
    try e.res.json(e.arena, .ok, download_json.Json{ .d = d });
}

fn pauseDownload(state: ?*anyopaque, e: *Exchange) anyerror!void {
    var failure: app_error.Failure = .{};
    const d = service(state).pause(e.arena, &failure, e.param("id").?) catch |err| return api_json.fail(e, err, &failure);
    try e.res.json(e.arena, .ok, download_json.Json{ .d = d });
}

fn resumeDownload(state: ?*anyopaque, e: *Exchange) anyerror!void {
    var failure: app_error.Failure = .{};
    const d = service(state).resume_(e.arena, &failure, e.param("id").?) catch |err| return api_json.fail(e, err, &failure);
    try e.res.json(e.arena, .ok, download_json.Json{ .d = d });
}

/// The body of PATCH: only these fields; the state is deliberately not one.
const PatchBody = struct {
    priority: ?[]const u8 = null,
    category_id: api_json.Nullable([]const u8) = .{},
    queue_position: ?struct { move: []const u8 = "" } = null,
};

fn patchDownload(state: ?*anyopaque, e: *Exchange) anyerror!void {
    const body = try api_json.decode(PatchBody, e) orelse return;
    var patch: svc_mod.Patch = .{ .priority = body.priority };
    if (body.category_id.set) patch.category = .{ .set = true, .id = body.category_id.value orelse "" };
    if (body.queue_position) |qp| {
        if (qp.move.len == 0) return e.res.fail(e.arena, .bad_request, "invalid_move", "queue_position needs a \"move\": top, bottom, up or down");
        patch.move = qp.move;
    }
    var failure: app_error.Failure = .{};
    const d = service(state).update(e.arena, &failure, e.param("id").?, patch) catch |err| return api_json.fail(e, err, &failure);
    try e.res.json(e.arena, .ok, download_json.Json{ .d = d });
}

fn deleteDownload(state: ?*anyopaque, e: *Exchange) anyerror!void {
    const pairs = try parseQuery(e.arena, e.query);
    if (try rejectUnknownParams(e, pairs, &.{"delete_data"})) return;
    var delete_data = false;
    const raw = queryGet(pairs, "delete_data");
    if (raw.len > 0) {
        delete_data = parseBool(raw) orelse
            return e.res.fail(e.arena, .bad_request, "invalid_delete_data", "delete_data must be true or false");
    }
    var failure: app_error.Failure = .{};
    service(state).delete(e.arena, &failure, e.param("id").?, delete_data) catch |err| return api_json.fail(e, err, &failure);
    e.res.status = .no_content;
}

// ---- tests ----

const Io = std.Io;
const testing = std.testing;
const srv = @import("http_server.zig");
const common = @import("http_common.zig");
const dl = @import("download.zig");
const tu = @import("downloads_testutil.zig");

const test_key = "bitnews_testkey0123456789";

const TestKeys = struct {
    fn check(_: *anyopaque, presented: []const u8) bool {
        return std.mem.eql(u8, presented, test_key);
    }
    var dummy: u8 = 0;
    const checker: common.KeyChecker = .{ .ctx = &dummy, .check = check };
};

const Reply = struct {
    status: std.http.Status,
    body: []const u8,
};

/// A real server with the downloads routes on a service over a temp database.
const Harness = struct {
    fx: svc_mod.Fixture,
    server: *srv.Server,
    fut: Io.Future(error{NotListening}!void),
    port: u16,
    client: std.http.Client,

    fn start(h: *Harness) !void {
        try h.fx.open();
        errdefer h.fx.close();
        h.server = try srv.Server.init(testing.allocator, testing.io, .{ .keys = TestKeys.checker });
        errdefer h.server.deinit();
        try register(&h.server.routes, &h.fx.svc);
        h.port = try h.server.listen("127.0.0.1:0");
        h.fut = try testing.io.concurrent(srv.Server.serve, .{h.server});
        h.client = .{ .allocator = testing.allocator, .io = testing.io };
    }

    fn deinit(h: *Harness) void {
        h.client.deinit();
        h.fut.cancel(testing.io) catch {};
        h.server.deinit();
        h.fx.close();
    }

    fn send(h: *Harness, method: std.http.Method, target: []const u8, body: ?[]const u8, key: ?[]const u8) !Reply {
        const a = h.fx.a();
        const url = try std.fmt.allocPrint(a, "http://127.0.0.1:{d}{s}", .{ h.port, target });
        var out: Io.Writer.Allocating = .init(a);
        const bearer = if (key) |k| try std.fmt.allocPrint(a, "Bearer {s}", .{k}) else null;
        const res = try h.client.fetch(.{
            .location = .{ .url = url },
            .method = method,
            .payload = body,
            .headers = .{
                .authorization = if (bearer) |b| .{ .override = b } else .default,
                .content_type = if (body != null) .{ .override = "application/json" } else .default,
            },
            .response_writer = &out.writer,
        });
        return .{ .status = res.status, .body = out.written() };
    }

    fn get(h: *Harness, target: []const u8) !Reply {
        return h.send(.GET, target, null, test_key);
    }

    /// The `error.code` of a reply.
    fn errorCode(h: *Harness, r: Reply) ![]const u8 {
        const Shape = struct { @"error": struct { code: []const u8 = "", message: []const u8 = "" } = .{} };
        const parsed = try std.json.parseFromSliceLeaky(Shape, h.fx.a(), r.body, .{ .ignore_unknown_fields = true });
        return parsed.@"error".code;
    }

    fn expectError(h: *Harness, r: Reply, status: std.http.Status, code: []const u8) !void {
        try testing.expectEqual(status, r.status);
        try testing.expectEqualStrings(code, try h.errorCode(r));
    }
};

fn idList(a: Allocator, body: []const u8) ![]const []const u8 {
    const Shape = struct { items: []const struct { id: []const u8 } = &.{}, next: ?[]const u8 = null };
    const parsed = try std.json.parseFromSliceLeaky(Shape, a, body, .{ .ignore_unknown_fields = true });
    const out = try a.alloc([]const u8, parsed.items.len);
    for (parsed.items, out) |it, *o| o.* = it.id;
    return out;
}

fn expectList(h: *Harness, target: []const u8, want: []const []const u8) !void {
    const r = try h.get(target);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const got = try idList(h.fx.a(), r.body);
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g);
}

test "every route needs the app key, and a wrong key opens nothing" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const routes = [_]struct { std.http.Method, []const u8, ?[]const u8 }{
        .{ .GET, "/api/v1/downloads", null },
        .{ .GET, "/api/v1/downloads/d1", null },
        .{ .POST, "/api/v1/downloads", "{}" },
        .{ .POST, "/api/v1/downloads/d1/pause", "" },
        .{ .POST, "/api/v1/downloads/d1/resume", "" },
        .{ .PATCH, "/api/v1/downloads/d1", "{\"priority\":\"high\"}" },
        .{ .DELETE, "/api/v1/downloads/d1", null },
    };
    for (routes) |r| {
        try testing.expectEqual(std.http.Status.unauthorized, (try h.send(r[0], r[1], r[2], null)).status);
        try testing.expectEqual(std.http.Status.unauthorized, (try h.send(r[0], r[1], r[2], "bitnews_wrong")).status);
    }
    const stored = try h.fx.rig.repo.get(h.fx.a(), "d1");
    try testing.expect(!stored.paused);
    try testing.expectEqual(dl.Priority.normal, stored.priority);
}

test "list returns the Download JSON, null next on the last page, and [] when empty" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    const empty = try h.get("/api/v1/downloads");
    try testing.expectEqual(std.http.Status.ok, empty.status);
    try testing.expectEqualStrings("{\"items\":[],\"next\":null}\n", empty.body);

    _ = try h.fx.add("d1", 0);
    const r = try h.get("/api/v1/downloads");
    // Expected string: Go's api.toDownloadJSON for the same Download.
    try testing.expectEqualStrings(
        "{\"items\":[{\"id\":\"d1\",\"name\":\"d1\",\"source_kind\":\"nzb\",\"category_id\":null,\"state\":\"queued\"," ++
            "\"priority\":\"normal\",\"queue_position\":0,\"paused\":false,\"total_bytes\":0,\"done_bytes\":0," ++
            "\"added_at\":\"2026-03-01T12:00:00Z\",\"fetched_at\":null,\"ready_at\":null," ++
            "\"updated_at\":\"2026-03-01T12:00:00Z\"}],\"next\":null}\n",
        r.body,
    );
}

test "list filters and pages with a cursor" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    try h.fx.rig.addCategory("cat1", "tv");
    _ = try h.fx.add("a", 0);
    var b = try h.fx.add("b", 1000);
    b.category_id = "cat1";
    try h.fx.rig.repo.update(b);
    _ = try h.fx.add("c", 2000);
    try h.fx.moveTo("c", &.{.fetching});

    try expectList(&h, "/api/v1/downloads?category_id=cat1", &.{"b"});
    try expectList(&h, "/api/v1/downloads?state=fetching", &.{"c"});
    try expectList(&h, "/api/v1/downloads?kind=torrent", &.{});
    try expectList(&h, "/api/v1/downloads?limit=2", &.{ "a", "b" });

    const first = try h.get("/api/v1/downloads?limit=2");
    const Next = struct { next: ?[]const u8 = null };
    const next = (try std.json.parseFromSliceLeaky(Next, h.fx.a(), first.body, .{ .ignore_unknown_fields = true })).next.?;
    const target = try std.fmt.allocPrint(h.fx.a(), "/api/v1/downloads?limit=2&after={s}", .{next});
    try expectList(&h, target, &.{"c"});
    const second = try h.get(target);
    try testing.expect(std.mem.endsWith(u8, second.body, "\"next\":null}\n"));
}

test "list refuses bad query parameters" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    const cases = [_]struct { []const u8, []const u8 }{
        .{ "state=bogus", "invalid_state" },
        .{ "kind=bogus", "invalid_kind" },
        .{ "limit=abc", "invalid_limit" },
        .{ "limit=-3", "invalid_limit" },
        .{ "limit=0", "invalid_limit" },
        .{ "limit=1_0", "invalid_limit" },
        .{ "after=!!!", "invalid_cursor" },
        .{ "unknown=what", "unknown_parameter" },
        .{ "stat=ready", "unknown_parameter" },
    };
    for (cases) |c| {
        const target = try std.fmt.allocPrint(h.fx.a(), "/api/v1/downloads?{s}", .{c[0]});
        try h.expectError(try h.get(target), .bad_request, c[1]);
    }
    const r = try h.get("/api/v1/downloads?x%22y=1");
    try testing.expect(std.mem.indexOf(u8, r.body, "unknown query parameter \\\"x\\\\\\\"y\\\"") != null);
    // Go ignores pairs with a bad escape or a ';', and reads '+' as a space.
    try expectList(&h, "/api/v1/downloads?bad=%zz&semi=1;2&limit=5", &.{});
    try h.expectError(try h.get("/api/v1/downloads?state=a+b"), .bad_request, "invalid_state");
}

test "get returns the Download or 404" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const found = try h.get("/api/v1/downloads/d1");
    try testing.expectEqual(std.http.Status.ok, found.status);
    try testing.expect(std.mem.startsWith(u8, found.body, "{\"id\":\"d1\""));
    try h.expectError(try h.get("/api/v1/downloads/ghost"), .not_found, "not_found");
}

test "pause and resume toggle the flag and publish events" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const paused = try h.send(.POST, "/api/v1/downloads/d1/pause", "", test_key);
    try testing.expectEqual(std.http.Status.ok, paused.status);
    try testing.expect(std.mem.indexOf(u8, paused.body, "\"paused\":true") != null);
    const resumed = try h.send(.POST, "/api/v1/downloads/d1/resume", "", test_key);
    try testing.expect(std.mem.indexOf(u8, resumed.body, "\"paused\":false") != null);
    try testing.expectEqual(@as(usize, 2), h.fx.recorder.events.items.len);
    try h.expectError(try h.send(.POST, "/api/v1/downloads/ghost/pause", "", test_key), .not_found, "not_found");
}

test "pausing a fetched Download is a 409 not_pausable" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    try h.fx.moveTo("d1", &.{ .fetching, .fetched });
    try h.expectError(try h.send(.POST, "/api/v1/downloads/d1/pause", "", test_key), .conflict, "not_pausable");
}

test "patch changes the priority and returns the Download" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const r = try h.send(.PATCH, "/api/v1/downloads/d1", "{\"priority\":\"force\"}", test_key);
    try testing.expectEqual(std.http.Status.ok, r.status);
    try testing.expect(std.mem.indexOf(u8, r.body, "\"priority\":\"force\"") != null);
    try testing.expectEqual(@as(usize, 1), h.fx.recorder.events.items.len);
}

test "patch sets and clears the Category" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    try h.fx.rig.addCategory("cat1", "tv");
    h.fx.cats.known = &.{"cat1"};
    _ = try h.fx.add("d1", 0);
    const set = try h.send(.PATCH, "/api/v1/downloads/d1", "{\"category_id\":\"cat1\"}", test_key);
    try testing.expectEqual(std.http.Status.ok, set.status);
    try testing.expect(std.mem.indexOf(u8, set.body, "\"category_id\":\"cat1\"") != null);
    const cleared = try h.send(.PATCH, "/api/v1/downloads/d1", "{\"category_id\":null}", test_key);
    try testing.expectEqual(std.http.Status.ok, cleared.status);
    try testing.expect(std.mem.indexOf(u8, cleared.body, "\"category_id\":null") != null);
}

test "patch moves the Download within its Queue" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("a", 0);
    _ = try h.fx.add("b", 1000);
    _ = try h.fx.add("c", 2000);
    const r = try h.send(.PATCH, "/api/v1/downloads/c", "{\"queue_position\":{\"move\":\"top\"}}", test_key);
    try testing.expectEqual(std.http.Status.ok, r.status);
    try testing.expect(std.mem.indexOf(u8, r.body, "\"queue_position\":0") != null);
    try testing.expectEqual(@as(i64, 2), (try h.fx.rig.repo.get(h.fx.a(), "b")).queue_position);
}

test "patch refuses invalid changes and changes nothing" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    const d = try h.fx.add("d1", 0);
    const cases = [_]struct { []const u8, []const u8 }{
        .{ "{\"state\":\"ready\"}", "invalid_json" },
        .{ "{\"name\":\"x\"}", "invalid_json" },
        .{ "{\"priority\":\"urgent\"}", "invalid_priority" },
        .{ "{\"category_id\":\"nope\"}", "invalid_category" },
        .{ "{\"queue_position\":{\"move\":\"sideways\"}}", "invalid_move" },
        .{ "{\"queue_position\":{}}", "invalid_move" },
        .{ "{\"queue_position\":3}", "invalid_json" },
        .{ "{}", "empty_patch" },
        .{ "{\"category_id\":5}", "invalid_json" },
        .{ "", "invalid_json" },
        .{ "{\"priority\":", "invalid_json" },
    };
    for (cases) |c| try h.expectError(try h.send(.PATCH, "/api/v1/downloads/d1", c[0], test_key), .bad_request, c[1]);
    try testing.expectEqualDeep(d, try h.fx.rig.repo.get(h.fx.a(), "d1"));
    try testing.expectEqual(@as(usize, 0), h.fx.recorder.events.items.len);
    try h.expectError(try h.send(.PATCH, "/api/v1/downloads/ghost", "{\"priority\":\"high\"}", test_key), .not_found, "not_found");
}

test "patch needs a JSON content type" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const url = try std.fmt.allocPrint(h.fx.a(), "http://127.0.0.1:{d}/api/v1/downloads/d1", .{h.port});
    var out: Io.Writer.Allocating = .init(h.fx.a());
    const res = try h.client.fetch(.{
        .location = .{ .url = url },
        .method = .PATCH,
        .payload = "{\"priority\":\"high\"}",
        .headers = .{ .authorization = .{ .override = "Bearer " ++ test_key }, .content_type = .{ .override = "text/plain" } },
        .response_writer = &out.writer,
    });
    try testing.expectEqual(std.http.Status.unsupported_media_type, res.status);
    try testing.expectEqualStrings("unsupported_media_type", try h.errorCode(.{ .status = res.status, .body = out.written() }));
}

test "delete answers 204, removes the row, publishes removed and keeps the data" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const r = try h.send(.DELETE, "/api/v1/downloads/d1", null, test_key);
    try testing.expectEqual(std.http.Status.no_content, r.status);
    try testing.expectEqualStrings("", r.body);
    try testing.expectError(error.NotFound, h.fx.rig.repo.get(h.fx.a(), "d1"));
    try testing.expectEqual(event_removed, h.fx.recorder.events.items[0].kind);
    try testing.expectEqual(@as(usize, 0), h.fx.remover.removed);
}

const event_removed = @import("event.zig").Kind.removed;

test "delete with delete_data=true asks the data remover" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    const r = try h.send(.DELETE, "/api/v1/downloads/d1?delete_data=true", null, test_key);
    try testing.expectEqual(std.http.Status.no_content, r.status);
    try testing.expectEqual(@as(usize, 1), h.fx.remover.removed);
}

test "delete of a Download being post-processed is a 409 and touches nothing" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    try h.fx.moveTo("d1", &.{ .fetching, .fetched, .post_processing });
    try h.expectError(try h.send(.DELETE, "/api/v1/downloads/d1?delete_data=true", null, test_key), .conflict, "download_busy");
    try testing.expectEqual(@as(usize, 0), h.fx.remover.removed);
    _ = try h.fx.rig.repo.get(h.fx.a(), "d1");
}

test "delete refuses a bad delete_data and an unknown Download" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    _ = try h.fx.add("d1", 0);
    try h.expectError(try h.send(.DELETE, "/api/v1/downloads/d1?delete_data=maybe", null, test_key), .bad_request, "invalid_delete_data");
    try h.expectError(try h.send(.DELETE, "/api/v1/downloads/d1?other=1", null, test_key), .bad_request, "unknown_parameter");
    try h.expectError(try h.send(.DELETE, "/api/v1/downloads/ghost", null, test_key), .not_found, "not_found");
    _ = try h.fx.rig.repo.get(h.fx.a(), "d1");
    for ([_][]const u8{ "1", "t", "T", "TRUE", "true", "True", "0", "f", "F", "FALSE", "false", "False" }) |v| {
        try testing.expect(parseBool(v) != null);
    }
    try testing.expect(parseBool("yes") == null);
    try testing.expect(parseBool("tRUE") == null);
}

test "adding answers 503 add_unavailable until the add milestones land" {
    var h: Harness = undefined;
    try h.start();
    defer h.deinit();
    try h.expectError(try h.send(.POST, "/api/v1/downloads", "{}", test_key), .service_unavailable, "add_unavailable");
}

fn fuzzQuery(_: void, smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const pairs = try parseQuery(arena.allocator(), buf[0..n]);
    // A pair never holds the characters Go drops it for.
    for (pairs) |p| try testing.expect(std.mem.indexOfScalar(u8, p.name, ';') == null);
}

fn fuzzSeed(comptime s: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, s.len)) ++ s.*);
}

test "fuzz: any query string parses without crashing or leaking" {
    try testing.fuzz({}, fuzzQuery, .{ .corpus = &.{
        fuzzSeed("state=queued&limit=5&after=abc"),
        fuzzSeed("a=%zz&b=%41+%2b&;&&="),
        fuzzSeed("%"),
        fuzzSeed(""),
    } });
}
