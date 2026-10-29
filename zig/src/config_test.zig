//! Tests of config.zig, ported from internal/bootstrap/bootstrap_test.go.
//! The error messages and edge cases at the end were checked against the Go
//! binary's output.
const std = @import("std");
const config = @import("config.zig");
const log = @import("log.zig");
const testing = std.testing;
const Pair = [2][]const u8;

const dev_env = [_]Pair{
    .{ "XDG_DATA_HOME", "/home/u/.local/share" },
    .{ "XDG_CONFIG_HOME", "/home/u/.config" },
    .{ "HOME", "/home/u" },
};

/// loads with `extra` variables layered over the minimal dev environment
/// (the first pair with a name wins).
fn loadWith(arena: std.mem.Allocator, extra: []const Pair, args: []const []const u8) !config.Result {
    const pairs = try std.mem.concat(arena, Pair, &.{ extra, &dev_env });
    return config.load(arena, config.SliceEnv{ .pairs = pairs }, args);
}

fn expectRun(arena: std.mem.Allocator, extra: []const Pair, args: []const []const u8) !config.Config {
    const r = try loadWith(arena, extra, args);
    switch (r) {
        .run => |c| return c,
        .invalid => |m| {
            std.debug.print("unexpected invalid: {s}", .{m});
            return error.TestUnexpectedResult;
        },
        else => return error.TestUnexpectedResult,
    }
}

fn expectInvalid(arena: std.mem.Allocator, extra: []const Pair, args: []const []const u8, wants: []const []const u8) !void {
    const r = try loadWith(arena, extra, args);
    const msg = switch (r) {
        .invalid => |m| m,
        else => return error.TestUnexpectedResult,
    };
    for (wants) |w| {
        if (std.mem.indexOf(u8, msg, w) == null) {
            std.debug.print("message {s} lacks {s}\n", .{ msg, w });
            return error.TestExpectedEqual;
        }
    }
    try testing.expect(std.mem.endsWith(u8, msg, "\nRun 'bitnews --help' for usage.\n"));
    try testing.expect(std.mem.startsWith(u8, msg, "bitnews: "));
}

fn expectMessage(extra: []const Pair, args: []const []const u8, expected: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const r = try loadWith(arena.allocator(), extra, args);
    switch (r) {
        .invalid => |m| {
            const want = try std.fmt.allocPrint(arena.allocator(), "bitnews: {s}\nRun 'bitnews --help' for usage.\n", .{expected});
            try testing.expectEqualStrings(want, m);
        },
        else => return error.TestUnexpectedResult,
    }
}

test "defaults come from the XDG directories" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const c = try expectRun(arena.allocator(), &.{}, &.{});
    try testing.expectEqualStrings("/home/u/.local/share/bitnews", c.config_dir);
    try testing.expectEqualStrings("/home/u/.local/share/bitnews/bitnews.key", c.key_file);
    try testing.expectEqualStrings("0.0.0.0:9494", c.listen);
    try testing.expectEqual(log.Level.info, c.log_level);
    try testing.expectEqual(log.Format.text, c.log_format);
    try testing.expectEqual(@as(u16, 0o002), c.umask);
    try testing.expect(c.admin_password_file == null);
    try testing.expectEqual(@as(u16, 0), c.torrent_port);
}

test "falls back to HOME when XDG_DATA_HOME is unset, empty or relative" {
    const cases = [_][]const Pair{
        &.{ .{ "XDG_DATA_HOME", "" }, .{ "XDG_CONFIG_HOME", "" } },
        &.{ .{ "XDG_DATA_HOME", "rel/share" }, .{ "XDG_CONFIG_HOME", "rel/config" } },
    };
    for (cases) |extra| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const c = try expectRun(arena.allocator(), extra, &.{});
        try testing.expectEqualStrings("/home/u/.local/share/bitnews", c.config_dir);
        try testing.expectEqualStrings("/home/u/.local/share/bitnews/bitnews.key", c.key_file);
    }
    // Unset altogether.
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const r = try config.load(arena.allocator(), config.SliceEnv{ .pairs = &.{.{ "HOME", "/home/u" }} }, &[_][]const u8{});
    try testing.expectEqualStrings("/home/u/.local/share/bitnews", r.run.config_dir);
}

