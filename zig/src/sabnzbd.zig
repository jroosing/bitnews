//! SABnzbd adapter at `/sabnzbd/api` (M4 / Z5). Register is the only public
//! seam; modes, shapes and auth stay inside.
const std = @import("std");
const ex = @import("http_exchange.zig");
const http_common = @import("http_common.zig");
const sabnzbd_params = @import("sabnzbd_params.zig");
const mime = @import("add_mime.zig");
const multipart = @import("add_multipart.zig");
const sabnzbd_config = @import("sabnzbd_config.zig");
const sabnzbd_add = @import("sabnzbd_add.zig");
const sabnzbd_format = @import("sabnzbd_format.zig");
const sabnzbd_queue = @import("sabnzbd_queue.zig");
const sabnzbd_history = @import("sabnzbd_history.zig");
const sabnzbd_status = @import("sabnzbd_status.zig");
const sabnzbd_text = @import("sabnzbd_text.zig");
const download = @import("download.zig");
const event = @import("event.zig");
const app_error = @import("app_error.zig");

pub const default_version = "4.5.0";

/// Upload ceiling: DefaultMaxNZBBytes + 1 MiB, as Go's addBodyLimit.
pub const body_limit: u64 = (64 << 20) + (1 << 20);

/// Go's maxParts. The next part is "Too many form fields".
const max_form_parts = 32;
/// Go's maxFieldBytes. A longer text part is "Form field too long".
const max_field_bytes = 16 << 10;

/// Optional category-name source for mode=get_cats. Default empty → `["*"]`.
pub const CatNames = struct {
    ctx: ?*anyopaque = null,
    list_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator) anyerror![]const []const u8 = null,

    pub fn list(self: CatNames, arena: std.mem.Allocator) ![]const []const u8 {
        if (self.list_fn) |f| return f(self.ctx, arena);
        return &.{};
    }
};

/// Usenet dirs for mode=get_config / fullstatus. Default empty strings.
pub const Paths = struct {
    complete: []const u8 = "",
    incomplete: []const u8 = "",
};

/// Optional global rate + pause for mode=fullstatus / status. Default 0,false.
pub const SpeedSample = struct {
    bytes_per_sec: i64 = 0,
    paused: bool = false,
};

pub const Speed = struct {
    ctx: ?*anyopaque = null,
    read_fn: ?*const fn (ctx: ?*anyopaque) SpeedSample = null,

    pub fn read(self: Speed) SpeedSample {
        if (self.read_fn) |f| {
            const s = f(self.ctx);
            return .{ .bytes_per_sec = @max(s.bytes_per_sec, 0), .paused = s.paused };
        }
        return .{};
    }
};

/// Optional free/total bytes for a path. Default 0,0 and not ok → gigabytes(0).
pub const DiskSample = struct {
    free: i64 = 0,
    total: i64 = 0,
    ok: bool = false,
};

pub const Disk = struct {
    ctx: ?*anyopaque = null,
    space_fn: ?*const fn (ctx: ?*anyopaque, path: []const u8) DiskSample = null,

    pub fn space(self: Disk, path: []const u8) DiskSample {
        if (self.space_fn) |f| {
            const s = f(self.ctx, path);
            if (!s.ok) return .{};
            return .{ .free = @max(s.free, 0), .total = @max(s.total, 0), .ok = true };
        }
        return .{};
    }
};

pub const AddUrlReq = struct {
    url: []const u8,
    nzbname: []const u8 = "",
    cat: []const u8 = "",
    priority: []const u8 = "",
    password: []const u8 = "",
};

pub const AddFileReq = struct {
    bytes: []const u8,
    file_name: []const u8 = "",
    nzbname: []const u8 = "",
    cat: []const u8 = "",
    priority: []const u8 = "",
    password: []const u8 = "",
};

/// Optional NZB-by-URL add for mode=addurl. Default unset → handler answers not implemented.
pub const Adder = struct {
    ctx: ?*anyopaque = null,
    add_url_fn: ?*const fn (ctx: ?*anyopaque, req: AddUrlReq, arena: std.mem.Allocator, fail: *app_error.Failure) anyerror!download.Download = null,
    add_file_fn: ?*const fn (ctx: ?*anyopaque, req: AddFileReq, arena: std.mem.Allocator, fail: *app_error.Failure) anyerror!download.Download = null,
};

pub const Downloads = struct {
    ctx: ?*anyopaque = null,
    all_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator) anyerror![]download.Download = null,
    pause_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8) anyerror!void = null,
    resume_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8) anyerror!void = null,
    /// Queue deletes always pass `delete_data` true. History passes `del_files`.
    delete_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8, delete_data: bool) anyerror!void = null,
    /// `priority` is `force`/`high`/`normal`/`low`. `move` is `top`/`bottom`/`up`/`down`.
    /// `paused` is SAB priority `-2`: apply the priority, then pause.
    update_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8, priority: ?[]const u8, paused: bool, move: []const u8) anyerror!void = null,
};

/// Category display name for a Download's `category_id`. Empty id is `*`.
pub const CatName = struct {
    ctx: ?*anyopaque = null,
    name_fn: ?*const fn (ctx: ?*anyopaque, arena: std.mem.Allocator, category_id: []const u8) []const u8 = null,

    pub fn name(self: CatName, arena: std.mem.Allocator, category_id: []const u8) []const u8 {
        if (category_id.len == 0) return "*";
        const f = self.name_fn orelse return "";
        const n = f(self.ctx, arena, category_id);
        return if (n.len == 0) "*" else n;
    }
};

/// Post-process stage and per-download rate from the live event cache.
pub const Feed = struct {
    ctx: ?*anyopaque = null,
    stage_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8) ?event.Stage = null,
    rate_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8) i64 = null,

    pub fn stage(self: Feed, id: []const u8) ?event.Stage {
        const f = self.stage_fn orelse return null;
        return f(self.ctx, id);
    }

    pub fn rate(self: Feed, id: []const u8) i64 {
        const f = self.rate_fn orelse return 0;
        return @max(f(self.ctx, id), 0);
    }
};

/// Process-lifetime handler. Parent stores it in Services and passes it to
/// `register`. `version` is what mode=version reports; empty reports
/// `default_version`.
pub const Handler = struct {
    version: []const u8 = default_version,
    keys: http_common.KeyChecker,
    cat_names: CatNames = .{},
    paths: Paths = .{},
    speed: Speed = .{},
    disk: Disk = .{},
    adder: Adder = .{},
    downloads: Downloads = .{},
    cat_name: CatName = .{},
    feed: Feed = .{},
};

/// Mounts `/sabnzbd/api` and `/sabnzbd/api/` (subtree) for every method,
/// including HEAD, with `Auth.none`. Auth of the other modes is a later slice.
pub fn register(routes: *ex.Routes, handler: *Handler) !void {
    const route: ex.Route = .{
        .handler = handle,
        .state = handler,
        .auth = .none,
        .body_limit = body_limit,
    };
    try routes.add("/sabnzbd/api", route);
    try routes.add("/sabnzbd/api/", route);
}

