//! The bitnews daemon: cmd/bitnews/main.go of the Go version. Start-up order
//! is the Go one, because the black-box suite (and operators) see it in the
//! log and in which files exist when something fails:
//!   1. `healthcheck` subcommand, or the bootstrap settings (exit 2 if invalid);
//!   2. the logger and the umask, before any file is created;
//!   3. the "starting bitnews" line;
//!   4. the config dir, the master key file, the database and its migrations;
//!   5. the app key and the first-run setup state;
//!   6. the HTTP server, until SIGINT or SIGTERM;
//!   7. shutdown: the server stops first, then the database is closed.
//! Exit codes: 0 on success, 1 for a start-up or runtime failure, 2 for
//! invalid flags or environment.
const std = @import("std");
const Io = std.Io;
const build_options = @import("build_options");
const config = @import("config.zig");
const log = @import("log.zig");
const keyfile = @import("keyfile.zig");
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const AppKeyRepo = @import("appkey_repo.zig").AppKeyRepo;
const appkey = @import("appkey.zig");
const setup_gate = @import("setup_gate.zig");
const accounts_bootstrap = @import("accounts_bootstrap.zig");
const accounts_setup = @import("accounts_setup.zig");
const services_mod = @import("services.zig");
const tool = @import("tool.zig");
const http = @import("http_server.zig");
const common = @import("http_common.zig");
const sabnzbd = @import("sabnzbd.zig");
const sabnzbd_live = @import("sabnzbd_live.zig");
const events_bus = @import("events_bus.zig");
const event = @import("event.zig");
const web = @import("web.zig");
const add_service = @import("add_service.zig");
const torrent_add = @import("torrent_add.zig");
const torrent_session = @import("torrent_session.zig");
const torrent_watch = @import("torrent_watch.zig");
const torrent_source_repo = @import("torrent_source_repo.zig");
const torrent_meta = @import("torrent_meta.zig");
const torrent_peers = @import("torrent_peers.zig");
const torrent_listen = @import("torrent_listen.zig");
const qbit = @import("qbit.zig");
const id_mod = @import("id.zig");
const app_error = @import("app_error.zig");
const download = @import("download.zig");
const downloads_service = @import("downloads_service.zig");
const fsutil_space = @import("fsutil_space.zig");
const secret_mod = @import("secret.zig");

const version = build_options.version;
const commit = build_options.commit;

/// How long the health probe waits for the daemon (Go: healthcheckTimeout).
const healthcheck_timeout_ms = 3000;

var stop_requested = std.atomic.Value(bool).init(false);
var sab_live_stop = std.atomic.Value(bool).init(false);
var sab_live_done = std.atomic.Value(bool).init(true);
var torrent_poll_stop = std.atomic.Value(bool).init(false);
var torrent_poll_done = std.atomic.Value(bool).init(true);

fn onSignal(_: std.posix.SIG) callconv(.c) void {
    stop_requested.store(true, .release);
}

pub fn main(init: std.process.Init) u8 {
    return run(init) catch |err| {
        std.debug.print("bitnews: {s}\n", .{@errorName(err)});
        return 1;
    };
}

fn run(init: std.process.Init) !u8 {
    const io = init.io;
    const arena = init.arena.allocator();
    const gpa = init.gpa;

    var out_buf: [1024]u8 = undefined;
    var out_file: Io.File.Writer = .initStreaming(.stdout(), io, &out_buf);
    const stdout = &out_file.interface;
    var err_buf: [4096]u8 = undefined;
    var err_file: Io.File.Writer = .initStreaming(.stderr(), io, &err_buf);
    const stderr = &err_file.interface;

    const all_args = try init.minimal.args.toSlice(arena);
    const args = all_args[1..];

    if (args.len > 0 and std.mem.eql(u8, args[0], "healthcheck")) {
        const code = try healthcheck(io, init.environ_map, stdout, stderr);
        try stdout.flush();
        try stderr.flush();
        return code;
    }

    const cfg = switch (try config.load(arena, init.environ_map, args)) {
        .run => |c| c,
        .help => {
            try stdout.writeAll(config.usage);
            try stdout.flush();
            return 0;
        },
        .version => {
            try stdout.print("bitnews {s} ({s})\n", .{ version, commit });
            try stdout.flush();
            return 0;
        },
        .invalid => |text| {
            try stderr.writeAll(text);
            try stderr.flush();
            return 2;
        },
    };

    // Step 2: the logger, and the umask before any file is created. bitnews
    // runs as whoever started it (ADR-0006), so there is no identity to change.
    var core: log.Core = .init(io, stderr, .{ .level = cfg.log_level, .format = cfg.log_format });
    const logger = core.logger();
    config.applyUmask(cfg.umask);

    logger.info("starting bitnews", &.{
        log.Attr.string("version", version),
        log.Attr.string("commit", commit),
        log.Attr.string("listen", cfg.listen),
        log.Attr.string("config_dir", cfg.config_dir),
        log.Attr.string("key_file", cfg.key_file),
        log.Attr.uint("torrent_port", cfg.torrent_port),
    });

    return serveAll(gpa, io, cfg, logger, stderr) catch |err| switch (err) {
        error.Reported => return 1,
        else => return err,
    };
}

const Reported = error{Reported};

/// Prints "bitnews: <message>" on stderr and fails with error.Reported.
fn fail(stderr: *Io.Writer, comptime fmt: []const u8, args: anytype) Reported!void {
    stderr.print("bitnews: " ++ fmt ++ "\n", args) catch {};
    stderr.flush() catch {};
    return error.Reported;
}