const Precedence = struct {
    name: []const u8,
    args: []const []const u8 = &.{},
    env: []const Pair = &.{},
    field: enum { config_dir, key_file, listen, log_level, log_format },
    want: []const u8,
};

test "precedence is flag over environment over default" {
    const cases = [_]Precedence{
        .{ .name = "config-dir from env", .env = &.{.{ "BITNEWS_CONFIG_DIR", "/srv/env" }}, .field = .config_dir, .want = "/srv/env" },
        .{ .name = "config-dir flag wins", .args = &.{ "--config-dir", "/srv/flag" }, .env = &.{.{ "BITNEWS_CONFIG_DIR", "/srv/env" }}, .field = .config_dir, .want = "/srv/flag" },
        .{ .name = "config-dir empty env", .env = &.{.{ "BITNEWS_CONFIG_DIR", "" }}, .field = .config_dir, .want = "/home/u/.local/share/bitnews" },
        .{ .name = "key-file from env", .env = &.{.{ "BITNEWS_KEY_FILE", "/run/secrets/env.key" }}, .field = .key_file, .want = "/run/secrets/env.key" },
        .{ .name = "key-file flag wins", .args = &.{"--key-file=/etc/bitnews/flag.key"}, .env = &.{.{ "BITNEWS_KEY_FILE", "/run/secrets/env.key" }}, .field = .key_file, .want = "/etc/bitnews/flag.key" },
        .{ .name = "listen default", .field = .listen, .want = "0.0.0.0:9494" },
        .{ .name = "listen from env", .env = &.{.{ "BITNEWS_LISTEN", "127.0.0.1:8080" }}, .field = .listen, .want = "127.0.0.1:8080" },
        .{ .name = "listen flag wins", .args = &.{ "--listen", "[::1]:9000" }, .env = &.{.{ "BITNEWS_LISTEN", "127.0.0.1:8080" }}, .field = .listen, .want = "[::1]:9000" },
        .{ .name = "log-level from env", .env = &.{.{ "BITNEWS_LOG_LEVEL", "debug" }}, .field = .log_level, .want = "DEBUG" },
        .{ .name = "log-level flag wins", .args = &.{ "--log-level", "error" }, .env = &.{.{ "BITNEWS_LOG_LEVEL", "debug" }}, .field = .log_level, .want = "ERROR" },
        .{ .name = "log-level warn", .args = &.{ "--log-level", "warn" }, .field = .log_level, .want = "WARN" },
        .{ .name = "log-format default", .field = .log_format, .want = "text" },
        .{ .name = "log-format from env", .env = &.{.{ "BITNEWS_LOG_FORMAT", "json" }}, .field = .log_format, .want = "json" },
        .{ .name = "log-format flag wins", .args = &.{ "--log-format", "text" }, .env = &.{.{ "BITNEWS_LOG_FORMAT", "json" }}, .field = .log_format, .want = "text" },
        .{ .name = "single dash and = work too", .args = &.{ "-log-format=json", "-listen", ":80" }, .field = .log_format, .want = "json" },
    };
    for (cases) |tc| {
        errdefer std.debug.print("case: {s}\n", .{tc.name});
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const c = try expectRun(arena.allocator(), tc.env, tc.args);
        const got = switch (tc.field) {
            .config_dir => c.config_dir,
            .key_file => c.key_file,
            .listen => c.listen,
            .log_level => c.log_level.name(),
            .log_format => @tagName(c.log_format),
        };
        try testing.expectEqualStrings(tc.want, got);
    }
}