fn handle(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const h: *Handler = @ptrCast(@alignCast(state.?));
    if (try readCall(h, e)) |call| {
        const params = call.params;
        const mode = params.get("mode");
        if (std.ascii.eqlIgnoreCase(mode, "version")) {
            try e.res.json(e.arena, .ok, .{ .version = versionOf(h) });
        } else if (!try authed(h, e, params)) {
            // authed wrote the SAB error. A multipart file is not used before this passes.
        } else if (std.ascii.eqlIgnoreCase(mode, "retry")) {
            try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = "retry is not supported" });
        } else if (std.ascii.eqlIgnoreCase(mode, "addurl")) {
            if (sabnzbd_add.addUrlError(params.get("name"))) |msg| {
                try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = msg });
            } else if (h.adder.add_url_fn) |add_url| {
                // SAB answers app refusals as HTTP 200 `{status:false,error}`
                // (Go appFail). Only `error.Rejected` maps; other errors bubble.
                var failure: app_error.Failure = .{};
                if (add_url(h.adder.ctx, .{
                    .url = params.get("name"),
                    .nzbname = params.get("nzbname"),
                    .cat = params.get("cat"),
                    .priority = params.get("priority"),
                    .password = params.get("password"),
                }, e.arena, &failure)) |dl| {
                    const nzo_ids = try e.arena.alloc([]const u8, 1);
                    nzo_ids[0] = sabnzbd_status.nzoID(e.arena, dl.id);
                    try e.res.json(e.arena, .ok, .{ .status = true, .nzo_ids = nzo_ids });
                } else |err| switch (err) {
                    error.Rejected => {
                        const msg = if (failure.message.len == 0) "Internal error" else failure.message;
                        try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = msg });
                    },
                    else => return err,
                }
            } else {
                try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = "not implemented" });
            }
        } else if (std.ascii.eqlIgnoreCase(mode, "addfile")) {
            try answerAddFile(h, e, params, call.file);
        } else if (std.ascii.eqlIgnoreCase(mode, "get_cats")) {
            const cat_names = try h.cat_names.list(e.arena);
            const names = try sabnzbd_config.categoryNames(e.arena, cat_names);
            try e.res.json(e.arena, .ok, .{ .categories = names });
        } else if (std.ascii.eqlIgnoreCase(mode, "get_config")) {
            try e.res.json(e.arena, .ok, try configJson(e.arena, h));
        } else if (std.ascii.eqlIgnoreCase(mode, "fullstatus") or std.ascii.eqlIgnoreCase(mode, "status")) {
            try e.res.json(e.arena, .ok, try fullStatusJson(e.arena, h));
        } else if (std.ascii.eqlIgnoreCase(mode, "queue") and std.ascii.eqlIgnoreCase(params.get("name"), "delete")) {
            if (try deleteIds(h, e, params.get("value"), true, true)) |ids| {
                try e.res.json(e.arena, .ok, .{ .status = true, .nzo_ids = ids });
            }
        } else if (std.ascii.eqlIgnoreCase(mode, "queue") and (std.ascii.eqlIgnoreCase(params.get("name"), "pause") or std.ascii.eqlIgnoreCase(params.get("name"), "resume"))) {
            const pausing = std.ascii.eqlIgnoreCase(params.get("name"), "pause");
            if (try pauseIds(h, e, params.get("value"), pausing)) |ids| {
                try e.res.json(e.arena, .ok, .{ .status = true, .nzo_ids = ids });
            }
        } else if (std.ascii.eqlIgnoreCase(mode, "queue") and std.ascii.eqlIgnoreCase(params.get("name"), "priority")) {
            try queuePriority(h, e, params);
        } else if (std.ascii.eqlIgnoreCase(mode, "queue") and std.ascii.eqlIgnoreCase(params.get("name"), "switch")) {
            try queueSwitch(h, e, params);
        } else if (std.ascii.eqlIgnoreCase(mode, "queue") and params.get("name").len == 0) {
            try e.res.json(e.arena, .ok, try queueJson(e.arena, h, params));
        } else if (std.ascii.eqlIgnoreCase(mode, "history") and std.ascii.eqlIgnoreCase(params.get("name"), "delete")) {
            // Go history delete returns status only. Unknown ids are ignored.
            // del_files decides whether the finished output goes too.
            const files = deleteFiles(params.get("del_files"));
            if (try deleteIds(h, e, params.get("value"), false, files)) |_| {
                try e.res.json(e.arena, .ok, .{ .status = true });
            }
        } else if (std.ascii.eqlIgnoreCase(mode, "history") and params.get("name").len == 0) {
            try e.res.json(e.arena, .ok, try historyJson(e.arena, h, params));
        } else {
            try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = "not implemented" });
        }
    }
    // Go sets this before encode; respond.json writes the type without charset.
    try e.res.setHeader(e.arena, "Content-Type", "application/json; charset=utf-8");
}

fn versionOf(h: *const Handler) []const u8 {
    return if (h.version.len == 0) default_version else h.version;
}

/// Wire shape of one SABnzbd queue slot (shapes.go queueSlot). Bytes and age
/// stay zero until a later slice has totals and a clock. `labels` is an empty
/// array, never null.
const QueueSlot = struct {
    status: []const u8,
    index: i64,
    nzo_id: []const u8,
    filename: []const u8,
    cat: []const u8,
    priority: []const u8,
    script: []const u8,
    mb: []const u8,
    mbleft: []const u8,
    mbmissing: []const u8,
    size: []const u8,
    sizeleft: []const u8,
    percentage: []const u8,
    timeleft: []const u8,
    avg_age: []const u8,
    password: []const u8,
    unpackopts: []const u8,
    has_rating: bool,
    labels: []const []const u8,
};
const empty_slots: []const QueueSlot = &.{};
const empty_labels: []const []const u8 = &.{};

/// Wire shape of one SABnzbd history slot (shapes.go historySlot). This slice
/// fills nzo_id, name, status and nzb_name. The other keys stay zero or empty
/// until a later slice has bytes, paths and times.
const HistorySlot = struct {
    nzo_id: []const u8,
    name: []const u8,
    nzb_name: []const u8,
    category: []const u8,
    status: []const u8,
    fail_message: []const u8,
    action_line: []const u8,
    storage: []const u8,
    path: []const u8,
    bytes: i64,
    size: []const u8,
    downloaded: i64,
    completed: i64,
    download_time: i64,
    postproc_time: i64,
    pp: []const u8,
    script: []const u8,
    script_line: []const u8,
    report: []const u8,
    password: []const u8,
    retry: i64,
    loaded: bool,
    has_rating: bool,
    url: []const u8,
    url_info: []const u8,
    md5sum: []const u8,
    duplicate_key: []const u8,
};
const empty_history_slots: []const HistorySlot = &.{};

fn historyJson(arena: std.mem.Allocator, h: *const Handler, params: sabnzbd_params.Params) !struct {
    history: struct {
        noofslots: i64,
        ppslots: i64,
        total_size: []const u8,
        month_size: []const u8,
        week_size: []const u8,
        day_size: []const u8,
        last_history_update: i64,
        slots: []const HistorySlot,
    },
} {
    const filter = blk: {
        const category = params.get("category");
        break :blk if (category.len > 0) category else params.get("cat");
    };
    const all_shown = if (h.downloads.all_fn) |all_fn|
        try historyShown(arena, h, try all_fn(h.downloads.ctx, arena), filter)
    else
        &.{};
    const start = parseNonNeg(params.get("start"));
    const limit = parseNonNeg(params.get("limit"));
    const window = sabnzbd_queue.page(@intCast(all_shown.len), start, limit);
    const page = all_shown[@intCast(window.lo)..@intCast(window.hi)];
    return .{
        .history = .{
            .noofslots = sabnzbd_history.noOfSlots(all_shown.len),
            .ppslots = sabnzbd_history.noOfSlots(historyPPCount(all_shown)),
            .total_size = "0 B",
            .month_size = "0 B",
            .week_size = "0 B",
            .day_size = "0 B",
            .last_history_update = 0,
            .slots = try historySlots(arena, h, page),
        },
    };
}

/// NZB rows `inHistoryListing` keeps that `filter` allows, newest first.
fn historyShown(arena: std.mem.Allocator, h: *const Handler, all: []const download.Download, filter: []const u8) ![]const download.Download {
    var n: usize = 0;
    for (all) |d| {
        if (sabnzbd_status.inHistoryListing(d) and try allowsCat(h, arena, d, filter)) n += 1;
    }
    if (n == 0) return &.{};
    const shown = try arena.alloc(download.Download, n);
    var i: usize = 0;
    for (all) |d| {
        if (!sabnzbd_status.inHistoryListing(d) or !try allowsCat(h, arena, d, filter)) continue;
        shown[i] = d;
        i += 1;
    }
    std.mem.sort(download.Download, shown, {}, historyNewer);
    return shown;
}

fn historyNewer(_: void, a: download.Download, b: download.Download) bool {
    const ka = historySortMs(a);
    const kb = historySortMs(b);
    if (ka != kb) return ka > kb;
    return std.mem.order(u8, a.id, b.id) == .gt;
}

fn historySortMs(d: download.Download) i64 {
    if (d.ready_at != 0) return d.ready_at;
    if (d.fetched_at != 0) return d.fetched_at;
    return d.updated_at;
}

fn historyPPCount(shown: []const download.Download) usize {
    var n: usize = 0;
    for (shown) |d| {
        if (d.state == .fetched or d.state == .post_processing) n += 1;
    }
    return n;
}

fn historySlots(arena: std.mem.Allocator, h: *const Handler, shown: []const download.Download) ![]const HistorySlot {
    if (shown.len == 0) return empty_history_slots;
    const slots = try arena.alloc(HistorySlot, shown.len);
    for (shown, slots) |d, *slot| slot.* = try historySlotOf(arena, h, d);
    return slots;
}

