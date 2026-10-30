//! Settings validation (Settings.Validate and its helpers in internal/core):
//! every problem found is listed, in the order of the Go code, joined with a
//! newline after the "core: invalid settings: " prefix. The text is what the
//! API shows as the message of the `invalid_settings` error.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const types = @import("settings_types.zig");
const text = @import("settings_text.zig");
const ip = @import("settings_ip.zig");

pub const prefix = "core: invalid settings: ";

/// One shape problem of one data root (core.PathProblem).
pub const PathProblem = struct {
    /// "usenet" or "torrent".
    protocol: []const u8,
    /// "incomplete" or "complete".
    root: []const u8,
    path: []const u8,
    /// Says what is wrong, for example "is not an absolute path".
    reason: []const u8 = "",

    pub fn message(p: PathProblem, arena: Allocator) Allocator.Error![]u8 {
        return std.fmt.allocPrint(arena, "{s} {s} root {s} {s}", .{ p.protocol, p.root, try text.quote(arena, p.path), p.reason });
    }
};

/// Checks the shape of the four data roots and nothing else: each is absolute
/// and clean, and no root lies inside another. Equal roots are fine. A nested
/// root is reported on the inner one.
pub fn pathProblems(arena: Allocator, p: types.Paths) Allocator.Error![]PathProblem {
    const roots = [_]PathProblem{
        .{ .protocol = "usenet", .root = "incomplete", .path = p.usenet.incomplete },
        .{ .protocol = "usenet", .root = "complete", .path = p.usenet.complete },
        .{ .protocol = "torrent", .root = "incomplete", .path = p.torrent.incomplete },
        .{ .protocol = "torrent", .root = "complete", .path = p.torrent.complete },
    };
    var problems: std.ArrayList(PathProblem) = .empty;
    var shaped: std.ArrayList(PathProblem) = .empty;
    for (roots) |r| {
        var root = r;
        if (!text.isAbsolute(root.path)) {
            root.reason = "is not an absolute path";
            try problems.append(arena, root);
        } else if (!text.isClean(root.path)) {
            root.reason = "is not a clean path (no \"..\", \".\" or doubled and trailing separators)";
            try problems.append(arena, root);
        } else {
            try shaped.append(arena, root);
        }
    }
    for (shaped.items, 0..) |a, i| {
        for (shaped.items[i + 1 ..]) |b| {
            var inner = b;
            var outer = a;
            if (text.isWithin(a.path, b.path)) {
                // b is inside a.
            } else if (text.isWithin(b.path, a.path)) {
                inner = a;
                outer = b;
            } else continue;
            inner.reason = try std.fmt.allocPrint(arena, "lies inside the {s} {s} root {s}", .{ outer.protocol, outer.root, try text.quote(arena, outer.path) });
            try problems.append(arena, inner);
        }
    }
    return problems.items;
}

const Problems = struct {
    arena: Allocator,
    out: Io.Writer.Allocating,
    count: usize = 0,

    fn begin(self: *Problems) Allocator.Error!*Io.Writer {
        if (self.count > 0) self.out.writer.writeByte('\n') catch return error.OutOfMemory;
        self.count += 1;
        return &self.out.writer;
    }

    fn add(self: *Problems, comptime fmt: []const u8, args: anytype) Allocator.Error!void {
        (try self.begin()).print(fmt, args) catch return error.OutOfMemory;
    }
};

