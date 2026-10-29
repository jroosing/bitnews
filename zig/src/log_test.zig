//! Tests of log.zig. Every expected string was produced by a real Go program
//! using log/slog's TextHandler and JSONHandler (Go 1.27) with the same
//! records; see log_text.zig for the helpers under test.
const std = @import("std");
const log = @import("log.zig");
const Secret = @import("secret.zig").Secret;
const Attr = log.Attr;
const testing = std.testing;

const t0: i128 = 1793361600 * std.time.ns_per_s; // 2026-10-30T12:00:00Z

var clock_now: log.Time = .{ .unix_ns = t0 };

fn fixedClock() log.Time {
    return clock_now;
}

const Rig = struct {
    out: std.Io.Writer.Allocating,
    core: log.Core,

    fn init(rig: *Rig, format: log.Format, level: log.Level) void {
        clock_now = .{ .unix_ns = t0 };
        rig.out = .init(testing.allocator);
        rig.core = .init(testing.io, &rig.out.writer, .{ .level = level, .format = format, .clock = fixedClock });
    }
    fn deinit(rig: *Rig) void {
        rig.out.deinit();
    }
    fn logger(rig: *Rig) log.Logger {
        return rig.core.logger();
    }
    fn written(rig: *Rig) []const u8 {
        return rig.out.written();
    }
};

fn expectLine(format: log.Format, expected: []const u8, msg: []const u8, attrs: []const Attr) !void {
    var rig: Rig = undefined;
    rig.init(format, .debug);
    defer rig.deinit();
    rig.logger().info(msg, attrs);
    try testing.expectEqualStrings(expected, rig.written());
}

test "text: the documented example" {
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=\"hello world\" key=value\n", "hello world", &.{Attr.string("key", "value")});
}

test "json: the documented example" {
    try expectLine(.json, "{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",\"msg\":\"hello world\",\"key\":\"value\"}\n", "hello world", &.{Attr.string("key", "value")});
}

test "levels and numbers" {
    var rig: Rig = undefined;
    rig.init(.text, .debug);
    defer rig.deinit();
    clock_now.unix_ns = t0 + 123456789;
    rig.logger().debug("m", &.{ Attr.int("i", -5), Attr.uint("u", std.math.maxInt(u64)), Attr.boolean("b", true) });
    clock_now.unix_ns = t0 + 500 * std.time.ns_per_ms;
    rig.logger().warn("m", &.{});
    clock_now.unix_ns = t0 + 1500 * std.time.ns_per_us;
    rig.logger().err("m", &.{});
    try testing.expectEqualStrings(
        \\time=2026-10-30T12:00:00.123Z level=DEBUG msg=m i=-5 u=18446744073709551615 b=true
        \\time=2026-10-30T12:00:00.500Z level=WARN msg=m
        \\time=2026-10-30T12:00:00.001Z level=ERROR msg=m
        \\
    , rig.written());
}

test "json times keep nanoseconds without trailing zeros" {
    var rig: Rig = undefined;
    rig.init(.json, .debug);
    defer rig.deinit();
    clock_now.unix_ns = t0 + 123456789;
    rig.logger().debug("m", &.{ Attr.int("i", -5), Attr.uint("u", std.math.maxInt(u64)), Attr.boolean("b", true) });
    clock_now.unix_ns = t0 + 500 * std.time.ns_per_ms;
    rig.logger().warn("m", &.{});
    clock_now.unix_ns = t0 + 1500 * std.time.ns_per_us;
    rig.logger().err("m", &.{});
    try testing.expectEqualStrings(
        \\{"time":"2026-10-30T12:00:00.123456789Z","level":"DEBUG","msg":"m","i":-5,"u":18446744073709551615,"b":true}
        \\{"time":"2026-10-30T12:00:00.5Z","level":"WARN","msg":"m"}
        \\{"time":"2026-10-30T12:00:00.0015Z","level":"ERROR","msg":"m"}
        \\
    , rig.written());
}

