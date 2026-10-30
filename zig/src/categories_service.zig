//! Managing Categories (internal/app/category.go): list, ensure, create,
//! update and delete, with the error codes the API shows (invalid_name,
//! invalid_dir_override, name_taken, empty_patch).
//!
//! Strings of results live in the arena the caller passes. Failures a client
//! can act on come back as `error.Rejected` with the details in the Failure the
//! caller passes (app_error.zig); a missing Category is `error.NotFound`.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const app_error = @import("app_error.zig");
const dl = @import("download.zig");
const event = @import("event.zig");
const id_mod = @import("id.zig");
const log = @import("log.zig");
const types = @import("categories_types.zig");
const Category = types.Category;
const CategoryRepo = @import("categories_repo.zig").CategoryRepo;
const repo_mod = @import("categories_repo.zig");
const Failure = app_error.Failure;

pub const Error = error{
    Rejected,
    NotFound,
} || repo_mod.Error;

/// The Downloads side of a delete, which the Downloads module implements:
/// `list_in_category` finds the Downloads that use a Category, `get` reads one
/// Download again (null when it is gone). Both allocate in `arena`.
pub const DownloadsLookup = struct {
    ctx: *anyopaque,
    list_in_category: *const fn (ctx: *anyopaque, arena: Allocator, category_id: []const u8) anyerror![]const dl.Download,
    get: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?dl.Download,
};

/// The changes `update` applies; null fields are left alone, and a folder
/// override that is the empty string removes the override.
pub const CategoryPatch = struct {
    name: ?[]const u8 = null,
    usenet_dir: ?[]const u8 = null,
    torrent_dir: ?[]const u8 = null,
};

pub const Service = struct {
    repo: CategoryRepo,
    downloads: DownloadsLookup,
    events: event.Publisher,
    io: Io,
    logger: log.Logger,
    /// Serializes update and delete (read-modify-write), like Go's mutex.
    lock: Io.Mutex = .init,

    pub fn init(repo: CategoryRepo, downloads: DownloadsLookup, events: event.Publisher, io: Io, logger: log.Logger) Service {
        return .{ .repo = repo, .downloads = downloads, .events = events, .io = io, .logger = logger };
    }

    /// Every Category ordered by name.
    pub fn list(self: *Service, arena: Allocator) Error![]const Category {
        return self.repo.list(arena);
    }

    /// The Category with the given name, ignoring case, created (lower case,
    /// default directory) when there is none. SABnzbd and qBittorrent create an
    /// unknown Category on add, so the Media managers rely on this.
    pub fn ensure(self: *Service, arena: Allocator, f: *Failure, name: []const u8) Error!Category {
        return ensureCategory(arena, self.io, self.repo, self.logger, f, name);
    }

    /// Validates and stores a new Category; a name that is taken is a conflict
    /// (code name_taken).
    pub fn create(self: *Service, arena: Allocator, f: *Failure, name: []const u8, usenet_dir: []const u8, torrent_dir: []const u8) Error!Category {
        const c: Category = .{
            .id = try arena.dupe(u8, &id_mod.new(self.io)),
            .name = name,
            .usenet_dir = usenet_dir,
            .torrent_dir = torrent_dir,
        };
        try validateCategory(f, c);
        self.repo.create(c) catch |err| return writeError(f, err);
        return c;
    }

    pub fn update(self: *Service, arena: Allocator, f: *Failure, id: []const u8, p: CategoryPatch) Error!Category {
        if (p.name == null and p.usenet_dir == null and p.torrent_dir == null) {
            return f.setLiteral(.invalid_input, "empty_patch", "nothing to change: give name, usenet_dir or torrent_dir");
        }
        try self.lock.lock(self.io);
        defer self.lock.unlock(self.io);
        var c = try self.repo.get(arena, id);
        if (p.name) |v| c.name = v;
        if (p.usenet_dir) |v| c.usenet_dir = v;
        if (p.torrent_dir) |v| c.torrent_dir = v;
        try validateCategory(f, c);
        self.repo.update(c) catch |err| return writeError(f, err);
        return c;
    }

    /// Removes a Category. The Downloads that used it lose it, and each is
    /// announced with a download_updated event. `arena` should be per request:
    /// it holds the Downloads read for the events.
    pub fn delete(self: *Service, arena: Allocator, id: []const u8) anyerror!void {
        try self.lock.lock(self.io);
        defer self.lock.unlock(self.io);
        const affected = try self.downloads.list_in_category(self.downloads.ctx, arena, id);
        // Only the IDs: the snapshots are read again below, after the foreign
        // key cleared the Category.
        const ids = try arena.alloc([]const u8, affected.len);
        for (affected, ids) |d, *out| out.* = try arena.dupe(u8, d.id);
        try self.repo.delete(id);
        for (ids) |did| {
            // Removed meanwhile (or unreadable): its Removed event covers it.
            const d = (self.downloads.get(self.downloads.ctx, arena, did) catch continue) orelse continue;
            self.events.publish(.{ .download_updated = d });
        }
    }
};