fn historySlotOf(arena: std.mem.Allocator, h: *const Handler, d: download.Download) !HistorySlot {
    const total = @max(d.total_bytes, 0);
    const done = @max(d.done_bytes, 0);
    const fail = if (d.state == .failed) blk: {
        const reason = d.failure_reason orelse .internal;
        break :blk try sabnzbd_text.failureText(arena, reason, d.failure_detail);
    } else "";
    const storage = if (d.state == .ready) d.output_path else "";
    const nzb = try std.fmt.allocPrint(arena, "{s}.nzb", .{d.name});
    return .{
        .nzo_id = sabnzbd_history.historySlotNzoID(arena, d),
        .name = d.name,
        .nzb_name = nzb,
        .category = h.cat_name.name(arena, d.category_id),
        .status = sabnzbd_status.sabStatus(d, h.feed.stage(d.id)),
        .fail_message = fail,
        .action_line = "",
        .storage = storage,
        .path = storage,
        .bytes = total,
        .size = try sabnzbd_format.humanBytes(arena, total),
        .downloaded = done,
        .completed = @divTrunc(historySortMs(d), 1000),
        .download_time = if (d.fetched_at > d.added_at) @divTrunc(d.fetched_at - d.added_at, 1000) else 0,
        .postproc_time = if (d.ready_at > d.fetched_at and d.fetched_at != 0) @divTrunc(d.ready_at - d.fetched_at, 1000) else 0,
        .pp = "D",
        .script = "None",
        .script_line = "",
        .report = "",
        .password = "",
        .retry = 0,
        .loaded = false,
        .has_rating = false,
        .url = nzb,
        .url_info = "",
        .md5sum = "",
        .duplicate_key = "",
    };
}

fn deleteFiles(v: []const u8) bool {
    return std.ascii.eqlIgnoreCase(v, "1") or std.ascii.eqlIgnoreCase(v, "true") or std.ascii.eqlIgnoreCase(v, "yes");
}

/// `null` when the response was already written (a refusal).
fn deleteIds(h: *Handler, e: *ex.Exchange, value: []const u8, from_queue: bool, delete_data: bool) !?[]const []const u8 {
    const parsed = sabnzbd_status.parseNzoIDs(e.arena, value);
    const del = h.downloads.delete_fn orelse return try nzoIDsFromParsed(e.arena, parsed);
    const listed = try listedIds(h, e.arena, from_queue);
    var kept: std.ArrayList([]const u8) = .empty;
    for (parsed) |id| {
        if (listed) |ids| if (!hasId(ids, id)) continue;
        del(h.downloads.ctx, e.arena, id, delete_data) catch |err| switch (err) {
            error.NotFound, error.Conflict => continue,
            error.Rejected => {
                try sabFail(e, "Internal error");
                return null;
            },
            else => return err,
        };
        try kept.append(e.arena, id);
    }
    return try nzoIDsFromParsed(e.arena, kept.items);
}

fn pauseIds(h: *Handler, e: *ex.Exchange, value: []const u8, pausing: bool) !?[]const []const u8 {
    const parsed = sabnzbd_status.parseNzoIDs(e.arena, value);
    const call = if (pausing) h.downloads.pause_fn else h.downloads.resume_fn;
    const f = call orelse return try nzoIDsFromParsed(e.arena, parsed);
    var kept: std.ArrayList([]const u8) = .empty;
    for (parsed) |id| {
        f(h.downloads.ctx, e.arena, id) catch |err| switch (err) {
            error.NotFound, error.Conflict => continue,
            error.Rejected => {
                try sabFail(e, "Internal error");
                return null;
            },
            else => return err,
        };
        try kept.append(e.arena, id);
    }
    return try nzoIDsFromParsed(e.arena, kept.items);
}

fn queuePriority(h: *Handler, e: *ex.Exchange, params: sabnzbd_params.Params) !void {
    const id = sabnzbd_status.parseNzoID(params.get("value")) orelse {
        try sabFail(e, "expect one parameter");
        return;
    };
    const parsed = sabnzbd_add.parsePriority(params.get("value2"));
    if (h.downloads.update_fn) |update| {
        update(h.downloads.ctx, e.arena, id, @tagName(parsed.priority), parsed.paused, "") catch |err| switch (err) {
            error.NotFound, error.Conflict => {
                try e.res.json(e.arena, .ok, .{ .status = false, .position = @as(i64, 0) });
                return;
            },
            error.Rejected => {
                try sabFail(e, "Internal error");
                return;
            },
            else => return err,
        };
    }
    try e.res.json(e.arena, .ok, .{ .status = true, .position = @as(i64, 0) });
}

fn queueSwitch(h: *Handler, e: *ex.Exchange, params: sabnzbd_params.Params) !void {
    const id = sabnzbd_status.parseNzoID(params.get("value"));
    const target = std.fmt.parseInt(i64, params.get("value2"), 10) catch null;
    if (id == null or target == null) {
        try sabFail(e, "expect two parameters");
        return;
    }
    const listed = (try listedIds(h, e.arena, true)) orelse &.{};
    var cur: ?usize = null;
    for (listed, 0..) |row, i| if (std.mem.eql(u8, row, id.?)) {
        cur = i;
        break;
    };
    const index = cur orelse {
        try e.res.json(e.arena, .ok, .{ .status = true, .position = @as(i64, 0) });
        return;
    };
    if (listed.len == 0) {
        try e.res.json(e.arena, .ok, .{ .status = true, .position = @as(i64, 0) });
        return;
    }
    const last: i64 = @intCast(listed.len - 1);
    const dest: i64 = @min(@max(target.?, 0), last);
    const here: i64 = @intCast(index);
    if (dest != here) {
        if (h.downloads.update_fn == null) {
            try sabFail(e, "not implemented");
            return;
        }
        const update = h.downloads.update_fn.?;
        const move: []const u8 = if (dest == 0) "top" else if (dest == last) "bottom" else if (dest < here) "up" else "down";
        const steps: i64 = if (std.mem.eql(u8, move, "up") or std.mem.eql(u8, move, "down")) @intCast(@max(dest - here, here - dest)) else 1;
        if (steps > 25) {
            try sabFail(e, "not supported: move further than 25 places");
            return;
        }
        var n: i64 = 0;
        while (n < steps) : (n += 1) {
            update(h.downloads.ctx, e.arena, id.?, null, false, move) catch |err| switch (err) {
                error.NotFound, error.Conflict => {
                    try e.res.json(e.arena, .ok, .{ .status = false, .position = @as(i64, 0) });
                    return;
                },
                error.Rejected => {
                    try sabFail(e, "Internal error");
                    return;
                },
                else => return err,
            };
        }
    }
    try e.res.json(e.arena, .ok, .{ .status = true, .position = dest });
}

fn listedIds(h: *Handler, arena: std.mem.Allocator, from_queue: bool) !?[]const []const u8 {
    const all_fn = h.downloads.all_fn orelse return null;
    const all = try all_fn(h.downloads.ctx, arena);
    var n: usize = 0;
    for (all) |d| {
        if (from_queue and sabnzbd_status.inQueueListing(d)) n += 1;
        if (!from_queue and sabnzbd_status.inHistoryListing(d)) n += 1;
    }
    const ids = try arena.alloc([]const u8, n);
    var i: usize = 0;
    for (all) |d| {
        const keep = if (from_queue) sabnzbd_status.inQueueListing(d) else sabnzbd_status.inHistoryListing(d);
        if (!keep) continue;
        ids[i] = d.id;
        i += 1;
    }
    if (from_queue) std.mem.sort([]const u8, ids, all, queueIdLess);
    return ids;
}

fn queueIdLess(all: []const download.Download, a: []const u8, b: []const u8) bool {
    const da = findDownload(all, a);
    const db = findDownload(all, b);
    if (da.queue_position != db.queue_position) return da.queue_position < db.queue_position;
    return std.mem.order(u8, a, b) == .lt;
}

fn findDownload(all: []const download.Download, id: []const u8) download.Download {
    for (all) |d| if (std.mem.eql(u8, d.id, id)) return d;
    return .{ .id = id, .name = "", .source_kind = .nzb, .state = .queued, .added_at = 0, .updated_at = 0 };
}

fn hasId(ids: []const []const u8, id: []const u8) bool {
    for (ids) |row| if (std.mem.eql(u8, row, id)) return true;
    return false;
}

/// Wire nzo_ids from parseNzoIDs (Download IDs). No store filter this slice.
fn nzoIDsFromParsed(arena: std.mem.Allocator, ids: []const []const u8) ![]const []const u8 {
    if (ids.len == 0) return &.{};
    const out = try arena.alloc([]const u8, ids.len);
    for (ids, out) |id, *slot| {
        slot.* = sabnzbd_status.nzoID(arena, id);
    }
    return out;
}

fn parseNonNeg(s: []const u8) i64 {
    const n = std.fmt.parseInt(i64, s, 10) catch return 0;
    return @max(n, 0);
}

fn normGigabytes(arena: std.mem.Allocator, n: i64) ![]const u8 {
    return std.fmt.allocPrint(arena, "{d:.1} G", .{@as(f64, @floatFromInt(n)) / @as(f64, 1 << 30)});
}

