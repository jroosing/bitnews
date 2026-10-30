//! The typed Settings (internal/core/settings.go, transfer_settings.go,
//! network_settings.go, seeding_settings.go, postprocess_settings.go): the
//! types, the defaults and the effective-value helpers.
//!
//! Field order is the Go struct order, which is the JSON order. A zero value
//! of every type is what Go's zero value is, so a PUT body that leaves a
//! section out resets it. Text fields are slices; whoever builds a Settings
//! owns the memory (`clone` copies into an allocator, normally an arena).
//! Values that Go keeps in a named string type (the MSE policy, the seeding
//! action) are plain strings here, because Go stores and shows whatever it
//! was given and only `validate` objects.
const std = @import("std");
const Allocator = std.mem.Allocator;
const download = @import("download.zig");

/// Limits of the disk, speed and post-processing settings.
pub const default_min_free: i64 = 5 << 30;
pub const max_min_free: i64 = 1 << 50;
pub const max_postproc_concurrency: i64 = 16;
pub const max_speed_kbps: i64 = 1 << 30;
pub const default_torrent_port: i64 = 51413;
/// The default seeding time is bounded to about ten years.
pub const max_seeding_time_minutes: i64 = 10 * 365 * 24 * 60;
pub const max_interface_name_len = 15;

/// How many non-force Downloads each Queue runs at once, and the limits of
/// the stages around the Queues.
pub const QueueLimits = struct {
    usenet: i64 = 0,
    torrent: i64 = 0,
    /// Zero means 1. Applies after a restart.
    postproc_concurrency: i64 = 0,
    /// Bytes that must stay free on top of what a Download needs. Zero means
    /// `default_min_free`.
    min_free: i64 = 0,

    pub const go_name = "core.QueueLimits";
    pub const go_types = .{ .min_free = "int64" };

    pub fn effectivePostprocConcurrency(q: QueueLimits) i64 {
        return @max(q.postproc_concurrency, 1);
    }

    pub fn effectiveMinFree(q: QueueLimits) u64 {
        if (q.min_free <= 0) return @intCast(default_min_free);
        return @intCast(q.min_free);
    }
};

/// UI authentication options. They apply to UI routes only and are off by
/// default; the API cannot change them (handlers_settings.go).
pub const Auth = struct {
    disable_auth: bool = false,
    /// CIDRs that skip UI login; LAN bypass is on when the list is not empty.
    lan_bypass_cidrs: []const []const u8 = &.{},
    /// CIDRs whose X-Forwarded-For is honoured.
    trusted_proxies: []const []const u8 = &.{},
    /// Host header values accepted while disable auth or LAN bypass is on.
    allowed_hosts: []const []const u8 = &.{},

    pub const go_name = "core.Auth";

    pub fn eql(a: Auth, b: Auth) bool {
        return a.disable_auth == b.disable_auth and
            listEql(a.lan_bypass_cidrs, b.lan_bypass_cidrs) and
            listEql(a.trusted_proxies, b.trusted_proxies) and
            listEql(a.allowed_hosts, b.allowed_hosts);
    }
};

fn listEql(a: []const []const u8, b: []const []const u8) bool {
    if (a.len != b.len) return false;
    for (a, b) |x, y| if (!std.mem.eql(u8, x, y)) return false;
    return true;
}

/// One protocol's absolute incomplete and complete directories.
pub const Roots = struct {
    incomplete: []const u8 = "",
    complete: []const u8 = "",

    pub const go_name = "core.Roots";
};

pub const Paths = struct {
    usenet: Roots = .{},
    torrent: Roots = .{},

    pub const go_name = "core.Paths";
};

/// Binds one protocol's sockets; both fields empty means no binding.
pub const ProtocolBinding = struct {
    interface: []const u8 = "",
    address: []const u8 = "",

    pub const go_name = "core.ProtocolBinding";

    pub fn enabled(b: ProtocolBinding) bool {
        return b.interface.len > 0 or b.address.len > 0;
    }
};

pub const NetworkSettings = struct {
    usenet: ProtocolBinding = .{},
    torrent: ProtocolBinding = .{},

    pub const go_name = "core.NetworkSettings";
};

pub const seeding_pause = "pause";
pub const seeding_remove = "remove";
pub const seeding_remove_data = "remove_data";

/// A limit that is not set is -1, as in Go's UnlimitedShareLimit.
pub const ShareLimit = struct {
    ratio: f64 = -1,
    seeding_time_minutes: i64 = -1,
};

