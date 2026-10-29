//! Bootstrap configuration: the settings that must be known before the
//! database is opened (internal/bootstrap of the Go version). Same
//! environment variables, flags, defaults, validation rules and messages;
//! the flag syntax is Go's `flag` package (-x, --x, -x=v, -x v, "--" ends the
//! flags), so a command line that works with the Go binary works here.
//!
//! What main must do (cmd/bitnews/main.go):
//!   1. If the first argument is `healthcheck`, run the health probe and
//!      exit with its own code (0, 1, or 2 for a bad BITNEWS_LISTEN); that
//!      path does not call `load`. `splitHostPort` is for it.
//!   2. Otherwise call `load(arena, init.environ_map, args)` where `args` is
//!      `try init.minimal.args.toSlice(arena)` without its first element
//!      (the program name). `init.environ_map` is a `*std.process.Environ.Map`.
//!   3. Switch on the Result:
//!        .run      -> continue with the Config.
//!        .help     -> write `usage` to stdout, exit 0.
//!        .version  -> write "bitnews <version> (<commit>)\n" to stdout, exit 0.
//!        .invalid  -> write the text to stderr, exit 2.
//!   4. Build the logger from `log_level` and `log_format`, then call
//!      `applyUmask(cfg.umask)` before any file is created.
//! Exit code 1 is for startup and runtime failures after this point.
const std = @import("std");
const log = @import("log.zig");
const text = @import("log_text.zig");
const Allocator = std.mem.Allocator;

/// The validated bootstrap settings. Strings live in the arena given to
/// `load`; the Config must not outlive it.
pub const Config = struct {
    /// Holds bitnews.db. Absolute and clean.
    config_dir: []const u8,
    /// The master key for secret encryption. Absolute, clean, never the
    /// config directory itself.
    key_file: []const u8,
    /// host:port of the single HTTP listener.
    listen: []const u8,
    log_level: log.Level,
    log_format: log.Format,
    /// File mode creation mask, 0 to 0o777, applied process-wide before any
    /// file is created.
    umask: u16,
    /// Optionally bootstraps the admin password. Absolute and clean; null
    /// when unset.
    admin_password_file: ?[]const u8,
    /// The TCP and uTP port of the torrent engine when the operator fixes it
    /// (BITNEWS_TORRENT_PORT); it overrides the torrent.listen_port setting.
    /// Zero means not set: the setting decides, by default 51413.
    torrent_port: u16,
};

pub const Result = union(enum) {
    run: Config,
    /// -h or --help: print `usage`, exit 0.
    help,
    /// --version: print the version line, exit 0. Nothing else is validated.
    version,
    /// A usage error: the complete text for stderr, ending in the hint to run
    /// --help. Exit 2. Allocated in the arena given to `load`.
    invalid: []const u8,
};

/// One row of the bootstrap table. `flag` is null for env-only rows; `arg`
/// and `default` only feed the usage text.
const Option = struct {
    flag: ?[]const u8,
    env: []const u8,
    arg: []const u8,
    usage: []const u8,
    default: []const u8,
};

/// In the order of the Go table, which is the order of the usage text.
const Opt = enum { config_dir, key_file, listen, log_level, log_format, umask, admin_password, torrent_port };

const table = [_]Option{
    .{ .flag = "config-dir", .env = "BITNEWS_CONFIG_DIR", .arg = "dir", .usage = "directory holding bitnews.db", .default = "$XDG_DATA_HOME/bitnews, or ~/.local/share/bitnews" },
    .{ .flag = "key-file", .env = "BITNEWS_KEY_FILE", .arg = "path", .usage = "master key that encrypts stored Server and archive passwords; created on first start", .default = "bitnews.key inside the config dir" },
    .{ .flag = "listen", .env = "BITNEWS_LISTEN", .arg = "host:port", .usage = "address for the API, adapters and UI", .default = "0.0.0.0:9494" },
    .{ .flag = "log-level", .env = "BITNEWS_LOG_LEVEL", .arg = "level", .usage = "debug, info, warn or error", .default = "info" },
    .{ .flag = "log-format", .env = "BITNEWS_LOG_FORMAT", .arg = "format", .usage = "text or json", .default = "text" },
    .{ .flag = null, .env = "UMASK", .arg = "octal", .usage = "file mode creation mask applied at startup, so files are group-writable for Sonarr and Radarr", .default = "002" },
    .{ .flag = null, .env = "BITNEWS_ADMIN_PASSWORD_FILE", .arg = "path", .usage = "absolute path of a file holding the initial admin password", .default = "unset" },
    .{ .flag = null, .env = "BITNEWS_TORRENT_PORT", .arg = "port", .usage = "TCP and uTP port the torrent engine listens on, overriding the setting; forward it for incoming peers (no UPnP)", .default = "the torrent port setting (51413)" },
};