fn queueJson(arena: std.mem.Allocator, h: *const Handler, params: sabnzbd_params.Params) !struct {
    queue: struct {
        version: []const u8,
        paused: bool,
        pause_int: []const u8,
        paused_all: bool,
        status: []const u8,
        speedlimit: []const u8,
        speedlimit_abs: []const u8,
        speed: []const u8,
        kbpersec: []const u8,
        size: []const u8,
        sizeleft: []const u8,
        mb: []const u8,
        mbleft: []const u8,
        timeleft: []const u8,
        noofslots: i64,
        noofslots_total: i64,
        start: i64,
        limit: i64,
        finish: i64,
        diskspace1: []const u8,
        diskspace2: []const u8,
        diskspacetotal1: []const u8,
        diskspacetotal2: []const u8,
        diskspace1_norm: []const u8,
        diskspace2_norm: []const u8,
        have_warnings: []const u8,
        cache_art: []const u8,
        cache_size: []const u8,
        finishaction: ?[]const u8,
        quota: []const u8,
        have_quota: bool,
        left_quota: []const u8,
        slots: []const QueueSlot,
    },
} {
    const sample = h.speed.read();
    const disk = h.disk.space(h.paths.complete);
    const start = parseNonNeg(params.get("start"));
    const limit = parseNonNeg(params.get("limit"));
    const listed = try queuePage(arena, h, start, limit, params.get("cat"));
    const left = listed.left;
    const gb_free = try sabnzbd_config.gigabytes(arena, disk.free);
    const gb_total = try sabnzbd_config.gigabytes(arena, disk.total);
    const gb_norm = try normGigabytes(arena, disk.free);
    return .{
        .queue = .{
            .version = versionOf(h),
            .paused = sample.paused,
            .pause_int = "0",
            .paused_all = false,
            .status = sabnzbd_queue.queueStatus(sample.paused, listed.downloading),
            .speedlimit = "",
            .speedlimit_abs = "",
            .speed = try sabnzbd_format.humanSpeed(arena, sample.bytes_per_sec),
            .kbpersec = try std.fmt.allocPrint(arena, "{d:.2}", .{@as(f64, @floatFromInt(sample.bytes_per_sec)) / 1024.0}),
            .size = try sabnzbd_format.humanBytes(arena, listed.total),
            .sizeleft = try sabnzbd_format.humanBytes(arena, left),
            .mb = try sabnzbd_format.formatMB(arena, listed.total),
            .mbleft = try sabnzbd_format.formatMB(arena, left),
            .timeleft = try sabnzbd_format.formatTimeLeft(arena, sabnzbd_queue.queueTimeLeft(left, sample.bytes_per_sec)),
            .noofslots = listed.noofslots,
            .noofslots_total = listed.noofslots_total,
            .start = listed.start,
            .limit = limit,
            .finish = 0,
            .diskspace1 = gb_free,
            .diskspace2 = gb_free,
            .diskspacetotal1 = gb_total,
            .diskspacetotal2 = gb_total,
            .diskspace1_norm = gb_norm,
            .diskspace2_norm = gb_norm,
            .have_warnings = "0",
            .cache_art = "0",
            .cache_size = "0 B",
            .finishaction = null,
            .quota = "0 ",
            .have_quota = false,
            .left_quota = "0 ",
            .slots = listed.slots,
        },
    };
}

const QueuePage = struct {
    noofslots: i64,
    noofslots_total: i64,
    start: i64,
    downloading: bool,
    total: i64,
    left: i64,
    slots: []const QueueSlot,
};

/// `noofslots` is the filtered queue. `noofslots_total` is every NZB still in the queue.
fn queuePage(arena: std.mem.Allocator, h: *const Handler, start: i64, limit: i64, filter: []const u8) !QueuePage {
    const all_fn = h.downloads.all_fn orelse return .{
        .noofslots = 0,
        .noofslots_total = 0,
        .start = sabnzbd_queue.page(0, start, limit).lo,
        .downloading = false,
        .total = 0,
        .left = 0,
        .slots = empty_slots,
    };
    const all = try all_fn(h.downloads.ctx, arena);
    const queued = try queueShown(arena, all);
    const shown = try filterCats(arena, h, queued, filter);
    var total: i64 = 0;
    var left: i64 = 0;
    for (shown) |d| {
        const bytes = @max(d.total_bytes, 0);
        total += bytes;
        left += @max(bytes - @max(d.done_bytes, 0), 0);
    }
    const listing_count: i64 = @intCast(queued.len);
    const shown_count: i64 = @intCast(shown.len);
    const window = sabnzbd_queue.page(shown_count, start, limit);
    const lo: usize = @intCast(window.lo);
    const hi: usize = @intCast(window.hi);
    return .{
        .noofslots = shown_count,
        .noofslots_total = listing_count,
        .start = window.lo,
        .downloading = queueDownloading(h, shown),
        .total = total,
        .left = left,
        .slots = try queueSlotPage(arena, h, shown, lo, hi),
    };
}

fn filterCats(arena: std.mem.Allocator, h: *const Handler, rows: []const download.Download, filter: []const u8) ![]const download.Download {
    var n: usize = 0;
    for (rows) |d| if (try allowsCat(h, arena, d, filter)) {
        n += 1;
    };
    if (n == rows.len) return rows;
    if (n == 0) return &.{};
    const out = try arena.alloc(download.Download, n);
    var i: usize = 0;
    for (rows) |d| if (try allowsCat(h, arena, d, filter)) {
        out[i] = d;
        i += 1;
    };
    return out;
}

/// Empty `filter` matches every Download. `*` is the uncategorised ones.
/// A comma list that names no real Category matches nothing.
fn allowsCat(h: *const Handler, arena: std.mem.Allocator, d: download.Download, filter: []const u8) !bool {
    const raw = std.mem.trim(u8, filter, &std.ascii.whitespace);
    if (raw.len == 0) return true;
    const names = try h.cat_names.list(arena);
    var any_known = false;
    var hit = false;
    var it = std.mem.splitScalar(u8, raw, ',');
    while (it.next()) |part| {
        const token = std.mem.trim(u8, part, &std.ascii.whitespace);
        if (token.len == 0) continue;
        if (std.mem.eql(u8, token, "*")) {
            any_known = true;
            if (d.category_id.len == 0) hit = true;
            continue;
        }
        var known = false;
        for (names) |name| if (std.ascii.eqlIgnoreCase(name, token)) {
            known = true;
            break;
        };
        if (!known) continue;
        any_known = true;
        if (std.ascii.eqlIgnoreCase(h.cat_name.name(arena, d.category_id), token)) hit = true;
    }
    return any_known and hit;
}

fn queueShown(arena: std.mem.Allocator, all: []const download.Download) ![]const download.Download {
    var n: usize = 0;
    for (all) |d| {
        if (sabnzbd_status.inQueueListing(d)) n += 1;
    }
    if (n == 0) return &.{};
    const shown = try arena.alloc(download.Download, n);
    var i: usize = 0;
    for (all) |d| {
        if (!sabnzbd_status.inQueueListing(d)) continue;
        shown[i] = d;
        i += 1;
    }
    std.mem.sort(download.Download, shown, {}, queueLess);
    return shown;
}

fn queueLess(_: void, a: download.Download, b: download.Download) bool {
    if (a.queue_position != b.queue_position) return a.queue_position < b.queue_position;
    return std.mem.order(u8, a.id, b.id) == .lt;
}

fn queueDownloading(h: *const Handler, shown: []const download.Download) bool {
    for (shown) |d| {
        if (std.mem.eql(u8, sabnzbd_status.sabStatus(d, h.feed.stage(d.id)), "Downloading")) return true;
    }
    return false;
}

fn queueSlotPage(arena: std.mem.Allocator, h: *const Handler, shown: []const download.Download, lo: usize, hi: usize) ![]const QueueSlot {
    if (hi <= lo) return empty_slots;
    const page = shown[lo..hi];
    const slots = try arena.alloc(QueueSlot, page.len);
    for (page, 0..) |d, j| {
        const total = @max(d.total_bytes, 0);
        const done = @min(@max(d.done_bytes, 0), total);
        const left = total - done;
        slots[j] = .{
            .status = sabnzbd_status.sabStatus(d, h.feed.stage(d.id)),
            .index = @intCast(lo + j),
            .nzo_id = sabnzbd_queue.queueSlotNzoID(arena, d),
            .filename = sabnzbd_queue.queueSlotFilename(d),
            .cat = h.cat_name.name(arena, d.category_id),
            .priority = sabnzbd_queue.queueSlotPriority(d),
            .script = "None",
            .mb = try sabnzbd_format.formatMB(arena, total),
            .mbleft = try sabnzbd_format.formatMB(arena, left),
            .mbmissing = "0.0",
            .size = try sabnzbd_format.humanBytes(arena, total),
            .sizeleft = try sabnzbd_format.humanBytes(arena, left),
            .percentage = try sabnzbd_format.percentage(arena, done, total),
            .timeleft = try sabnzbd_format.formatTimeLeft(arena, sabnzbd_queue.queueTimeLeft(left, h.feed.rate(d.id))),
            .avg_age = try sabnzbd_format.formatAge(arena, 0),
            .password = "",
            .unpackopts = "3",
            .has_rating = false,
            .labels = empty_labels,
        };
    }
    return slots;
}

