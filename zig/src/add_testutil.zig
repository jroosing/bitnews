//! A rig for the tests of the add group: a temp database with the real
//! migrations, a temp incomplete root, the real source and category stores, a
//! fake fetcher and a recorder of the events. Only the tests import this file.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const testing = std.testing;
const app_error = @import("app_error.zig");
const dl = @import("download.zig");
const event = @import("event.zig");
const log = @import("log.zig");
const keyfile = @import("keyfile.zig");
const categories_repo = @import("categories_repo.zig");
const categories_service = @import("categories_service.zig");
const downloads_service = @import("downloads_service.zig");
const tu = @import("downloads_testutil.zig");
const fetch = @import("add_fetch.zig");
const service = @import("add_service.zig");
const source_repo = @import("add_source_repo.zig");

/// The base time of the tests (the downloads tests' epoch), in unix milliseconds.
pub const epoch_ms: i64 = tu.epoch;

/// A valid NZB with one file of 10 bytes.
pub const tiny_nzb = "<nzb><file subject=\"&quot;a.rar&quot;\"><segments><segment bytes=\"10\" number=\"1\">a@b</segment></segments></file></nzb>";

/// An NZB with the given head metadata (name, password, category; "" leaves one
/// out) and two files of 4000 and 1000 bytes.
pub fn nzbWith(a: Allocator, name: []const u8, password: []const u8, category: []const u8) ![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    try out.appendSlice(a, "<?xml version=\"1.0\"?><nzb xmlns=\"http://www.newzbin.com/DTD/2003/nzb\"><head>");
    if (name.len > 0) try out.print(a, "<meta type=\"name\">{s}</meta>", .{name});
    if (password.len > 0) try out.print(a, "<meta type=\"password\">{s}</meta>", .{password});
    if (category.len > 0) try out.print(a, "<meta type=\"category\">{s}</meta>", .{category});
    try out.appendSlice(a, "</head>" ++
        "<file poster=\"p\" date=\"1\" subject=\"&quot;first.bin&quot; yEnc (1/2)\"><groups><group>alt.binaries.x</group></groups><segments>" ++
        "<segment bytes=\"2000\" number=\"1\">a1@x</segment><segment bytes=\"2000\" number=\"2\">a2@x</segment></segments></file>" ++
        "<file poster=\"p\" date=\"1\" subject=\"&quot;second.bin&quot; yEnc (1/1)\"><groups><group>alt.binaries.x</group></groups><segments>" ++
        "<segment bytes=\"1000\" number=\"1\">b1@x</segment></segments></file></nzb>");
    return out.items;
}

/// What the fake fetcher answers (`fetch.Outcome` with borrowed bytes).
pub const Answer = union(enum) {
    body: []const u8,
    magnet: []const u8,
    status: u16,
    too_large,
    failed: []const u8,
    timeout,
};

/// What the rig's fetcher answers and what it was asked.
pub const FakeFetch = struct {
    /// The outcome to give: bytes in it are copied for each call.
    answer: Answer = .{ .status = 404 },
    calls: usize = 0,
    last_url: []const u8 = "",
    url_buf: [512]u8 = undefined,
    last_accept: []const u8 = "",
    last_max_bytes: u64 = 0,
    last_timeout_ms: u32 = 0,

    pub fn fetcher(self: *FakeFetch) fetch.Fetcher {
        return .{ .ctx = self, .fetch_fn = run };
    }

    fn run(ctx: ?*anyopaque, gpa: Allocator, _: Io, req: fetch.Request) fetch.FetchError!fetch.Outcome {
        const self: *FakeFetch = @ptrCast(@alignCast(ctx.?));
        self.calls += 1;
        const n = @min(req.url.len, self.url_buf.len);
        @memcpy(self.url_buf[0..n], req.url[0..n]);
        self.last_url = self.url_buf[0..n];
        self.last_accept = req.accept;
        self.last_max_bytes = req.max_bytes;
        self.last_timeout_ms = req.timeout_ms;
        return switch (self.answer) {
            .body => |b| .{ .body = try gpa.dupe(u8, b) },
            .magnet => |b| .{ .magnet = try gpa.dupe(u8, b) },
            .failed => |b| .{ .failed = try gpa.dupe(u8, b) },
            .status => |c| .{ .status = c },
            .too_large => .too_large,
            .timeout => .timeout,
        };
    }
};