fn opt(o: Opt) Option {
    return table[@backingInt(o)];
}

/// The help text for every bootstrap option.
pub const usage = blk: {
    var s: []const u8 = "Usage: bitnews [options]\n       bitnews healthcheck\n\n" ++
        "healthcheck asks the running daemon on 127.0.0.1 (port from BITNEWS_LISTEN) for /api/v1/health\n" ++
        "and exits 0 on a 200 within 3 seconds, otherwise 1. It is meant for container health checks.\n\n" ++
        "Options (a flag wins over its environment variable; empty variables are ignored):\n";
    for (table) |o| {
        if (o.flag) |f| s = s ++ "  --" ++ f ++ " " ++ o.arg ++ "\n        " ++ o.usage ++ " (env " ++ o.env ++ ", default " ++ o.default ++ ")\n";
    }
    s = s ++ "  --version\n        print version and exit\n";
    s = s ++ "  -h, --help\n        print this help and exit\n";
    s = s ++ "\nEnvironment only:\n";
    for (table) |o| {
        if (o.flag == null) s = s ++ "  " ++ o.env ++ "=" ++ o.arg ++ "\n        " ++ o.usage ++ " (default " ++ o.default ++ ")\n";
    }
    break :blk s;
};

/// A raw value together with where it came from, so an error can name the
/// flag or variable the user has to fix.
const Setting = struct {
    value: []const u8,
    from: []const u8,
};

/// Formats a string as Go's %q, for `{f}`.
const Quoted = struct {
    s: []const u8,
    pub fn format(q: Quoted, w: *std.Io.Writer) std.Io.Writer.Error!void {
        try text.writeQuoted(w, q.s);
    }
};

fn settingError(arena: Allocator, s: Setting, comptime fmt: []const u8, args: anytype) Allocator.Error![]const u8 {
    const reason = try std.fmt.allocPrint(arena, fmt, args);
    return std.fmt.allocPrint(arena, "{s}={f}: {s}", .{ s.from, Quoted{ .s = s.value }, reason });
}

/// The hint every usage error ends with.
fn invalid(arena: Allocator, comptime fmt: []const u8, args: anytype) Allocator.Error!Result {
    const msg = try std.fmt.allocPrint(arena, fmt, args);
    return .{ .invalid = try std.fmt.allocPrint(arena, "bitnews: {s}\nRun 'bitnews --help' for usage.\n", .{msg}) };
}

const FlagError = union(enum) {
    help,
    message: []const u8,
};

/// What the flags of the command line set. A flag given twice keeps its last
/// value; one given with an empty value counts as given.
const Flags = struct {
    values: [table.len]?[]const u8 = @splat(null),
    version: bool = false,
};