pub const SeedingSettings = struct {
    /// Zero or negative means no limit.
    ratio_limit: f64 = 0,
    /// Zero or negative means no limit.
    seeding_time_minutes: i64 = 0,
    /// "pause", "remove" or "remove_data"; empty means pause.
    action: []const u8 = "",

    pub const go_name = "core.SeedingSettings";
    pub const go_types = .{ .action = "core.SeedingAction" };

    pub fn shareLimit(s: SeedingSettings) ShareLimit {
        var l: ShareLimit = .{};
        if (s.ratio_limit > 0) l.ratio = s.ratio_limit;
        if (s.seeding_time_minutes > 0) l.seeding_time_minutes = s.seeding_time_minutes;
        return l;
    }

    pub fn effectiveAction(s: SeedingSettings) []const u8 {
        return if (s.action.len == 0) seeding_pause else s.action;
    }
};

/// A download and an upload limit in KB/s (1 KB = 1024 bytes); zero means
/// unlimited.
pub const SpeedLimit = struct {
    down_kbps: i64 = 0,
    up_kbps: i64 = 0,

    pub const go_name = "core.SpeedLimit";
};

pub const Rates = struct { down: i64, up: i64 };

/// The overall limit is a cap on each protocol, not one shared budget.
pub const SpeedSettings = struct {
    global: SpeedLimit = .{},
    usenet: SpeedLimit = .{},
    torrent: SpeedLimit = .{},

    pub const go_name = "core.SpeedSettings";

    /// The limits in bytes per second that the Queue of `kind` runs with: the
    /// tighter of the overall and the protocol's own limit, 0 for unlimited.
    /// A Usenet Queue never uploads.
    pub fn effective(s: SpeedSettings, kind: download.SourceKind) Rates {
        return switch (kind) {
            .nzb => .{ .down = tighter(s.global.down_kbps, s.usenet.down_kbps), .up = 0 },
            .torrent => .{
                .down = tighter(s.global.down_kbps, s.torrent.down_kbps),
                .up = tighter(s.global.up_kbps, s.torrent.up_kbps),
            },
        };
    }
};

fn tighter(a: i64, b: i64) i64 {
    if (a <= 0 and b <= 0) return 0;
    if (a <= 0) return b * 1024;
    if (b <= 0) return a * 1024;
    return @min(a, b) * 1024;
}

pub const mse_prefer = "prefer";
pub const mse_require = "require";
pub const mse_disable = "disable";

/// The torrent engine's network options. The zero value is the default, which
/// is why DHT and PEX are switched off with Disable fields.
pub const TorrentSettings = struct {
    /// Zero means `default_torrent_port`.
    listen_port: i64 = 0,
    disable_dht: bool = false,
    disable_pex: bool = false,
    lsd: bool = false,
    /// "prefer", "require" or "disable"; empty means prefer.
    mse: []const u8 = "",

    pub const go_name = "core.TorrentSettings";
    pub const go_types = .{ .mse = "core.MSEPolicy" };

    pub fn effectivePort(t: TorrentSettings) i64 {
        return if (t.listen_port == 0) default_torrent_port else t.listen_port;
    }

    pub fn dhtEnabled(t: TorrentSettings) bool {
        return !t.disable_dht;
    }

    pub fn pexEnabled(t: TorrentSettings) bool {
        return !t.disable_pex;
    }

    pub fn effectiveMse(t: TorrentSettings) []const u8 {
        return if (t.mse.len == 0) mse_prefer else t.mse;
    }
};

/// The two optional name-tidying steps; both off by default.
pub const PostProcessOptions = struct {
    clean_names: bool = false,
    underscores_to_dots: bool = false,

    pub const go_name = "core.PostProcessOptions";
};

pub const Settings = struct {
    queues: QueueLimits = .{},
    auth: Auth = .{},
    paths: Paths = .{},
    network: NetworkSettings = .{},
    seeding: SeedingSettings = .{},
    speed: SpeedSettings = .{},
    torrent: TorrentSettings = .{},
    postprocess: PostProcessOptions = .{},

    pub const go_name = "core.Settings";

    /// The Settings of a fresh install (core.DefaultSettings). Strings are
    /// literals.
    pub fn defaults() Settings {
        return .{
            .queues = .{ .usenet = 1, .torrent = 5, .postproc_concurrency = 1, .min_free = default_min_free },
            .paths = .{
                .usenet = .{ .incomplete = "/data/usenet/incomplete", .complete = "/data/usenet/complete" },
                .torrent = .{ .incomplete = "/data/torrents", .complete = "/data/torrents" },
            },
            .seeding = .{ .action = seeding_pause },
        };
    }

    /// A deep copy whose strings live in `a`.
    pub fn clone(s: Settings, a: Allocator) Allocator.Error!Settings {
        return cloneValue(Settings, a, s);
    }

    /// JSON, with the field order, escaping and number format of Go's
    /// encoding/json; empty lists are `[]`, as handlers_settings.go shows them.
    pub fn jsonStringify(s: Settings, jw: anytype) !void {
        try jw.beginWriteRaw();
        try @import("settings_json.zig").write(jw.writer, s);
        jw.endWriteRaw();
    }
};