test "a flag given twice keeps the last value" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const c = try expectRun(arena.allocator(), &.{}, &.{ "--listen=1:2", "--listen", "[::1]:80" });
    try testing.expectEqualStrings("[::1]:80", c.listen);
}

const Invalid = struct {
    name: []const u8,
    args: []const []const u8 = &.{},
    env: []const Pair = &.{},
    wants: []const []const u8,
};

test "invalid settings are rejected and name the culprit" {
    const cases = [_]Invalid{
        .{ .name = "unknown log level from flag", .args = &.{ "--log-level", "verbose" }, .wants = &.{ "--log-level", "verbose" } },
        .{ .name = "unknown log level from env", .env = &.{.{ "BITNEWS_LOG_LEVEL", "INFO2" }}, .wants = &.{ "BITNEWS_LOG_LEVEL", "INFO2" } },
        .{ .name = "unknown log format", .env = &.{.{ "BITNEWS_LOG_FORMAT", "xml" }}, .wants = &.{ "BITNEWS_LOG_FORMAT", "xml" } },
        .{ .name = "listen without port", .args = &.{ "--listen", "0.0.0.0" }, .wants = &.{ "--listen", "0.0.0.0" } },
        .{ .name = "listen with non-numeric port", .env = &.{.{ "BITNEWS_LISTEN", "localhost:http" }}, .wants = &.{ "BITNEWS_LISTEN", "localhost:http" } },
        .{ .name = "listen with out-of-range port", .args = &.{ "--listen", ":70000" }, .wants = &.{ "--listen", ":70000" } },
        .{ .name = "listen given empty on the command line", .args = &.{"--listen="}, .wants = &.{"--listen"} },
        .{ .name = "umask not octal", .env = &.{.{ "UMASK", "008" }}, .wants = &.{ "UMASK", "008", "octal" } },
        .{ .name = "umask not a number", .env = &.{.{ "UMASK", "u=rwx" }}, .wants = &.{ "UMASK", "octal" } },
        .{ .name = "umask above 0777", .env = &.{.{ "UMASK", "1000" }}, .wants = &.{ "UMASK", "1000" } },
        .{ .name = "umask negative", .env = &.{.{ "UMASK", "-2" }}, .wants = &.{"UMASK"} },
        .{ .name = "relative admin password file", .env = &.{.{ "BITNEWS_ADMIN_PASSWORD_FILE", "secrets/admin" }}, .wants = &.{ "BITNEWS_ADMIN_PASSWORD_FILE", "absolute" } },
        .{ .name = "relative config-dir", .args = &.{ "--config-dir", "data" }, .wants = &.{ "--config-dir", "absolute" } },
        .{ .name = "relative key-file", .env = &.{.{ "BITNEWS_KEY_FILE", "./bitnews.key" }}, .wants = &.{ "BITNEWS_KEY_FILE", "absolute" } },
        .{ .name = "default config-dir relative because HOME is unset", .env = &.{ .{ "HOME", "" }, .{ "XDG_DATA_HOME", "" } }, .wants = &.{ "BITNEWS_CONFIG_DIR", "absolute" } },
        .{ .name = "key-file equal to config-dir", .args = &.{ "--config-dir", "/config", "--key-file", "/config/" }, .wants = &.{ "--key-file", "directory" } },
        .{ .name = "unknown flag", .args = &.{"--no-such-flag"}, .wants = &.{"flag provided but not defined: -no-such-flag"} },
        .{ .name = "unexpected argument", .args = &.{"serve"}, .wants = &.{"unexpected argument \"serve\""} },
    };
    for (cases) |tc| {
        errdefer std.debug.print("case: {s}\n", .{tc.name});
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        try expectInvalid(arena.allocator(), tc.env, tc.args, tc.wants);
    }
}

