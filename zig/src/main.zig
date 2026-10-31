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
const services_mod = @import("services.zig");
const tool = @import("tool.zig");
const http = @import("http_server.zig");
const common = @import("http_common.zig");

const version = build_options.version;
const commit = build_options.commit;

/// How long the health probe waits for the daemon (Go: healthcheckTimeout).
const healthcheck_timeout_ms = 3000;

var stop_requested = std.atomic.Value(bool).init(false);

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
    var gate = try setup_gate.Gate.init(store);
    if (cfg.admin_password_file != null) {
        logger.warn("BITNEWS_ADMIN_PASSWORD_FILE is not supported by the Zig build yet; create the admin in the web UI", &.{});
    }

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
    try services.register(&server.routes);
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