/// Reports whether `s` may be stored: null when it may, else the message with
/// all the problems, in the arena.
pub fn validate(arena: Allocator, s: types.Settings) Allocator.Error!?[]const u8 {
    var p: Problems = .{ .arena = arena, .out = .init(arena) };

    if (s.queues.usenet < 0) try p.add("usenet queue limit {d} is negative", .{s.queues.usenet});
    if (s.queues.torrent < 0) try p.add("torrent queue limit {d} is negative", .{s.queues.torrent});
    for (s.auth.lan_bypass_cidrs) |cidr| {
        if (try ip.prefixError(arena, cidr)) |e| try p.add("lan bypass cidr: {s}", .{e});
    }
    for (s.auth.trusted_proxies) |cidr| {
        if (try ip.prefixError(arena, cidr)) |e| try p.add("trusted proxy: {s}", .{e});
    }
    for (s.auth.allowed_hosts) |host| {
        if (text.isBlank(host)) {
            try p.add("allowed hosts must not contain blank entries", .{});
            break;
        }
    }
    if ((s.auth.disable_auth or s.auth.lan_bypass_cidrs.len > 0) and s.auth.allowed_hosts.len == 0) {
        try p.add("disable auth and lan bypass require at least one allowed host", .{});
    }
    for (try pathProblems(arena, s.paths)) |pp| try p.add("{s}", .{try pp.message(arena)});
    try validateBinding(&p, "network.usenet", s.network.usenet);
    try validateBinding(&p, "network.torrent", s.network.torrent);
    try validateSeeding(&p, s.seeding);
    try validateTransfer(&p, s);

    if (p.count == 0) return null;
    return try std.fmt.allocPrint(arena, "{s}{s}", .{ prefix, p.out.written() });
}

fn validateBinding(p: *Problems, name: []const u8, b: types.ProtocolBinding) Allocator.Error!void {
    if (b.interface.len > 0) {
        if (try interfaceNameProblem(p.arena, b.interface)) |why| {
            try p.add("{s}.interface {s}: {s}", .{ name, try text.quote(p.arena, b.interface), why });
        }
    }
    if (b.address.len > 0) {
        if (ip.bindAddressProblem(b.address)) |why| {
            try p.add("{s}.address {s}: {s}", .{ name, try text.quote(p.arena, b.address), why });
        }
    }
}

/// ValidInterfaceName: at most 15 bytes of [A-Za-z0-9_.:-], so a hostile name
/// never reaches a socket option. Returns the reason, or null when fine.
pub fn interfaceNameProblem(arena: Allocator, name: []const u8) Allocator.Error!?[]const u8 {
    if (name.len == 0) return "must not be empty";
    if (name.len > types.max_interface_name_len) {
        return try std.fmt.allocPrint(arena, "longer than {d} characters", .{types.max_interface_name_len});
    }
    for (name) |c| switch (c) {
        'a'...'z', 'A'...'Z', '0'...'9', '_', '.', ':', '-' => {},
        else => {
            var aw: Io.Writer.Allocating = .init(arena);
            aw.writer.writeAll("character ") catch return error.OutOfMemory;
            text.writeQuotedByte(&aw.writer, c) catch return error.OutOfMemory;
            aw.writer.writeAll(" is not allowed (use letters, digits and _ . : -)") catch return error.OutOfMemory;
            return aw.written();
        },
    };
    return null;
}

fn validateSeeding(p: *Problems, s: types.SeedingSettings) Allocator.Error!void {
    const a = s.action;
    if (a.len > 0 and !std.mem.eql(u8, a, types.seeding_pause) and !std.mem.eql(u8, a, types.seeding_remove) and !std.mem.eql(u8, a, types.seeding_remove_data)) {
        try p.add("seeding action {s} is not pause, remove or remove_data", .{try text.quote(p.arena, a)});
    }
    if (std.math.isNan(s.ratio_limit)) {
        try p.add("seeding ratio limit NaN is not a number", .{});
    } else if (std.math.isInf(s.ratio_limit)) {
        try p.add("seeding ratio limit {s}Inf is not a number", .{if (s.ratio_limit > 0) "+" else "-"});
    }
    if (s.seeding_time_minutes > types.max_seeding_time_minutes) {
        try p.add("seeding time {d} minutes is more than {d}", .{ s.seeding_time_minutes, types.max_seeding_time_minutes });
    }
}