test "key file may live outside the config dir, and paths are cleaned" {
    const cases = [_][4][]const u8{
        .{ "/config", "/config-keys/bitnews.key", "/config", "/config-keys/bitnews.key" },
        .{ "/var/lib/bitnews/", "/var/lib/bitnews.key", "/var/lib/bitnews", "/var/lib/bitnews.key" },
        .{ "/config", "/config/../run/secrets/bitnews_key", "/config", "/run/secrets/bitnews_key" },
        .{ "/config", "/config/bitnews.key", "/config", "/config/bitnews.key" },
        .{ "/config/", "/config/secrets/bitnews.key", "/config", "/config/secrets/bitnews.key" },
        .{ "//a//b/./c/..", "/a/b/k", "/a/b", "/a/b/k" },
        .{ "/", "/bitnews.key", "/", "/bitnews.key" },
    };
    for (cases) |tc| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        const c = try expectRun(arena.allocator(), &.{}, &.{ "--config-dir", tc[0], "--key-file", tc[1] });
        try testing.expectEqualStrings(tc[2], c.config_dir);
        try testing.expectEqualStrings(tc[3], c.key_file);
    }
}

test "the default key file sits inside an explicit config dir" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const c = try expectRun(arena.allocator(), &.{}, &.{ "--config-dir", "/config" });
    try testing.expectEqualStrings("/config/bitnews.key", c.key_file);
    const root = try expectRun(arena.allocator(), &.{}, &.{ "--config-dir", "/" });
    try testing.expectEqualStrings("/bitnews.key", root.key_file);
}

test "umask is octal with default 002" {
    const cases = [_]struct { value: ?[]const u8, want: u16 }{
        .{ .value = null, .want = 0o002 },
        .{ .value = "", .want = 0o002 },
        .{ .value = "022", .want = 0o022 },
        .{ .value = "0027", .want = 0o027 },
        .{ .value = "7", .want = 0o007 },
        .{ .value = "777", .want = 0o777 },
    };
    for (cases) |tc| {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        var extra: []const Pair = &.{};
        if (tc.value) |v| extra = &.{.{ "UMASK", v }};
        const c = try expectRun(arena.allocator(), extra, &.{});
        try testing.expectEqual(tc.want, c.umask);
    }
}

test "PUID and PGID are not looked at, even when malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    _ = try expectRun(arena.allocator(), &.{ .{ "PUID", "not-a-number" }, .{ "PGID", "-1" } }, &.{});
}

test "the admin password file is optional and cleaned" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const none = try expectRun(arena.allocator(), &.{}, &.{});
    try testing.expect(none.admin_password_file == null);
    const set = try expectRun(arena.allocator(), &.{.{ "BITNEWS_ADMIN_PASSWORD_FILE", "/run/secrets/admin_password" }}, &.{});
    try testing.expectEqualStrings("/run/secrets/admin_password", set.admin_password_file.?);
    const messy = try expectRun(arena.allocator(), &.{.{ "BITNEWS_ADMIN_PASSWORD_FILE", "/a/../b//c/" }}, &.{});
    try testing.expectEqualStrings("/b/c", messy.admin_password_file.?);
}

test "help and version" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for ([_][]const u8{ "--help", "-h", "-help", "--help=1" }) |a| {
        try testing.expect((try loadWith(arena.allocator(), &.{}, &.{a})) == .help);
    }
    // Version is answered before anything is validated.
    const r = try loadWith(arena.allocator(), &.{ .{ "BITNEWS_LISTEN", "not-an-address" }, .{ "UMASK", "999" } }, &.{"--version"});
    try testing.expect(r == .version);
    try testing.expect((try loadWith(arena.allocator(), &.{}, &.{ "--version", "x" })) == .version);
    try testing.expect((try loadWith(arena.allocator(), &.{}, &.{"--version=T"})) == .version);
    try testing.expect((try loadWith(arena.allocator(), &.{}, &.{"--version=false"})) == .run);
}