fn fullStatusJson(arena: std.mem.Allocator, h: *const Handler) !struct {
    status: struct {
        version: []const u8,
        completedir: []const u8,
        downloaddir: []const u8,
        paused: bool,
        pause_int: []const u8,
        have_warnings: []const u8,
        warnings: []const []const u8,
        diskspace1: []const u8,
        diskspace2: []const u8,
    },
} {
    const paused = h.speed.read().paused;
    const gb = try sabnzbd_config.gigabytes(arena, h.disk.space(h.paths.complete).free);
    return .{
        .status = .{
            .version = versionOf(h),
            .completedir = h.paths.complete,
            .downloaddir = h.paths.incomplete,
            .paused = paused,
            .pause_int = "0",
            .have_warnings = "0",
            .warnings = empty_names,
            .diskspace1 = gb,
            .diskspace2 = gb,
        },
    };
}

const empty_names: []const []const u8 = &.{};

const ConfigStar = struct {
    name: []const u8 = "*",
    order: i32 = 0,
    pp: []const u8 = "3",
    script: []const u8 = "None",
    dir: []const u8 = "",
    priority: i32 = -100,
};

const ConfigEmpty = struct {};

fn configJson(arena: std.mem.Allocator, h: *const Handler) !struct {
    config: struct {
        misc: struct {
            complete_dir: []const u8,
            download_dir: []const u8,
            enable_tv_sorting: bool,
            enable_movie_sorting: bool,
            enable_date_sorting: bool,
            pre_check: bool,
            tv_categories: []const []const u8,
            movie_categories: []const []const u8,
            date_categories: []const []const u8,
            history_retention_option: []const u8,
            history_retention: []const u8,
            history_retention_number: i32,
        },
        categories: []const ConfigStar,
        servers: []const ConfigEmpty,
        sorters: []const ConfigEmpty,
    },
} {
    const empty_objs: []const ConfigEmpty = &.{};
    const names = try h.cat_names.list(arena);
    const cats = try arena.alloc(ConfigStar, names.len + 1);
    cats[0] = .{};
    for (names, 0..) |name, i| {
        cats[i + 1] = .{ .name = name, .dir = name, .order = @intCast(i + 1) };
    }
    return .{
        .config = .{
            .misc = .{
                .complete_dir = h.paths.complete,
                .download_dir = h.paths.incomplete,
                .enable_tv_sorting = false,
                .enable_movie_sorting = false,
                .enable_date_sorting = false,
                .pre_check = false,
                .tv_categories = empty_names,
                .movie_categories = empty_names,
                .date_categories = empty_names,
                .history_retention_option = "all",
                .history_retention = "",
                .history_retention_number = 0,
            },
            .categories = cats,
            .servers = empty_objs,
            .sorters = empty_objs,
        },
    };
}

/// One request after the body has been read. `file` is the first addfile file
/// part; its bytes live in the request arena until the handler returns.
const Call = struct {
    params: sabnzbd_params.Params,
    file: ?StagedNZB = null,
};

const StagedNZB = struct {
    bytes: []const u8,
    file_name: []const u8,
};

/// Query plus urlencoded body, or a multipart body. Null when the handler
/// already answered (body unusable, or auth failed before an addfile part).
fn readCall(h: *Handler, e: *ex.Exchange) !?Call {
    if (e.header("Content-Type")) |ct| {
        if (mime.parse(e.arena, ct)) |media| {
            if (std.mem.eql(u8, media.type, "multipart/form-data")) return readMultipart(h, e, media);
        } else |err| switch (err) {
            error.InvalidMediaType => {},
            else => return err,
        }
    }
    const form = try urlencodedForm(e) orelse return null;
    return .{ .params = try sabnzbd_params.parse(e.arena, e.query, form) };
}

/// Streams a multipart body. Text parts are percent-encoded into a form and
/// merged with `sabnzbd_params.parse`, so the query wins and the first text
/// value of a name wins. The first file part is the NZB only when mode is
/// already addfile (query or an earlier text part). Auth runs before those
/// bytes are read: an apikey that appears only after the file is too late.
/// Further file parts, and file parts of other modes, are skipped.
fn readMultipart(h: *Handler, e: *ex.Exchange, media: mime.Media) !?Call {
    const boundary = media.get("boundary") orelse {
        try sabFail(e, "Invalid multipart request");
        return null;
    };
    const buf = try e.arena.alloc(u8, 16 << 10);
    const src = e.bodyReader(buf) catch |err| switch (err) {
        error.PayloadTooLarge => {
            try sabFail(e, "Request entity too large");
            return null;
        },
        error.ReadFailed, error.BodyTaken => {
            try sabFail(e, "body failed");
            return null;
        },
        error.OutOfMemory => return error.OutOfMemory,
    };
    const reader = multipart.Reader.init(e.arena, src, boundary, e.body_limit) catch |err| switch (err) {
        error.Malformed => {
            try sabFail(e, "Invalid multipart request");
            return null;
        },
        error.PayloadTooLarge => {
            try sabFail(e, "Request entity too large");
            return null;
        },
        error.OutOfMemory => return error.OutOfMemory,
    };

    var form: std.ArrayList(u8) = .empty;
    var file: ?StagedNZB = null;
    var n: usize = 0;
    while (true) : (n += 1) {
        const maybe = reader.next(e.arena) catch |err| return multipartFailed(e, err);
        const part = maybe orelse break;
        if (n >= max_form_parts) {
            try sabFail(e, "Too many form fields");
            return null;
        }
        if (part.file_name.len != 0) {
            if (file == null) {
                const now = try sabnzbd_params.parse(e.arena, e.query, form.items);
                if (std.ascii.eqlIgnoreCase(now.get("mode"), "addfile")) {
                    // Do not read the part on failure: a later apikey must not count.
                    if (!try authed(h, e, now)) return null;
                    const bytes = reader.readUpTo(e.arena, @intCast(e.body_limit)) catch |err| return multipartFailed(e, err);
                    if (@as(u64, bytes.len) > e.body_limit) {
                        try sabFail(e, "Request entity too large");
                        return null;
                    }
                    file = .{ .bytes = bytes, .file_name = part.file_name };
                }
            }
            continue;
        }
        const raw = reader.readUpTo(e.arena, max_field_bytes) catch |err| return multipartFailed(e, err);
        if (raw.len > max_field_bytes) {
            try sabFail(e, "Form field too long");
            return null;
        }
        if (part.name.len == 0) continue;
        try appendFormPair(e.arena, &form, part.name, raw);
    }
    return .{
        .params = try sabnzbd_params.parse(e.arena, e.query, form.items),
        .file = file,
    };
}

fn multipartFailed(e: *ex.Exchange, err: multipart.Error) !?Call {
    switch (err) {
        error.PayloadTooLarge => try sabFail(e, "Request entity too large"),
        error.Malformed => try sabFail(e, "Invalid request body"),
        error.OutOfMemory => return error.OutOfMemory,
    }
    return null;
}

/// Percent-encodes so `sabnzbd_params.parse` round-trips `&`, `+` and `%`.
fn appendFormPair(arena: std.mem.Allocator, form: *std.ArrayList(u8), name: []const u8, value: []const u8) !void {
    if (form.items.len > 0) try form.append(arena, '&');
    try appendPct(arena, form, name);
    try form.append(arena, '=');
    try appendPct(arena, form, value);
}

fn appendPct(arena: std.mem.Allocator, out: *std.ArrayList(u8), s: []const u8) !void {
    const hex = "0123456789ABCDEF";
    for (s) |c| {
        if (std.ascii.isAlphanumeric(c) or c == '-' or c == '.' or c == '_' or c == '~') {
            try out.append(arena, c);
        } else {
            try out.appendSlice(arena, &.{ '%', hex[c >> 4], hex[c & 0xf] });
        }
    }
}

/// Checks apikey once the caller asks. On failure the SAB JSON is already written.
fn authed(h: *Handler, e: *ex.Exchange, params: sabnzbd_params.Params) !bool {
    const apikey = params.get("apikey");
    if (apikey.len == 0) {
        try sabFail(e, "API Key Required");
        return false;
    }
    if (!h.keys.ok(apikey)) {
        try sabFail(e, "API Key Incorrect");
        return false;
    }
    return true;
}