fn serveAll(gpa: std.mem.Allocator, io: Io, cfg: config.Config, logger: log.Logger, stderr: *Io.Writer) !u8 {
    try prepareConfigDir(io, cfg.config_dir, stderr);

    // The master key encrypts Server passwords and archive passwords. Loading
    // it creates it on first start and fails early when it cannot be read.
    var loaded = keyfile.loadOrCreate(io, cfg.key_file) catch |err| {
        stderr.writeAll("bitnews: ") catch {};
        keyfile.writeErrorMessage(stderr, err, cfg.key_file, keyfile.decodedLength(io, cfg.key_file)) catch {};
        stderr.writeAll("\n") catch {};
        stderr.flush() catch {};
        return error.Reported;
    };
    defer loaded.key.deinit();
    if (loaded.generated) {
        logger.warn("generated a new master key for secret encryption; back this file up, because without it stored secrets cannot be decrypted", &.{
            log.Attr.string("path", cfg.key_file),
        });
    }
    if (loaded.broad_mode) |mode| {
        var mode_buf: [16]u8 = undefined;
        const text = std.fmt.bufPrint(&mode_buf, "{o:0>4}", .{mode}) catch "";
        logger.warn("key file is accessible by other users; consider chmod 600", &.{
            log.Attr.string("path", cfg.key_file),
            log.Attr.string("mode", text),
        });
    }

    const db_path = try std.fs.path.join(gpa, &.{ cfg.config_dir, "bitnews.db" });
    defer gpa.free(db_path);
    var failure: sqlite.Failure = undefined;
    const store = sqlite.Store.open(gpa, io, db_path, .{}, &failure) catch |err| {
        try fail(stderr, "store: open {s}: {s} ({f})", .{ db_path, @errorName(err), failure });
        unreachable;
    };
    defer store.close();
    if (store.broad_permissions) |mode| {
        var mode_buf: [16]u8 = undefined;
        const text = std.fmt.bufPrint(&mode_buf, "{o:0>4}", .{mode}) catch "";
        logger.warn("database file permissions are broader than 0600", &.{
            log.Attr.string("path", db_path),
            log.Attr.string("mode", text),
        });
    }
    {
        const w = try store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        migrate.run(gpa, w.db, migrate.embedded, Io.Clock.now(.real, io).toMilliseconds(), &diag) catch {
            try fail(stderr, "{s}", .{diag.message()});
            unreachable;
        };
    }

    var keys = try appkey.Service.init(gpa, io, .{ .store = store }, logger);
    defer keys.deinit();
    var qbit_sessions = qbit.Sessions.init(gpa, io);
    defer qbit_sessions.deinit();
    if (cfg.admin_password_file) |path| {
        _ = accounts_bootstrap.fromFile(gpa, io, store, path, logger) catch |err| {
            try stderr.writeAll("bitnews: ");
            try accounts_bootstrap.writeErrorMessage(stderr, err, path);
            try stderr.writeAll("\n");
            stderr.flush() catch {};
            return error.Reported;
        };
    }
    var gate = try setup_gate.Gate.init(store);
    var setup = try accounts_setup.openSetup(gpa, io, &gate, logger);
    defer setup.deinit();

    var http_log: HttpLog = .{ .logger = logger };
    var key_check: KeyCheck = .{ .service = &keys };
    const server = try http.Server.init(gpa, io, .{
        .logger = http_log.adapter(),
        .keys = key_check.adapter(),
        .version = .{ .version = version, .commit = commit },
        .setup_gate = gate.asSetupGate(),
    });
    defer server.deinit();

    const tools = try locateTools(gpa, io, stderr);
    defer gpa.free(tools.seven_zip);
    defer gpa.free(tools.par2);

    var services: services_mod.Services = undefined;
    try services.init(gpa, io, store, &loaded.key, tools, logger);
    defer services.deinit();
    const snap_listen = services.settings.current();
    var listen_buf: [72]u8 = undefined;
    const listen_port: u16 = if (cfg.torrent_port != 0) cfg.torrent_port else torrentPort(snap_listen.settings.torrent.effectivePort());
    const listen = torrent_listen.format(
        &listen_buf,
        snap_listen.settings.network.torrent.address,
        snap_listen.settings.network.torrent.interface,
        listen_port,
    );
    const dht = snap_listen.settings.torrent.dhtEnabled();
    snap_listen.release();
    var torrent_ses = torrent_session.Session.openFrom(listen, dht) catch |err| {
        try fail(stderr, "torrent session: {s}", .{@errorName(err)});
        unreachable;
    };
    defer torrent_ses.close();
    var torrent_watch_map = torrent_watch.Watch.init(gpa);
    defer torrent_watch_map.deinit();
    const torrent_sources: torrent_source_repo.Repo = .{ .store = store, .key = services.source_repo.key };
    var torrent_bridge: TorrentBridge = .{
        .services = &services,
        .io = io,
        .ses = &torrent_ses,
        .watch = &torrent_watch_map,
        .sources = torrent_sources,
    };
    services.add.deps.torrents = .{ .ctx = &torrent_bridge, .add_fn = TorrentBridge.add };
    var torrent_dirs: TorrentDirs = .{ .watch = &torrent_watch_map, .services = &services };
    services.work_dirs.torrent = .{ .ctx = &torrent_dirs, .dir_fn = TorrentDirs.dir };
    reloadTorrents(gpa, io, torrent_sources, &torrent_watch_map, &torrent_ses, &services);
    torrent_poll_stop.store(false, .release);
    torrent_poll_done.store(false, .release);
    var torrent_poll: TorrentPoll = .{
        .gpa = gpa,
        .io = io,
        .services = &services,
        .ses = &torrent_ses,
        .watch = &torrent_watch_map,
    };
    var torrent_follow = try io.concurrent(followTorrents, .{&torrent_poll});
    defer {
        torrent_poll_stop.store(true, .release);
        torrent_follow.cancel(io);
        var spins: u32 = 0;
        while (!torrent_poll_done.load(.acquire) and spins < 40) : (spins += 1) {
            io.sleep(.fromMilliseconds(25), .awake) catch break;
        }
    }
    try services.register(&server.routes);
    const snap = services.settings.current();
    const sab_complete = try gpa.dupe(u8, snap.settings.paths.usenet.complete);
    const sab_incomplete = try gpa.dupe(u8, snap.settings.paths.usenet.incomplete);
    snap.release();
    defer gpa.free(sab_complete);
    defer gpa.free(sab_incomplete);
    var sab_live = sabnzbd_live.Live.init(gpa);
    defer sab_live.deinit();
    var sab_feed: SabFeed = .{ .live = &sab_live, .queue = services.usenet.queueOf() };
    sab_live_stop.store(false, .release);
    sab_live_done.store(false, .release);
    var sab_follow = try io.concurrent(followSab, .{ &sab_live, services.bus });
    defer {
        sab_live_stop.store(true, .release);
        sab_follow.cancel(io);
        var spins: u32 = 0;
        while (!sab_live_done.load(.acquire) and spins < 40) : (spins += 1) {
            io.sleep(.fromMilliseconds(25), .awake) catch break;
        }
    }
    var sab_handler: sabnzbd.Handler = .{
        .keys = key_check.adapter(),
        .cat_names = .{ .ctx = &services.categories, .list_fn = listSabCats },
        .paths = .{ .complete = sab_complete, .incomplete = sab_incomplete },
        .disk = .{ .space_fn = sabDisk },
        .adder = .{ .ctx = &services.add, .add_url_fn = sabAddUrl, .add_file_fn = sabAddFile },
        .downloads = .{ .ctx = &services.downloads, .all_fn = sabAllDownloads, .pause_fn = sabPause, .resume_fn = sabResume, .delete_fn = sabDelete, .update_fn = sabUpdate },
        .cat_name = .{ .ctx = &services.categories, .name_fn = sabCatName },
        .speed = .{ .ctx = &sab_feed, .read_fn = sabSpeed },
        .feed = .{ .ctx = &sab_feed, .stage_fn = sabStage, .rate_fn = sabRate },
    };
    try sabnzbd.register(&server.routes, &sab_handler);
    var qbit_handler: qbit.Handler = .{
        .keys = &keys,
        .sessions = &qbit_sessions,
        .ops = .{
            .ctx = &services,
            .add_magnet_fn = qbitAddMagnet,
            .add_file_fn = qbitAddFile,
            .list_fn = qbitList,
            .act_fn = qbitAct,
            .prefs_fn = qbitPrefs,
            .cats_fn = qbitCats,
            .ensure_cat_fn = qbitEnsureCat,
            .set_category_fn = qbitSetCategory,
            .move_fn = qbitMove,
            .priority_fn = qbitPriority,
            .share_fn = qbitShare,
        },
    };
    try qbit.register(&server.routes, &qbit_handler);
    try web.register(&server.routes);
    try services.start();

    const port = server.listen(cfg.listen) catch |err| {
        try fail(stderr, "listen {s}: {s}", .{ cfg.listen, @errorName(err) });
        unreachable;
    };
    var addr_buf: [128]u8 = undefined;
    logger.info("listening", &.{log.Attr.string("addr", boundAddr(&addr_buf, cfg.listen, port))});

    installSignalHandlers();
    var serving = try io.concurrent(http.Server.serve, .{server});
    while (!stop_requested.load(.acquire)) io.sleep(.fromMilliseconds(50), .awake) catch break;
    serving.cancel(io) catch {};
    services.stop();
    services.closeStreams();

    logger.info("shutdown complete", &.{});
    return 0;
}