/// `ensure` on a repository, for the services that create Categories on the
/// fly. Two callers creating the same name at once end up with the one
/// Category: on a conflict the list is read once more.
pub fn ensureCategory(arena: Allocator, io: Io, repo: CategoryRepo, logger: log.Logger, f: *Failure, name_in: []const u8) Error!Category {
    const name = std.mem.trim(u8, name_in, &std.ascii.whitespace);
    var attempt: usize = 0;
    while (true) : (attempt += 1) {
        const cats = try repo.list(arena);
        if (types.findIgnoreCase(cats, name)) |c| return c;
        const c: Category = .{
            .id = try arena.dupe(u8, &id_mod.new(io)),
            .name = try std.ascii.allocLowerString(arena, name),
        };
        try validateCategory(f, c);
        repo.create(c) catch |err| {
            // Someone created it between our list and create: look again.
            if (err == error.Conflict and attempt == 0) continue;
            return writeError(f, err);
        };
        logger.info("category created", &.{log.Attr.string("category", c.name)});
        return c;
    }
}

fn validateCategory(f: *Failure, c: Category) error{Rejected}!void {
    types.validate(c) catch |err| switch (err) {
        error.InvalidName => return f.setLiteral(.invalid_input, "invalid_name", "category name must use only a-z, 0-9, '.', '_' and '-', be 1 to 64 characters long, and not be '.' or '..'"),
        error.InvalidDirOverride => return f.setLiteral(.invalid_input, "invalid_dir_override", "usenet_dir and torrent_dir must be absolute paths"),
    };
}

/// A repository error of a create or update as the client sees it.
fn writeError(f: *Failure, err: repo_mod.Error) Error {
    return switch (err) {
        error.Conflict => f.setLiteral(.conflict, "name_taken", "a category with this name already exists"),
        else => err,
    };
}

// ---- tests ----

const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");