fn answerAddFile(h: *Handler, e: *ex.Exchange, params: sabnzbd_params.Params, file: ?StagedNZB) !void {
    const staged = file orelse {
        if (sabnzbd_add.addFileError(false)) |msg| try sabFail(e, msg);
        return;
    };
    const add_file = h.adder.add_file_fn orelse {
        try sabFail(e, "not implemented");
        return;
    };
    // SAB answers app refusals as HTTP 200 `{status:false,error}` (Go appFail).
    // Only `error.Rejected` maps; other errors bubble.
    var failure: app_error.Failure = .{};
    if (add_file(h.adder.ctx, .{
        .bytes = staged.bytes,
        .file_name = staged.file_name,
        .nzbname = params.get("nzbname"),
        .cat = params.get("cat"),
        .priority = params.get("priority"),
        .password = params.get("password"),
    }, e.arena, &failure)) |dl| {
        const nzo_ids = try e.arena.alloc([]const u8, 1);
        nzo_ids[0] = sabnzbd_status.nzoID(e.arena, dl.id);
        try e.res.json(e.arena, .ok, .{ .status = true, .nzo_ids = nzo_ids });
    } else |err| switch (err) {
        error.Rejected => {
            const msg = if (failure.message.len == 0) "Internal error" else failure.message;
            try sabFail(e, msg);
        },
        else => return err,
    }
}

fn sabFail(e: *ex.Exchange, msg: []const u8) !void {
    try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = msg });
}

/// Urlencoded body bytes, or "" when the type is not form-urlencoded (multipart
/// is read by `readMultipart` instead). Null when the handler already answered
/// a body error.
fn urlencodedForm(e: *ex.Exchange) !?[]const u8 {
    if (!isUrlencoded(e.header("Content-Type"))) return @as([]const u8, "");
    return e.bodyAlloc() catch |err| switch (err) {
        error.PayloadTooLarge => {
            try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = "Request entity too large" });
            return null;
        },
        error.ReadFailed, error.BodyTaken => {
            try e.res.json(e.arena, .ok, .{ .status = false, .@"error" = "body failed" });
            return null;
        },
        else => return err,
    };
}

/// Media type only; parameters after ';' are ignored. Match is case-insensitive.
fn isUrlencoded(header: ?[]const u8) bool {
    const h = header orelse return false;
    const end = std.mem.indexOfScalar(u8, h, ';') orelse h.len;
    return std.ascii.eqlIgnoreCase(std.mem.trim(u8, h[0..end], &std.ascii.whitespace), "application/x-www-form-urlencoded");
}

// ---- tests: version through loopback (ports auth_test.go) ----

const testing = std.testing;
const Io = std.Io;
const ts = @import("add_testserver.zig");

const TestKeys = struct {
    fn check(_: *anyopaque, presented: []const u8) bool {
        return std.mem.eql(u8, presented, ts.test_key);
    }
    var dummy: u8 = 0;
    const checker: http_common.KeyChecker = .{ .ctx = &dummy, .check = check };
};

var test_handler: Handler = .{ .keys = TestKeys.checker };

fn testSetup(routes: *ex.Routes) anyerror!void {
    try register(routes, &test_handler);
}

const Reply = struct {
    status: std.http.Status,
    content_type: ?[]const u8,
    body: []const u8,
};

const Env = struct {
    server: ts.Server,
    client: std.http.Client,

    fn open(env: *Env) !void {
        return env.openWith(testSetup);
    }

    fn openWith(env: *Env, setup: ts.Setup) !void {
        try env.server.start(setup);
        env.client = .{ .allocator = testing.allocator, .io = testing.io };
    }

    fn close(env: *Env) void {
        env.client.deinit();
        env.server.stop();
    }

    fn a(_: *Env) std.mem.Allocator {
        return testing.allocator;
    }

    fn get(env: *Env, path: []const u8) !Reply {
        const url = try env.server.url(env.a(), path);
        defer env.a().free(url);
        const uri = try std.Uri.parse(url);
        var req = try env.client.request(.GET, uri, .{});
        defer req.deinit();
        try req.sendBodiless();
        var redirect_buffer: [8 * 1024]u8 = undefined;
        var response = try req.receiveHead(&redirect_buffer);
        // readerDecompressing invalidates head strings; copy while they live.
        const ct_src = response.head.content_type;
        var out: Io.Writer.Allocating = .init(env.a());
        errdefer out.deinit();
        var transfer_buffer: [64]u8 = undefined;
        var decompress: std.http.Decompress = undefined;
        const reader = response.readerDecompressing(&transfer_buffer, &decompress, &.{});
        _ = try reader.streamRemaining(&out.writer);
        const ct = if (ct_src) |c| try env.a().dupe(u8, c) else null;
        errdefer if (ct) |c| env.a().free(c);
        return .{
            .status = response.head.status,
            .content_type = ct,
            .body = try out.toOwnedSlice(),
        };
    }

    fn post(env: *Env, path: []const u8, content_type: []const u8, body: []const u8) !Reply {
        const url = try env.server.url(env.a(), path);
        defer env.a().free(url);
        var out: Io.Writer.Allocating = .init(env.a());
        errdefer out.deinit();
        const res = try env.client.fetch(.{
            .location = .{ .url = url },
            .method = .POST,
            .payload = body,
            .headers = .{
                .content_type = .{ .override = content_type },
            },
            .response_writer = &out.writer,
        });
        return .{
            .status = res.status,
            .content_type = null,
            .body = try out.toOwnedSlice(),
        };
    }
};

fn expectVersion(env: *Env, path: []const u8) !void {
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const ct = r.content_type orelse return error.MissingContentType;
    try testing.expect(std.mem.indexOf(u8, ct, "application/json") != null);
    const Shape = struct { version: []const u8 };
    const parsed = try std.json.parseFromSliceLeaky(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    try testing.expectEqualStrings(default_version, parsed.version);
}

test "GET mode=version without apikey returns SAB version JSON" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    try expectVersion(&env, "/sabnzbd/api?mode=version");
    try expectVersion(&env, "/sabnzbd/api/?mode=VERSION");
}

test "GET mode=queue without apikey returns API Key Required" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const r = try env.get("/sabnzbd/api?mode=queue");
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSliceLeaky(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    try testing.expect(!parsed.status);
    try testing.expectEqualStrings("API Key Required", parsed.@"error");
}

test "GET mode=queue with wrong apikey returns API Key Incorrect" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const r = try env.get("/sabnzbd/api?mode=queue&apikey=nope");
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSliceLeaky(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    try testing.expect(!parsed.status);
    try testing.expectEqualStrings("API Key Incorrect", parsed.@"error");
}

test "GET mode=queue with valid apikey returns idle empty queue JSON" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=queue&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        queue: struct {
            status: []const u8,
            noofslots: i64,
            slots: []const struct {},
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqualStrings("Idle", parsed.value.queue.status);
    try testing.expectEqual(@as(i64, 0), parsed.value.queue.noofslots);
    try testing.expectEqual(@as(usize, 0), parsed.value.queue.slots.len);
}