/// Finds 7zz and par2 on PATH so a missing one stops the daemon with a clear
/// message instead of failing the first Download (internal/daemon/tools.go).
fn locateTools(gpa: std.mem.Allocator, io: Io, stderr: *Io.Writer) !services_mod.Tools {
    const seven = tool.locate(gpa, io, "7zz") catch |err| switch (err) {
        error.NotFound => null,
        error.OutOfMemory => return err,
    };
    const par = tool.locate(gpa, io, "par2") catch |err| switch (err) {
        error.NotFound => null,
        error.OutOfMemory => return err,
    };
    if (seven != null and par != null) return .{ .seven_zip = seven.?, .par2 = par.? };
    defer if (seven) |p| gpa.free(p);
    defer if (par) |p| gpa.free(p);
    var buf: std.ArrayList(u8) = .empty;
    defer buf.deinit(gpa);
    if (seven == null) {
        const msg = try tool.notFoundMessage(gpa, "7zz");
        defer gpa.free(msg);
        try buf.appendSlice(gpa, msg);
    }
    if (par == null) {
        const msg = try tool.notFoundMessage(gpa, "par2");
        defer gpa.free(msg);
        if (buf.items.len > 0) try buf.append(gpa, '\n');
        try buf.appendSlice(gpa, msg);
    }
    try fail(stderr, "daemon: post-processing tools are missing: {s}", .{buf.items});
    unreachable;
}

/// "host:port" with the port that was actually bound (the setting may say 0).
fn boundAddr(buf: []u8, listen: []const u8, port: u16) []const u8 {
    const host = if (config.splitHostPort(listen)) |hp| hp.host else "";
    const shown = if (host.len == 0) "0.0.0.0" else host;
    return std.fmt.bufPrint(buf, "{s}:{d}", .{ shown, port }) catch listen;
}

fn installSignalHandlers() void {
    const act: std.posix.Sigaction = .{ .handler = .{ .handler = onSignal }, .mask = std.posix.sigemptyset(), .flags = 0 };
    std.posix.sigaction(.INT, &act, null);
    std.posix.sigaction(.TERM, &act, null);
}

/// Creates the config dir and checks that bitnews can write in it. bitnews
/// runs as whoever starts it and cannot change an owner (ADR-0006), so a
/// directory it may not write to is a start-up error that says which user it
/// runs as and how to fix it. The usual cause is Docker: it creates a missing
/// bind-mount directory on the host as root.
fn prepareConfigDir(io: Io, dir: []const u8, stderr: *Io.Writer) !void {
    const created = Io.Dir.cwd().createDirPathStatus(io, dir, .fromMode(0o700));
    if (created) |_| {
        if (Io.Dir.accessAbsolute(io, dir, .{ .write = true, .execute = true })) |_| return else |_| {}
    } else |err| switch (err) {
        error.PermissionDenied, error.AccessDenied => {},
        else => {
            try fail(stderr, "create config dir: {s}", .{@errorName(err)});
            unreachable;
        },
    }
    const uid = std.c.getuid();
    const gid = std.c.getgid();
    try fail(stderr, "the config dir \"{s}\" is not writable by uid {d} gid {d}, the user bitnews runs as. " ++
        "bitnews cannot change the owner itself. With the Docker image, drop `user:` and set PUID/PGID instead, " ++
        "or give that user the directory on the host, for example:\n" ++
        "  sudo mkdir -p <host dir> && sudo chown {d}:{d} <host dir>\n" ++
        "where <host dir> is what you mount at {s} (Docker creates a missing bind-mount directory as root)", .{ dir, uid, gid, uid, gid, dir });
}

/// Adapts the daemon's logger to the HTTP server's Logger.
const HttpLog = struct {
    logger: log.Logger,

    fn adapter(self: *HttpLog) common.Logger {
        return .{ .ctx = self, .log = write };
    }

    fn write(ctx: *anyopaque, level: common.Level, msg: []const u8, attrs: []const common.Attr) void {
        const self: *HttpLog = @ptrCast(@alignCast(ctx));
        var buf: [16]log.Attr = undefined;
        const n = @min(attrs.len, buf.len);
        for (attrs[0..n], buf[0..n]) |a, *out| out.* = switch (a.value) {
            .str => |v| log.Attr.string(a.key, v),
            .int => |v| log.Attr.int(a.key, v),
            .uint => |v| log.Attr.uint(a.key, v),
            .duration_ns => |v| log.Attr.durationNs(a.key, @intCast(@min(v, std.math.maxInt(i64)))),
        };
        const lvl: log.Level = switch (level) {
            .debug => .debug,
            .info => .info,
            .warn => .warn,
            .err => .err,
        };
        self.logger.log(lvl, msg, buf[0..n]);
    }
};