/// A fake Downloads side: a list of (download id, category id) pairs in
/// memory. Deleting a Category clears it from the Downloads, like the foreign
/// key does in the database, but only when the test wires `repo` in.
pub const FakeDownloads = struct {
    pub const Row = struct { id: []const u8, category_id: []const u8 };
    rows: []Row,
    /// Downloads removed after the category was listed, by ID.
    gone: []const []const u8 = &.{},
    repo: ?CategoryRepo = null,

    pub fn lookup(self: *FakeDownloads) DownloadsLookup {
        return .{ .ctx = self, .list_in_category = listIn, .get = getOne };
    }

    fn make(row: Row) dl.Download {
        return .{ .id = row.id, .name = "show", .source_kind = .nzb, .category_id = row.category_id, .state = .queued, .added_at = 1, .updated_at = 1 };
    }

    fn listIn(ctx: *anyopaque, arena: Allocator, category_id: []const u8) anyerror![]const dl.Download {
        const self: *FakeDownloads = @ptrCast(@alignCast(ctx));
        var out: std.ArrayList(dl.Download) = .empty;
        for (self.rows) |r| if (std.mem.eql(u8, r.category_id, category_id)) try out.append(arena, make(r));
        return out.items;
    }

    fn getOne(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?dl.Download {
        _ = arena;
        const self: *FakeDownloads = @ptrCast(@alignCast(ctx));
        for (self.gone) |g| if (std.mem.eql(u8, g, id)) return null;
        for (self.rows) |r| if (std.mem.eql(u8, r.id, id)) {
            var d = make(r);
            // What the foreign key did when the Category row went away.
            if (self.repo) |repo| {
                var a: std.heap.ArenaAllocator = .init(testing.allocator);
                defer a.deinit();
                _ = repo.get(a.allocator(), r.category_id) catch |err| switch (err) {
                    error.NotFound => d.category_id = "",
                    else => return err,
                };
            }
            return d;
        };
        return null;
    }
};

/// Records what is published: download ids and their category ids.
pub const Recorder = struct {
    seen: std.ArrayList(struct { id: []const u8, category_id: []const u8 }) = .empty,

    pub fn publisher(self: *Recorder) event.Publisher {
        return .{ .ctx = self, .publish_fn = publish };
    }

    fn publish(ctx: *anyopaque, e: event.Event) void {
        const self: *Recorder = @ptrCast(@alignCast(ctx));
        switch (e) {
            .download_updated => |d| self.seen.append(testing.allocator, .{
                .id = testing.allocator.dupe(u8, d.id) catch @panic("oom"),
                .category_id = testing.allocator.dupe(u8, d.category_id) catch @panic("oom"),
            }) catch @panic("oom"),
            else => @panic("unexpected event"),
        }
    }

    pub fn deinit(self: *Recorder) void {
        for (self.seen.items) |s| {
            testing.allocator.free(s.id);
            testing.allocator.free(s.category_id);
        }
        self.seen.deinit(testing.allocator);
    }
};

pub const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    out: Io.Writer.Allocating,
    core: log.Core,
    arena: std.heap.ArenaAllocator,
    downloads: FakeDownloads,
    recorder: Recorder = .{},
    svc: Service,
    f: Failure = .{},

    pub fn open(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        {
            const w = try rig.store.acquireWriter();
            defer w.release();
            var diag: migrate.Diagnostic = .{};
            try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        }
        rig.out = .init(testing.allocator);
        rig.core = .init(testing.io, &rig.out.writer, .{ .level = .debug, .format = .text });
        rig.arena = .init(testing.allocator);
        rig.downloads = .{ .rows = &.{} };
        rig.recorder = .{};
        rig.f = .{};
        rig.svc = Service.init(.{ .store = rig.store }, rig.downloads.lookup(), rig.recorder.publisher(), testing.io, rig.core.logger());
    }

    pub fn close(rig: *Rig) void {
        rig.recorder.deinit();
        rig.arena.deinit();
        rig.out.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }

    pub fn a(rig: *Rig) Allocator {
        return rig.arena.allocator();
    }

    pub fn expectRejected(rig: *Rig, result: anytype, code: []const u8) !void {
        try testing.expectError(error.Rejected, result);
        try testing.expectEqualStrings(code, rig.f.code);
    }
};

test "create stores the category with a fresh ID" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const got = try rig.svc.create(rig.a(), &rig.f, "tv-4k", "/data/usenet/tv", "/data/torrents/tv");
    try testing.expect(id_mod.isValid(got.id));
    try testing.expectEqualStrings("tv-4k", got.name);
    const stored = try rig.svc.repo.get(rig.a(), got.id);
    try testing.expectEqualStrings("/data/usenet/tv", stored.usenet_dir);
    try testing.expectEqualStrings("/data/torrents/tv", stored.torrent_dir);
    const listed = try rig.svc.list(rig.a());
    try testing.expectEqual(1, listed.len);
    try testing.expectEqualStrings(got.id, listed[0].id);
}

