//! qBittorrent WebUI API v2, the subset media managers use (M15-08).
//! Login is the fixed username `bitnews` and the app key as the password.
//! A session is a `SID` cookie. Without one, every call except login is 403.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const ex = @import("http_exchange.zig");
const appkey = @import("appkey.zig");
const token = @import("token.zig");
const mime = @import("add_mime.zig");
const multipart = @import("add_multipart.zig");

const username = "bitnews";
const version = "v4.6.7";
const webapi = "2.9.3";
const sid_bytes = 16;

pub const Sessions = struct {
    gpa: Allocator,
    io: Io,
    mu: Io.Mutex = .init,
    live: std.StringHashMapUnmanaged(void) = .empty,

    pub fn init(gpa: Allocator, io: Io) Sessions {
        return .{ .gpa = gpa, .io = io };
    }

    pub fn deinit(self: *Sessions) void {
        var it = self.live.keyIterator();
        while (it.next()) |k| self.gpa.free(k.*);
        self.live.deinit(self.gpa);
    }

    fn create(self: *Sessions) ![]const u8 {
        var raw: [sid_bytes]u8 = undefined;
        self.io.random(&raw);
        const hex = std.fmt.bytesToHex(raw, .lower);
        const owned = try self.gpa.dupe(u8, &hex);
        errdefer self.gpa.free(owned);
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        try self.live.put(self.gpa, owned, {});
        return owned;
    }

    fn valid(self: *Sessions, sid: []const u8) bool {
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        return self.live.contains(sid);
    }

    fn drop(self: *Sessions, sid: []const u8) void {
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        if (self.live.fetchRemove(sid)) |kv| self.gpa.free(kv.key);
    }
};

pub const Handler = struct {
    keys: *appkey.Service,
    sessions: *Sessions,
    ops: Ops = .{},
};

pub const Act = enum { pause, unpause, delete_keep, delete_data };

/// One torrent row before it is turned into qBittorrent's JSON.
pub const Listed = struct {
    hash: []const u8,
    name: []const u8,
    /// Download state tag: `fetching_metadata`, `queued`, `ready`, ...
    state: []const u8,
    /// Category name, empty when the Download has none.
    category: []const u8 = "",
    paused: bool,
    total_bytes: i64,
    done_bytes: i64,
    /// Unix milliseconds.
    added_at: i64,
};

pub const Ops = struct {
    ctx: ?*anyopaque = null,
    add_magnet_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, magnet: []const u8, paused: bool, category: []const u8) anyerror!void = null,
    add_file_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, bytes: []const u8, file_name: []const u8, paused: bool, category: []const u8) anyerror!void = null,
    list_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator) anyerror![]Listed = null,
    /// `hash` is 40 hex. A missing torrent is not an error.
    act_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, hash: []const u8, act: Act) anyerror!void = null,
    prefs_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator) anyerror!Prefs = null,
    cats_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator) anyerror![]Cat = null,
    /// Empty name is refused by the handler. A bad name is `error.BadName`.
    ensure_cat_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, name: []const u8) anyerror!void = null,
    set_category_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, hash: []const u8, category_id: []const u8) anyerror!void = null,
    move_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, hash: []const u8, move: []const u8) anyerror!void = null,
    priority_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, hash: []const u8, priority: []const u8) anyerror!void = null,
    /// `ratio` and `minutes` use qBittorrent's codes: -2 global, -1 none, or the value.
    share_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, hash: []const u8, ratio: f64, minutes: f64) anyerror!void = null,
};

pub const Prefs = struct {
    save_path: []const u8,
    temp_path: []const u8,
    temp_path_enabled: bool,
    queueing_enabled: bool = true,
    create_subfolder_enabled: bool = true,
    max_active_downloads: i64,
    max_active_uploads: i64 = 0,
    max_active_torrents: i64 = 0,
    max_ratio_enabled: bool,
    max_ratio: f64,
    max_ratio_act: i64,
    max_seeding_time_enabled: bool,
    max_seeding_time: i64,
    dht: bool,
    pex: bool,
    lsd: bool,
    listen_port: i64,
    encryption: i64,
    dl_limit: i64 = 0,
    up_limit: i64 = 0,
};