test "time zone offsets" {
    var rig: Rig = undefined;
    rig.init(.text, .debug);
    defer rig.deinit();
    // 12:00 local at +01:30 and at -05:00, as the same wall clock reading.
    clock_now = .{ .unix_ns = t0 - 5400 * std.time.ns_per_s, .offset_seconds = 5400 };
    rig.logger().info("m", &.{});
    clock_now = .{ .unix_ns = t0 + 5 * 3600 * std.time.ns_per_s, .offset_seconds = -5 * 3600 };
    rig.logger().info("m", &.{});
    try testing.expectEqualStrings(
        \\time=2026-10-30T12:00:00.000+01:30 level=INFO msg=m
        \\time=2026-10-30T12:00:00.000-05:00 level=INFO msg=m
        \\
    , rig.written());

    var json: Rig = undefined;
    json.init(.json, .debug);
    defer json.deinit();
    clock_now = .{ .unix_ns = t0 + 5 * 3600 * std.time.ns_per_s + 1500 * std.time.ns_per_us, .offset_seconds = -5 * 3600 };
    json.logger().info("m", &.{});
    try testing.expectEqualStrings("{\"time\":\"2026-10-30T12:00:00.0015-05:00\",\"level\":\"INFO\",\"msg\":\"m\"}\n", json.written());
}

test "records below the level are dropped" {
    var rig: Rig = undefined;
    rig.init(.text, .warn);
    defer rig.deinit();
    rig.logger().debug("d", &.{});
    rig.logger().info("hidden", &.{});
    rig.logger().warn("shown", &.{});
    rig.logger().err("also", &.{});
    try testing.expectEqualStrings(
        \\time=2026-10-30T12:00:00.000Z level=WARN msg=shown
        \\time=2026-10-30T12:00:00.000Z level=ERROR msg=also
        \\
    , rig.written());
}

const Quoting = struct { input: []const u8, text: []const u8, json: []const u8 };

