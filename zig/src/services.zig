//! The services of the own API, built once and joined at the seams the slices
//! left open (categories <-> downloads, servers -> events, downloads -> data
//! remover). `Services` lives at one address for the life of the daemon: the
//! services hold mutexes and the routes point at them.
//!
//! Teardown order matters: the HTTP server stops first (nothing calls the
//! services any more), then `deinit`, then the bus is closed last so streams
//! that were still open have ended, and finally the database.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const sqlite = @import("sqlite.zig");
const keyfile = @import("keyfile.zig");
const log = @import("log.zig");
const ex = @import("http_exchange.zig");
const event = @import("event.zig");
const id_mod = @import("id.zig");
const dl = @import("download.zig");

const events_bus = @import("events_bus.zig");
const events_api = @import("events_api.zig");
const categories_repo = @import("categories_repo.zig");
const categories_service = @import("categories_service.zig");
const categories_api = @import("categories_api.zig");
const downloads_repo = @import("downloads_repo.zig");
const downloads_service = @import("downloads_service.zig");
const downloads_api = @import("downloads_api.zig");
const remover_mod = @import("remover.zig");
const settings_service = @import("settings_service.zig");
const settings_api = @import("settings_api.zig");
const servers_service = @import("servers_service.zig");
const servers_api = @import("servers_api.zig");
const servers_prober = @import("servers_prober.zig");
const nntp = @import("nntp.zig");
const queue_adapters = @import("queue_adapters.zig");
const runner_mod = @import("runner.zig");
const runner_seams = @import("runner_seams.zig");
const runner_store = @import("runner_store.zig");
const add_service = @import("add_service.zig");
const add_curl = @import("add_curl.zig");
const add_source_repo = @import("add_source_repo.zig");

pub const Services = struct {
    gpa: Allocator,
    io: Io,
    store: *sqlite.Store,

    bus: *events_bus.Bus,
    category_repo: categories_repo.CategoryRepo,
    download_repo: downloads_repo.DownloadRepo,
    categories: categories_service.Service,
    downloads: downloads_service.Service,
    servers: servers_service.ServerService,
    settings: settings_service.Service,
    remover: remover_mod.Remover,
    /// One TLS context for every NNTP connection and the connection test.
    tls_dialer: nntp.TlsDialer,
    prober: servers_prober.NntpProber,
    source_repo: add_source_repo.SourceRepo,
    runner_servers: runner_seams.ServiceServers,
    runner_policy: queue_adapters.SettingsPolicy,
    runner_store: runner_store.Sqlite,
    usenet: *runner_mod.Runner,
    add: add_service.AddService,
    instance_id: [id_mod.length]u8,
    events: events_api.Events,

    /// `self` must stay where it is until `deinit`. `key` must outlive it.
    pub fn init(self: *Services, gpa: Allocator, io: Io, store: *sqlite.Store, key: *const keyfile.Key, logger: log.Logger) !void {
        const bus = try events_bus.Bus.init(gpa, io, .{});
        errdefer bus.deinit();
        const publisher = bus.publisher();

        self.gpa = gpa;
        self.io = io;
        self.store = store;
        self.bus = bus;
        self.category_repo = .{ .store = store };
        self.download_repo = .{ .store = store };
        self.categories = categories_service.Service.init(self.category_repo, .{
            .ctx = &self.download_repo,
            .list_in_category = listInCategory,
            .get = findDownload,
        }, publisher, io, logger);
        self.servers = servers_service.ServerService.init(io, store, key, .{ .ctx = bus, .notify_fn = serversChanged }, servers_service.realClock);
        self.settings = try settings_service.Service.init(gpa, io, .{ .store = store }, publisher, null);
        errdefer self.settings.deinit();
        self.tls_dialer = try nntp.TlsDialer.init(.{});
        errdefer self.tls_dialer.deinit();
        self.prober = servers_prober.NntpProber.init(gpa, self.tls_dialer.dialer());
        self.servers.prober = self.prober.prober();
        self.source_repo = .{ .store = store, .key = key };
        self.runner_servers = .{ .gpa = gpa, .service = &self.servers, .log = logger };
        errdefer self.runner_servers.deinit();
        self.runner_policy = .{ .service = &self.settings, .kind = .nzb };
        self.runner_store = .{ .store = store };
        self.usenet = try runner_mod.Runner.create(gpa, io, .{
            .bus = bus,
            .downloads = queue_adapters.downloadsFromRepo(&self.download_repo),
            .policy = self.runner_policy.source(),
            .servers = self.runner_servers.source(),
            .sources = .{ .ctx = &self.source_repo, .path_fn = nzbPath },
            .dialer = self.tls_dialer.dialer(),
            .progress = self.runner_store.progress(),
            .volumes = self.runner_store.volumes(),
            .reports = self.runner_store.reports(),
            .speed = .{ .ctx = &self.settings, .read_fn = usenetSpeed },
            .log = logger,
        });
        errdefer self.usenet.destroy();
        self.remover = remover_mod.Remover.init(gpa, io, .{
            .paths = remover_mod.fromSettings(&self.settings),
            .categories = remover_mod.fromRepo(&self.category_repo),
            .usenet_holder = self.usenet.fetchHolder(),
            .log = logger,
        });
        errdefer self.remover.deinit();
        self.downloads = downloads_service.Service.init(gpa, io, self.download_repo, .{
            .categories = .{ .ctx = &self.category_repo, .exists_fn = categoryExists },
            .remover = self.remover.asDataRemover(),
            .publisher = publisher,
        });
        self.add = add_service.AddService.init(gpa, io, &self.downloads, .{
            .settings = add_service.settingsFromService(&self.settings),
            .categories = add_service.categoriesFromService(&self.categories),
            .sources = add_service.sourcesFromRepo(&self.source_repo),
            .fetcher = add_curl.fetcher,
            .logger = logger,
            .on_added = .{ .ctx = self.usenet, .added_fn = wakeRunner },
        });
        self.instance_id = id_mod.new(io);
        self.events = .{ .bus = bus, .instance_id = &self.instance_id };
    }

    /// Resumes the fetches a restart interrupted and starts the runner. Call
    /// it once the routes are registered, before listening.
    pub fn start(self: *Services) !void {
        try self.usenet.recover();
        try self.usenet.start();
    }

    /// Stops the fetches and saves their resume state. Call it after the HTTP
    /// server has returned and before `closeStreams`.
    pub fn stop(self: *Services) void {
        self.usenet.stop();
    }

    pub fn deinit(self: *Services) void {
        self.usenet.destroy();
        self.runner_servers.deinit();
        self.tls_dialer.deinit();
        self.downloads.deinit();
        self.remover.deinit();
        self.settings.deinit();
        self.bus.close();
        self.bus.deinit();
    }

    /// Ends the open event streams, which end on their own when the server
    /// stops; call it after the server has returned, before `deinit`.
    pub fn closeStreams(self: *Services) void {
        self.bus.close();
    }

    pub fn register(self: *Services, routes: *ex.Routes) !void {
        try categories_api.register(routes, &self.categories);
        try downloads_api.registerWithAdd(routes, &self.downloads, &self.add);
        try servers_api.register(routes, &self.servers);
        try settings_api.register(routes, &self.settings);
        try events_api.register(routes, &self.events);
    }
};