pub const Cat = struct {
    id: []const u8 = "",
    name: []const u8,
    save_path: []const u8,
};

const HashOp = union(enum) {
    move: []const u8,
    category: []const u8,
    priority: []const u8,
    share: struct { ratio: f64, minutes: f64 },
};

pub fn register(routes: *ex.Routes, handler: *Handler) !void {
    const route: ex.Route = .{
        .handler = handle,
        .state = handler,
        .auth = .none,
        .body_limit = 32 << 20,
    };
    try routes.add("GET /api/v2/", route);
    try routes.add("POST /api/v2/", route);
}

fn handle(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const h: *Handler = @ptrCast(@alignCast(state.?));
    const call = callName(e.path);
    if (std.mem.eql(u8, call, "auth/login")) {
        if (e.method != .POST) return forbidden(e);
        return login(h, e);
    }
    const sid = cookieSid(e) orelse return forbidden(e);
    if (!h.sessions.valid(sid)) return forbidden(e);
    if (std.mem.eql(u8, call, "auth/logout") and e.method == .POST) {
        h.sessions.drop(sid);
        try e.res.setHeader(e.arena, "Set-Cookie", "SID=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict");
        return plain(e, .ok, "Ok.");
    }
    if (e.method == .POST) return post(h, e, call);
    if (e.method != .GET) return plain(e, .not_found, "Not Found");
    if (std.mem.eql(u8, call, "app/version")) return plain(e, .ok, version);
    if (std.mem.eql(u8, call, "app/webapiVersion")) return plain(e, .ok, webapi);
    if (std.mem.eql(u8, call, "app/preferences")) return preferences(h, e);
    if (std.mem.eql(u8, call, "torrents/info")) return info(h, e);
    if (std.mem.eql(u8, call, "torrents/categories")) return categories(h, e);
    if (std.mem.eql(u8, call, "torrents/properties")) return properties(h, e);
    if (std.mem.eql(u8, call, "torrents/files")) return files(h, e);
    if (std.mem.eql(u8, call, "torrents/tags")) return writeJsonBytes(e, "[]");
    if (std.mem.eql(u8, call, "sync/maindata")) return maindata(h, e);
    return plain(e, .not_found, "Not Found");
}

fn post(h: *Handler, e: *ex.Exchange, call: []const u8) !void {
    if (std.mem.eql(u8, call, "app/setPreferences")) return plain(e, .ok, "Ok.");
    if (std.mem.eql(u8, call, "torrents/add")) return add(h, e);
    const body = e.bodyAlloc() catch return plain(e, .bad_request, "Bad Request");
    if (std.mem.eql(u8, call, "torrents/createCategory")) return createCategory(h, e, body);
    if (std.mem.eql(u8, call, "torrents/setCategory")) return setCategory(h, e, body);
    if (std.mem.eql(u8, call, "torrents/setShareLimits")) return setShareLimits(h, e, body);
    if (std.mem.eql(u8, call, "torrents/setForceStart")) return setForce(h, e, body);
    if (moveName(call)) |move| return eachHash(h, e, body, .{ .move = move });
    const act: Act = if (std.mem.eql(u8, call, "torrents/pause") or std.mem.eql(u8, call, "torrents/stop"))
        .pause
    else if (std.mem.eql(u8, call, "torrents/resume") or std.mem.eql(u8, call, "torrents/start"))
        .unpause
    else if (std.mem.eql(u8, call, "torrents/delete"))
        if (formFlag(body, "deleteFiles")) .delete_data else .delete_keep
    else
        return plain(e, .not_found, "Not Found");
    const act_fn = h.ops.act_fn orelse return plain(e, .ok, "Ok.");
    const hashes = try formDecoded(e.arena, body, "hashes");
    if (std.mem.eql(u8, hashes, "all")) {
        const rows = if (h.ops.list_fn) |list| try list(h.ops.ctx, e.arena) else &.{};
        for (rows) |row| act_fn(h.ops.ctx, e.arena, row.hash, act) catch {};
        return plain(e, .ok, "Ok.");
    }
    var it = std.mem.splitScalar(u8, hashes, '|');
    while (it.next()) |part| {
        const hash = std.mem.trim(u8, part, " ");
        if (hash.len == 0) continue;
        act_fn(h.ops.ctx, e.arena, hash, act) catch {};
    }
    return plain(e, .ok, "Ok.");
}