// For each input s: msg=s k=s <s>=v, in slog's two handlers.
const quoting_cases = [_]Quoting{
    .{ .input = "", .text = "msg=\"\" k=\"\" \"\"=v", .json = "\"msg\":\"\",\"k\":\"\",\"\":\"v\"" },
    .{ .input = "plain", .text = "msg=plain k=plain plain=v", .json = "\"msg\":\"plain\",\"k\":\"plain\",\"plain\":\"v\"" },
    .{ .input = "has space", .text = "msg=\"has space\" k=\"has space\" \"has space\"=v", .json = "\"msg\":\"has space\",\"k\":\"has space\",\"has space\":\"v\"" },
    .{ .input = "a=b", .text = "msg=\"a=b\" k=\"a=b\" \"a=b\"=v", .json = "\"msg\":\"a=b\",\"k\":\"a=b\",\"a=b\":\"v\"" },
    .{ .input = "q\"uote", .text = "msg=\"q\\\"uote\" k=\"q\\\"uote\" \"q\\\"uote\"=v", .json = "\"msg\":\"q\\\"uote\",\"k\":\"q\\\"uote\",\"q\\\"uote\":\"v\"" },
    .{ .input = "back\\slash", .text = "msg=back\\slash k=back\\slash back\\slash=v", .json = "\"msg\":\"back\\\\slash\",\"k\":\"back\\\\slash\",\"back\\\\slash\":\"v\"" },
    .{ .input = "nl\nx", .text = "msg=\"nl\\nx\" k=\"nl\\nx\" \"nl\\nx\"=v", .json = "\"msg\":\"nl\\nx\",\"k\":\"nl\\nx\",\"nl\\nx\":\"v\"" },
    .{ .input = "tab\tx", .text = "msg=\"tab\\tx\" k=\"tab\\tx\" \"tab\\tx\"=v", .json = "\"msg\":\"tab\\tx\",\"k\":\"tab\\tx\",\"tab\\tx\":\"v\"" },
    .{ .input = "del\x7fx", .text = "msg=del\x7fx k=del\x7fx del\x7fx=v", .json = "\"msg\":\"del\x7fx\",\"k\":\"del\x7fx\",\"del\x7fx\":\"v\"" },
    .{ .input = "µs", .text = "msg=µs k=µs µs=v", .json = "\"msg\":\"µs\",\"k\":\"µs\",\"µs\":\"v\"" },
    .{ .input = "日本", .text = "msg=日本 k=日本 日本=v", .json = "\"msg\":\"日本\",\"k\":\"日本\",\"日本\":\"v\"" },
    .{ .input = "nb\xc2\xa0sp", .text = "msg=\"nb\\u00a0sp\" k=\"nb\\u00a0sp\" \"nb\\u00a0sp\"=v", .json = "\"msg\":\"nb\xc2\xa0sp\",\"k\":\"nb\xc2\xa0sp\",\"nb\xc2\xa0sp\":\"v\"" },
    .{ .input = "\xe2\x80\xa8", .text = "msg=\"\\u2028\" k=\"\\u2028\" \"\\u2028\"=v", .json = "\"msg\":\"\\u2028\",\"k\":\"\\u2028\",\"\\u2028\":\"v\"" },
    .{ .input = "bad\xffutf", .text = "msg=\"bad\\xffutf\" k=\"bad\\xffutf\" \"bad\\xffutf\"=v", .json = "\"msg\":\"bad\xef\xbf\xbdutf\",\"k\":\"bad\xef\xbf\xbdutf\",\"bad\xef\xbf\xbdutf\":\"v\"" },
    .{ .input = "\x01", .text = "msg=\"\\x01\" k=\"\\x01\" \"\\x01\"=v", .json = "\"msg\":\"\\u0001\",\"k\":\"\\u0001\",\"\\u0001\":\"v\"" },
    .{ .input = "<>&", .text = "msg=<>& k=<>& <>&=v", .json = "\"msg\":\"<>&\",\"k\":\"<>&\",\"<>&\":\"v\"" },
    .{ .input = "a,b", .text = "msg=a,b k=a,b a,b=v", .json = "\"msg\":\"a,b\",\"k\":\"a,b\",\"a,b\":\"v\"" },
    .{ .input = "x\xc2\xad", .text = "msg=\"x\\u00ad\" k=\"x\\u00ad\" \"x\\u00ad\"=v", .json = "\"msg\":\"x\xc2\xad\",\"k\":\"x\xc2\xad\",\"x\xc2\xad\":\"v\"" },
    .{ .input = "\xe2\x80\x8b", .text = "msg=\"\\u200b\" k=\"\\u200b\" \"\\u200b\"=v", .json = "\"msg\":\"\xe2\x80\x8b\",\"k\":\"\xe2\x80\x8b\",\"\xe2\x80\x8b\":\"v\"" },
    .{ .input = "\xc2\x85", .text = "msg=\"\\u0085\" k=\"\\u0085\" \"\\u0085\"=v", .json = "\"msg\":\"\xc2\x85\",\"k\":\"\xc2\x85\",\"\xc2\x85\":\"v\"" },
    .{ .input = "a\x08b", .text = "msg=\"a\\bb\" k=\"a\\bb\" \"a\\bb\"=v", .json = "\"msg\":\"a\\u0008b\",\"k\":\"a\\u0008b\",\"a\\u0008b\":\"v\"" },
    .{ .input = "a\x0cb", .text = "msg=\"a\\fb\" k=\"a\\fb\" \"a\\fb\"=v", .json = "\"msg\":\"a\\u000cb\",\"k\":\"a\\u000cb\",\"a\\u000cb\":\"v\"" },
    .{ .input = "a\x07b", .text = "msg=\"a\\ab\" k=\"a\\ab\" \"a\\ab\"=v", .json = "\"msg\":\"a\\u0007b\",\"k\":\"a\\u0007b\",\"a\\u0007b\":\"v\"" },
    .{ .input = "a b\x7f", .text = "msg=\"a b\\x7f\" k=\"a b\\x7f\" \"a b\\x7f\"=v", .json = "\"msg\":\"a b\x7f\",\"k\":\"a b\x7f\",\"a b\x7f\":\"v\"" },
    .{ .input = "emoji \xf0\x9f\x98\x80", .text = "msg=\"emoji \xf0\x9f\x98\x80\" k=\"emoji \xf0\x9f\x98\x80\" \"emoji \xf0\x9f\x98\x80\"=v", .json = "\"msg\":\"emoji \xf0\x9f\x98\x80\",\"k\":\"emoji \xf0\x9f\x98\x80\",\"emoji \xf0\x9f\x98\x80\":\"v\"" },
    .{ .input = "x\xf3\xa0\x80\x81", .text = "msg=\"x\\U000e0001\" k=\"x\\U000e0001\" \"x\\U000e0001\"=v", .json = "\"msg\":\"x\xf3\xa0\x80\x81\",\"k\":\"x\xf3\xa0\x80\x81\",\"x\xf3\xa0\x80\x81\":\"v\"" },
    .{ .input = "a\xe2\x82", .text = "msg=\"a\\xe2\\x82\" k=\"a\\xe2\\x82\" \"a\\xe2\\x82\"=v", .json = "\"msg\":\"a\xef\xbf\xbd\xef\xbf\xbd\",\"k\":\"a\xef\xbf\xbd\xef\xbf\xbd\",\"a\xef\xbf\xbd\xef\xbf\xbd\":\"v\"" },
    .{ .input = "\\ ", .text = "msg=\"\\\\ \" k=\"\\\\ \" \"\\\\ \"=v", .json = "\"msg\":\"\\\\ \",\"k\":\"\\\\ \",\"\\\\ \":\"v\"" },
    .{ .input = "a\x00b", .text = "msg=\"a\\x00b\" k=\"a\\x00b\" \"a\\x00b\"=v", .json = "\"msg\":\"a\\u0000b\",\"k\":\"a\\u0000b\",\"a\\u0000b\":\"v\"" },
};

