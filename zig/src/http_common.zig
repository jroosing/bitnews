//! Types the HTTP server shares with the code that wires it up. The server
//! imports neither the logger nor the key store of the daemon: the lead writes
//! adapters from those to the two small interfaces here.
const std = @import("std");

pub const Method = std.http.Method;
pub const Header = std.http.Header;

pub const Level = enum { debug, info, warn, err };

/// One structured log field. Values are borrowed for the duration of the call.
pub const Attr = struct {
    key: []const u8,
    value: Value,

    pub const Value = union(enum) {
        str: []const u8,
        int: i64,
        uint: u64,
        /// A duration in nanoseconds.
        duration_ns: u64,
    };

    pub fn str(key: []const u8, value: []const u8) Attr {
        return .{ .key = key, .value = .{ .str = value } };
    }
    pub fn int(key: []const u8, value: i64) Attr {
        return .{ .key = key, .value = .{ .int = value } };
    }
    pub fn uint(key: []const u8, value: u64) Attr {
        return .{ .key = key, .value = .{ .uint = value } };
    }
    pub fn duration(key: []const u8, ns: u64) Attr {
        return .{ .key = key, .value = .{ .duration_ns = ns } };
    }
};

/// Where the server logs to. `log` is called from many threads at once and must
/// be thread-safe; `msg` and `attrs` are only valid during the call. String
/// values can carry anything a client sent (the URL, a request ID is checked
/// but the remote and method are not), so the implementation must quote or
/// escape them.
pub const Logger = struct {
    ctx: *anyopaque,
    log: *const fn (ctx: *anyopaque, level: Level, msg: []const u8, attrs: []const Attr) void,

    pub fn write(l: Logger, level: Level, msg: []const u8, attrs: []const Attr) void {
        l.log(l.ctx, level, msg, attrs);
    }

    var discard_ctx: u8 = 0;
    fn discardLog(_: *anyopaque, _: Level, _: []const u8, _: []const Attr) void {}

    /// Drops everything. For tests and for callers without a logger.
    pub const discard: Logger = .{ .ctx = &discard_ctx, .log = discardLog };
};

/// Decides whether a presented app key is the current one. The implementation
/// does the constant-time comparison (hash both sides, compare in constant
/// time) and may be called from many threads at once.
pub const KeyChecker = struct {
    ctx: *anyopaque,
    check: *const fn (ctx: *anyopaque, presented: []const u8) bool,

    pub fn ok(k: KeyChecker, presented: []const u8) bool {
        return k.check(k.ctx, presented);
    }
};

/// Tells the server whether first-run setup is still open (no admin exists
/// yet). While it is, every request but GET /api/v1/health is answered with
/// 503 setup_required before routing, as auth.Gate does. The implementation is
/// one atomic load; it may be called from many threads at once.
pub const SetupGate = struct {
    ctx: *anyopaque,
    required: *const fn (ctx: *anyopaque) bool,
};
