//! Tests of POST /api/v1/downloads through a real HTTP server on loopback
//! (ports of internal/api/add_test.go and add_url_test.go, and the upload
//! scenarios of internal/blackbox/usenet_add_test.go).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const testing = std.testing;
const ex = @import("http_exchange.zig");
const add_api = @import("add_api.zig");
const downloads_api = @import("downloads_api.zig");
const downloads_service = @import("downloads_service.zig");
const service = @import("add_service.zig");
const ts = @import("add_testserver.zig");
const tu = @import("add_testutil.zig");
const Rig = tu.Rig;

const route_limit: u64 = 64 << 10;

var current_service: *service.AddService = undefined;
var current_downloads: *downloads_service.Service = undefined;

/// The real route table of the Downloads, adding included.
fn setupFull(routes: *ex.Routes) anyerror!void {
    try downloads_api.registerWithAdd(routes, current_downloads, current_service);
}

fn setup(routes: *ex.Routes) anyerror!void {
    try routes.add("POST /api/v1/downloads", .{ .handler = add_api.addDownload, .state = current_service, .body_limit = route_limit });
}

const Reply = struct {
    status: std.http.Status,
    body: []const u8,
};

const Env = struct {
    rig: Rig,
    server: ts.Server,
    client: std.http.Client,

    fn open(env: *Env) !void {
        return env.openWith(setup);
    }

    fn openWith(env: *Env, routes: ts.Setup) !void {
        try env.rig.open();
        errdefer env.rig.close();
        current_service = &env.rig.add;
        current_downloads = &env.rig.downloads;
        try env.server.start(routes);
        env.client = .{ .allocator = testing.allocator, .io = testing.io };
    }

    fn close(env: *Env) void {
        env.client.deinit();
        env.server.stop();
        env.rig.close();
    }

    fn a(env: *Env) Allocator {
        return env.rig.a();
    }

    fn post(env: *Env, content_type: ?[]const u8, body: []const u8, key: ?[]const u8) !Reply {
        const url = try env.server.url(env.a(), "/api/v1/downloads");
        var out: Io.Writer.Allocating = .init(env.a());
        const bearer = if (key) |k| try std.fmt.allocPrint(env.a(), "Bearer {s}", .{k}) else null;
        const res = try env.client.fetch(.{
            .location = .{ .url = url },
            .method = .POST,
            .payload = body,
            .headers = .{
                .authorization = if (bearer) |b| .{ .override = b } else .default,
                .content_type = if (content_type) |c| .{ .override = c } else .default,
            },
            .response_writer = &out.writer,
        });
        return .{ .status = res.status, .body = out.written() };
    }

    fn postForm(env: *Env, body: []const u8) !Reply {
        return env.post(form_type, body, ts.test_key);
    }

    fn postJson(env: *Env, body: []const u8) !Reply {
        return env.post("application/json", body, ts.test_key);
    }

    /// The `error.code` and `error.message` of a reply.
    fn errorOf(env: *Env, r: Reply) !struct { code: []const u8, message: []const u8 } {
        const Shape = struct { @"error": struct { code: []const u8 = "", message: []const u8 = "" } = .{} };
        const parsed = try std.json.parseFromSliceLeaky(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
        return .{ .code = parsed.@"error".code, .message = parsed.@"error".message };
    }

    fn expectError(env: *Env, r: Reply, status: std.http.Status, code: []const u8) !void {
        const got = try env.errorOf(r);
        testing.expectEqual(status, r.status) catch |err| {
            std.debug.print("{s}\n", .{r.body});
            return err;
        };
        try testing.expectEqualStrings(code, got.code);
    }

    fn expectNothingAdded(env: *Env) !void {
        try env.rig.expectNothingAdded();
    }
};

const boundary = "----bitnewsTestBoundary";
const form_type = "multipart/form-data; boundary=" ++ boundary;

const Part = struct { name: []const u8, file_name: []const u8 = "", value: []const u8 };

fn formBody(a: Allocator, parts: []const Part) ![]u8 {
    var out: std.ArrayList(u8) = .empty;
    for (parts) |p| {
        try out.print(a, "--{s}\r\nContent-Disposition: form-data; name=\"{s}\"", .{ boundary, p.name });
        if (p.file_name.len > 0) try out.print(a, "; filename=\"{s}\"", .{p.file_name});
        try out.print(a, "\r\n\r\n{s}\r\n", .{p.value});
    }
    try out.print(a, "--{s}--\r\n", .{boundary});
    return out.items;
}

fn upload(env: *Env, file_name: []const u8, nzb: []const u8, extra: []const Part) !Reply {
    var parts: std.ArrayList(Part) = .empty;
    try parts.append(env.a(), .{ .name = "nzb", .file_name = file_name, .value = nzb });
    try parts.appendSlice(env.a(), extra);
    return env.postForm(try formBody(env.a(), parts.items));
}

const Created = struct {
    id: []const u8,
    name: []const u8,
    source_kind: []const u8,
    category_id: ?[]const u8,
    state: []const u8,
    priority: []const u8,
    queue_position: i64,
    paused: bool,
    total_bytes: i64,
    done_bytes: i64,
    added_at: []const u8,
    fetched_at: ?[]const u8,
    ready_at: ?[]const u8,
    updated_at: []const u8,
};

fn created(env: *Env, r: Reply) !Created {
    testing.expectEqual(std.http.Status.created, r.status) catch |err| {
        std.debug.print("{s}\n", .{r.body});
        return err;
    };
    return std.json.parseFromSliceLeaky(Created, env.a(), r.body, .{});
}

test "an upload answers 201 with the new Download and stores the NZB" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const nzb = try tu.nzbWith(env.a(), "", "", "");

    const r = try upload(&env, "My.Release.nzb", nzb, &.{});
    const d = try created(&env, r);

    try testing.expectEqual(@as(usize, 26), d.id.len);
    try testing.expectEqualStrings("My.Release", d.name);
    try testing.expectEqualStrings("nzb", d.source_kind);
    try testing.expect(d.category_id == null);
    try testing.expectEqualStrings("queued", d.state);
    try testing.expectEqualStrings("normal", d.priority);
    try testing.expectEqual(@as(i64, 0), d.queue_position);
    try testing.expect(!d.paused);
    try testing.expectEqual(@as(i64, 5000), d.total_bytes);
    try testing.expectEqual(@as(i64, 0), d.done_bytes);
    try testing.expect(d.fetched_at == null and d.ready_at == null);
    // The key order of the Go DTO; no failure or output fields on a new Download.
    try testing.expect(std.mem.startsWith(u8, r.body, "{\"id\":\""));
    try testing.expect(std.mem.indexOf(u8, r.body, "\"failure_reason\"") == null);
    try testing.expect(std.mem.indexOf(u8, r.body, "\"output_path\"") == null);
    const files = try env.rig.filesOf(d.id);
    try testing.expectEqual(@as(usize, 1), files.len);
    try testing.expectEqualStrings("My.Release.nzb", files[0]);
    try testing.expectEqual(@as(usize, 1), env.rig.recorder.added.items.len);
    try testing.expectEqualStrings(d.id, env.rig.recorder.added.items[0].id);
}