fn createCategory(h: *Handler, e: *ex.Exchange, body: []const u8) !void {
    const name = std.mem.trim(u8, try formDecoded(e.arena, body, "category"), " ");
    if (name.len == 0) return plain(e, .bad_request, "Category name is empty");
    const ensure = h.ops.ensure_cat_fn orelse return plain(e, .ok, "Ok.");
    ensure(h.ops.ctx, e.arena, name) catch |err| switch (err) {
        error.BadName => return plain(e, .conflict, "Incorrect category name"),
        else => return err,
    };
    return plain(e, .ok, "Ok.");
}

fn preferences(h: *Handler, e: *ex.Exchange) !void {
    const prefs = if (h.ops.prefs_fn) |f| try f(h.ops.ctx, e.arena) else return plain(e, .ok, "{}");
    return writeJson(e, prefs);
}

fn categories(h: *Handler, e: *ex.Exchange) !void {
    const cats = if (h.ops.cats_fn) |f| try f(h.ops.ctx, e.arena) else &.{};
    var aw: std.Io.Writer.Allocating = .init(e.arena);
    try aw.writer.writeByte('{');
    for (cats, 0..) |c, i| {
        if (i > 0) try aw.writer.writeByte(',');
        try std.json.Stringify.value(c.name, .{}, &aw.writer);
        try aw.writer.writeAll(":{\"name\":");
        try std.json.Stringify.value(c.name, .{}, &aw.writer);
        try aw.writer.writeAll(",\"savePath\":");
        try std.json.Stringify.value(c.save_path, .{}, &aw.writer);
        try aw.writer.writeByte('}');
    }
    try aw.writer.writeByte('}');
    return writeJsonBytes(e, aw.written());
}

fn properties(h: *Handler, e: *ex.Exchange) !void {
    const row = try oneHash(h, e) orelse return plain(e, .not_found, "Not Found");
    const props = .{
        .hash = row.hash,
        .save_path = "",
        .creation_date = @as(i64, -1),
        .piece_size = @as(i64, -1),
        .total_size = row.total_bytes,
        .addition_date = @divTrunc(row.added_at, 1000),
        .share_ratio = @as(f64, 0),
        .eta = @as(i64, 8640000),
        .dl_speed = @as(i64, 0),
        .up_speed = @as(i64, 0),
        .pieces_have = @as(i64, -1),
        .pieces_num = @as(i64, -1),
    };
    return writeJson(e, props);
}

fn files(h: *Handler, e: *ex.Exchange) !void {
    const row = try oneHash(h, e) orelse return plain(e, .not_found, "Not Found");
    if (row.total_bytes <= 0) return writeJsonBytes(e, "[]");
    const one = .{
        .index = 0,
        .name = row.name,
        .size = row.total_bytes,
        .progress = qbitProgress(row),
        .priority = 1,
        .is_seed = std.mem.eql(u8, row.state, "ready"),
        .availability = @as(f64, 1),
    };
    return writeJson(e, .{one});
}

fn oneHash(h: *Handler, e: *ex.Exchange) !?Listed {
    const hash = queryValue(e.query, "hash") orelse return null;
    if (hash.len != 40) return null;
    const rows = if (h.ops.list_fn) |list| try list(h.ops.ctx, e.arena) else return null;
    for (rows) |row| if (std.ascii.eqlIgnoreCase(row.hash, hash)) return row;
    return null;
}