fn sabAddFields(req_priority: []const u8, req_cat: []const u8) struct { prio: []const u8, paused: bool, cat: []const u8 } {
    const parsed = @import("sabnzbd_add.zig").parsePriority(req_priority);
    const cat = std.mem.trim(u8, req_cat, &std.ascii.whitespace);
    return .{
        .prio = @tagName(parsed.priority),
        .paused = parsed.paused,
        .cat = if (cat.len == 0 or std.mem.eql(u8, cat, "*")) "" else cat,
    };
}

fn sabAddUrl(ctx: ?*anyopaque, req: sabnzbd.AddUrlReq, arena: std.mem.Allocator, rejected: *app_error.Failure) anyerror!download.Download {
    const svc: *add_service.AddService = @ptrCast(@alignCast(ctx.?));
    const fields = sabAddFields(req.priority, req.cat);
    var password: ?secret_mod.Secret = null;
    defer if (password) |*s| s.deinit();
    if (req.password.len > 0) password = try secret_mod.Secret.init(arena, req.password);
    return svc.add(arena, rejected, .{
        .url = req.url,
        .name = req.nzbname,
        .category_name = fields.cat,
        .priority = fields.prio,
        .password = password,
        .paused = fields.paused,
        .only = .nzb,
    });
}

fn sabAddFile(ctx: ?*anyopaque, req: sabnzbd.AddFileReq, arena: std.mem.Allocator, rejected: *app_error.Failure) anyerror!download.Download {
    const svc: *add_service.AddService = @ptrCast(@alignCast(ctx.?));
    const fields = sabAddFields(req.priority, req.cat);
    var password: ?secret_mod.Secret = null;
    defer if (password) |*s| s.deinit();
    if (req.password.len > 0) password = try secret_mod.Secret.init(arena, req.password);
    const upload = try svc.receive(arena, rejected, req.bytes, req.file_name, .nzb);
    return svc.add(arena, rejected, .{
        .upload = upload,
        .name = req.nzbname,
        .file_name = req.file_name,
        .category_name = fields.cat,
        .priority = fields.prio,
        .password = password,
        .paused = fields.paused,
        .only = .nzb,
    });
}

fn sabAllDownloads(ctx: ?*anyopaque, arena: std.mem.Allocator) anyerror![]download.Download {
    const svc: *downloads_service.Service = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    return svc.all(arena, &failure, .{});
}

fn sabQuiet(err: anyerror) anyerror!void {
    if (err == error.NotFound or err == error.Conflict) return;
    return err;
}

fn sabPause(ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8) anyerror!void {
    const svc: *downloads_service.Service = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    _ = svc.pause(arena, &failure, id) catch |err| return sabQuiet(err);
}

fn sabResume(ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8) anyerror!void {
    const svc: *downloads_service.Service = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    _ = svc.resume_(arena, &failure, id) catch |err| return sabQuiet(err);
}

fn sabDelete(ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8, delete_data: bool) anyerror!void {
    const svc: *downloads_service.Service = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    svc.delete(arena, &failure, id, delete_data) catch |err| return sabQuiet(err);
}

fn sabUpdate(ctx: ?*anyopaque, arena: std.mem.Allocator, id: []const u8, priority: ?[]const u8, paused: bool, move: []const u8) anyerror!void {
    const svc: *downloads_service.Service = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    if (priority != null or move.len > 0) {
        var patch: downloads_service.Patch = .{ .move = move };
        patch.priority = priority;
        _ = svc.update(arena, &failure, id, patch) catch |err| return sabQuiet(err);
    }
    if (paused) _ = svc.pause(arena, &failure, id) catch |err| return sabQuiet(err);
}

const SabFeed = struct {
    live: *sabnzbd_live.Live,
    queue: *@import("queue.zig").Queue,
};

fn sabSpeed(ctx: ?*anyopaque) sabnzbd.SpeedSample {
    const feed: *SabFeed = @ptrCast(@alignCast(ctx.?));
    return .{ .bytes_per_sec = feed.live.speed(), .paused = feed.queue.isPaused() };
}

fn sabStage(ctx: ?*anyopaque, id: []const u8) ?event.Stage {
    const feed: *SabFeed = @ptrCast(@alignCast(ctx.?));
    return feed.live.stage(id);
}

fn sabRate(ctx: ?*anyopaque, id: []const u8) i64 {
    const feed: *SabFeed = @ptrCast(@alignCast(ctx.?));
    return feed.live.rate(id);
}

/// Applies bus events to the SABnzbd live cache until shutdown. A dropped
/// subscription (missed events) clears the cache and subscribes again, as
/// the Go tracker does.
fn followSab(live: *sabnzbd_live.Live, bus: *events_bus.Bus) void {
    defer sab_live_done.store(true, .release);
    while (!sab_live_stop.load(.acquire)) {
        const sub = bus.subscribe() catch return;
        defer sub.deinit();
        var missed = false;
        while (!sab_live_stop.load(.acquire)) {
            const item = sub.next(250) catch |err| switch (err) {
                error.Timeout => continue,
                error.Closed => {
                    missed = sub.reason() != null;
                    break;
                },
                else => return,
            };
            defer item.release();
            live.apply(item.event());
        }
        if (sab_live_stop.load(.acquire) or !missed) return;
        live.reset();
    }
}

fn sabCatName(ctx: ?*anyopaque, arena: std.mem.Allocator, category_id: []const u8) []const u8 {
    const svc: *@import("categories_service.zig").Service = @ptrCast(@alignCast(ctx.?));
    const list = svc.list(arena) catch return "";
    for (list) |c| if (std.mem.eql(u8, c.id, category_id)) return c.name;
    return "";
}

fn sabDisk(_: ?*anyopaque, path: []const u8) sabnzbd.DiskSample {
    const s = fsutil_space.diskSpace(path) catch return .{};
    return .{
        .free = std.math.cast(i64, s.free) orelse std.math.maxInt(i64),
        .total = std.math.cast(i64, s.total) orelse std.math.maxInt(i64),
        .ok = true,
    };
}