test "form fields work before and after the file, and set what they say" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    try env.rig.addCategory("tvid", "tv");
    const nzb = tu.tiny_nzb;

    const body = try formBody(env.a(), &.{
        .{ .name = "priority", .value = "high" },
        .{ .name = "nzb", .file_name = "x.nzb", .value = nzb },
        .{ .name = "paused", .value = "true" },
        .{ .name = "category_id", .value = "tvid" },
        .{ .name = "name", .value = "Chosen {{pw}}" },
    });
    const d = try created(&env, try env.postForm(body));

    try testing.expectEqualStrings("Chosen", d.name);
    try testing.expectEqualStrings("high", d.priority);
    try testing.expect(d.paused);
    try testing.expectEqualStrings("tvid", d.category_id.?);
    const src = try env.rig.sources.get(env.a(), d.id);
    try testing.expectEqualStrings("pw", src.password);
}

test "paused takes Go's booleans" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    for ([_]struct { []const u8, bool }{ .{ "true", true }, .{ "1", true }, .{ "t", true }, .{ "false", false }, .{ "0", false }, .{ "", false } }) |c| {
        const d = try created(&env, try upload(&env, "x.nzb", tu.tiny_nzb, &.{.{ .name = "paused", .value = c[0] }}));
        try testing.expectEqual(c[1], d.paused);
    }
}

