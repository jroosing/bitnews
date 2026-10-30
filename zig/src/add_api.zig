//! POST /api/v1/downloads (internal/api/handlers_add.go): an NZB or a torrent as
//! a multipart upload (part "nzb" or "torrent", plus the optional fields name,
//! category_id, priority, password and paused), or a JSON body with a "url" (of
//! an NZB or a torrent) or a "magnet" link. Other content types are refused
//! with 415.
//!
//! The upload is read as a stream through add_multipart.zig, which is bounded
//! at every step: at most 16 parts, 4096 bytes in a text field, the NZB size
//! cap in a file part, the route's body limit in all. The NZB part is held in
//! memory once (one buffer, freed as soon as it is parsed and written to disk),
//! like the Go version's bufio window plus the parser's tokens.
//!
//! Register it with `downloads_api.registerWithAdd`, which owns the route.
const std = @import("std");
const Allocator = std.mem.Allocator;
const ex = @import("http_exchange.zig");
const Exchange = ex.Exchange;
const api_json = @import("api_json.zig");
const app_error = @import("app_error.zig");
const download_json = @import("download_json.zig");
const downloads_service = @import("downloads_service.zig");
const mime = @import("add_mime.zig");
const multipart = @import("add_multipart.zig");
const service = @import("add_service.zig");
const secret_mod = @import("secret.zig");

/// Each text field of an upload is at most this long.
pub const max_form_field_bytes = 4096;
/// An upload has at most this many parts, so a flood of tiny parts cannot keep
/// the handler busy.
pub const max_form_parts = 16;
/// A JSON body is small (smallBodyLimit).
pub const max_json_bytes = 16 << 10;

/// The body of the JSON form. Every field has a default (strict_json.zig).
const JsonBody = struct {
    url: []const u8 = "",
    magnet: []const u8 = "",
    name: []const u8 = "",
    category_id: []const u8 = "",
    priority: []const u8 = "",
    password: []const u8 = "",
    paused: bool = false,
};

/// The route handler; its state is the `*service.AddService`.
pub fn addDownload(state: ?*anyopaque, e: *Exchange) anyerror!void {
    const svc: *service.AddService = @ptrCast(@alignCast(state.?));
    const media = mime.parse(e.arena, e.header("Content-Type") orelse "") catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.InvalidMediaType => return unsupported(e),
    };
    if (std.mem.eql(u8, media.type, "multipart/form-data")) return addFromUpload(svc, e, media);
    if (std.mem.eql(u8, media.type, "application/json")) return addFromJson(svc, e);
    return unsupported(e);
}

fn unsupported(e: *Exchange) !void {
    return e.res.fail(e.arena, .unsupported_media_type, "unsupported_media_type", "send multipart/form-data with an nzb or torrent part, or application/json with a url or magnet");
}

fn badRequest(e: *Exchange, code: []const u8, message: []const u8) !void {
    return e.res.fail(e.arena, .bad_request, code, message);
}

fn isMagnet(v: []const u8) bool {
    const t = mime.trimSpace(v);
    return t.len >= 7 and std.ascii.eqlIgnoreCase(t[0..7], "magnet:");
}

fn addFromJson(svc: *service.AddService, e: *Exchange) !void {
    const buf = try e.arena.alloc(u8, 1024);
    const reader = try e.bodyReader(buf);
    const raw = reader.allocRemaining(e.arena, .limited(max_json_bytes)) catch |err| switch (err) {
        error.StreamTooLong => return error.PayloadTooLarge,
        error.ReadFailed => return error.ReadFailed,
        error.OutOfMemory => return error.OutOfMemory,
    };
    const body = switch (try api_json.parseBody(JsonBody, e.arena, raw)) {
        .ok => |v| v,
        .invalid => |msg| return badRequest(e, "invalid_json", msg),
    };
    if (body.url.len == 0 and body.magnet.len == 0) return badRequest(e, "missing_url", "give the url of an NZB or a torrent, or a magnet link");
    if (body.url.len > 0 and body.magnet.len > 0) return badRequest(e, "ambiguous_source", "give either a url or a magnet, not both");
    // Each field takes its own kind of link, as documented.
    if (body.url.len > 0 and isMagnet(body.url)) return badRequest(e, "invalid_url", "url must be an absolute http or https URL; send a magnet link as magnet");
    if (body.magnet.len > 0 and !isMagnet(body.magnet)) return badRequest(e, "invalid_magnet", "not a usable magnet link");

    var password = try secret_mod.Secret.init(e.arena, body.password);
    defer password.deinit();
    return finish(svc, e, .{
        .url = if (body.url.len > 0) body.url else body.magnet,
        .name = body.name,
        .category_id = body.category_id,
        .priority = body.priority,
        .password = password,
        .paused = body.paused,
    });
}