fn queryValue(query: []const u8, key: []const u8) ?[]const u8 {
    var it = std.mem.splitScalar(u8, query, '&');
    while (it.next()) |pair| {
        const eq = std.mem.indexOfScalar(u8, pair, '=') orelse continue;
        if (std.mem.eql(u8, pair[0..eq], key)) return pair[eq + 1 ..];
    }
    return null;
}

fn writeJson(e: *ex.Exchange, value: anytype) !void {
    var aw: std.Io.Writer.Allocating = .init(e.arena);
    std.json.Stringify.value(value, .{}, &aw.writer) catch return error.OutOfMemory;
    return writeJsonBytes(e, aw.written());
}

fn writeJsonBytes(e: *ex.Exchange, text: []const u8) !void {
    const body = try e.arena.alloc(u8, text.len + 1);
    @memcpy(body[0..text.len], text);
    body[text.len] = '\n';
    e.res.status = .ok;
    e.res.body = body;
    try e.res.setHeader(e.arena, "Content-Type", "application/json");
    try e.res.setHeader(e.arena, "Cache-Control", "no-store");
}

fn setCategory(h: *Handler, e: *ex.Exchange, body: []const u8) !void {
    const name = std.mem.trim(u8, try formDecoded(e.arena, body, "category"), " ");
    var id: []const u8 = "";
    if (name.len > 0) {
        const cats = if (h.ops.cats_fn) |f| try f(h.ops.ctx, e.arena) else return plain(e, .conflict, "Category name does not exist");
        const found = for (cats) |c| {
            if (std.ascii.eqlIgnoreCase(c.name, name)) break c;
        } else null;
        if (found == null) return plain(e, .conflict, "Category name does not exist");
        id = found.?.id;
    }
    return eachHash(h, e, body, .{ .category = id });
}

fn setShareLimits(h: *Handler, e: *ex.Exchange, body: []const u8) !void {
    const ratio = shareArg(try formDecoded(e.arena, body, "ratioLimit"), true) orelse return plain(e, .bad_request, "Bad Request");
    const minutes = shareArg(try formDecoded(e.arena, body, "seedingTimeLimit"), false) orelse return plain(e, .bad_request, "Bad Request");
    return eachHash(h, e, body, .{ .share = .{ .ratio = ratio, .minutes = minutes } });
}

fn setForce(h: *Handler, e: *ex.Exchange, body: []const u8) !void {
    const priority: []const u8 = if (formFlag(body, "value")) "force" else "normal";
    return eachHash(h, e, body, .{ .priority = priority });
}

fn moveName(call: []const u8) ?[]const u8 {
    if (std.mem.eql(u8, call, "torrents/topPrio")) return "top";
    if (std.mem.eql(u8, call, "torrents/bottomPrio")) return "bottom";
    if (std.mem.eql(u8, call, "torrents/increasePrio")) return "up";
    if (std.mem.eql(u8, call, "torrents/decreasePrio")) return "down";
    return null;
}

fn eachHash(h: *Handler, e: *ex.Exchange, body: []const u8, op: HashOp) !void {
    const hashes = try formDecoded(e.arena, body, "hashes");
    var list: std.ArrayList([]const u8) = .empty;
    if (std.mem.eql(u8, hashes, "all")) {
        const rows = if (h.ops.list_fn) |list_fn| try list_fn(h.ops.ctx, e.arena) else &.{};
        for (rows) |row| try list.append(e.arena, row.hash);
    } else {
        var it = std.mem.splitScalar(u8, hashes, '|');
        while (it.next()) |part| {
            const hash = std.mem.trim(u8, part, " ");
            if (hash.len > 0) try list.append(e.arena, hash);
        }
    }
    if (op == .move and (std.mem.eql(u8, op.move, "top") or std.mem.eql(u8, op.move, "down"))) {
        std.mem.reverse([]const u8, list.items);
    }
    for (list.items) |hash| switch (op) {
        .move => |m| if (h.ops.move_fn) |f| f(h.ops.ctx, e.arena, hash, m) catch {},
        .category => |id| if (h.ops.set_category_fn) |f| f(h.ops.ctx, e.arena, hash, id) catch {},
        .priority => |p| if (h.ops.priority_fn) |f| f(h.ops.ctx, e.arena, hash, p) catch {},
        .share => |s| if (h.ops.share_fn) |f| f(h.ops.ctx, e.arena, hash, s.ratio, s.minutes) catch {},
    };
    return plain(e, .ok, "Ok.");
}