test "bad fields are 400 and leave nothing behind" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const long: [5000]u8 = @splat('x');
    const pw_long: [1025]u8 = @splat('p');
    const cases = [_]struct { []const Part, []const u8 }{
        .{ &.{.{ .name = "category_id", .value = "nope" }}, "unknown_category" },
        .{ &.{.{ .name = "priority", .value = "urgent" }}, "invalid_priority" },
        .{ &.{.{ .name = "priority", .value = "HIGH" }}, "invalid_priority" },
        .{ &.{.{ .name = "paused", .value = "maybe" }}, "invalid_paused" },
        .{ &.{.{ .name = "password", .value = &pw_long }}, "invalid_password" },
        .{ &.{.{ .name = "url", .value = "http://x" }}, "unknown_field" },
        .{ &.{ .{ .name = "name", .value = "a" }, .{ .name = "name", .value = "b" } }, "duplicate_field" },
        .{ &.{.{ .name = "name", .value = &long }}, "field_too_long" },
        .{ &.{.{ .name = "torrent", .file_name = "t.torrent", .value = "d4:infoe" }}, "multiple_sources" },
    };
    for (cases) |c| {
        const r = try upload(&env, "x.nzb", tu.tiny_nzb, c[0]);
        try env.expectError(r, .bad_request, c[1]);
        try env.expectNothingAdded();
    }
    const r = try upload(&env, "x.nzb", tu.tiny_nzb, &.{.{ .name = "bogus", .value = "x" }});
    try testing.expectEqualStrings("unknown form field \"bogus\"", (try env.errorOf(r)).message);
}

test "two nzb parts are a duplicate field" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = try formBody(env.a(), &.{
        .{ .name = "nzb", .file_name = "a.nzb", .value = tu.tiny_nzb },
        .{ .name = "nzb", .file_name = "a.nzb", .value = tu.tiny_nzb },
    });
    try env.expectError(try env.postForm(body), .bad_request, "duplicate_field");
    try env.expectNothingAdded();
}

test "what is not a valid NZB is 400 invalid_nzb and leaves nothing" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    for ([_][]const u8{ "this is not an nzb", "", "<nzb></nzb>", "<html><body>login</body></html>" }) |doc| {
        try env.expectError(try upload(&env, "x.nzb", doc, &.{}), .bad_request, "invalid_nzb");
        try env.expectNothingAdded();
    }
}

test "a form without a file part is 400 missing_nzb" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = try formBody(env.a(), &.{.{ .name = "name", .value = "x" }});
    const r = try env.postForm(body);
    try env.expectError(r, .bad_request, "missing_nzb");
    try testing.expectEqualStrings("the form needs an \"nzb\" or \"torrent\" file part", (try env.errorOf(r)).message);
    // An empty body ends the form the same way.
    try env.expectError(try env.postForm(""), .bad_request, "missing_nzb");
}

test "a torrent part is 503 while the engine is not wired in" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = try formBody(env.a(), &.{.{ .name = "torrent", .file_name = "t.torrent", .value = "d4:infoe" }});
    const r = try env.postForm(body);
    try env.expectError(r, .service_unavailable, "add_unavailable");
    try env.expectNothingAdded();
}