test "GET mode=queue with valid apikey lists queued download from handler" {
    const FakeListing = struct {
        fn all(_: ?*anyopaque, arena: std.mem.Allocator) ![]download.Download {
            const out = try arena.alloc(download.Download, 1);
            out[0] = .{
                .id = "abc",
                .name = "Show",
                .source_kind = .nzb,
                .state = .queued,
                .added_at = 1,
                .updated_at = 1,
            };
            return out;
        }
        var dummy: u8 = 0;
    };
    const saved_downloads = test_handler.downloads;
    defer test_handler.downloads = saved_downloads;
    test_handler.downloads = .{
        .ctx = &FakeListing.dummy,
        .all_fn = FakeListing.all,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=queue&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        queue: struct {
            noofslots: i64,
            slots: []const struct {
                nzo_id: []const u8,
                filename: []const u8,
            },
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqual(@as(i64, 1), parsed.value.queue.noofslots);
    try testing.expectEqual(@as(usize, 1), parsed.value.queue.slots.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.queue.slots[0].nzo_id);
    try testing.expectEqualStrings("Show", parsed.value.queue.slots[0].filename);
}

test "GET mode=queue cat=tv keeps that category and reports the full queue size" {
    const Fake = struct {
        fn all(_: ?*anyopaque, arena: std.mem.Allocator) ![]download.Download {
            const out = try arena.alloc(download.Download, 2);
            out[0] = .{ .id = "aaa", .name = "Other", .source_kind = .nzb, .state = .queued, .category_id = "c2", .added_at = 1, .updated_at = 1 };
            out[1] = .{ .id = "bbb", .name = "Show", .source_kind = .nzb, .state = .queued, .category_id = "c1", .added_at = 1, .updated_at = 1 };
            return out;
        }
        fn cats(_: ?*anyopaque, arena: std.mem.Allocator) ![]const []const u8 {
            const out = try arena.alloc([]const u8, 2);
            out[0] = "tv";
            out[1] = "movies";
            return out;
        }
        fn name(_: ?*anyopaque, _: std.mem.Allocator, id: []const u8) []const u8 {
            if (std.mem.eql(u8, id, "c1")) return "tv";
            if (std.mem.eql(u8, id, "c2")) return "movies";
            return "";
        }
        var dummy: u8 = 0;
    };
    const saved_downloads = test_handler.downloads;
    const saved_names = test_handler.cat_names;
    const saved_name = test_handler.cat_name;
    defer test_handler.downloads = saved_downloads;
    defer test_handler.cat_names = saved_names;
    defer test_handler.cat_name = saved_name;
    test_handler.downloads = .{ .ctx = &Fake.dummy, .all_fn = Fake.all };
    test_handler.cat_names = .{ .ctx = &Fake.dummy, .list_fn = Fake.cats };
    test_handler.cat_name = .{ .ctx = &Fake.dummy, .name_fn = Fake.name };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=queue&apikey={s}&cat=TV", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        queue: struct {
            noofslots: i64,
            noofslots_total: i64,
            slots: []const struct { filename: []const u8, cat: []const u8 },
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqual(@as(i64, 1), parsed.value.queue.noofslots);
    try testing.expectEqual(@as(i64, 2), parsed.value.queue.noofslots_total);
    try testing.expectEqualStrings("Show", parsed.value.queue.slots[0].filename);
    try testing.expectEqualStrings("tv", parsed.value.queue.slots[0].cat);
}

test "queue slot timeleft and history status follow the live feed" {
    const Fake = struct {
        fn all(_: ?*anyopaque, arena: std.mem.Allocator) ![]download.Download {
            const out = try arena.alloc(download.Download, 2);
            out[0] = .{
                .id = "abc",
                .name = "Show",
                .source_kind = .nzb,
                .state = .fetching,
                .total_bytes = 2048,
                .done_bytes = 0,
                .added_at = 1,
                .updated_at = 1,
            };
            out[1] = .{
                .id = "def",
                .name = "Done",
                .source_kind = .nzb,
                .state = .post_processing,
                .added_at = 1,
                .updated_at = 1,
            };
            return out;
        }
        fn rate(_: ?*anyopaque, id: []const u8) i64 {
            return if (std.mem.eql(u8, id, "abc")) 1024 else 0;
        }
        fn stage(_: ?*anyopaque, id: []const u8) ?event.Stage {
            if (std.mem.eql(u8, id, "def")) return .repairing;
            return null;
        }
        var dummy: u8 = 0;
    };
    const saved_downloads = test_handler.downloads;
    const saved_feed = test_handler.feed;
    defer test_handler.downloads = saved_downloads;
    defer test_handler.feed = saved_feed;
    test_handler.downloads = .{ .ctx = &Fake.dummy, .all_fn = Fake.all };
    test_handler.feed = .{ .ctx = &Fake.dummy, .rate_fn = Fake.rate, .stage_fn = Fake.stage };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const queue_path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=queue&apikey={s}", .{ts.test_key});
    defer env.a().free(queue_path);
    const queue = try env.get(queue_path);
    defer env.a().free(queue.body);
    defer if (queue.content_type) |ct| env.a().free(ct);
    const Q = struct {
        queue: struct { slots: []const struct { filename: []const u8, timeleft: []const u8 } },
    };
    const q = try std.json.parseFromSlice(Q, env.a(), queue.body, .{ .ignore_unknown_fields = true });
    defer q.deinit();
    try testing.expectEqual(@as(usize, 1), q.value.queue.slots.len);
    try testing.expectEqualStrings("0:00:02", q.value.queue.slots[0].timeleft);

    const hist_path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=history&apikey={s}", .{ts.test_key});
    defer env.a().free(hist_path);
    const hist = try env.get(hist_path);
    defer env.a().free(hist.body);
    defer if (hist.content_type) |ct| env.a().free(ct);
    const H = struct {
        history: struct { slots: []const struct { name: []const u8, status: []const u8 } },
    };
    const h = try std.json.parseFromSlice(H, env.a(), hist.body, .{ .ignore_unknown_fields = true });
    defer h.deinit();
    try testing.expectEqualStrings("Done", h.value.history.slots[0].name);
    try testing.expectEqualStrings("Repairing", h.value.history.slots[0].status);
}

test "GET mode=history with valid apikey returns empty history JSON" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=history&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        history: struct {
            noofslots: i64,
            slots: []const struct {},
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqual(@as(i64, 0), parsed.value.history.noofslots);
    try testing.expectEqual(@as(usize, 0), parsed.value.history.slots.len);
}

test "GET mode=history with valid apikey lists ready download from handler" {
    const FakeListing = struct {
        fn all(_: ?*anyopaque, arena: std.mem.Allocator) ![]download.Download {
            const out = try arena.alloc(download.Download, 1);
            out[0] = .{
                .id = "abc",
                .name = "Show",
                .source_kind = .nzb,
                .state = .ready,
                .added_at = 1,
                .updated_at = 1,
            };
            return out;
        }
        var dummy: u8 = 0;
    };
    const saved_downloads = test_handler.downloads;
    defer test_handler.downloads = saved_downloads;
    test_handler.downloads = .{
        .ctx = &FakeListing.dummy,
        .all_fn = FakeListing.all,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=history&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        history: struct {
            noofslots: i64,
            slots: []const struct {
                nzo_id: []const u8,
                name: []const u8,
                status: []const u8,
            },
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqual(@as(i64, 1), parsed.value.history.noofslots);
    try testing.expectEqual(@as(usize, 1), parsed.value.history.slots.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.history.slots[0].nzo_id);
    try testing.expectEqualStrings("Show", parsed.value.history.slots[0].name);
    try testing.expectEqualStrings("Completed", parsed.value.history.slots[0].status);
}

test "GET mode=retry with valid apikey returns retry is not supported" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=retry&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSliceLeaky(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    try testing.expect(!parsed.status);
    try testing.expectEqualStrings("retry is not supported", parsed.@"error");
}

test "GET mode=addurl with valid apikey and no name returns expect one parameter" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=addurl&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(!parsed.value.status);
    try testing.expectEqualStrings("expect one parameter", parsed.value.@"error");
}

test "GET mode=addfile with valid apikey and no multipart returns No file given" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=addfile&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(!parsed.value.status);
    try testing.expectEqualStrings("No file given", parsed.value.@"error");
}

test "POST mode=queue with apikey in form body is not API Key Required" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const body = try std.fmt.allocPrint(env.a(), "mode=queue&apikey={s}", .{ts.test_key});
    defer env.a().free(body);
    const r = try env.post("/sabnzbd/api", "application/x-www-form-urlencoded", body);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        queue: struct {
            status: []const u8,
            noofslots: i64,
            slots: []const struct {},
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqualStrings("Idle", parsed.value.queue.status);
    try testing.expectEqual(@as(i64, 0), parsed.value.queue.noofslots);
    try testing.expectEqual(@as(usize, 0), parsed.value.queue.slots.len);
}

test "GET mode=get_cats with valid apikey returns default categories JSON" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=get_cats&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { categories: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqual(@as(usize, 1), parsed.value.categories.len);
    try testing.expectEqualStrings("*", parsed.value.categories[0]);
}

test "GET mode=get_config with valid apikey returns default config JSON" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=get_config&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        config: struct {
            categories: []const struct { name: []const u8 },
            misc: struct {
                enable_tv_sorting: bool,
                history_retention_option: []const u8,
            },
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqual(@as(usize, 1), parsed.value.config.categories.len);
    try testing.expectEqualStrings("*", parsed.value.config.categories[0].name);
    try testing.expect(!parsed.value.config.misc.enable_tv_sorting);
    try testing.expectEqualStrings("all", parsed.value.config.misc.history_retention_option);
}

test "GET mode=fullstatus with valid apikey returns default full status JSON" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(env.a(), "/sabnzbd/api?mode=fullstatus&apikey={s}", .{ts.test_key});
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        status: struct {
            version: []const u8,
            have_warnings: []const u8,
            paused: bool,
        },
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expectEqualStrings(default_version, parsed.value.status.version);
    try testing.expectEqualStrings("0", parsed.value.status.have_warnings);
    try testing.expect(!parsed.value.status.paused);
}

test "GET mode=queue name=delete empty value returns status true and empty nzo_ids" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=queue&name=delete&apikey={s}&value=",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, nzo_ids: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 0), parsed.value.nzo_ids.len);
}

test "GET mode=queue name=pause with valid apikey and no value returns status true and empty nzo_ids" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=queue&name=pause&apikey={s}",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, nzo_ids: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 0), parsed.value.nzo_ids.len);
}