test "create refuses bad names, relative dirs and duplicates" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    _ = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");

    try rig.expectRejected(rig.svc.create(rig.a(), &rig.f, "TV Shows", "", ""), "invalid_name");
    try testing.expectEqual(app_error.Kind.invalid_input, rig.f.kind);
    try rig.expectRejected(rig.svc.create(rig.a(), &rig.f, "..", "", ""), "invalid_name");
    try rig.expectRejected(rig.svc.create(rig.a(), &rig.f, "", "", ""), "invalid_name");
    try rig.expectRejected(rig.svc.create(rig.a(), &rig.f, "movies", "", "movies/here"), "invalid_dir_override");
    try testing.expectEqual(app_error.Kind.invalid_input, rig.f.kind);
    try rig.expectRejected(rig.svc.create(rig.a(), &rig.f, "tv", "", ""), "name_taken");
    try testing.expectEqual(app_error.Kind.conflict, rig.f.kind);
}

test "list is empty when there are no categories" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectEqual(0, (try rig.svc.list(rig.a())).len);
}

test "update renames, sets and clears the folder overrides" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const c = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");

    const renamed = try rig.svc.update(rig.a(), &rig.f, c.id, .{ .name = "series" });
    try testing.expectEqualStrings("series", renamed.name);
    const with_dir = try rig.svc.update(rig.a(), &rig.f, c.id, .{ .torrent_dir = "/data/series" });
    try testing.expectEqualStrings("", with_dir.usenet_dir);
    try testing.expectEqualStrings("/data/series", with_dir.torrent_dir);
    const both = try rig.svc.update(rig.a(), &rig.f, c.id, .{ .usenet_dir = "/data/series" });
    try testing.expectEqualStrings("/data/series", both.usenet_dir);
    try testing.expectEqualStrings("/data/series", both.torrent_dir);
    const cleared = try rig.svc.update(rig.a(), &rig.f, c.id, .{ .usenet_dir = "", .torrent_dir = "" });
    try testing.expectEqualStrings("series", cleared.name);
    try testing.expectEqualStrings("", cleared.usenet_dir);
    try testing.expectEqualStrings("", cleared.torrent_dir);
    const stored = try rig.svc.repo.get(rig.a(), c.id);
    try testing.expectEqualStrings("", stored.torrent_dir);
}

test "update validates and reports conflicts and missing rows" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const tv = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    _ = try rig.svc.create(rig.a(), &rig.f, "movies", "", "");

    try rig.expectRejected(rig.svc.update(rig.a(), &rig.f, tv.id, .{ .name = "Bad Name" }), "invalid_name");
    try rig.expectRejected(rig.svc.update(rig.a(), &rig.f, tv.id, .{ .usenet_dir = "relative" }), "invalid_dir_override");
    try rig.expectRejected(rig.svc.update(rig.a(), &rig.f, tv.id, .{ .name = "movies" }), "name_taken");
    try testing.expectEqual(app_error.Kind.conflict, rig.f.kind);
    try rig.expectRejected(rig.svc.update(rig.a(), &rig.f, tv.id, .{}), "empty_patch");
    try testing.expectEqual(app_error.Kind.invalid_input, rig.f.kind);
    try testing.expectError(error.NotFound, rig.svc.update(rig.a(), &rig.f, "nope", .{ .name = "x" }));

    const stored = try rig.svc.repo.get(rig.a(), tv.id);
    try testing.expectEqualStrings("tv", stored.name);
    try testing.expectEqualStrings("", stored.usenet_dir);
}