fn listSabCats(ctx: ?*anyopaque, arena: std.mem.Allocator) anyerror![]const []const u8 {
    const svc: *@import("categories_service.zig").Service = @ptrCast(@alignCast(ctx.?));
    const list = try svc.list(arena);
    const names = try arena.alloc([]const u8, list.len);
    for (list, names) |c, *n| n.* = c.name;
    return names;
}

/// Adapts the app key service to the HTTP server's KeyChecker.
const KeyCheck = struct {
    service: *appkey.Service,

    fn adapter(self: *KeyCheck) common.KeyChecker {
        return .{ .ctx = self, .check = check };
    }

    fn check(ctx: *anyopaque, presented: []const u8) bool {
        const self: *KeyCheck = @ptrCast(@alignCast(ctx));
        return self.service.authenticate(presented);
    }
};

/// Magnet adds and .torrent file adds. A file already has metadata, so it is queued.
const TorrentBridge = struct {
    services: *services_mod.Services,
    io: Io,
    ses: *torrent_session.Session,
    watch: *torrent_watch.Watch,
    sources: torrent_source_repo.Repo,

    fn add(ctx: ?*anyopaque, arena: std.mem.Allocator, f: *app_error.Failure, req: add_service.TorrentRequest) !download.Download {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        return switch (req.source) {
            .magnet => |m| blk: {
                const d = try torrent_add.addMagnet(
                    arena,
                    f,
                    m,
                    req.name,
                    req.category_id,
                    req.priority,
                    req.paused,
                    .{ .ctx = self.ses, .add_magnet_fn = torrent_session.addMagnet },
                    self.store(),
                );
                offerMagnetPeers(self.ses, m);
                break :blk d;
            },
            .file => |bytes| torrent_add.addFile(
                arena,
                f,
                bytes,
                req.name,
                req.category_id,
                req.priority,
                req.paused,
                .{
                    .ctx = self.ses,
                    .add_magnet_fn = torrent_session.addMagnet,
                    .add_torrent_fn = torrent_session.addTorrent,
                    .pause_fn = torrent_session.pauseHash,
                },
                self.fileStore(),
            ),
        };
    }

    fn store(self: *TorrentBridge) torrent_add.Store {
        return .{
            .ctx = self,
            .now_fn = now,
            .new_id_fn = newId,
            .incomplete_fn = incomplete,
            .create_fn = create,
            .publish_fn = publish,
            .remember_fn = remember,
            .source_fn = source,
            .delete_fn = deleteDownload,
            .io = self.io,
        };
    }

    fn fileStore(self: *TorrentBridge) torrent_add.Store {
        var base = self.store();
        base.source_fn = null;
        base.file_source_fn = fileSource;
        base.meta_fn = noteSize;
        return base;
    }

    fn now(ctx: ?*anyopaque) i64 {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        return self.services.downloads.deps.now_fn(self.io);
    }

    fn newId(ctx: ?*anyopaque) [id_mod.length]u8 {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        return id_mod.new(self.io);
    }

    fn incomplete(ctx: ?*anyopaque, arena: std.mem.Allocator) ![]const u8 {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        const snap = self.services.settings.current();
        defer snap.release();
        return arena.dupe(u8, snap.settings.paths.torrent.incomplete);
    }

    fn create(ctx: ?*anyopaque, d: download.Download) !void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        try self.services.downloads.repo.create(d);
    }

    fn publish(ctx: ?*anyopaque, d: download.Download) void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        self.services.downloads.deps.publisher.publish(.{ .download_added = d });
    }

    fn remember(ctx: ?*anyopaque, id: []const u8, hash: [20]u8) void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        self.watch.note(id, hash) catch {};
    }

    fn source(ctx: ?*anyopaque, id: []const u8, hash: [20]u8, magnet: []const u8) !void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        try self.sources.createMagnet(self.services.gpa, id, hash, magnet);
    }

    fn fileSource(ctx: ?*anyopaque, id: []const u8, hash: [20]u8, path: []const u8) !void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        try self.sources.createFile(id, hash, path);
    }

    fn noteSize(ctx: ?*anyopaque, id: []const u8, hash: [20]u8, replace_name: bool) void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        var buf: [256]u8 = undefined;
        const st = self.ses.status(&hash, &buf) orelse return;
        const name: []const u8 = if (replace_name) st.name else "";
        self.services.downloads.repo.updateDisplay(id, name, st.total_wanted) catch {};
    }

    fn deleteDownload(ctx: ?*anyopaque, id: []const u8) void {
        const self: *TorrentBridge = @ptrCast(@alignCast(ctx.?));
        self.services.downloads.repo.delete(id) catch {};
    }
};

