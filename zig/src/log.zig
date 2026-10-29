//! Structured logging with the output of Go's log/slog (text and JSON
//! handlers), so log lines are the same as the Go version's.
//!
//! One `Core` owns the sink, the level, the format and the lock; a `Logger`
//! is a cheap copyable view of a Core plus the attributes added by `with`.
//! Ownership: the Core is created once, at a stable address, by main and
//! outlives every Logger made from it. A Logger made by `with` owns nothing
//! but its pre-rendered attributes, which live in the allocator given to
//! `with` (an arena of the caller: nothing to free one by one).
//!
//! A record is written under the Core's lock with a single flush, so lines
//! never interleave. Errors from the sink are dropped, as slog does: logging
//! must not fail the caller.
const std = @import("std");
const text = @import("log_text.zig");
const Secret = @import("secret.zig").Secret;
const Writer = std.Io.Writer;

pub const Level = enum(i8) {
    debug = -4,
    info = 0,
    warn = 4,
    err = 8,

    /// The name slog prints: DEBUG, INFO, WARN, ERROR.
    pub fn name(l: Level) []const u8 {
        return switch (l) {
            .debug => "DEBUG",
            .info => "INFO",
            .warn => "WARN",
            .err => "ERROR",
        };
    }

    /// Parses the bootstrap spelling: debug, info, warn or error.
    pub fn parse(s: []const u8) ?Level {
        if (std.mem.eql(u8, s, "debug")) return .debug;
        if (std.mem.eql(u8, s, "info")) return .info;
        if (std.mem.eql(u8, s, "warn")) return .warn;
        if (std.mem.eql(u8, s, "error")) return .err;
        return null;
    }
};

pub const Format = enum {
    text,
    json,

    pub fn parse(s: []const u8) ?Format {
        return std.meta.stringToEnum(Format, s);
    }
};

pub const Value = union(enum) {
    string: []const u8,
    int: i64,
    uint: u64,
    boolean: bool,
    float: f64,
    /// Nanoseconds. Text prints Go's Duration string (1.5s), JSON the number.
    duration: i64,
    /// Prints [redacted]; the value itself never reaches the logger.
    secret,
};

pub const Attr = struct {
    key: []const u8,
    value: Value,

    pub fn string(key: []const u8, v: []const u8) Attr {
        return .{ .key = key, .value = .{ .string = v } };
    }
    pub fn int(key: []const u8, v: i64) Attr {
        return .{ .key = key, .value = .{ .int = v } };
    }
    pub fn uint(key: []const u8, v: u64) Attr {
        return .{ .key = key, .value = .{ .uint = v } };
    }
    pub fn boolean(key: []const u8, v: bool) Attr {
        return .{ .key = key, .value = .{ .boolean = v } };
    }
    pub fn float(key: []const u8, v: f64) Attr {
        return .{ .key = key, .value = .{ .float = v } };
    }
    pub fn duration(key: []const u8, d: std.Io.Duration) Attr {
        return durationNs(key, std.math.lossyCast(i64, d.nanoseconds));
    }
    pub fn durationNs(key: []const u8, ns: i64) Attr {
        return .{ .key = key, .value = .{ .duration = ns } };
    }
    /// Takes the Secret only so that the type system makes the caller say
    /// what it is logging; the bytes are not looked at.
    pub fn secret(key: []const u8, s: *const Secret) Attr {
        _ = s;
        return .{ .key = key, .value = .secret };
    }
};

/// A point in time with the UTC offset to print it in.
pub const Time = struct {
    unix_ns: i128,
    offset_seconds: i32 = 0,
};

pub const Options = struct {
    level: Level = .info,
    format: Format = .text,
    /// Injectable so tests print a fixed time.
    clock: *const fn () Time = systemClock,
};

/// The wall clock in the local time zone.
pub fn systemClock() Time {
    var ts: std.c.timespec = undefined;
    if (std.c.clock_gettime(.REALTIME, &ts) != 0) return .{ .unix_ns = 0 };
    var tm: Tm = undefined;
    const secs: c_long = @intCast(ts.sec);
    const offset: i32 = if (localtime_r(&secs, &tm) != null) @intCast(tm.gmtoff) else 0;
    return .{ .unix_ns = @as(i128, ts.sec) * std.time.ns_per_s + ts.nsec, .offset_seconds = offset };
}

// struct tm with the BSD/glibc extension fields, which both macOS and Linux
// (glibc and musl) lay out the same way.
const Tm = extern struct {
    sec: c_int,
    min: c_int,
    hour: c_int,
    mday: c_int,
    mon: c_int,
    year: c_int,
    wday: c_int,
    yday: c_int,
    isdst: c_int,
    gmtoff: c_long,
    zone: ?[*:0]const u8,
};
extern "c" fn localtime_r(timep: *const c_long, result: *Tm) ?*Tm;

pub const Core = struct {
    io: std.Io,
    mutex: std.Io.Mutex = .init,
    sink: *Writer,
    level: Level,
    format: Format,
    clock: *const fn () Time,

    /// `sink` is written to and flushed under the lock, one line per record,
    /// and must outlive the Core.
    pub fn init(io: std.Io, sink: *Writer, opts: Options) Core {
        return .{ .io = io, .sink = sink, .level = opts.level, .format = opts.format, .clock = opts.clock };
    }

    pub fn logger(core: *Core) Logger {
        return .{ .core = core };
    }
};