fn shareArg(raw: []const u8, float: bool) ?f64 {
    const s = std.mem.trim(u8, raw, " ");
    if (s.len == 0) return null;
    if (float) return std.fmt.parseFloat(f64, s) catch null;
    const n = std.fmt.parseInt(i64, s, 10) catch return null;
    const clamped = std.math.clamp(n, -2, 100 * 365 * 24 * 60);
    return @floatFromInt(clamped);
}

fn add(h: *Handler, e: *ex.Exchange) !void {
    const ct = e.header("Content-Type") orelse "";
    if (mime.parse(e.arena, ct)) |media| {
        if (std.mem.eql(u8, media.type, "multipart/form-data")) return addMultipart(h, e, media);
    } else |_| {}
    const body = e.bodyAlloc() catch return plain(e, .bad_request, "Bad Request");
    return addForm(h, e, body);
}

fn addForm(h: *Handler, e: *ex.Exchange, body: []const u8) !void {
    const add_fn = h.ops.add_magnet_fn orelse return plain(e, .ok, "Fails.");
    const urls = try formDecoded(e.arena, body, "urls");
    const paused = formFlag(body, "paused") or formFlag(body, "stopped");
    const category = std.mem.trim(u8, try formDecoded(e.arena, body, "category"), " ");
    var added = false;
    var it = std.mem.splitScalar(u8, urls, '\n');
    while (it.next()) |line| {
        const magnet = std.mem.trim(u8, line, " \r\t");
        if (magnet.len == 0) continue;
        add_fn(h.ops.ctx, e.arena, magnet, paused, category) catch continue;
        added = true;
    }
    return plain(e, .ok, if (added) "Ok." else "Fails.");
}

const max_torrent_bytes: usize = 10 << 20;

fn addMultipart(h: *Handler, e: *ex.Exchange, media: mime.Media) !void {
    const boundary = media.get("boundary") orelse return plain(e, .bad_request, "Bad Request");
    const buf = try e.arena.alloc(u8, 16 << 10);
    const src = e.bodyReader(buf) catch return plain(e, .bad_request, "Bad Request");
    var reader = multipart.Reader.init(e.arena, src, boundary, e.body_limit) catch return plain(e, .bad_request, "Bad Request");
    const File = struct { bytes: []const u8, name: []const u8 };
    var uploads: std.ArrayList(File) = .empty;
    var urls: []const u8 = "";
    var category: []const u8 = "";
    var paused = false;
    var n: usize = 0;
    while (true) : (n += 1) {
        if (n >= 32) return plain(e, .bad_request, "Bad Request");
        const maybe = reader.next(e.arena) catch return plain(e, .bad_request, "Bad Request");
        const part = maybe orelse break;
        if (part.file_name.len > 0) {
            const bytes = reader.readUpTo(e.arena, max_torrent_bytes + 1) catch return plain(e, .bad_request, "Bad Request");
            if (bytes.len > max_torrent_bytes) return plain(e, .payload_too_large, "Request Entity Too Large");
            if (bytes.len == 0) continue;
            if (uploads.items.len >= 8) return plain(e, .bad_request, "Bad Request");
            try uploads.append(e.arena, .{ .bytes = bytes, .name = part.file_name });
            continue;
        }
        const raw = reader.readUpTo(e.arena, 64 << 10) catch return plain(e, .bad_request, "Bad Request");
        if (std.mem.eql(u8, part.name, "urls")) urls = raw;
        if (std.mem.eql(u8, part.name, "category")) category = std.mem.trim(u8, raw, " ");
        if (std.mem.eql(u8, part.name, "paused") or std.mem.eql(u8, part.name, "stopped")) {
            paused = std.mem.eql(u8, raw, "true") or std.mem.eql(u8, raw, "1");
        }
    }
    var added = false;
    if (h.ops.add_file_fn) |add_file| {
        for (uploads.items) |file| {
            add_file(h.ops.ctx, e.arena, file.bytes, file.name, paused, category) catch continue;
            added = true;
        }
    }
    if (h.ops.add_magnet_fn) |add_magnet| {
        var it = std.mem.splitScalar(u8, urls, '\n');
        while (it.next()) |line| {
            const magnet = std.mem.trim(u8, line, " \r\t");
            if (magnet.len == 0) continue;
            add_magnet(h.ops.ctx, e.arena, magnet, paused, category) catch continue;
            added = true;
        }
    }
    return plain(e, .ok, if (added) "Ok." else "Fails.");
}