const TorrentPoll = struct {
    gpa: std.mem.Allocator,
    io: Io,
    services: *services_mod.Services,
    ses: *torrent_session.Session,
    watch: *torrent_watch.Watch,
    name_buf: [256]u8 = undefined,

    fn status(ctx: ?*anyopaque, hash: *const [20]u8) ?torrent_watch.Live {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        const st = self.ses.status(hash, &self.name_buf) orelse return null;
        return .{ .has_metadata = st.has_metadata, .total_wanted = st.total_wanted, .total_done = st.total_done, .name = st.name };
    }

    fn queueLimit(self: *TorrentPoll) u32 {
        const snap = self.services.settings.current();
        defer snap.release();
        const n = snap.settings.queues.torrent;
        if (n <= 0) return 0;
        if (n > 1000) return 1000;
        return @intCast(n);
    }

    fn display(ctx: ?*anyopaque, id: []const u8, name: []const u8, total_bytes: i64) !void {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        try self.services.downloads.repo.updateDisplay(id, name, total_bytes);
    }

    fn saveMeta(ctx: ?*anyopaque, id: []const u8, hash: *const [20]u8, name: []const u8) !void {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        const sources: torrent_source_repo.Repo = .{ .store = self.services.store, .key = self.services.source_repo.key };
        var existing = try sources.get(self.gpa, id);
        defer existing.deinit(self.gpa);
        if (existing.torrent_path.len > 0) return;
        const bytes = try self.ses.readTorrent(self.gpa, hash);
        defer self.gpa.free(bytes);
        const dir = if (try self.watch.location(self.io, self.gpa, id)) |path|
            path
        else blk: {
            const snap = self.services.settings.current();
            defer snap.release();
            break :blk try std.fs.path.join(self.gpa, &.{ snap.settings.paths.torrent.incomplete, id });
        };
        defer self.gpa.free(dir);
        const path = try torrent_meta.write(self.gpa, self.io, dir, name, bytes);
        defer self.gpa.free(path);
        try sources.setTorrentPath(id, path);
    }

    fn queue(ctx: ?*anyopaque, id: []const u8) !void {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        var arena_state: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena_state.deinit();
        const d = try self.services.downloads.repo.updateState(arena_state.allocator(), id, .{
            .to = .queued,
            .at = self.services.downloads.deps.now_fn(self.io),
        });
        self.services.downloads.deps.publisher.publish(.{ .download_updated = d });
    }

    fn activate(ctx: ?*anyopaque, id: []const u8) !void {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        var arena_state: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena_state.deinit();
        const d = try self.services.downloads.repo.updateState(arena_state.allocator(), id, .{
            .to = .fetching,
            .at = self.services.downloads.deps.now_fn(self.io),
        });
        self.services.downloads.deps.publisher.publish(.{ .download_updated = d });
    }

    fn move(ctx: ?*anyopaque, id: []const u8, hash: *const [20]u8) !void {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        var arena_state: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena_state.deinit();
        const arena = arena_state.allocator();
        const d = try self.services.downloads.repo.get(arena, id);
        var diag: @import("postprocess_ports.zig").Diag = .{};
        const dest = try self.services.finisher.destination(arena, self.io, d, &diag);
        const parent = std.fs.path.dirname(dest) orelse return error.BadDest;
        Io.Dir.cwd().createDirPath(self.io, parent) catch return error.Dir;
        const dest_z = try arena.dupeSentinel(u8, dest, 0);
        if (self.ses.moveStorage(hash, dest_z) != 0) return error.MoveFailed;
        const sources: torrent_source_repo.Repo = .{ .store = self.services.store, .key = self.services.source_repo.key };
        sources.markMoved(id, dest) catch {};
        try self.watch.place(self.io, id, dest);
    }

    fn fetch(ctx: ?*anyopaque, id: []const u8, done: i64, total: i64) !void {
        const self: *TorrentPoll = @ptrCast(@alignCast(ctx.?));
        try self.services.downloads.repo.updateProgress(id, done, total);
        var arena_state: std.heap.ArenaAllocator = .init(self.gpa);
        defer arena_state.deinit();
        const d = try self.services.downloads.repo.updateState(arena_state.allocator(), id, .{
            .to = .fetched,
            .at = self.services.downloads.deps.now_fn(self.io),
        });
        self.services.downloads.deps.publisher.publish(.{ .download_updated = d });
        self.services.downloads.deps.publisher.publish(.{ .fetched = d.id });
    }
};

const TorrentDirs = struct {
    watch: *torrent_watch.Watch,
    services: *services_mod.Services,

    fn dir(ctx: *anyopaque, arena: std.mem.Allocator, d: download.Download) ![]const u8 {
        const self: *TorrentDirs = @ptrCast(@alignCast(ctx));
        if (try self.watch.location(self.services.io, arena, d.id)) |path| return path;
        const snap = self.services.settings.current();
        defer snap.release();
        return std.fs.path.join(arena, &.{ snap.settings.paths.torrent.incomplete, d.id });
    }
};

fn qbitPrefs(ctx: ?*anyopaque, arena: std.mem.Allocator) !qbit.Prefs {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const snap = services.settings.current();
    defer snap.release();
    const s = snap.settings;
    const seeding = s.seeding;
    const action = seeding.effectiveAction();
    const act: i64 = if (std.mem.eql(u8, action, "remove_data")) 3 else if (std.mem.eql(u8, action, "remove")) 1 else 0;
    const mse = s.torrent.effectiveMse();
    const enc: i64 = if (std.mem.eql(u8, mse, "require")) 1 else if (std.mem.eql(u8, mse, "disable")) 2 else 0;
    const save_path = try arena.dupe(u8, s.paths.torrent.complete);
    const temp_path = try arena.dupe(u8, s.paths.torrent.incomplete);
    const same = std.mem.eql(u8, save_path, temp_path);
    const max_active = s.queues.torrent;
    const ratio_on = seeding.ratio_limit > 0;
    const ratio = if (ratio_on) seeding.ratio_limit else -1;
    const seed_on = seeding.seeding_time_minutes > 0;
    const seed_min = if (seed_on) seeding.seeding_time_minutes else -1;
    const dht = s.torrent.dhtEnabled();
    const pex = s.torrent.pexEnabled();
    const lsd = s.torrent.lsd;
    const listen_port = s.torrent.effectivePort();
    return .{
        .save_path = save_path,
        .temp_path = temp_path,
        .temp_path_enabled = !same,
        .max_active_downloads = max_active,
        .max_ratio_enabled = ratio_on,
        .max_ratio = ratio,
        .max_ratio_act = act,
        .max_seeding_time_enabled = seed_on,
        .max_seeding_time = seed_min,
        .dht = dht,
        .pex = pex,
        .lsd = lsd,
        .listen_port = listen_port,
        .encryption = enc,
    };
}

fn qbitCats(ctx: ?*anyopaque, arena: std.mem.Allocator) ![]qbit.Cat {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const snap = services.settings.current();
    defer snap.release();
    const root = snap.settings.paths.torrent.complete;
    const cats = try services.categories.list(arena);
    const out = try arena.alloc(qbit.Cat, cats.len);
    for (cats, out) |c, *slot| {
        const path = if (c.torrent_dir.len > 0) c.torrent_dir else try std.fs.path.join(arena, &.{ root, c.name });
        slot.* = .{ .id = c.id, .name = c.name, .save_path = path };
    }
    return out;
}

fn qbitSetCategory(ctx: ?*anyopaque, arena: std.mem.Allocator, hash: []const u8, category_id: []const u8) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const id = (try qbitRepo(services).findByHash(arena, hash)) orelse return;
    var failure: app_error.Failure = .{};
    _ = services.downloads.update(arena, &failure, id, .{ .category = .{ .set = true, .id = category_id } }) catch |err| {
        if (err == error.NotFound or err == error.Rejected or err == error.Conflict) return;
        return err;
    };
}