fn validateTransfer(p: *Problems, s: types.Settings) Allocator.Error!void {
    const speeds = [_]struct { name: []const u8, v: i64 }{
        .{ .name = "speed.global.down_kbps", .v = s.speed.global.down_kbps },
        .{ .name = "speed.global.up_kbps", .v = s.speed.global.up_kbps },
        .{ .name = "speed.usenet.down_kbps", .v = s.speed.usenet.down_kbps },
        .{ .name = "speed.usenet.up_kbps", .v = s.speed.usenet.up_kbps },
        .{ .name = "speed.torrent.down_kbps", .v = s.speed.torrent.down_kbps },
        .{ .name = "speed.torrent.up_kbps", .v = s.speed.torrent.up_kbps },
    };
    for (speeds) |l| {
        if (l.v < 0 or l.v > types.max_speed_kbps) {
            try p.add("{s} {d} is not between 0 and {d}", .{ l.name, l.v, types.max_speed_kbps });
        }
    }
    if (s.speed.usenet.up_kbps != 0) try p.add("speed.usenet.up_kbps must be 0: Usenet only downloads", .{});

    const port = s.torrent.listen_port;
    if (port < 0 or port > std.math.maxInt(u16)) {
        try p.add("torrent.listen_port {d} is not between 0 and {d}", .{ port, std.math.maxInt(u16) });
    }
    const m = s.torrent.mse;
    if (m.len > 0 and !std.mem.eql(u8, m, types.mse_prefer) and !std.mem.eql(u8, m, types.mse_require) and !std.mem.eql(u8, m, types.mse_disable)) {
        try p.add("torrent.mse {s} is not prefer, require or disable", .{try text.quote(p.arena, m)});
    }

    const pc = s.queues.postproc_concurrency;
    if (pc < 0 or pc > types.max_postproc_concurrency) {
        try p.add("postproc_concurrency {d} is not between 0 and {d}", .{ pc, types.max_postproc_concurrency });
    }
    const mf = s.queues.min_free;
    if (mf < 0 or mf > types.max_min_free) {
        try p.add("min_free {d} is not between 0 and {d}", .{ mf, types.max_min_free });
    }
}

const testing = std.testing;

fn check(s: types.Settings, want: ?[]const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try validate(arena.allocator(), s);
    if (want) |w| {
        try testing.expectEqualStrings(w, got orelse return error.TestExpectedMessage);
    } else {
        try testing.expectEqual(null, got);
    }
}

test "the default settings are valid" {
    try check(types.Settings.defaults(), null);
}

test "queue limits may be zero but not negative" {
    var s = types.Settings.defaults();
    s.queues.usenet = 0;
    s.queues.torrent = 0;
    try check(s, null);
    s.queues.usenet = -1;
    try check(s, prefix ++ "usenet queue limit -1 is negative");
    s.queues = .{ .usenet = 1, .torrent = -1, .postproc_concurrency = 1 };
    try check(s, prefix ++ "torrent queue limit -1 is negative");
}

test "every problem is listed, in order, one per line" {
    var s: types.Settings = .{};
    s.queues = .{ .usenet = -1, .torrent = -2, .postproc_concurrency = 17, .min_free = -1 };
    try check(s, prefix ++
        "usenet queue limit -1 is negative\n" ++
        "torrent queue limit -2 is negative\n" ++
        "usenet incomplete root \"\" is not an absolute path\n" ++
        "usenet complete root \"\" is not an absolute path\n" ++
        "torrent incomplete root \"\" is not an absolute path\n" ++
        "torrent complete root \"\" is not an absolute path\n" ++
        "postproc_concurrency 17 is not between 0 and 16\n" ++
        "min_free -1 is not between 0 and 1125899906842624");
}

test "auth bypass needs allowed hosts and no blank entries" {
    var s = types.Settings.defaults();
    s.auth.disable_auth = true;
    try check(s, prefix ++ "disable auth and lan bypass require at least one allowed host");
    s.auth.allowed_hosts = &.{ "bitnews.lan", "  " };
    try check(s, prefix ++ "allowed hosts must not contain blank entries");
    s.auth.allowed_hosts = &.{"bitnews.lan"};
    try check(s, null);
    s.auth.disable_auth = false;
    s.auth.lan_bypass_cidrs = &.{"192.168.1.0/24"};
    try check(s, null);
    s.auth.allowed_hosts = &.{};
    try check(s, prefix ++ "disable auth and lan bypass require at least one allowed host");
}