test "flags are handled in order: the first problem wins" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expect((try loadWith(arena.allocator(), &.{}, &.{ "--help", "--bogus" })) == .help);
    try testing.expect((try loadWith(arena.allocator(), &.{}, &.{ "--bogus", "--help" })) == .invalid);
    try testing.expect((try loadWith(arena.allocator(), &.{}, &.{ "--version", "--bogus" })) == .invalid);
}

test "torrent port: zero when unset, else 1 to 65535" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const unset = try expectRun(arena.allocator(), &.{}, &.{});
    try testing.expectEqual(@as(u16, 0), unset.torrent_port);
    const set = try expectRun(arena.allocator(), &.{.{ "BITNEWS_TORRENT_PORT", "6881" }}, &.{});
    try testing.expectEqual(@as(u16, 6881), set.torrent_port);
    const zeros = try expectRun(arena.allocator(), &.{.{ "BITNEWS_TORRENT_PORT", "00080" }}, &.{});
    try testing.expectEqual(@as(u16, 80), zeros.torrent_port);
    for ([_][]const u8{ "0", "65536", "-1", "abc", "51413.5", "1 2", "+5", "" ++ "99999999999999999999" }) |v| {
        try expectInvalid(arena.allocator(), &.{.{ "BITNEWS_TORRENT_PORT", v }}, &.{}, &.{ "BITNEWS_TORRENT_PORT", "1 to 65535" });
    }
}

// Messages as the Go binary prints them (after the "bitnews: " prefix).
test "messages match the Go binary" {
    try expectMessage(&.{}, &.{"-nope=1"}, "flag provided but not defined: -nope");
    try expectMessage(&.{}, &.{"-listen"}, "flag needs an argument: -listen");
    try expectMessage(&.{}, &.{"---x"}, "bad flag syntax: ---x");
    try expectMessage(&.{}, &.{"--=x"}, "bad flag syntax: --=x");
    try expectMessage(&.{}, &.{ "--", "x" }, "unexpected argument \"x\"");
    try expectMessage(&.{}, &.{"-"}, "unexpected argument \"-\"");
    try expectMessage(&.{}, &.{"--version=maybe"}, "invalid boolean value \"maybe\" for -version: parse error");
    try expectMessage(&.{}, &.{ "--listen", "--version" }, "--listen=\"--version\": must be host:port");
    try expectMessage(&.{}, &.{"-?"}, "flag provided but not defined: -?");
    try expectMessage(&.{}, &.{"--log-level=\xc3\x9cn\xc3\xaf\x01\xff"}, "--log-level=\"\xc3\x9cn\xc3\xaf\\x01\\xff\": must be debug, info, warn or error");
    try expectMessage(&.{}, &.{"--config-dir="}, "--config-dir=\"\": must be an absolute path");
    try expectMessage(&.{}, &.{"--listen=:99999"}, "--listen=\":99999\": port must be a number from 0 to 65535");
    try expectMessage(&.{}, &.{"--listen=[::1]"}, "--listen=\"[::1]\": must be host:port");
    try expectMessage(&.{}, &.{"--listen=a:b:c"}, "--listen=\"a:b:c\": must be host:port");
    try expectMessage(&.{}, &.{"--listen=[::1]:80x"}, "--listen=\"[::1]:80x\": port must be a number from 0 to 65535");
    try expectMessage(&.{}, &.{"--listen=[a]b:80"}, "--listen=\"[a]b:80\": must be host:port");
    try expectMessage(&.{}, &.{"--listen=a[:80"}, "--listen=\"a[:80\": must be host:port");
    try expectMessage(&.{}, &.{"--listen=host:+80"}, "--listen=\"host:+80\": port must be a number from 0 to 65535");
    try expectMessage(&.{}, &.{"--listen=host:"}, "--listen=\"host:\": port must be a number from 0 to 65535");
    try expectMessage(&.{}, &.{"--listen=:"}, "--listen=\":\": port must be a number from 0 to 65535");
    try expectMessage(&.{.{ "UMASK", "0o7" }}, &.{}, "UMASK=\"0o7\": must be an octal mode from 000 to 777");
    try expectMessage(&.{.{ "UMASK", "+7" }}, &.{}, "UMASK=\"+7\": must be an octal mode from 000 to 777");
    try expectMessage(&.{.{ "UMASK", "1_0" }}, &.{}, "UMASK=\"1_0\": must be an octal mode from 000 to 777");
    try expectMessage(&.{.{ "UMASK", "7777777777777" }}, &.{}, "UMASK=\"7777777777777\": must be an octal mode from 000 to 777");
    try expectMessage(&.{.{ "UMASK", " 7" }}, &.{}, "UMASK=\" 7\": must be an octal mode from 000 to 777");
    try expectMessage(&.{ .{ "HOME", "rel" }, .{ "XDG_DATA_HOME", "" } }, &.{}, "BITNEWS_CONFIG_DIR (default)=\"rel/.local/share/bitnews\": must be an absolute path");
    try expectMessage(&.{.{ "BITNEWS_LOG_LEVEL", "x y" }}, &.{}, "BITNEWS_LOG_LEVEL=\"x y\": must be debug, info, warn or error");
    try expectMessage(&.{}, &.{ "--config-dir=/config", "--key-file=/config" }, "--key-file=\"/config\": must be a file, but it is the config directory \"/config\"");
}