fn qbitMove(ctx: ?*anyopaque, arena: std.mem.Allocator, hash: []const u8, move: []const u8) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const id = (try qbitRepo(services).findByHash(arena, hash)) orelse return;
    var failure: app_error.Failure = .{};
    _ = services.downloads.update(arena, &failure, id, .{ .move = move }) catch |err| {
        if (err == error.NotFound or err == error.Rejected or err == error.Conflict) return;
        return err;
    };
}

fn qbitPriority(ctx: ?*anyopaque, arena: std.mem.Allocator, hash: []const u8, priority: []const u8) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const id = (try qbitRepo(services).findByHash(arena, hash)) orelse return;
    var failure: app_error.Failure = .{};
    _ = services.downloads.update(arena, &failure, id, .{ .priority = priority }) catch |err| {
        if (err == error.NotFound or err == error.Rejected or err == error.Conflict) return;
        return err;
    };
}

fn qbitShare(ctx: ?*anyopaque, arena: std.mem.Allocator, hash: []const u8, ratio: f64, minutes: f64) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const id = (try qbitRepo(services).findByHash(arena, hash)) orelse return;
    try qbitRepo(services).setShareLimits(id, qbitLimitReal(ratio), qbitLimitSeconds(minutes));
}

fn qbitLimitReal(v: f64) ?f64 {
    if (v == -2) return null;
    if (v < 0) return -1;
    return v;
}

fn qbitLimitSeconds(minutes: f64) ?i64 {
    if (minutes == -2) return null;
    if (minutes < 0) return -1;
    return @intFromFloat(minutes * 60);
}

fn qbitRepo(services: *services_mod.Services) torrent_source_repo.Repo {
    return .{ .store = services.store, .key = services.source_repo.key };
}

fn qbitEnsureCat(ctx: ?*anyopaque, arena: std.mem.Allocator, name: []const u8) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    _ = services.categories.ensure(arena, &failure, name) catch |err| switch (err) {
        error.Rejected => return error.BadName,
        else => return err,
    };
}

fn qbitAddFile(ctx: ?*anyopaque, arena: std.mem.Allocator, bytes: []const u8, file_name: []const u8, paused: bool, category: []const u8) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    const up = try services.add.receive(arena, &failure, bytes, file_name, .torrent);
    defer up.discard(services.io);
    _ = try services.add.add(arena, &failure, .{
        .upload = up,
        .only = .torrent,
        .paused = paused,
        .category_name = category,
        .file_name = file_name,
    });
}

fn qbitAddMagnet(ctx: ?*anyopaque, arena: std.mem.Allocator, magnet: []const u8, paused: bool, category: []const u8) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    var failure: app_error.Failure = .{};
    _ = try services.add.add(arena, &failure, .{
        .url = magnet,
        .only = .torrent,
        .paused = paused,
        .category_name = category,
    });
}

fn qbitList(ctx: ?*anyopaque, arena: std.mem.Allocator) ![]qbit.Listed {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const repo: torrent_source_repo.Repo = .{ .store = services.store, .key = services.source_repo.key };
    const rows = try repo.listTorrents(arena);
    const out = try arena.alloc(qbit.Listed, rows.len);
    for (rows, out) |row, *slot| {
        slot.* = .{
            .hash = row.hash,
            .name = row.name,
            .state = row.state,
            .category = row.category,
            .paused = row.paused,
            .total_bytes = row.total_bytes,
            .done_bytes = row.done_bytes,
            .added_at = row.added_at,
        };
    }
    return out;
}

fn qbitAct(ctx: ?*anyopaque, arena: std.mem.Allocator, hash: []const u8, act: qbit.Act) !void {
    const services: *services_mod.Services = @ptrCast(@alignCast(ctx.?));
    const repo: torrent_source_repo.Repo = .{ .store = services.store, .key = services.source_repo.key };
    const id = (try repo.findByHash(arena, hash)) orelse return;
    var failure: app_error.Failure = .{};
    const result = switch (act) {
        .pause => services.downloads.pause(arena, &failure, id),
        .unpause => services.downloads.resume_(arena, &failure, id),
        .delete_keep => {
            services.downloads.delete(arena, &failure, id, false) catch |err| return if (err == error.NotFound or err == error.Conflict) {} else err;
            return;
        },
        .delete_data => {
            services.downloads.delete(arena, &failure, id, true) catch |err| return if (err == error.NotFound or err == error.Conflict) {} else err;
            return;
        },
    };
    if (result) |_| {} else |err| {
        if (err != error.NotFound and err != error.Rejected and err != error.Conflict) return err;
    }
}

fn torrentPort(port: i64) u16 {
    if (port <= 0 or port > 65535) return 51413;
    return @intCast(port);
}

fn offerMagnetPeers(ses: *torrent_session.Session, magnet: []const u8) void {
    const hash = torrent_add.v1Hash(magnet) orelse return;
    var found: [torrent_peers.max_peers][]const u8 = undefined;
    const n = torrent_peers.hints(magnet, &found);
    var z: [129]u8 = undefined;
    for (found[0..n]) |addr| {
        if (addr.len >= z.len) continue;
        @memcpy(z[0..addr.len], addr);
        z[addr.len] = 0;
        _ = ses.addPeer(&hash, z[0..addr.len :0]);
    }
}

fn reloadTorrents(
    gpa: std.mem.Allocator,
    io: Io,
    sources: torrent_source_repo.Repo,
    watch: *torrent_watch.Watch,
    ses: *torrent_session.Session,
    services: *services_mod.Services,
) void {
    var arena_state: std.heap.ArenaAllocator = .init(gpa);
    defer arena_state.deinit();
    const arena = arena_state.allocator();
    const active = sources.listActive(arena) catch null;
    const moved = sources.listMoved(arena) catch null;
    defer if (active) |rows| for (rows) |row| if (row.magnet.len > 0) std.crypto.secureZero(u8, row.magnet);
    defer if (moved) |rows| for (rows) |row| if (row.magnet.len > 0) std.crypto.secureZero(u8, row.magnet);
    const n_active = if (active) |rows| rows.len else 0;
    const n_moved = if (moved) |rows| rows.len else 0;
    if (n_active + n_moved == 0) return;
    const resumes = arena.alloc(torrent_watch.Resume, n_active + n_moved) catch return;
    var n: usize = 0;
    if (active) |rows| for (rows) |row| {
        resumes[n] = .{ .id = row.id, .hash = row.hash, .magnet = row.magnet, .paused = row.paused };
        n += 1;
    };
    if (moved) |rows| for (rows) |row| {
        watch.place(io, row.id, row.location) catch {};
        resumes[n] = .{
            .id = row.id,
            .hash = row.hash,
            .magnet = row.magnet,
            .paused = row.paused,
            .save_path = row.location,
        };
        n += 1;
    };
    const snap = services.settings.current();
    defer snap.release();
    var rel: TorrentReload = .{
        .io = io,
        .ses = ses,
        .incomplete = snap.settings.paths.torrent.incomplete,
        .arena = arena,
    };
    watch.reload(resumes, .{ .ctx = &rel, .add_fn = TorrentReload.add, .pause_fn = TorrentReload.pause });
}