test "ensure finds a category ignoring case, else creates it in lower case" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const tv = try rig.svc.create(rig.a(), &rig.f, "tv", "/data/tv", "");

    const found = try rig.svc.ensure(rig.a(), &rig.f, "  TV ");
    try testing.expectEqualStrings(tv.id, found.id);
    try testing.expectEqual(0, rig.out.written().len);

    const made = try rig.svc.ensure(rig.a(), &rig.f, "Movies");
    try testing.expectEqualStrings("movies", made.name);
    try testing.expectEqualStrings("", made.usenet_dir);
    const again = try rig.svc.ensure(rig.a(), &rig.f, "MOVIES");
    try testing.expectEqualStrings(made.id, again.id);
    try testing.expectEqual(2, (try rig.svc.list(rig.a())).len);
    try testing.expect(std.mem.find(u8, rig.out.written(), "category created") != null);
    try testing.expect(std.mem.find(u8, rig.out.written(), "category=movies") != null);
}

test "ensure refuses a name that cannot be a category" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.expectRejected(rig.svc.ensure(rig.a(), &rig.f, "tv shows"), "invalid_name");
    try rig.expectRejected(rig.svc.ensure(rig.a(), &rig.f, ""), "invalid_name");
    try rig.expectRejected(rig.svc.ensure(rig.a(), &rig.f, ".."), "invalid_name");
    try testing.expectEqual(0, (try rig.svc.list(rig.a())).len);
}

test "concurrent ensures of one name yield one category" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    const Worker = struct {
        fn run(svc: *Service, out: *[26]u8, ok: *bool) void {
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            var f: Failure = .{};
            const c = svc.ensure(arena.allocator(), &f, "Anime") catch return;
            @memcpy(out, c.id[0..26]);
            ok.* = true;
        }
    };
    var ids: [4][26]u8 = undefined;
    var oks: [4]bool = @splat(false);
    var group: Io.Group = .init;
    for (&ids, &oks) |*id, *ok| group.async(testing.io, Worker.run, .{ &rig.svc, id, ok });
    try group.await(testing.io);

    for (oks) |ok| try testing.expect(ok);
    for (ids[1..]) |id| try testing.expectEqualSlices(u8, &ids[0], &id);
    try testing.expectEqual(1, (try rig.svc.list(rig.a())).len);
}

test "delete clears the category from its downloads and announces each" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const c = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    var rows = [_]FakeDownloads.Row{
        .{ .id = "d1", .category_id = c.id },
        .{ .id = "d2", .category_id = "other" },
        .{ .id = "d3", .category_id = c.id },
    };
    rig.downloads.rows = &rows;
    rig.downloads.repo = rig.svc.repo;

    try rig.svc.delete(rig.a(), c.id);

    try testing.expectError(error.NotFound, rig.svc.repo.get(rig.a(), c.id));
    try testing.expectEqual(2, rig.recorder.seen.items.len);
    try testing.expectEqualStrings("d1", rig.recorder.seen.items[0].id);
    try testing.expectEqualStrings("d3", rig.recorder.seen.items[1].id);
    // The snapshots were read after the delete, so they show no category.
    for (rig.recorder.seen.items) |s| try testing.expectEqualStrings("", s.category_id);
}

test "a download removed meanwhile is skipped" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const c = try rig.svc.create(rig.a(), &rig.f, "tv", "", "");
    var rows = [_]FakeDownloads.Row{ .{ .id = "d1", .category_id = c.id }, .{ .id = "d2", .category_id = c.id } };
    rig.downloads.rows = &rows;
    rig.downloads.gone = &.{"d1"};

    try rig.svc.delete(rig.a(), c.id);
    try testing.expectEqual(1, rig.recorder.seen.items.len);
    try testing.expectEqualStrings("d2", rig.recorder.seen.items[0].id);
}

test "delete of an unknown category is not found and announces nothing" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectError(error.NotFound, rig.svc.delete(rig.a(), "nope"));
    try testing.expectEqual(0, rig.recorder.seen.items.len);
}