/// Records the events as the Download or ID they carry.
pub const Recorder = struct {
    arena: std.heap.ArenaAllocator,
    mu: Io.Mutex = .init,
    added: std.ArrayList(dl.Download) = .empty,
    others: usize = 0,

    pub fn publisher(r: *Recorder) event.Publisher {
        return .{ .ctx = r, .publish_fn = record };
    }

    fn record(ctx: *anyopaque, e: event.Event) void {
        const r: *Recorder = @ptrCast(@alignCast(ctx));
        r.mu.lockUncancelable(testing.io);
        defer r.mu.unlock(testing.io);
        switch (e) {
            .download_added => |d| {
                var copy = d;
                const a = r.arena.allocator();
                copy.id = a.dupe(u8, d.id) catch @panic("oom");
                copy.name = a.dupe(u8, d.name) catch @panic("oom");
                copy.category_id = a.dupe(u8, d.category_id) catch @panic("oom");
                r.added.append(a, copy) catch @panic("oom");
            },
            else => r.others += 1,
        }
    }
};

pub const Rig = struct {
    db: tu.Rig,
    arena_state: std.heap.ArenaAllocator,
    incomplete: []const u8,
    key: keyfile.Key,
    sources: source_repo.SourceRepo,
    category_repo: categories_repo.CategoryRepo,
    out: Io.Writer.Allocating,
    core: log.Core,
    recorder: Recorder,
    downloads: downloads_service.Service,
    fetcher: FakeFetch,
    clean_names: bool,
    wake_count: std.atomic.Value(usize),
    /// The sources the AddService uses: the real table unless a test swaps it.
    sources_seam: service.Sources,
    torrent_calls: usize,
    add: service.AddService,
    f: app_error.Failure,

    pub fn open(rig: *Rig) !void {
        try rig.db.open();
        errdefer rig.db.close();
        rig.arena_state = .init(testing.allocator);
        errdefer rig.arena_state.deinit();
        const alloc = rig.arena_state.allocator();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.db.tmp.dir.realPath(testing.io, &buf);
        rig.incomplete = try std.fmt.allocPrint(alloc, "{s}/work/incomplete", .{buf[0..n]});
        rig.key = .{ .bytes = @splat(7) };
        rig.sources = .{ .store = rig.db.store, .key = &rig.key };
        rig.category_repo = .{ .store = rig.db.store };
        rig.out = .init(testing.allocator);
        rig.core = .init(testing.io, &rig.out.writer, .{ .level = .debug, .format = .text });
        rig.recorder = .{ .arena = .init(testing.allocator) };
        rig.downloads = downloads_service.Service.init(testing.allocator, testing.io, rig.db.repo, .{
            .categories = .{ .ctx = rig, .exists_fn = categoryExists },
            .publisher = rig.recorder.publisher(),
            .now_fn = downloads_service.fakeNow,
        });
        downloads_service.fake_now = tu.epoch;
        rig.fetcher = .{};
        rig.clean_names = false;
        rig.wake_count = .init(0);
        rig.torrent_calls = 0;
        rig.f = .{};
        rig.sources_seam = service.sourcesFromRepo(&rig.sources);
        rig.rebuild(.{});
    }

    /// (Re)builds the AddService; `tweak` changes what the rig would give it.
    pub const Tweak = struct {
        max_nzb_bytes: ?u64 = null,
        torrents: service.Torrents = .{},
        name_cleaner: ?service.NameCleaner = null,
        fetch_timeout_ms: ?u32 = null,
    };

    pub fn rebuild(rig: *Rig, t: Tweak) void {
        rig.add = service.AddService.init(testing.allocator, testing.io, &rig.downloads, .{
            .settings = .{ .ctx = rig, .read_fn = readSettings },
            .categories = .{ .ctx = rig, .exists_fn = catExists, .list_fn = catList, .ensure_fn = catEnsure },
            .sources = rig.sources_seam,
            .fetcher = rig.fetcher.fetcher(),
            .logger = rig.core.logger(),
            .on_added = .{ .ctx = rig, .added_fn = wake },
            .torrents = t.torrents,
            .name_cleaner = t.name_cleaner,
            .max_nzb_bytes = t.max_nzb_bytes orelse service.default_max_nzb_bytes,
            .fetch_timeout_ms = t.fetch_timeout_ms orelse fetch.default_timeout_ms,
        });
    }

    pub fn close(rig: *Rig) void {
        rig.downloads.deinit();
        rig.recorder.arena.deinit();
        rig.out.deinit();
        rig.arena_state.deinit();
        rig.db.close();
    }

    /// Memory that lives until the end of the test.
    pub fn a(rig: *Rig) Allocator {
        return rig.arena_state.allocator();
    }

    fn categoryExists(ctx: *anyopaque, id: []const u8) anyerror!bool {
        const rig: *Rig = @ptrCast(@alignCast(ctx));
        _ = rig.category_repo.get(rig.a(), id) catch |err| switch (err) {
            error.NotFound => return false,
            else => return err,
        };
        return true;
    }

    fn readSettings(ctx: ?*anyopaque, _: Allocator) anyerror!service.SettingsSource.View {
        const rig: *Rig = @ptrCast(@alignCast(ctx.?));
        return .{ .usenet_incomplete = rig.incomplete, .clean_names = rig.clean_names };
    }

    fn catExists(ctx: ?*anyopaque, arena: Allocator, id: []const u8) anyerror!bool {
        const rig: *Rig = @ptrCast(@alignCast(ctx.?));
        _ = rig.category_repo.get(arena, id) catch |err| switch (err) {
            error.NotFound => return false,
            else => return err,
        };
        return true;
    }

    fn catList(ctx: ?*anyopaque, arena: Allocator) anyerror![]const @import("categories_types.zig").Category {
        const rig: *Rig = @ptrCast(@alignCast(ctx.?));
        return rig.category_repo.list(arena);
    }

    fn catEnsure(ctx: ?*anyopaque, arena: Allocator, f: *app_error.Failure, name: []const u8) anyerror!@import("categories_types.zig").Category {
        const rig: *Rig = @ptrCast(@alignCast(ctx.?));
        return categories_service.ensureCategory(arena, testing.io, rig.category_repo, rig.core.logger(), f, name);
    }

    fn wake(ctx: ?*anyopaque, _: dl.Download) void {
        const rig: *Rig = @ptrCast(@alignCast(ctx.?));
        _ = rig.wake_count.fetchAdd(1, .monotonic);
    }

    // ---- helpers for tests ----

    pub fn addCategory(rig: *Rig, id: []const u8, name: []const u8) !void {
        try rig.db.addCategory(id, name);
    }

    /// The entries of the incomplete root; empty when it does not exist.
    pub fn staged(rig: *Rig) ![]const []const u8 {
        var out: std.ArrayList([]const u8) = .empty;
        var dir = Io.Dir.cwd().openDir(testing.io, rig.incomplete, .{ .iterate = true }) catch return out.items;
        defer dir.close(testing.io);
        var it = dir.iterate();
        while (try it.next(testing.io)) |entry| try out.append(rig.a(), try rig.a().dupe(u8, entry.name));
        return out.items;
    }

    /// The names inside a Download's directory.
    pub fn filesOf(rig: *Rig, id: []const u8) ![]const []const u8 {
        const path = try std.fmt.allocPrint(rig.a(), "{s}/{s}", .{ rig.incomplete, id });
        var out: std.ArrayList([]const u8) = .empty;
        var dir = try Io.Dir.cwd().openDir(testing.io, path, .{ .iterate = true });
        defer dir.close(testing.io);
        var it = dir.iterate();
        while (try it.next(testing.io)) |entry| try out.append(rig.a(), try rig.a().dupe(u8, entry.name));
        return out.items;
    }

    pub fn readFile(rig: *Rig, path: []const u8) ![]u8 {
        return Io.Dir.cwd().readFileAlloc(testing.io, path, rig.a(), .limited(1 << 20));
    }

    /// Receives `bytes` as an NZB upload and adds it with `req`.
    pub fn addUpload(rig: *Rig, file_name: []const u8, bytes: []const u8, req: service.Request) !dl.Download {
        const up = try rig.add.receive(rig.a(), &rig.f, bytes, file_name, .nzb);
        var r = req;
        r.upload = up;
        return rig.add.add(rig.a(), &rig.f, r);
    }

    pub fn expectRejected(rig: *Rig, result: anytype, kind: app_error.Kind, code: []const u8) !void {
        try testing.expectError(error.Rejected, result);
        try testing.expectEqual(kind, rig.f.kind);
        try testing.expectEqualStrings(code, rig.f.code);
    }

    pub fn expectNothingAdded(rig: *Rig) !void {
        try testing.expectEqual(@as(usize, 0), try rig.db.countWhere("1 = 1"));
        try testing.expectEqual(@as(usize, 0), (try rig.staged()).len);
        try testing.expectEqual(@as(usize, 0), rig.recorder.added.items.len);
    }
};