/// Go's flag.FlagSet.Parse with ContinueOnError for this table plus --version.
/// Returns the index of the first positional argument (args.len when none).
fn parseFlags(arena: Allocator, args: anytype, flags: *Flags) Allocator.Error!union(enum) { ok: usize, stop: FlagError } {
    var i: usize = 0;
    while (i < args.len) {
        const arg: []const u8 = args[i];
        // A lone "-" or anything not starting with "-" is the first
        // positional argument: parsing stops there.
        if (arg.len < 2 or arg[0] != '-') return .{ .ok = i };
        var minuses: usize = 1;
        if (arg[1] == '-') {
            minuses = 2;
            if (arg.len == 2) return .{ .ok = i + 1 }; // "--" ends the flags
        }
        i += 1;
        const body = arg[minuses..];
        if (body[0] == '-' or body[0] == '=') {
            return .{ .stop = .{ .message = try std.fmt.allocPrint(arena, "bad flag syntax: {s}", .{arg}) } };
        }
        const eq = std.mem.indexOfScalar(u8, body, '=');
        const name = if (eq) |e| body[0..e] else body;
        const value: ?[]const u8 = if (eq) |e| body[e + 1 ..] else null;

        if (std.mem.eql(u8, name, "version")) {
            if (value) |v| {
                flags.version = parseBool(v) orelse return .{ .stop = .{
                    .message = try std.fmt.allocPrint(arena, "invalid boolean value {f} for -version: parse error", .{Quoted{ .s = v }}),
                } };
            } else flags.version = true;
            continue;
        }
        const idx = flagIndex(name) orelse {
            if (std.mem.eql(u8, name, "help") or std.mem.eql(u8, name, "h")) return .{ .stop = .help };
            return .{ .stop = .{ .message = try std.fmt.allocPrint(arena, "flag provided but not defined: -{s}", .{name}) } };
        };
        if (value) |v| {
            flags.values[idx] = v;
        } else if (i < args.len) {
            // The next argument is the value, even if it looks like a flag.
            flags.values[idx] = args[i];
            i += 1;
        } else {
            return .{ .stop = .{ .message = try std.fmt.allocPrint(arena, "flag needs an argument: -{s}", .{name}) } };
        }
    }
    return .{ .ok = i };
}

fn flagIndex(name: []const u8) ?usize {
    for (table, 0..) |o, i| {
        if (o.flag) |f| if (std.mem.eql(u8, f, name)) return i;
    }
    return null;
}

/// strconv.ParseBool.
fn parseBool(s: []const u8) ?bool {
    for ([_][]const u8{ "1", "t", "T", "TRUE", "true", "True" }) |t| if (std.mem.eql(u8, s, t)) return true;
    for ([_][]const u8{ "0", "f", "F", "FALSE", "false", "False" }) |f| if (std.mem.eql(u8, s, f)) return false;
    return null;
}