fn cloneValue(comptime T: type, a: Allocator, v: T) Allocator.Error!T {
    switch (T) {
        []const u8 => return if (v.len == 0) "" else try a.dupe(u8, v),
        []const []const u8 => {
            if (v.len == 0) return &.{};
            const out = try a.alloc([]const u8, v.len);
            for (v, out) |item, *slot| slot.* = try cloneValue([]const u8, a, item);
            return out;
        },
        else => {},
    }
    switch (@typeInfo(T)) {
        .@"struct" => |info| {
            var out: T = undefined;
            inline for (info.field_names, info.field_types) |name, ftype| @field(out, name) = try cloneValue(ftype, a, @field(v, name));
            return out;
        },
        else => return v,
    }
}

const testing = std.testing;

test "the defaults are the install defaults of Go" {
    const s = Settings.defaults();
    try testing.expectEqual(1, s.queues.usenet);
    try testing.expectEqual(5, s.queues.torrent);
    try testing.expectEqual(1, s.queues.postproc_concurrency);
    try testing.expectEqual(5368709120, s.queues.min_free);
    try testing.expectEqualStrings("/data/usenet/incomplete", s.paths.usenet.incomplete);
    try testing.expectEqualStrings("/data/torrents", s.paths.torrent.complete);
    try testing.expectEqualStrings("pause", s.seeding.action);
    try testing.expect(!s.auth.disable_auth);
    try testing.expectEqual(0, s.auth.lan_bypass_cidrs.len);
    try testing.expect(!s.network.usenet.enabled());
}

test "effective values read the zero value as the default" {
    var s: Settings = .{};
    try testing.expectEqual(1, s.queues.effectivePostprocConcurrency());
    try testing.expectEqual(5 << 30, s.queues.effectiveMinFree());
    try testing.expectEqual(51413, s.torrent.effectivePort());
    try testing.expectEqualStrings("prefer", s.torrent.effectiveMse());
    try testing.expectEqualStrings("pause", s.seeding.effectiveAction());
    try testing.expect(s.torrent.dhtEnabled() and s.torrent.pexEnabled());

    s.queues = .{ .postproc_concurrency = 4, .min_free = 100 };
    s.torrent = .{ .listen_port = 40000, .disable_dht = true, .mse = "require" };
    s.seeding.action = "remove";
    try testing.expectEqual(4, s.queues.effectivePostprocConcurrency());
    try testing.expectEqual(100, s.queues.effectiveMinFree());
    try testing.expectEqual(40000, s.torrent.effectivePort());
    try testing.expect(!s.torrent.dhtEnabled());
    try testing.expectEqualStrings("require", s.torrent.effectiveMse());
    try testing.expectEqualStrings("remove", s.seeding.effectiveAction());
}

test "the speed limit of a Queue is the tighter of the overall and its own" {
    const s: SpeedSettings = .{
        .global = .{ .down_kbps = 100, .up_kbps = 0 },
        .usenet = .{ .down_kbps = 50, .up_kbps = 9 },
        .torrent = .{ .down_kbps = 0, .up_kbps = 7 },
    };
    const u = s.effective(.nzb);
    try testing.expectEqual(50 * 1024, u.down);
    try testing.expectEqual(0, u.up);
    const t = s.effective(.torrent);
    try testing.expectEqual(100 * 1024, t.down);
    try testing.expectEqual(7 * 1024, t.up);
    const none = (SpeedSettings{}).effective(.torrent);
    try testing.expectEqual(0, none.down);
    try testing.expectEqual(0, none.up);
}

test "the seeding defaults become a share limit, -1 for unset" {
    var l = (SeedingSettings{}).shareLimit();
    try testing.expectEqual(-1, l.ratio);
    try testing.expectEqual(-1, l.seeding_time_minutes);
    l = (SeedingSettings{ .ratio_limit = 1.5, .seeding_time_minutes = 30 }).shareLimit();
    try testing.expectEqual(1.5, l.ratio);
    try testing.expectEqual(30, l.seeding_time_minutes);
    l = (SeedingSettings{ .ratio_limit = -2, .seeding_time_minutes = -2 }).shareLimit();
    try testing.expectEqual(-1, l.ratio);
}

test "clone is deep" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var hosts = [_][]const u8{"a.lan"};
    var s = Settings.defaults();
    s.auth.allowed_hosts = &hosts;
    const c = try s.clone(arena.allocator());
    try testing.expect(c.auth.allowed_hosts.ptr != s.auth.allowed_hosts.ptr);
    try testing.expect(c.auth.allowed_hosts[0].ptr != s.auth.allowed_hosts[0].ptr);
    try testing.expect(c.auth.eql(s.auth));
    try testing.expectEqualStrings(s.paths.usenet.complete, c.paths.usenet.complete);
    s.auth.allowed_hosts = &.{};
    try testing.expect(!c.auth.eql(s.auth));
}