const TorrentReload = struct {
    io: Io,
    ses: *torrent_session.Session,
    incomplete: []const u8,
    arena: std.mem.Allocator,

    fn add(ctx: ?*anyopaque, magnet: []const u8, id: []const u8, save_path: []const u8) !void {
        const self: *TorrentReload = @ptrCast(@alignCast(ctx.?));
        const save = if (save_path.len > 0) save_path else try std.fs.path.join(self.arena, &.{ self.incomplete, id });
        Io.Dir.cwd().createDirPath(self.io, save) catch return error.Dir;
        const magnet_z = try self.arena.dupeSentinel(u8, magnet, 0);
        const save_z = try self.arena.dupeSentinel(u8, save, 0);
        if (self.ses.addMagnet(magnet_z, save_z) != 0) return error.AddFailed;
        offerMagnetPeers(self.ses, magnet);
    }

    fn pause(ctx: ?*anyopaque, hash: *const [20]u8) void {
        const self: *TorrentReload = @ptrCast(@alignCast(ctx.?));
        _ = self.ses.pause(hash);
    }
};

fn followTorrents(poll: *TorrentPoll) void {
    defer torrent_poll_done.store(true, .release);
    var arena_state: std.heap.ArenaAllocator = .init(poll.gpa);
    defer arena_state.deinit();
    while (!torrent_poll_stop.load(.acquire)) {
        _ = arena_state.reset(.retain_capacity);
        const arena = arena_state.allocator();
        const page = poll.services.downloads.repo.list(arena, .{
            .state = .fetching_metadata,
            .source_kind = .torrent,
            .limit = 200,
        }) catch {
            poll.io.sleep(.fromMilliseconds(250), .awake) catch break;
            continue;
        };
        const ids = arena.alloc([]const u8, page.items.len) catch continue;
        for (page.items, ids) |d, *slot| slot.* = d.id;
        poll.watch.promote(ids, .{ .ctx = poll, .status_fn = TorrentPoll.status }, .{
            .ctx = poll,
            .display_fn = TorrentPoll.display,
            .queue_fn = TorrentPoll.queue,
            .save_fn = TorrentPoll.saveMeta,
        });
        const queued = poll.services.downloads.repo.list(arena, .{
            .state = .queued,
            .source_kind = .torrent,
            .limit = 200,
        }) catch null;
        const fetching = poll.services.downloads.repo.list(arena, .{
            .state = .fetching,
            .source_kind = .torrent,
            .limit = 200,
        }) catch null;
        const nq = if (queued) |p| p.items.len else 0;
        const nf = if (fetching) |p| p.items.len else 0;
        const jobs = arena.alloc(torrent_watch.Job, nq + nf) catch {
            poll.io.sleep(.fromMilliseconds(250), .awake) catch break;
            continue;
        };
        var n: usize = 0;
        if (queued) |p| for (p.items) |d| {
            jobs[n] = .{ .id = d.id, .phase = .queued, .paused = d.paused };
            n += 1;
        };
        if (fetching) |p| for (p.items) |d| {
            jobs[n] = .{ .id = d.id, .phase = .fetching, .paused = d.paused };
            n += 1;
        };
        poll.watch.drive(jobs, .{ .ctx = poll, .status_fn = TorrentPoll.status }, .{
            .ctx = poll,
            .move_fn = TorrentPoll.move,
            .activate_fn = TorrentPoll.activate,
            .fetch_fn = TorrentPoll.fetch,
        }, poll.queueLimit());
        poll.io.sleep(.fromMilliseconds(250), .awake) catch break;
    }
}

/// The `healthcheck` subcommand (Docker HEALTHCHECK): GET /api/v1/health on
/// the local daemon. Exit 0 and "ok" when it answers 200, 1 when it does not,
/// 2 when BITNEWS_LISTEN is not host:port.
fn healthcheck(io: Io, env: anytype, stdout: *Io.Writer, stderr: *Io.Writer) !u8 {
    const listen = if (env.get("BITNEWS_LISTEN")) |v| (if (v.len > 0) v else "0.0.0.0:9494") else "0.0.0.0:9494";
    const port: u16 = port: {
        const hp = config.splitHostPort(listen) orelse break :port 0;
        break :port std.fmt.parseInt(u16, hp.port, 10) catch 0;
    };
    if (port == 0) {
        try stderr.print("bitnews healthcheck: BITNEWS_LISTEN=\"{f}\": must be host:port\n", .{std.zig.fmtString(listen)});
        return 2;
    }

    const addr = Io.net.IpAddress.parse("127.0.0.1", port) catch return 1;
    // Connecting to the local daemon fails fast or succeeds at once (std's
    // threaded Io has no connect timeout yet); the read has a deadline.
    const stream = addr.connect(io, .{ .mode = .stream }) catch |err| {
        try stderr.print("bitnews healthcheck: {s}\n", .{@errorName(err)});
        return 1;
    };
    defer stream.close(io);

    var wbuf: [256]u8 = undefined;
    var w = stream.writer(io, &wbuf);
    try w.interface.writeAll("GET /api/v1/health HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
    try w.interface.flush();

    var rbuf: [256]u8 = undefined;
    var r: http.TimedReader = .init(io, stream, &rbuf);
    r.deadline = http.deadlineIn(io, healthcheck_timeout_ms);
    const line = r.interface.takeDelimiterExclusive('\n') catch {
        try stderr.writeAll("bitnews healthcheck: no answer\n");
        return 1;
    };
    // "HTTP/1.1 200 OK"
    const ok = std.mem.startsWith(u8, line, "HTTP/1.1 200") or std.mem.startsWith(u8, line, "HTTP/1.0 200");
    if (!ok) {
        const status = if (line.len >= 12) line[9..12] else "?";
        try stderr.print("bitnews healthcheck: unhealthy: status {s}\n", .{status});
        return 1;
    }
    try stdout.writeAll("ok\n");
    return 0;
}