test "paths from odd HOME and XDG values are cleaned" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = try expectRun(arena.allocator(), &.{ .{ "HOME", "/h//x/" }, .{ "XDG_DATA_HOME", "" } }, &.{});
    try testing.expectEqualStrings("/h/x/.local/share/bitnews", a.config_dir);
    const b = try expectRun(arena.allocator(), &.{ .{ "HOME", "" }, .{ "XDG_DATA_HOME", "/x/./y/" } }, &.{});
    try testing.expectEqualStrings("/x/y/bitnews", b.config_dir);
    try testing.expectEqualStrings("/x/y/bitnews/bitnews.key", b.key_file);
}

test "splitHostPort" {
    const hp = config.splitHostPort("[::1]:80").?;
    try testing.expectEqualStrings("::1", hp.host);
    try testing.expectEqualStrings("80", hp.port);
    const all = config.splitHostPort(":9494").?;
    try testing.expectEqualStrings("", all.host);
    try testing.expectEqualStrings("9494", all.port);
    for ([_][]const u8{ "", "host", "[::1]", "[::1", "a:b:c", "[a]b:80", "a[:80", "[a]:80]", "[a]:b:80" }) |bad| {
        try testing.expect(config.splitHostPort(bad) == null);
    }
}

test "parseUmask" {
    try testing.expectEqual(@as(?u16, 0o022), config.parseUmask("022"));
    try testing.expectEqual(@as(?u16, 0), config.parseUmask("000000000000000000000"));
    try testing.expectEqual(@as(?u16, 0o777), config.parseUmask("0777"));
    for ([_][]const u8{ "", "8", "1000", "-1", "+1", "0x7", "7 ", "99999999999999999999999" }) |bad| {
        try testing.expect(config.parseUmask(bad) == null);
    }
}

test "applyUmask changes the process umask" {
    const old = std.c.umask(0o077);
    defer _ = std.c.umask(old);
    config.applyUmask(0o022);
    const now = std.c.umask(0o022);
    try testing.expectEqual(@as(@TypeOf(now), 0o022), now);
}