test "string quoting and escaping match slog" {
    for (quoting_cases) |c| {
        var buf: [512]u8 = undefined;
        const t = try std.fmt.bufPrint(&buf, "time=2026-10-30T12:00:00.000Z level=INFO {s}\n", .{c.text});
        try expectLine(.text, t, c.input, &.{ Attr.string("k", c.input), Attr.string(c.input, "v") });
        var buf2: [512]u8 = undefined;
        const j = try std.fmt.bufPrint(&buf2, "{{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",{s}}}\n", .{c.json});
        try expectLine(.json, j, c.input, &.{ Attr.string("k", c.input), Attr.string(c.input, "v") });
    }
}

test "floats match slog" {
    const attrs = [_]Attr{
        Attr.float("a", 1.5),              Attr.float("b", 100),       Attr.float("c", 0.1),
        Attr.float("d", 1e6),              Attr.float("e", 123456789), Attr.float("f", 1e20),
        Attr.float("g", 1e21),             Attr.float("h", 1e-5),      Attr.float("i", 0.0001),
        Attr.float("j", 0.00001234),       Attr.float("k", -2.5e-7),   Attr.float("l", 1e-7),
        Attr.float("m", 3.14159265358979), Attr.float("n", 0),
    };
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=floats a=1.5 b=100 c=0.1 d=1e+06 e=1.23456789e+08 f=1e+20 g=1e+21 h=1e-05 i=0.0001 j=1.234e-05 k=-2.5e-07 l=1e-07 m=3.14159265358979 n=0\n", "floats", &attrs);
    try expectLine(.json, "{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",\"msg\":\"floats\",\"a\":1.5,\"b\":100,\"c\":0.1,\"d\":1000000,\"e\":123456789,\"f\":100000000000000000000,\"g\":1e+21,\"h\":0.00001,\"i\":0.0001,\"j\":0.00001234,\"k\":-2.5e-7,\"l\":1e-7,\"m\":3.14159265358979,\"n\":0}\n", "floats", &attrs);

    const odd = [_]Attr{
        Attr.float("nan", std.math.nan(f64)),
        Attr.float("inf", std.math.inf(f64)),
        Attr.float("ninf", -std.math.inf(f64)),
        Attr.float("negz", -0.0),
        Attr.float("f9", 123456.0),
        Attr.float("f10", 1234567.0),
        Attr.float("f11", 21e20),
        Attr.float("big", 1.7976931348623157e308),
    };
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=floats2 nan=NaN inf=+Inf ninf=-Inf negz=-0 f9=123456 f10=1.234567e+06 f11=2.1e+21 big=1.7976931348623157e+308\n", "floats2", &odd);
    try expectLine(.json, "{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",\"msg\":\"floats2\",\"nan\":\"!ERROR:json: unsupported value: NaN\",\"inf\":\"!ERROR:json: unsupported value: +Inf\",\"ninf\":\"!ERROR:json: unsupported value: -Inf\",\"negz\":-0,\"f9\":123456,\"f10\":1234567,\"f11\":2.1e+21,\"big\":1.7976931348623157e+308}\n", "floats2", &odd);
}