/// Parses the flags in `args` (without the program name) and the environment
/// into a validated Config. `env` is anything with
/// `get([]const u8) ?[]const u8` (a `*std.process.Environ.Map` or
/// `SliceEnv`); `args` is a slice of strings. The environment is read only
/// through `get`, so tests need no process state.
///
/// Precedence: a flag wins over its environment variable, which wins over the
/// default; an empty variable counts as unset. Everything the Result refers
/// to is allocated in `arena`, so nothing needs to be freed.
pub fn load(arena: Allocator, env: anytype, args: anytype) Allocator.Error!Result {
    var flags: Flags = .{};
    const first_positional = switch (try parseFlags(arena, args, &flags)) {
        .ok => |n| n,
        .stop => |e| switch (e) {
            .help => return .help,
            .message => |m| return invalid(arena, "{s}", .{m}),
        },
    };
    if (flags.version) return .version;
    if (first_positional < args.len) {
        const extra: []const u8 = args[first_positional];
        return invalid(arena, "unexpected argument {f}", .{Quoted{ .s = extra }});
    }

    var cfg: Config = undefined;

    const config_dir = lookup(env, &flags, .config_dir, try defaultConfigDir(arena, env));
    if (!isAbs(config_dir.value)) return invalid(arena, "{s}", .{try settingError(arena, config_dir, "must be an absolute path", .{})});
    cfg.config_dir = try clean(arena, config_dir.value);

    // The key is generated on first start and lives next to the database by
    // default, so there is nothing to set up. A separate location (a mounted
    // secret, say) keeps a copy of the database alone from exposing the
    // stored passwords; it is optional.
    const key_file = lookup(env, &flags, .key_file, try join(arena, &.{ cfg.config_dir, "bitnews.key" }));
    if (!isAbs(key_file.value)) return invalid(arena, "{s}", .{try settingError(arena, key_file, "must be an absolute path", .{})});
    cfg.key_file = try clean(arena, key_file.value);
    if (std.mem.eql(u8, cfg.key_file, cfg.config_dir)) {
        return invalid(arena, "{s}", .{try settingError(arena, key_file, "must be a file, but it is the config directory {f}", .{Quoted{ .s = cfg.config_dir }})});
    }

    const listen = lookup(env, &flags, .listen, "0.0.0.0:9494");
    if (validateListen(listen.value)) |why| return invalid(arena, "{s}", .{try settingError(arena, listen, "{s}", .{why})});
    cfg.listen = try arena.dupe(u8, listen.value);

    const level = lookup(env, &flags, .log_level, "info");
    cfg.log_level = log.Level.parse(level.value) orelse
        return invalid(arena, "{s}", .{try settingError(arena, level, "must be debug, info, warn or error", .{})});

    const format = lookup(env, &flags, .log_format, "text");
    cfg.log_format = log.Format.parse(format.value) orelse
        return invalid(arena, "{s}", .{try settingError(arena, format, "must be text or json", .{})});

    const umask = lookup(env, &flags, .umask, "002");
    cfg.umask = parseUmask(umask.value) orelse
        return invalid(arena, "{s}", .{try settingError(arena, umask, "must be an octal mode from 000 to 777", .{})});

    const admin = lookup(env, &flags, .admin_password, "");
    cfg.admin_password_file = null;
    if (admin.value.len > 0) {
        if (!isAbs(admin.value)) return invalid(arena, "{s}", .{try settingError(arena, admin, "must be an absolute path", .{})});
        cfg.admin_password_file = try clean(arena, admin.value);
    }

    const port = lookup(env, &flags, .torrent_port, "");
    cfg.torrent_port = 0;
    if (port.value.len > 0) {
        const n = parseUint(port.value, 10, std.math.maxInt(u16)) orelse 0;
        if (n == 0) return invalid(arena, "{s}", .{try settingError(arena, port, "must be a port number from 1 to 65535", .{})});
        cfg.torrent_port = @intCast(n);
    }
    return .{ .run = cfg };
}

/// The flag if given, else a non-empty environment variable, else `default`.
fn lookup(env: anytype, flags: *const Flags, o: Opt, default: []const u8) Setting {
    const row = opt(o);
    if (flags.values[@backingInt(o)]) |v| return .{ .value = v, .from = flagFrom(o) };
    if (env.get(row.env)) |v| {
        if (v.len > 0) return .{ .value = v, .from = row.env };
    }
    return .{ .value = default, .from = defaultFrom(o) };
}

fn flagFrom(o: Opt) []const u8 {
    return switch (o) {
        inline else => |c| "--" ++ (table[@backingInt(c)].flag orelse "(env only)"),
    };
}

fn defaultFrom(o: Opt) []const u8 {
    return switch (o) {
        inline else => |c| table[@backingInt(c)].env ++ " (default)",
    };
}

/// $XDG_DATA_HOME/bitnews, or ~/.local/share/bitnews. Per the XDG spec an
/// unset, empty or relative XDG_DATA_HOME is ignored in favour of
/// $HOME/.local/share.
fn defaultConfigDir(arena: Allocator, env: anytype) Allocator.Error![]const u8 {
    const base = if (env.get("XDG_DATA_HOME")) |d| (if (isAbs(d)) d else null) else null;
    const data_home = base orelse try join(arena, &.{ env.get("HOME") orelse "", ".local/share" });
    return join(arena, &.{ data_home, "bitnews" });
}

/// strconv.ParseUint(s, base, bits) for a maximum of `max`: digits only, no
/// sign, no underscores, no empty string, no overflow.
fn parseUint(s: []const u8, base: u8, max: u64) ?u64 {
    if (s.len == 0) return null;
    var n: u64 = 0;
    for (s) |c| {
        const d: u8 = switch (c) {
            '0'...'9' => c - '0',
            else => return null,
        };
        if (d >= base) return null;
        n = std.math.mul(u64, n, base) catch return null;
        n = std.math.add(u64, n, d) catch return null;
        if (n > max) return null;
    }
    return n;
}