test "data roots: absolute, clean, not nested, equal is fine" {
    var s = types.Settings.defaults();
    s.paths.usenet.incomplete = "/data/usenet";
    s.paths.usenet.complete = "/data/usenet";
    s.paths.torrent = .{ .incomplete = "/data/usenet", .complete = "/data/usenet" };
    try check(s, null);
    s.paths.usenet.complete = "/data/usenet-done";
    s.paths.torrent = .{ .incomplete = "/t", .complete = "/t" };
    try check(s, null);
    s.paths.torrent.complete = "/t/done";
    try check(s, prefix ++ "torrent complete root \"/t/done\" lies inside the torrent incomplete root \"/t\"");
    s.paths.torrent.complete = "/t";
    s.paths.usenet.complete = "/data/../etc";
    try check(s, prefix ++ "usenet complete root \"/data/../etc\" is not a clean path (no \"..\", \".\" or doubled and trailing separators)");
}

test "path problems name the root that is wrong" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var paths = types.Settings.defaults().paths;
    try testing.expectEqual(0, (try pathProblems(arena.allocator(), paths)).len);
    paths.usenet.complete = "relative";
    paths.torrent = .{ .incomplete = "/t", .complete = "/t/done" };
    const got = try pathProblems(arena.allocator(), paths);
    try testing.expectEqual(2, got.len);
    try testing.expectEqualStrings("complete", got[0].root);
    try testing.expectEqualStrings("usenet", got[0].protocol);
    try testing.expectEqualStrings("torrent", got[1].protocol);
    try testing.expectEqualStrings("complete", got[1].root);
    try testing.expectEqualStrings("lies inside the torrent incomplete root \"/t\"", got[1].reason);
}

test "network bindings" {
    var s = types.Settings.defaults();
    s.network.usenet = .{ .interface = "wg0", .address = "10.2.0.2" };
    s.network.torrent = .{ .interface = "br-lan_1.100:1", .address = "fd00::2" };
    try check(s, null);
    s.network.usenet = .{ .interface = "../wg0" };
    s.network.torrent = .{ .address = "vpn.example.com" };
    try check(s, prefix ++
        "network.usenet.interface \"../wg0\": character '/' is not allowed (use letters, digits and _ . : -)\n" ++
        "network.torrent.address \"vpn.example.com\": not an IP address");
    s.network.usenet = .{ .interface = "abcdefghijklmnop" };
    s.network.torrent = .{};
    try check(s, prefix ++ "network.usenet.interface \"abcdefghijklmnop\": longer than 15 characters");
}

test "seeding, speed, torrent" {
    var s = types.Settings.defaults();
    s.seeding = .{ .action = "bogus", .seeding_time_minutes = types.max_seeding_time_minutes + 1 };
    try check(s, prefix ++
        "seeding action \"bogus\" is not pause, remove or remove_data\n" ++
        "seeding time 5256001 minutes is more than 5256000");
    s.seeding = .{ .ratio_limit = std.math.inf(f64) };
    try check(s, prefix ++ "seeding ratio limit +Inf is not a number");
    s.seeding = .{ .ratio_limit = std.math.nan(f64) };
    try check(s, prefix ++ "seeding ratio limit NaN is not a number");
    s.seeding = .{};
    s.speed.usenet.up_kbps = 5;
    s.speed.global.down_kbps = -1;
    try check(s, prefix ++
        "speed.global.down_kbps -1 is not between 0 and 1073741824\n" ++
        "speed.usenet.up_kbps must be 0: Usenet only downloads");
    s.speed = .{};
    s.torrent = .{ .listen_port = 70000, .mse = "x" };
    try check(s, prefix ++
        "torrent.listen_port 70000 is not between 0 and 65535\n" ++
        "torrent.mse \"x\" is not prefer, require or disable");
}