const InfoJson = struct {
    hash: []const u8,
    infohash_v1: []const u8,
    name: []const u8,
    state: []const u8,
    progress: f64,
    size: i64,
    total_size: i64,
    added_on: i64,
    amount_left: i64,
    completed: i64,
    eta: i64 = 8640000,
    dlspeed: i64 = 0,
    upspeed: i64 = 0,
    category: []const u8 = "",
    save_path: []const u8 = "",
};

fn maindata(h: *Handler, e: *ex.Exchange) !void {
    const raw = queryValue(e.query, "rid") orelse "0";
    const parsed = std.fmt.parseInt(i64, raw, 10) catch 0;
    const rid: i64 = if (parsed < 0) 1 else parsed + 1;
    const rows = if (h.ops.list_fn) |list| try list(h.ops.ctx, e.arena) else &.{};
    const cats = if (h.ops.cats_fn) |f| try f(h.ops.ctx, e.arena) else &.{};
    var aw: std.Io.Writer.Allocating = .init(e.arena);
    try aw.writer.print("{{\"rid\":{d},\"full_update\":true,\"torrents\":{{", .{rid});
    for (rows, 0..) |row, i| {
        if (i > 0) try aw.writer.writeByte(',');
        try std.json.Stringify.value(row.hash, .{}, &aw.writer);
        try aw.writer.writeByte(':');
        const progress = qbitProgress(row);
        const left = if (row.total_bytes > row.done_bytes) row.total_bytes - row.done_bytes else 0;
        const item: InfoJson = .{
            .hash = row.hash,
            .infohash_v1 = row.hash,
            .name = row.name,
            .state = qbitState(row),
            .progress = progress,
            .size = row.total_bytes,
            .total_size = row.total_bytes,
            .added_on = @divTrunc(row.added_at, 1000),
            .amount_left = left,
            .completed = row.done_bytes,
            .category = row.category,
        };
        std.json.Stringify.value(item, .{}, &aw.writer) catch return error.OutOfMemory;
    }
    try aw.writer.writeAll("},\"categories\":{");
    for (cats, 0..) |c, i| {
        if (i > 0) try aw.writer.writeByte(',');
        try std.json.Stringify.value(c.name, .{}, &aw.writer);
        try aw.writer.writeAll(":{\"name\":");
        try std.json.Stringify.value(c.name, .{}, &aw.writer);
        try aw.writer.writeAll(",\"savePath\":");
        try std.json.Stringify.value(c.save_path, .{}, &aw.writer);
        try aw.writer.writeByte('}');
    }
    try aw.writer.writeAll("},\"tags\":[],\"trackers\":{},\"server_state\":{\"connection_status\":\"connected\",\"dl_info_speed\":0,\"up_info_speed\":0,\"free_space_on_disk\":0,\"queueing\":true,\"use_alt_speed_limits\":false}}");
    return writeJsonBytes(e, aw.written());
}