pub const Logger = struct {
    core: *Core,
    /// Attributes of `with`, already rendered in the Core's format, each
    /// starting with its separator.
    pre: []const u8 = "",

    pub fn init(core: *Core) Logger {
        return .{ .core = core };
    }

    pub fn debug(l: Logger, msg: []const u8, attrs: []const Attr) void {
        l.log(.debug, msg, attrs);
    }
    pub fn info(l: Logger, msg: []const u8, attrs: []const Attr) void {
        l.log(.info, msg, attrs);
    }
    pub fn warn(l: Logger, msg: []const u8, attrs: []const Attr) void {
        l.log(.warn, msg, attrs);
    }
    pub fn err(l: Logger, msg: []const u8, attrs: []const Attr) void {
        l.log(.err, msg, attrs);
    }

    /// Returns a Logger that adds `attrs` to every record, before the
    /// record's own. The rendered attributes are allocated in `allocator`,
    /// which must outlive the returned Logger.
    pub fn with(l: Logger, allocator: std.mem.Allocator, attrs: []const Attr) std.mem.Allocator.Error!Logger {
        var out: Writer.Allocating = .init(allocator);
        errdefer out.deinit();
        out.writer.writeAll(l.pre) catch return error.OutOfMemory;
        for (attrs) |a| writeAttr(&out.writer, l.core.format, a) catch return error.OutOfMemory;
        return .{ .core = l.core, .pre = try out.toOwnedSlice() };
    }

    pub fn log(l: Logger, level: Level, msg: []const u8, attrs: []const Attr) void {
        const core = l.core;
        if (@backingInt(level) < @backingInt(core.level)) return;
        const now = core.clock();
        core.mutex.lockUncancelable(core.io);
        defer core.mutex.unlock(core.io);
        l.writeRecord(core.sink, now, level, msg, attrs) catch return;
        core.sink.flush() catch {};
    }

    fn writeRecord(l: Logger, w: *Writer, now: Time, level: Level, msg: []const u8, attrs: []const Attr) Writer.Error!void {
        const format = l.core.format;
        switch (format) {
            .text => {
                try w.writeAll("time=");
                try writeTime(w, now, .text);
                try w.print(" level={s} msg=", .{level.name()});
                try text.writeTextString(w, msg);
            },
            .json => {
                try w.writeAll("{\"time\":\"");
                try writeTime(w, now, .json);
                try w.print("\",\"level\":\"{s}\",\"msg\":", .{level.name()});
                try text.writeJsonString(w, msg);
            },
        }
        try w.writeAll(l.pre);
        for (attrs) |a| try writeAttr(w, format, a);
        try w.writeAll(if (format == .json) "}\n" else "\n");
    }
};

fn writeAttr(w: *Writer, format: Format, a: Attr) Writer.Error!void {
    switch (format) {
        .text => {
            try w.writeByte(' ');
            try text.writeTextString(w, a.key);
            try w.writeByte('=');
            switch (a.value) {
                .string => |s| try text.writeTextString(w, s),
                .int => |v| try w.print("{d}", .{v}),
                .uint => |v| try w.print("{d}", .{v}),
                .boolean => |v| try w.writeAll(if (v) "true" else "false"),
                .float => |v| try text.writeFloatText(w, v),
                .duration => |v| try text.writeDuration(w, v),
                .secret => try w.writeAll(Redacted),
            }
        },
        .json => {
            try w.writeByte(',');
            try text.writeJsonString(w, a.key);
            try w.writeByte(':');
            switch (a.value) {
                .string => |s| try text.writeJsonString(w, s),
                .int => |v| try w.print("{d}", .{v}),
                .uint => |v| try w.print("{d}", .{v}),
                .boolean => |v| try w.writeAll(if (v) "true" else "false"),
                .float => |v| try text.writeFloatJson(w, v),
                .duration => |v| try w.print("{d}", .{v}),
                .secret => try text.writeJsonString(w, Redacted),
            }
        },
    }
}

const Redacted = @import("secret.zig").redacted;

/// RFC 3339 as slog prints it: text has fixed milliseconds, JSON the
/// nanoseconds without trailing zeros (and none at all when they are zero).
/// A zero offset is Z.
fn writeTime(w: *Writer, t: Time, format: Format) Writer.Error!void {
    const local_ns = t.unix_ns + @as(i128, t.offset_seconds) * std.time.ns_per_s;
    const local_s: i128 = @divFloor(local_ns, std.time.ns_per_s);
    const nanos: u32 = @intCast(@mod(local_ns, std.time.ns_per_s));
    const secs: u64 = if (local_s < 0) 0 else @intCast(local_s);

    const epoch: std.time.epoch.EpochSeconds = .{ .secs = secs };
    const year_day = epoch.getEpochDay().calculateYearDay();
    const month_day = year_day.calculateMonthDay();
    const day = epoch.getDaySeconds();
    try w.print("{d:0>4}-{d:0>2}-{d:0>2}T{d:0>2}:{d:0>2}:{d:0>2}", .{
        year_day.year,
        month_day.month.numeric(),
        month_day.day_index + 1,
        day.getHoursIntoDay(),
        day.getMinutesIntoHour(),
        day.getSecondsIntoMinute(),
    });
    switch (format) {
        .text => try w.print(".{d:0>3}", .{nanos / std.time.ns_per_ms}),
        .json => if (nanos != 0) {
            var buf: [9]u8 = undefined;
            _ = std.fmt.bufPrint(&buf, "{d:0>9}", .{nanos}) catch unreachable;
            var n: usize = 9;
            while (buf[n - 1] == '0') n -= 1;
            try w.writeByte('.');
            try w.writeAll(buf[0..n]);
        },
    }
    if (t.offset_seconds == 0) return w.writeByte('Z');
    const off: u32 = @abs(t.offset_seconds);
    try w.print("{c}{d:0>2}:{d:0>2}", .{ @as(u8, if (t.offset_seconds < 0) '-' else '+'), off / 3600, off % 3600 / 60 });
}