test "durations match slog" {
    const ns = std.time.ns_per_s;
    const attrs = [_]Attr{
        Attr.durationNs("a", 1500 * std.time.ns_per_ms),
        Attr.durationNs("b", 0),
        Attr.durationNs("c", 60 * ns),
        Attr.durationNs("d", 3600 * ns + 2 * 60 * ns + 3500 * std.time.ns_per_ms),
        Attr.durationNs("e", 150 * std.time.ns_per_ms),
        Attr.durationNs("f", 1500),
        Attr.durationNs("g", 5),
        Attr.durationNs("h", -90 * ns),
        Attr.durationNs("i", std.math.minInt(i64)),
        Attr.duration("j", .fromSeconds(100 * 3600)),
    };
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=durs a=1.5s b=0s c=1m0s d=1h2m3.5s e=150ms f=1.5µs g=5ns h=-1m30s i=-2562047h47m16.854775808s j=100h0m0s\n", "durs", &attrs);
    try expectLine(.json, "{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",\"msg\":\"durs\",\"a\":1500000000,\"b\":0,\"c\":60000000000,\"d\":3723500000000,\"e\":150000000,\"f\":1500,\"g\":5,\"h\":-90000000000,\"i\":-9223372036854775808,\"j\":360000000000000}\n", "durs", &attrs);
}

test "a Secret attr prints redacted and never the value" {
    var s = try Secret.init(testing.allocator, "hunter2-very-secret");
    defer s.deinit();
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=secret pw=[redacted]\n", "secret", &.{Attr.secret("pw", &s)});
    try expectLine(.json, "{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",\"msg\":\"secret\",\"pw\":\"[redacted]\"}\n", "secret", &.{Attr.secret("pw", &s)});
}

test "min int and empty or duplicate keys are kept as slog keeps them" {
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=ints min=-9223372036854775808 z=0\n", "ints", &.{ Attr.int("min", std.math.minInt(i64)), Attr.int("z", 0) });
    try expectLine(.text, "time=2026-10-30T12:00:00.000Z level=INFO msg=emptykey \"\"=v k=x\n", "emptykey", &.{ Attr.string("", "v"), Attr.string("k", "x") });
    try expectLine(.json, "{\"time\":\"2026-10-30T12:00:00Z\",\"level\":\"INFO\",\"msg\":\"dupkey\",\"k\":\"1\",\"k\":\"2\"}\n", "dupkey", &.{ Attr.string("k", "1"), Attr.string("k", "2") });
}

test "with adds attributes before the record's own, and children chain" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    inline for (.{ log.Format.text, log.Format.json }) |format| {
        var rig: Rig = undefined;
        rig.init(format, .info);
        defer rig.deinit();
        const root = rig.logger();
        const child = try root.with(arena.allocator(), &.{ Attr.int("a", 1), Attr.string("b", "two words") });
        const grandchild = try child.with(arena.allocator(), &.{Attr.boolean("c", true)});
        grandchild.info("m", &.{Attr.int("d", 4)});
        child.info("m", &.{});
        root.info("m", &.{});
        const expected = if (format == .text)
            \\time=2026-10-30T12:00:00.000Z level=INFO msg=m a=1 b="two words" c=true d=4
            \\time=2026-10-30T12:00:00.000Z level=INFO msg=m a=1 b="two words"
            \\time=2026-10-30T12:00:00.000Z level=INFO msg=m
            \\
        else
            \\{"time":"2026-10-30T12:00:00Z","level":"INFO","msg":"m","a":1,"b":"two words","c":true,"d":4}
            \\{"time":"2026-10-30T12:00:00Z","level":"INFO","msg":"m","a":1,"b":"two words"}
            \\{"time":"2026-10-30T12:00:00Z","level":"INFO","msg":"m"}
            \\
        ;
        try testing.expectEqualStrings(expected, rig.written());
    }
}