/// An octal mode from 0 to 0o777. Leading zeros are fine, anything else that
/// is not an octal number is not.
pub fn parseUmask(s: []const u8) ?u16 {
    return @intCast(parseUint(s, 8, 0o777) orelse return null);
}

/// net.SplitHostPort: "host:port", "[ipv6]:port" or ":port". Null when the
/// address is malformed. Neither part is validated further. The healthcheck
/// uses it to find the port of BITNEWS_LISTEN.
pub fn splitHostPort(hostport: []const u8) ?struct { host: []const u8, port: []const u8 } {
    const i = std.mem.lastIndexOfScalar(u8, hostport, ':') orelse return null;
    var host: []const u8 = undefined;
    var j: usize = 0;
    var k: usize = 0;
    if (hostport.len > 0 and hostport[0] == '[') {
        // The first ']' must come just before the last ':'.
        const end = std.mem.indexOfScalar(u8, hostport, ']') orelse return null;
        if (end + 1 != i) return null;
        host = hostport[1..end];
        j = 1;
        k = end + 1;
    } else {
        host = hostport[0..i];
        if (std.mem.indexOfScalar(u8, host, ':') != null) return null;
    }
    if (std.mem.indexOfScalar(u8, hostport[j..], '[') != null) return null;
    if (std.mem.indexOfScalar(u8, hostport[k..], ']') != null) return null;
    return .{ .host = host, .port = hostport[i + 1 ..] };
}

/// Null when `addr` is host:port with a numeric port (the host may be empty
/// to listen on all interfaces), else the reason it is not.
fn validateListen(addr: []const u8) ?[]const u8 {
    const hp = splitHostPort(addr) orelse return "must be host:port";
    if (parseUint(hp.port, 10, std.math.maxInt(u16)) == null) return "port must be a number from 0 to 65535";
    return null;
}

fn isAbs(path: []const u8) bool {
    return path.len > 0 and path[0] == '/';
}

/// filepath.Join: the non-empty elements joined and cleaned.
fn join(arena: Allocator, parts: []const []const u8) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    for (parts) |p| {
        if (p.len == 0) continue;
        if (out.items.len > 0) try out.append(arena, '/');
        try out.appendSlice(arena, p);
    }
    if (out.items.len == 0) return "";
    return clean(arena, out.items);
}

/// filepath.Clean: lexical, so no file system access. Repeated slashes and
/// "." elements go, ".." removes the element before it, and ".." at the root
/// is dropped.
fn clean(arena: Allocator, path: []const u8) Allocator.Error![]const u8 {
    const rooted = isAbs(path);
    var elems: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, path, '/');
    while (it.next()) |elem| {
        if (elem.len == 0 or std.mem.eql(u8, elem, ".")) continue;
        if (std.mem.eql(u8, elem, "..")) {
            if (elems.items.len > 0 and !std.mem.eql(u8, elems.items[elems.items.len - 1], "..")) {
                elems.items.len -= 1;
                continue;
            }
            if (rooted) continue;
        }
        try elems.append(arena, elem);
    }
    if (elems.items.len == 0) return if (rooted) "/" else ".";
    var out: std.ArrayList(u8) = .empty;
    for (elems.items) |elem| {
        if (rooted or out.items.len > 0) try out.append(arena, '/');
        try out.appendSlice(arena, elem);
    }
    return out.items;
}

/// Sets the process-wide file mode creation mask. Call it once at startup,
/// before any file is created. Does nothing where there is no umask.
pub fn applyUmask(mode: u16) void {
    switch (@import("builtin").os.tag) {
        .windows, .wasi, .freestanding => {},
        else => _ = std.c.umask(@intCast(mode)),
    }
}

/// An environment backed by name/value pairs, for tests and fuzzing.
pub const SliceEnv = struct {
    pairs: []const [2][]const u8,

    pub fn get(self: SliceEnv, name: []const u8) ?[]const u8 {
        for (self.pairs) |p| if (std.mem.eql(u8, p[0], name)) return p[1];
        return null;
    }
};