test "GET mode=queue name=pause with valid apikey and nzo_id calls pause_fn with download id" {
    const FakePause = struct {
        called: bool = false,
        fn pause(ctx: ?*anyopaque, _: std.mem.Allocator, id: []const u8) !void {
            const self: *@This() = @ptrCast(@alignCast(ctx.?));
            if (std.mem.eql(u8, id, "abc")) self.called = true;
        }
        var state: @This() = .{};
    };
    const saved_downloads = test_handler.downloads;
    defer test_handler.downloads = saved_downloads;
    FakePause.state = .{};
    test_handler.downloads = .{
        .ctx = &FakePause.state,
        .pause_fn = FakePause.pause,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=queue&name=pause&apikey={s}&value=SABnzbd_nzo_abc",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, nzo_ids: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 1), parsed.value.nzo_ids.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.nzo_ids[0]);
    try testing.expect(FakePause.state.called);
}

test "GET mode=queue name=resume with valid apikey and nzo_id calls resume_fn with download id" {
    const FakeResume = struct {
        called: bool = false,
        fn callback(ctx: ?*anyopaque, _: std.mem.Allocator, id: []const u8) !void {
            const self: *@This() = @ptrCast(@alignCast(ctx.?));
            if (std.mem.eql(u8, id, "abc")) self.called = true;
        }
        var state: @This() = .{};
    };
    const saved_downloads = test_handler.downloads;
    defer test_handler.downloads = saved_downloads;
    FakeResume.state = .{};
    test_handler.downloads = .{
        .ctx = &FakeResume.state,
        .resume_fn = FakeResume.callback,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=queue&name=resume&apikey={s}&value=SABnzbd_nzo_abc",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, nzo_ids: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 1), parsed.value.nzo_ids.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.nzo_ids[0]);
    try testing.expect(FakeResume.state.called);
}

test "GET mode=queue name=pause when pause rejects returns HTTP 200 and status false" {
    const FakePause = struct {
        fn pause(_: ?*anyopaque, _: std.mem.Allocator, _: []const u8) !void {
            return error.Rejected;
        }
        var dummy: u8 = 0;
    };
    const saved_downloads = test_handler.downloads;
    defer test_handler.downloads = saved_downloads;
    test_handler.downloads = .{
        .ctx = &FakePause.dummy,
        .pause_fn = FakePause.pause,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=queue&name=pause&apikey={s}&value=SABnzbd_nzo_abc",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": ?[]const u8 = null };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(!parsed.value.status);
    try testing.expect(parsed.value.@"error".?.len > 0);
}

test "GET mode=queue name=priority with valid apikey and no value returns expect one parameter" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=queue&name=priority&apikey={s}",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(!parsed.value.status);
    try testing.expectEqualStrings("expect one parameter", parsed.value.@"error");
}

test "GET mode=history name=delete unknown nzo returns status true" {
    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=history&name=delete&apikey={s}&value=SABnzbd_nzo_unknown",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
}

test "GET mode=addurl with valid apikey and name returns status true and nzo_ids" {
    const FakeAdder = struct {
        fn addUrl(_: ?*anyopaque, req: AddUrlReq, _: std.mem.Allocator, _: *app_error.Failure) !download.Download {
            if (std.mem.indexOf(u8, req.url, "http://x") == null) return error.UnexpectedUrl;
            return .{
                .id = "abc",
                .name = "n",
                .source_kind = .nzb,
                .state = .queued,
                .added_at = 1,
                .updated_at = 1,
            };
        }
        var dummy: u8 = 0;
    };
    const saved_adder = test_handler.adder;
    defer test_handler.adder = saved_adder;
    test_handler.adder = .{
        .ctx = &FakeAdder.dummy,
        .add_url_fn = FakeAdder.addUrl,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=addurl&apikey={s}&name=http://x",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, nzo_ids: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 1), parsed.value.nzo_ids.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.nzo_ids[0]);
}

test "GET mode=addurl when add rejects returns status false and error message" {
    const FakeAdder = struct {
        fn addUrl(_: ?*anyopaque, _: AddUrlReq, _: std.mem.Allocator, fail: *app_error.Failure) !download.Download {
            return fail.setLiteral(.invalid_input, "not_an_nzb", "not an nzb");
        }
        var dummy: u8 = 0;
    };
    const saved_adder = test_handler.adder;
    defer test_handler.adder = saved_adder;
    test_handler.adder = .{
        .ctx = &FakeAdder.dummy,
        .add_url_fn = FakeAdder.addUrl,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=addurl&apikey={s}&name=http://x",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, @"error": []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(!parsed.value.status);
    try testing.expectEqualStrings("not an nzb", parsed.value.@"error");
}

test "GET mode=addurl passes nzbname cat priority password to adder" {
    const FakeAdder = struct {
        fn addUrl(_: ?*anyopaque, req: AddUrlReq, _: std.mem.Allocator, _: *app_error.Failure) !download.Download {
            if (std.mem.indexOf(u8, req.url, "http://x") == null) return error.UnexpectedUrl;
            if (!std.mem.eql(u8, req.nzbname, "From.Url")) return error.UnexpectedUrl;
            if (!std.mem.eql(u8, req.cat, "tv")) return error.UnexpectedUrl;
            if (!std.mem.eql(u8, req.priority, "2")) return error.UnexpectedUrl;
            if (!std.mem.eql(u8, req.password, "pw")) return error.UnexpectedUrl;
            return .{
                .id = "abc",
                .name = "n",
                .source_kind = .nzb,
                .state = .queued,
                .added_at = 1,
                .updated_at = 1,
            };
        }
        var dummy: u8 = 0;
    };
    const saved_adder = test_handler.adder;
    defer test_handler.adder = saved_adder;
    test_handler.adder = .{
        .ctx = &FakeAdder.dummy,
        .add_url_fn = FakeAdder.addUrl,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    const path = try std.fmt.allocPrint(
        env.a(),
        "/sabnzbd/api?mode=addurl&apikey={s}&name=http://x&nzbname=From.Url&cat=tv&priority=2&password=pw",
        .{ts.test_key},
    );
    defer env.a().free(path);
    const r = try env.get(path);
    defer env.a().free(r.body);
    defer if (r.content_type) |ct| env.a().free(ct);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct { status: bool, nzo_ids: []const []const u8 };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 1), parsed.value.nzo_ids.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.nzo_ids[0]);
}

test "POST mode=addfile multipart nzbfile returns status true and nzo_ids" {
    const FakeAdder = struct {
        fn addFile(_: ?*anyopaque, req: AddFileReq, _: std.mem.Allocator, _: *app_error.Failure) !download.Download {
            if (std.mem.indexOf(u8, req.file_name, "a.nzb") == null) return error.UnexpectedFile;
            return .{
                .id = "abc",
                .name = "n",
                .source_kind = .nzb,
                .state = .queued,
                .added_at = 1,
                .updated_at = 1,
            };
        }
        var dummy: u8 = 0;
    };
    const saved_adder = test_handler.adder;
    defer test_handler.adder = saved_adder;
    test_handler.adder = .{
        .ctx = &FakeAdder.dummy,
        .add_file_fn = FakeAdder.addFile,
    };

    var env: Env = undefined;
    try env.open();
    defer env.close();
    // A Zig multiline string does not turn `\r\n` into CRLF, so the body is a normal string.
    const body = try std.fmt.allocPrint(env.a(), "------x\r\n" ++
        "Content-Disposition: form-data; name=\"mode\"\r\n" ++
        "\r\n" ++
        "addfile\r\n" ++
        "------x\r\n" ++
        "Content-Disposition: form-data; name=\"apikey\"\r\n" ++
        "\r\n" ++
        "{s}\r\n" ++
        "------x\r\n" ++
        "Content-Disposition: form-data; name=\"nzbfile\"; filename=\"a.nzb\"\r\n" ++
        "Content-Type: application/octet-stream\r\n" ++
        "\r\n" ++
        "ok\r\n" ++
        "------x--\r\n", .{ts.test_key});
    defer env.a().free(body);
    const r = try env.post("/sabnzbd/api", "multipart/form-data; boundary=----x", body);
    defer env.a().free(r.body);
    try testing.expectEqual(std.http.Status.ok, r.status);
    const Shape = struct {
        status: bool,
        @"error": ?[]const u8 = null,
        nzo_ids: []const []const u8 = &.{},
    };
    const parsed = try std.json.parseFromSlice(Shape, env.a(), r.body, .{ .ignore_unknown_fields = true });
    defer parsed.deinit();
    try testing.expect(parsed.value.status);
    try testing.expectEqual(@as(usize, 1), parsed.value.nzo_ids.len);
    try testing.expectEqualStrings("SABnzbd_nzo_abc", parsed.value.nzo_ids[0]);
}