test "an NZB over the limit is 413 nzb_too_large, a body over the route's cap is 413 too" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    env.rig.rebuild(.{ .max_nzb_bytes = 1024 });
    const big = try env.a().alloc(u8, 2048 + tu.tiny_nzb.len);
    @memset(big, ' ');
    @memcpy(big[0..tu.tiny_nzb.len], tu.tiny_nzb);
    const r = try upload(&env, "x.nzb", big, &.{});
    try env.expectError(r, .payload_too_large, "nzb_too_large");
    try testing.expectEqualStrings("the NZB is larger than 0 MiB", (try env.errorOf(r)).message);
    try env.expectNothingAdded();

    // The route's body limit is 64 KiB: more is cut off.
    env.rig.rebuild(.{});
    const huge = try env.a().alloc(u8, 80 << 10);
    @memset(huge, ' ');
    @memcpy(huge[0..tu.tiny_nzb.len], tu.tiny_nzb);
    const r2 = try upload(&env, "x.nzb", huge, &.{});
    try env.expectError(r2, .payload_too_large, "request_too_large");
    try env.expectNothingAdded();
}

test "other content types are 415" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    for ([_]?[]const u8{ "text/plain", "application/x-nzb", "application/x-www-form-urlencoded", "", null, "multipart/mixed; boundary=x", "application/json; a=1; a=2" }) |ct| {
        const r = try env.post(ct, "<nzb/>", ts.test_key);
        try env.expectError(r, .unsupported_media_type, "unsupported_media_type");
    }
    try env.expectNothingAdded();
}

test "a multipart type without a boundary is invalid_multipart" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    try env.expectError(try env.post("multipart/form-data", "x", ts.test_key), .bad_request, "invalid_multipart");
    // A boundary that is empty or far too long.
    try env.expectError(try env.post("multipart/form-data; boundary=\"\"", "x", ts.test_key), .bad_request, "invalid_multipart");
    const long = try std.fmt.allocPrint(env.a(), "multipart/form-data; boundary={s}", .{&@as([300]u8, @splat('b'))});
    try env.expectError(try env.post(long, "x", ts.test_key), .bad_request, "invalid_multipart");
}

test "a body cut inside the file is invalid_multipart" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = try formBody(env.a(), &.{.{ .name = "nzb", .file_name = "a.nzb", .value = tu.tiny_nzb }});
    const cut = body[0 .. body.len - 40];
    try env.expectError(try env.postForm(cut), .bad_request, "invalid_multipart");
    try env.expectNothingAdded();
}

test "bare LF line ends and a boundary in quotes are accepted" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = "--B\nContent-Disposition: form-data; name=\"nzb\"; filename=\"lf.nzb\"\n\n" ++ tu.tiny_nzb ++ "\n--B--\n";
    const r = try env.post("multipart/form-data; boundary=\"B\"", body, ts.test_key);
    const d = try created(&env, r);
    try testing.expectEqualStrings("lf", d.name);
}

test "the key is needed" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = try formBody(env.a(), &.{.{ .name = "nzb", .file_name = "x.nzb", .value = tu.tiny_nzb }});
    try testing.expectEqual(std.http.Status.unauthorized, (try env.post(form_type, body, null)).status);
    try testing.expectEqual(std.http.Status.unauthorized, (try env.post(form_type, body, "bitnews_wrong")).status);
    try env.expectNothingAdded();
    _ = try created(&env, try env.post(form_type, body, ts.test_key));
}

// ---- JSON ----

test "a JSON url is fetched and added" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    env.rig.fetcher.answer = .{ .body = try tu.nzbWith(env.a(), "Meta Name", "pw-from-nzb", "") };

    const r = try env.postJson("{\"url\":\"http://nzb.example/get?apikey=SECRETKEY\",\"priority\":\"high\",\"paused\":true,\"name\":\"Mine\",\"password\":\"mine\"}");
    const d = try created(&env, r);

    try testing.expectEqualStrings("Mine", d.name);
    try testing.expectEqualStrings("high", d.priority);
    try testing.expect(d.paused);
    try testing.expectEqualStrings("mine", (try env.rig.sources.get(env.a(), d.id)).password);
    try testing.expectEqualStrings("http://nzb.example/get?apikey=SECRETKEY", env.rig.fetcher.last_url);
    try testing.expect(std.mem.indexOf(u8, r.body, "SECRETKEY") == null);
    try testing.expect(std.mem.indexOf(u8, env.rig.out.written(), "SECRETKEY") == null);
    try testing.expect(std.mem.indexOf(u8, env.rig.out.written(), "mine") == null);
}