// ---- the seams ----

fn listInCategory(ctx: *anyopaque, arena: Allocator, category_id: []const u8) anyerror![]const dl.Download {
    const repo: *downloads_repo.DownloadRepo = @ptrCast(@alignCast(ctx));
    return repo.listInCategory(arena, category_id);
}

fn findDownload(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?dl.Download {
    const repo: *downloads_repo.DownloadRepo = @ptrCast(@alignCast(ctx));
    return repo.find(arena, id);
}

fn categoryExists(ctx: *anyopaque, id: []const u8) anyerror!bool {
    const repo: *categories_repo.CategoryRepo = @ptrCast(@alignCast(ctx));
    var arena: std.heap.ArenaAllocator = .init(std.heap.page_allocator);
    defer arena.deinit();
    _ = repo.get(arena.allocator(), id) catch |err| switch (err) {
        error.NotFound => return false,
        else => return err,
    };
    return true;
}

fn serversChanged(ctx: *anyopaque) void {
    const bus: *events_bus.Bus = @ptrCast(@alignCast(ctx));
    bus.publish(.servers_changed);
}

fn nzbPath(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?[]const u8 {
    const repo: *add_source_repo.SourceRepo = @ptrCast(@alignCast(ctx));
    const src = repo.get(arena, id) catch |err| switch (err) {
        error.NotFound => return null,
        else => return err,
    };
    // The path lives in the arena; the password is not needed here.
    @import("secretbox.zig").wipeAndFree(arena, src.password);
    return src.nzb_path;
}

fn usenetSpeed(ctx: *anyopaque) i64 {
    const settings: *settings_service.Service = @ptrCast(@alignCast(ctx));
    const snap = settings.current();
    defer snap.release();
    return snap.settings.speed.effective(.nzb).down;
}

fn wakeRunner(ctx: ?*anyopaque, _: dl.Download) void {
    const r: *runner_mod.Runner = @ptrCast(@alignCast(ctx.?));
    r.poke();
}