fn info(h: *Handler, e: *ex.Exchange) !void {
    const rows = if (h.ops.list_fn) |list| try list(h.ops.ctx, e.arena) else &.{};
    const out = try e.arena.alloc(InfoJson, rows.len);
    for (rows, out) |row, *slot| {
        const progress = qbitProgress(row);
        const left = if (row.total_bytes > row.done_bytes) row.total_bytes - row.done_bytes else 0;
        slot.* = .{
            .hash = row.hash,
            .infohash_v1 = row.hash,
            .name = row.name,
            .state = qbitState(row),
            .progress = progress,
            .size = row.total_bytes,
            .total_size = row.total_bytes,
            .added_on = @divTrunc(row.added_at, 1000),
            .amount_left = left,
            .completed = row.done_bytes,
            .category = row.category,
        };
    }
    var aw: std.Io.Writer.Allocating = .init(e.arena);
    std.json.Stringify.value(out, .{}, &aw.writer) catch return error.OutOfMemory;
    const text = aw.written();
    const body = try e.arena.alloc(u8, text.len + 1);
    @memcpy(body[0..text.len], text);
    body[text.len] = '\n';
    e.res.status = .ok;
    e.res.body = body;
    try e.res.setHeader(e.arena, "Content-Type", "application/json");
    try e.res.setHeader(e.arena, "Cache-Control", "no-store");
}

fn qbitState(row: Listed) []const u8 {
    if (std.mem.eql(u8, row.state, "failed")) return "error";
    if (std.mem.eql(u8, row.state, "ready")) return if (row.paused) "pausedUP" else "uploading";
    if (std.mem.eql(u8, row.state, "fetched") or std.mem.eql(u8, row.state, "post_processing")) return "checkingUP";
    if (row.paused) return "pausedDL";
    if (std.mem.eql(u8, row.state, "fetching_metadata")) return "metaDL";
    if (std.mem.eql(u8, row.state, "fetching")) return "downloading";
    return "queuedDL";
}

fn qbitProgress(row: Listed) f64 {
    if (std.mem.eql(u8, row.state, "ready")) return 1;
    if (row.total_bytes <= 0) return 0;
    const p = @as(f64, @floatFromInt(row.done_bytes)) / @as(f64, @floatFromInt(row.total_bytes));
    if (p >= 1) return 0.9999;
    return p;
}

fn formFlag(body: []const u8, key: []const u8) bool {
    var it = std.mem.splitScalar(u8, body, '&');
    while (it.next()) |pair| {
        const eq = std.mem.indexOfScalar(u8, pair, '=') orelse continue;
        if (!std.mem.eql(u8, pair[0..eq], key)) continue;
        const v = pair[eq + 1 ..];
        return std.mem.eql(u8, v, "true") or std.mem.eql(u8, v, "1") or std.mem.eql(u8, v, "yes");
    }
    return false;
}

fn formDecoded(arena: Allocator, body: []const u8, key: []const u8) ![]u8 {
    var it = std.mem.splitScalar(u8, body, '&');
    while (it.next()) |pair| {
        const eq = std.mem.indexOfScalar(u8, pair, '=') orelse continue;
        if (!std.mem.eql(u8, pair[0..eq], key)) continue;
        return pctDecode(arena, pair[eq + 1 ..]);
    }
    return arena.dupe(u8, "");
}