test "with copies strings, so the caller's buffer may change" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var rig: Rig = undefined;
    rig.init(.text, .info);
    defer rig.deinit();
    var buf = "first".*;
    const child = try rig.logger().with(arena.allocator(), &.{Attr.string("k", &buf)});
    buf[0] = 'X';
    child.info("m", &.{});
    try testing.expectEqualStrings("time=2026-10-30T12:00:00.000Z level=INFO msg=m k=first\n", rig.written());
}

test "level and format parsing follow the bootstrap spelling" {
    try testing.expectEqual(log.Level.debug, log.Level.parse("debug").?);
    try testing.expectEqual(log.Level.info, log.Level.parse("info").?);
    try testing.expectEqual(log.Level.warn, log.Level.parse("warn").?);
    try testing.expectEqual(log.Level.err, log.Level.parse("error").?);
    try testing.expect(log.Level.parse("err") == null);
    try testing.expect(log.Level.parse("INFO") == null);
    try testing.expectEqual(log.Format.json, log.Format.parse("json").?);
    try testing.expect(log.Format.parse("xml") == null);
}

test "concurrent records do not interleave" {
    var rig: Rig = undefined;
    rig.init(.text, .info);
    defer rig.deinit();
    const worker = struct {
        fn run(l: log.Logger, n: u64) void {
            for (0..200) |i| l.info("line from a worker", &.{ Attr.uint("worker", n), Attr.uint("i", i) });
        }
    }.run;
    var threads: [4]std.Thread = undefined;
    for (&threads, 0..) |*t, n| t.* = try std.Thread.spawn(.{}, worker, .{ rig.logger(), @as(u64, n) });
    for (threads) |t| t.join();
    var lines: usize = 0;
    var it = std.mem.splitScalar(u8, rig.written(), '\n');
    while (it.next()) |line| {
        if (line.len == 0) continue;
        lines += 1;
        try testing.expect(std.mem.startsWith(u8, line, "time=2026-10-30T12:00:00.000Z level=INFO msg=\"line from a worker\" worker="));
    }
    try testing.expectEqual(@as(usize, 800), lines);
}

// Fuzz: whatever bytes go into a message, key or value, each record is
// exactly one line, and a JSON record parses as JSON (duplicate keys are
// legal to write, as in slog).
fn fuzzOne(_: void, smith: *testing.Smith) anyerror!void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    const s = buf[0..n];
    inline for (.{ log.Format.text, log.Format.json }) |format| {
        var rig: Rig = undefined;
        rig.init(format, .debug);
        defer rig.deinit();
        rig.logger().info(s, &.{ Attr.string(s, s), Attr.string("k", s) });
        const out = rig.written();
        try testing.expect(out.len > 0 and out[out.len - 1] == '\n');
        try testing.expect(std.mem.indexOfScalar(u8, out[0 .. out.len - 1], '\n') == null);
        if (format == .json) {
            var parsed = try std.json.parseFromSlice(std.json.Value, testing.allocator, out, .{ .duplicate_field_behavior = .use_last });
            parsed.deinit();
        }
    }
}

test "fuzz: one record is one line and JSON stays valid" {
    try testing.fuzz({}, fuzzOne, .{ .corpus = &.{
        "\x00\x00\x00\x00",
        "\x09\x00\x00\x00has space",
        "\x07\x00\x00\x00a\"b\\c\nd",
        "\x04\x00\x00\x00\xff\xfe\xe2\x82",
        "\x05\x00\x00\x00\xe2\x80\xa8\xc2\x85",
    } });
}

test "the system clock is the current time with a plausible offset" {
    const t = log.systemClock();
    // After 2024-01-01, and within a day of UTC.
    try testing.expect(t.unix_ns > 1704067200 * std.time.ns_per_s);
    try testing.expect(@abs(t.offset_seconds) <= 24 * 3600);
}