test "JSON bodies are strict and small" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    try env.expectError(try env.postJson("{\"url\":\"http://x\",\"tls\":false}"), .bad_request, "invalid_json");
    try env.expectError(try env.postJson(""), .bad_request, "invalid_json");
    try testing.expectEqualStrings("the request body is empty", (try env.errorOf(try env.postJson(""))).message);
    try env.expectError(try env.postJson("{\"url\":\"http://x\"} extra"), .bad_request, "invalid_json");
    const huge = try std.fmt.allocPrint(env.a(), "{{\"url\":\"http://x/{s}\"}}", .{&@as([20 << 10]u8, @splat('a'))});
    try env.expectError(try env.postJson(huge), .payload_too_large, "request_too_large");
    try testing.expectEqual(@as(usize, 0), env.rig.fetcher.calls);
}

test "a JSON body needs exactly one link, of the right kind" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567";
    try env.expectError(try env.postJson("{}"), .bad_request, "missing_url");
    try env.expectError(try env.postJson("{\"url\":\"http://x\",\"magnet\":\"" ++ magnet ++ "\"}"), .bad_request, "ambiguous_source");
    const r = try env.postJson("{\"url\":\"" ++ magnet ++ "\"}");
    try env.expectError(r, .bad_request, "invalid_url");
    try testing.expectEqualStrings("url must be an absolute http or https URL; send a magnet link as magnet", (try env.errorOf(r)).message);
    try env.expectError(try env.postJson("{\"magnet\":\"http://x\"}"), .bad_request, "invalid_magnet");
    // A magnet link goes to the torrent engine, which is not here yet.
    try env.expectError(try env.postJson("{\"magnet\":\"" ++ magnet ++ "\"}"), .service_unavailable, "add_unavailable");
    try env.expectError(try env.postJson("{\"url\":\"ftp://host.example/a.nzb\"}"), .bad_request, "invalid_url");
    try testing.expectEqual(@as(usize, 0), env.rig.fetcher.calls);
    try env.expectNothingAdded();
}

test "a JSON url that fails to fetch is 502 without the URL" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    env.rig.fetcher.answer = .{ .status = 403 };
    const r = try env.postJson("{\"url\":\"http://nzb.example/x?apikey=SECRETKEY\"}");
    try env.expectError(r, .bad_gateway, "fetch_failed");
    try testing.expectEqualStrings("could not fetch the NZB or torrent: the server answered 403 Forbidden", (try env.errorOf(r)).message);
    try testing.expect(std.mem.indexOf(u8, r.body, "SECRETKEY") == null);
    try env.expectNothingAdded();
}

test "registered with the Downloads routes, an added Download shows in the list and by ID" {
    var env: Env = undefined;
    try env.openWith(setupFull);
    defer env.close();
    const d = try created(&env, try upload(&env, "Listed.nzb", tu.tiny_nzb, &.{}));

    const list_url = try env.server.url(env.a(), "/api/v1/downloads");
    var out: Io.Writer.Allocating = .init(env.a());
    const res = try env.client.fetch(.{
        .location = .{ .url = list_url },
        .headers = .{ .authorization = .{ .override = "Bearer " ++ ts.test_key } },
        .response_writer = &out.writer,
    });
    try testing.expectEqual(std.http.Status.ok, res.status);
    try testing.expect(std.mem.indexOf(u8, out.written(), d.id) != null);
    try testing.expect(std.mem.indexOf(u8, out.written(), "\"state\":\"queued\"") != null);
}