fn login(h: *Handler, e: *ex.Exchange) !void {
    const body = e.bodyAlloc() catch return plain(e, .ok, "Fails.");
    const fields = formFields(body);
    const user = pctDecode(e.arena, fields.username) catch return plain(e, .ok, "Fails.");
    const pass = pctDecode(e.arena, fields.password) catch return plain(e, .ok, "Fails.");
    const ok_key = h.keys.authenticate(pass);
    const ok_user = token.hashEqual(token.hashToken(user), token.hashToken(username));
    if (!ok_key or !ok_user) return plain(e, .ok, "Fails.");
    const sid = h.sessions.create() catch return plain(e, .internal_server_error, "Internal Server Error");
    const cookie = try std.fmt.allocPrint(e.arena, "SID={s}; Path=/; HttpOnly; SameSite=Strict", .{sid});
    try e.res.setHeader(e.arena, "Set-Cookie", cookie);
    return plain(e, .ok, "Ok.");
}

fn plain(e: *ex.Exchange, status: std.http.Status, text: []const u8) !void {
    e.res.status = status;
    e.res.body = text;
    try e.res.setHeader(e.arena, "Content-Type", "text/plain; charset=utf-8");
}

fn forbidden(e: *ex.Exchange) !void {
    return plain(e, .forbidden, "Forbidden");
}

fn callName(path: []const u8) []const u8 {
    const rest = if (std.mem.startsWith(u8, path, "/api/v2/")) path["/api/v2/".len..] else "";
    if (rest.len > 0 and rest[rest.len - 1] == '/') return rest[0 .. rest.len - 1];
    return rest;
}

fn cookieSid(e: *const ex.Exchange) ?[]const u8 {
    const raw = e.header("Cookie") orelse return null;
    var it = std.mem.splitScalar(u8, raw, ';');
    while (it.next()) |part| {
        const p = std.mem.trim(u8, part, " ");
        if (std.mem.startsWith(u8, p, "SID=")) return p["SID=".len..];
    }
    return null;
}

const Fields = struct { username: []const u8 = "", password: []const u8 = "" };

fn formFields(body: []const u8) Fields {
    var out: Fields = .{};
    var it = std.mem.splitScalar(u8, body, '&');
    while (it.next()) |pair| {
        const eq = std.mem.indexOfScalar(u8, pair, '=') orelse continue;
        const key = pair[0..eq];
        const val = pair[eq + 1 ..];
        if (std.mem.eql(u8, key, "username") and out.username.len == 0) out.username = val;
        if (std.mem.eql(u8, key, "password") and out.password.len == 0) out.password = val;
    }
    return out;
}

fn pctDecode(arena: Allocator, s: []const u8) Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) : (i += 1) {
        if (s[i] == '+') {
            try out.append(arena, ' ');
            continue;
        }
        if (s[i] == '%' and i + 2 < s.len) {
            const v = std.fmt.parseInt(u8, s[i + 1 .. i + 3], 16) catch {
                try out.append(arena, s[i]);
                continue;
            };
            try out.append(arena, v);
            i += 2;
            continue;
        }
        try out.append(arena, s[i]);
    }
    return out.toOwnedSlice(arena);
}

test "paused metadata is pausedDL and ready is uploading" {
    try std.testing.expectEqualStrings("pausedDL", qbitState(.{
        .hash = "",
        .name = "",
        .state = "fetching_metadata",
        .paused = true,
        .total_bytes = 0,
        .done_bytes = 0,
        .added_at = 0,
    }));
    try std.testing.expectEqualStrings("uploading", qbitState(.{
        .hash = "",
        .name = "",
        .state = "ready",
        .paused = false,
        .total_bytes = 10,
        .done_bytes = 10,
        .added_at = 0,
    }));
    try std.testing.expectEqual(@as(f64, 1), qbitProgress(.{
        .hash = "",
        .name = "",
        .state = "ready",
        .paused = false,
        .total_bytes = 10,
        .done_bytes = 10,
        .added_at = 0,
    }));
}

test "callName strips the /api/v2 prefix" {
    try std.testing.expectEqualStrings("auth/login", callName("/api/v2/auth/login"));
    try std.testing.expectEqualStrings("torrents/info", callName("/api/v2/torrents/info/"));
}