/// Go's strconv.ParseBool.
fn parseBool(s: []const u8) ?bool {
    for ([_][]const u8{ "1", "t", "T", "TRUE", "true", "True" }) |y| if (std.mem.eql(u8, s, y)) return true;
    for ([_][]const u8{ "0", "f", "F", "FALSE", "false", "False" }) |n| if (std.mem.eql(u8, s, n)) return false;
    return null;
}

fn invalidMultipart(e: *Exchange) !void {
    return badRequest(e, "invalid_multipart", "the multipart body is not valid");
}

/// The answer for an error of the multipart reader: too large when the body
/// cap was hit, else a malformed form.
fn readFailed(e: *Exchange, err: multipart.Error) !void {
    return switch (err) {
        error.PayloadTooLarge => error.PayloadTooLarge,
        error.OutOfMemory => error.OutOfMemory,
        error.Malformed => invalidMultipart(e),
    };
}

fn addFromUpload(svc: *service.AddService, e: *Exchange, media: mime.Media) !void {
    const boundary = media.get("boundary") orelse return invalidMultipart(e);
    const buf = try e.arena.alloc(u8, 16 << 10);
    const body = try e.bodyReader(buf);
    const reader = multipart.Reader.init(e.arena, body, boundary, e.body_limit) catch |err| switch (err) {
        error.Malformed => return invalidMultipart(e),
        error.PayloadTooLarge => return error.PayloadTooLarge,
        error.OutOfMemory => return error.OutOfMemory,
    };

    var req: service.Request = .{};
    var failure: app_error.Failure = .{};
    var upload: ?*service.Upload = null;
    defer if (upload) |u| u.discard(e.io); // after a successful add this does nothing
    var seen: std.ArrayList([]const u8) = .empty;
    var category_id: []const u8 = "";
    var priority: []const u8 = "";
    var password: []const u8 = "";
    var paused: []const u8 = "";

    var n: usize = 0;
    while (true) : (n += 1) {
        const maybe = reader.next(e.arena) catch |err| return readFailed(e, err);
        const part = maybe orelse break;
        if (n >= max_form_parts) return badRequest(e, "too_many_parts", "the form has too many parts");
        for (seen.items) |s| if (std.mem.eql(u8, s, part.name)) {
            const quoted = try downloads_service.quote(e.arena, part.name);
            return badRequest(e, "duplicate_field", try std.fmt.allocPrint(e.arena, "the form field {s} is given twice", .{quoted}));
        };
        try seen.append(e.arena, part.name);

        const is_nzb = std.mem.eql(u8, part.name, "nzb");
        if (is_nzb or std.mem.eql(u8, part.name, "torrent")) {
            if (upload != null) return badRequest(e, "multiple_sources", "give one nzb or torrent file, not several");
            req.file_name = part.file_name;
            const cap: usize = if (is_nzb) @intCast(svc.deps.max_nzb_bytes) else @intCast(service.max_torrent_bytes);
            const bytes = reader.readUpTo(svc.gpa, cap) catch |err| return readFailed(e, err);
            defer svc.gpa.free(bytes);
            upload = svc.receive(e.arena, &failure, bytes, part.file_name, if (is_nzb) .nzb else .torrent) catch |err| return api_json.fail(e, err, &failure);
            continue;
        }
        const dst: *[]const u8 = if (std.mem.eql(u8, part.name, "name"))
            &req.name
        else if (std.mem.eql(u8, part.name, "category_id"))
            &category_id
        else if (std.mem.eql(u8, part.name, "priority"))
            &priority
        else if (std.mem.eql(u8, part.name, "password"))
            &password
        else if (std.mem.eql(u8, part.name, "paused"))
            &paused
        else {
            const quoted = try downloads_service.quote(e.arena, part.name);
            return badRequest(e, "unknown_field", try std.fmt.allocPrint(e.arena, "unknown form field {s}", .{quoted}));
        };
        const raw = reader.readUpTo(e.arena, max_form_field_bytes) catch |err| return readFailed(e, err);
        if (raw.len > max_form_field_bytes) {
            const quoted = try downloads_service.quote(e.arena, part.name);
            return badRequest(e, "field_too_long", try std.fmt.allocPrint(e.arena, "the form field {s} is too long", .{quoted}));
        }
        dst.* = raw;
    }
    if (upload == null) return badRequest(e, "missing_nzb", "the form needs an \"nzb\" or \"torrent\" file part");
    req.category_id = category_id;
    req.priority = priority;
    var secret = try secret_mod.Secret.init(e.arena, password);
    defer secret.deinit();
    req.password = secret;
    if (paused.len > 0) {
        req.paused = parseBool(paused) orelse return badRequest(e, "invalid_paused", "paused must be true or false");
    }
    req.upload = upload;
    return finish(svc, e, req);
}

fn finish(svc: *service.AddService, e: *Exchange, req: service.Request) !void {
    var failure: app_error.Failure = .{};
    const d = svc.add(e.arena, &failure, req) catch |err| return api_json.fail(e, err, &failure);
    try e.res.json(e.arena, .created, download_json.Json{ .d = d });
}