test "usage matches the Go help text" {
    // Produced by internal/bootstrap's Usage.
    const expected =
        \\Usage: bitnews [options]
        \\       bitnews healthcheck
        \\
        \\healthcheck asks the running daemon on 127.0.0.1 (port from BITNEWS_LISTEN) for /api/v1/health
        \\and exits 0 on a 200 within 3 seconds, otherwise 1. It is meant for container health checks.
        \\
        \\Options (a flag wins over its environment variable; empty variables are ignored):
        \\  --config-dir dir
        \\        directory holding bitnews.db (env BITNEWS_CONFIG_DIR, default $XDG_DATA_HOME/bitnews, or ~/.local/share/bitnews)
        \\  --key-file path
        \\        master key that encrypts stored Server and archive passwords; created on first start (env BITNEWS_KEY_FILE, default bitnews.key inside the config dir)
        \\  --listen host:port
        \\        address for the API, adapters and UI (env BITNEWS_LISTEN, default 0.0.0.0:9494)
        \\  --log-level level
        \\        debug, info, warn or error (env BITNEWS_LOG_LEVEL, default info)
        \\  --log-format format
        \\        text or json (env BITNEWS_LOG_FORMAT, default text)
        \\  --version
        \\        print version and exit
        \\  -h, --help
        \\        print this help and exit
        \\
        \\Environment only:
        \\  UMASK=octal
        \\        file mode creation mask applied at startup, so files are group-writable for Sonarr and Radarr (default 002)
        \\  BITNEWS_ADMIN_PASSWORD_FILE=path
        \\        absolute path of a file holding the initial admin password (default unset)
        \\  BITNEWS_TORRENT_PORT=port
        \\        TCP and uTP port the torrent engine listens on, overriding the setting; forward it for incoming peers (no UPnP) (default the torrent port setting (51413))
        \\
    ;
    try testing.expectEqualStrings(expected, config.usage);
}

// Fuzz: hostile argument and variable values never crash or leak, a Config
// that comes out always satisfies the documented invariants, and an error
// message is always a complete usage error.
fn fuzzOne(_: void, smith: *testing.Smith) anyerror!void {
    var a: [64]u8 = undefined;
    var b: [64]u8 = undefined;
    var c: [64]u8 = undefined;
    const arg = a[0..smith.slice(&a)];
    const umask = b[0..smith.slice(&b)];
    const listen = c[0..smith.slice(&c)];

    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const env = config.SliceEnv{ .pairs = &.{
        .{ "HOME", "/home/u" },
        .{ "UMASK", umask },
        .{ "BITNEWS_LISTEN", listen },
        .{ "BITNEWS_TORRENT_PORT", listen },
        .{ "BITNEWS_LOG_LEVEL", arg },
        .{ "BITNEWS_KEY_FILE", arg },
        .{ "BITNEWS_CONFIG_DIR", umask },
    } };
    const args = [_][]const u8{arg};
    const result = try config.load(arena.allocator(), env, &args);
    switch (result) {
        .run => |cfg| {
            try testing.expect(cfg.umask <= 0o777);
            try testing.expect(cfg.listen.len > 0 and config.splitHostPort(cfg.listen) != null);
            try testing.expect(cfg.config_dir.len > 0 and cfg.config_dir[0] == '/');
            try testing.expect(cfg.key_file.len > 0 and cfg.key_file[0] == '/');
            try testing.expect(!std.mem.eql(u8, cfg.key_file, cfg.config_dir));
        },
        .invalid => |m| try testing.expect(std.mem.endsWith(u8, m, "\nRun 'bitnews --help' for usage.\n")),
        .help, .version => {},
    }
    if (config.parseUmask(umask)) |m| try testing.expect(m <= 0o777);
}

test "fuzz: configuration values" {
    try testing.fuzz({}, fuzzOne, .{ .corpus = &.{
        "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
        "\x02\x00\x00\x00-h\x03\x00\x00\x00022\x0a\x00\x00\x00127.0.0.1:1",
        "\x0b\x00\x00\x00--listen=:1\x04\x00\x00\x00ffff\x07\x00\x00\x00[::1]:9",
        "\x09\x00\x00\x00--version\x01\x00\x00\x008\x02\x00\x00\x00::",
        "\x0c\x00\x00\x00--log-level=\x03\x00\x00\x001_0\x04\x00\x00\x00a:b:",
    } });
}
