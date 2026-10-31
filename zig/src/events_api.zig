//! GET /api/v1/events as server-sent events (internal/api/events.go):
//!
//!     event: hello            {"instance_id": "..."}   first, once
//!     event: <name>           one of the events, with its payload
//!     : ping                  a comment, every ping interval
//!     event: resync           the client missed events: refetch state; the stream ends
//!
//! Events are not persisted, so there is nothing to replay: Last-Event-ID is
//! ignored and a client refetches its state after (re)connecting. The stream
//! ends when the client disconnects, when the server shuts down, or after
//! resync.
//!
//! Memory: the subscription is owned by the handler and freed when it
//! returns; every frame is rendered in a scratch arena that is reset after the
//! write, so a stream that runs for days does not grow the request arena.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const ex = @import("http_exchange.zig");
const respond = @import("respond.zig");
const event = @import("event.zig");
const dl = @import("download.zig");
const download_json = @import("download_json.zig");
const bus_mod = @import("events_bus.zig");

pub const Bus = bus_mod.Bus;

/// The state behind the route. It must outlive the router.
pub const Events = struct {
    bus: *Bus,
    /// Identifies this process in the hello event, so a client can tell a
    /// restarted daemon from a dropped connection.
    instance_id: []const u8,
    /// How often an idle stream gets a ": ping" comment (PingInterval).
    ping_interval_ms: u32 = 15_000,
    /// How often a waiting stream looks at the shutdown flag and the socket.
    /// An event does not wait for it.
    poll_ms: u32 = 250,
};

pub fn register(routes: *ex.Routes, svc: *const Events) !void {
    try routes.add("GET /api/v1/events", .{
        .handler = handle,
        .state = @constCast(svc),
        .auth = .key,
        .body_limit = respond.small_body_limit,
    });
}

fn handle(state: ?*anyopaque, e: *ex.Exchange) anyerror!void {
    const svc: *const Events = @ptrCast(@alignCast(state.?));
    e.res.status = .ok;
    try e.res.setHeader(e.arena, "Content-Type", "text/event-stream");
    try e.res.setHeader(e.arena, "Cache-Control", "no-cache");
    try e.res.setHeader(e.arena, "X-Accel-Buffering", "no"); // tell nginx not to buffer the stream
    // HEAD (which the GET route also answers) gets the headers and no stream.
    if (e.method == .HEAD) return;

    run(svc, e) catch |err| switch (err) {
        // The client left or stopped reading: nothing more to say.
        error.StreamClosed => {},
        else => |other| return other,
    };
}

fn nowMs(io: Io) u64 {
    return @intCast(@max(@divTrunc(Io.Clock.Timestamp.now(io, .awake).raw.nanoseconds, std.time.ns_per_ms), 0));
}

fn run(svc: *const Events, e: *ex.Exchange) (ex.StreamError || Allocator.Error)!void {
    const bus = svc.bus;
    // Subscribe before the first byte goes out: once a client has seen hello,
    // it misses no event.
    const sub = try bus.subscribe();
    defer sub.deinit();

    var scratch: std.heap.ArenaAllocator = .init(bus.gpa);
    defer scratch.deinit();

    try e.startStream();
    try sendFrame(e, &scratch, "hello", HelloPayload{ .instance_id = svc.instance_id });

    const ping_ms: u64 = @max(svc.ping_interval_ms, 1);
    const poll_ms: u64 = @max(svc.poll_ms, 1);
    var ping_due = nowMs(e.io) + ping_ms;
    var checked = nowMs(e.io);
    while (true) {
        const now = nowMs(e.io);
        if (now >= ping_due) {
            try writeAll(e, &scratch, ": ping\n\n");
            ping_due += ping_ms;
            if (ping_due <= now) ping_due = now + ping_ms;
        }
        if (now >= checked + poll_ms) {
            checked = now;
            switch (try e.streamState()) {
                .open => {},
                .peer_gone, .stopping => return,
            }
        }
        const wait = @min(ping_due - now, checked + poll_ms - now);
        const item = sub.next(@intCast(@max(wait, 1))) catch |err| switch (err) {
            error.Timeout => continue,
            error.Canceled => return error.Canceled,
            error.Closed => {
                if (sub.reason() != null) try sendFrame(e, &scratch, "resync", EmptyPayload{});
                return;
            },
        };
        defer item.release();
        try sendEvent(e, &scratch, item.event());
    }
}

fn writeAll(e: *ex.Exchange, scratch: *std.heap.ArenaAllocator, bytes: []const u8) ex.StreamError!void {
    _ = scratch.reset(.retain_capacity);
    return e.streamWrite(bytes);
}

/// Writes one event with a JSON data line. JSON has no raw newlines, so one
/// data line always holds the whole payload. Every event has a data line, even
/// an empty object: a browser's EventSource drops events without one.
fn sendFrame(e: *ex.Exchange, scratch: *std.heap.ArenaAllocator, name: []const u8, payload: anytype) (ex.StreamError || Allocator.Error)!void {
    _ = scratch.reset(.retain_capacity);
    var aw: Io.Writer.Allocating = .init(scratch.allocator());
    const w = &aw.writer;
    w.print("event: {s}\ndata: ", .{name}) catch return error.OutOfMemory;
    payload.write(w) catch return error.OutOfMemory;
    w.writeAll("\n\n") catch return error.OutOfMemory;
    // The download payloads come from std.json; Go escapes <, > and & (and
    // U+2028/9) in them. The other payloads are escaped already, which the
    // escaper leaves alone.
    try e.streamWrite(try respond.escapeLikeGo(scratch.allocator(), aw.written()));
}

fn sendEvent(e: *ex.Exchange, scratch: *std.heap.ArenaAllocator, ev: event.Event) (ex.StreamError || Allocator.Error)!void {
    switch (ev) {
        .download_added => |d| try sendFrame(e, scratch, "download_added", DownloadPayload{ .d = d }),
        .download_updated => |d| try sendFrame(e, scratch, "download_updated", DownloadPayload{ .d = d }),
        .download_progress => |p| try sendFrame(e, scratch, "download_progress", ProgressPayload{ .p = p }),
        .fetched => |id| try sendFrame(e, scratch, "fetched", IdPayload{ .id = id }),
        .ready => |r| try sendFrame(e, scratch, "ready", ReadyPayload{ .r = r }),
        .failed => |f| try sendFrame(e, scratch, "failed", FailedPayload{ .f = f }),
        .removed => |id| try sendFrame(e, scratch, "removed", IdPayload{ .id = id }),
        // Neither carries a payload: receivers fetch the current settings and
        // servers, which hold secrets.
        .settings_changed => try sendFrame(e, scratch, "settings_changed", EmptyPayload{}),
        .servers_changed => try sendFrame(e, scratch, "servers_changed", EmptyPayload{}),
        .queue_paused => |q| try sendFrame(e, scratch, switch (q.source) {
            .nzb => "usenet_queue_paused",
            .torrent => "torrent_queue_paused",
        }, QueuePayload{ .reason = q.reason, .detail = q.detail }),
        .queue_resumed => |source| try sendFrame(e, scratch, switch (source) {
            .nzb => "usenet_queue_resumed",
            .torrent => "torrent_queue_resumed",
        }, QueuePayload{ .reason = "", .detail = "" }),
        .postprocess_stage => |p| try sendFrame(e, scratch, "postprocess_stage", StagePayload{ .p = p }),
    }
}

// ---- payloads --------------------------------------------------------------
//
// Written by hand rather than with std.json: Go's encoder escapes <, > and &,
// U+2028 and U+2029, and turns bytes that are not UTF-8 into U+FFFD, while
// std.json would write such a string as an array of numbers. Field order is
// the order of the Go structs.

const hex = "0123456789abcdef";

/// A JSON string the way Go's json.Marshal writes it.
pub fn writeString(w: *Io.Writer, s: []const u8) Io.Writer.Error!void {
    try w.writeByte('"');
    var i: usize = 0;
    var start: usize = 0;
    while (i < s.len) {
        const c = s[i];
        if (c < 0x80) {
            const escape: ?[]const u8 = switch (c) {
                '"' => "\\\"",
                '\\' => "\\\\",
                '\n' => "\\n",
                '\r' => "\\r",
                '\t' => "\\t",
                0x08 => "\\b",
                0x0c => "\\f",
                '<' => "\\u003c",
                '>' => "\\u003e",
                '&' => "\\u0026",
                else => null,
            };
            if (escape == null and c >= 0x20) {
                i += 1;
                continue;
            }
            try w.writeAll(s[start..i]);
            if (escape) |text| {
                try w.writeAll(text);
            } else {
                try w.writeAll("\\u00");
                try w.writeByte(hex[c >> 4]);
                try w.writeByte(hex[c & 0xf]);
            }
            i += 1;
            start = i;
            continue;
        }
        const len = std.unicode.utf8ByteSequenceLength(c) catch 0;
        const cp = if (len > 0 and i + len <= s.len) std.unicode.utf8Decode(s[i..][0..len]) catch null else null;
        if (cp) |point| {
            if (point == 0x2028 or point == 0x2029) {
                try w.writeAll(s[start..i]);
                try w.writeAll(if (point == 0x2028) "\\u2028" else "\\u2029");
                i += len;
                start = i;
            } else {
                i += len;
            }
            continue;
        }
        try w.writeAll(s[start..i]);
        try w.writeAll("\u{FFFD}");
        i += 1;
        start = i;
    }
    try w.writeAll(s[start..]);
    try w.writeByte('"');
}

fn writeField(w: *Io.Writer, comptime key: []const u8, value: []const u8) Io.Writer.Error!void {
    try w.writeAll("\"" ++ key ++ "\":");
    try writeString(w, value);
}

const HelloPayload = struct {
    instance_id: []const u8,

    fn write(p: HelloPayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "instance_id", p.instance_id);
        try w.writeByte('}');
    }
};

const EmptyPayload = struct {
    fn write(_: EmptyPayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeAll("{}");
    }
};

const IdPayload = struct {
    id: []const u8,

    fn write(p: IdPayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "id", p.id);
        try w.writeByte('}');
    }
};

const ReadyPayload = struct {
    r: event.Ready,

    fn write(p: ReadyPayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "id", p.r.id);
        try w.writeByte(',');
        try writeField(w, "output_path", p.r.output_path);
        try w.writeByte('}');
    }
};

const FailedPayload = struct {
    f: event.Failed,

    fn write(p: FailedPayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "id", p.f.id);
        try w.writeByte(',');
        try writeField(w, "reason", @tagName(p.f.reason));
        try w.writeByte(',');
        try writeField(w, "detail", p.f.detail);
        try w.writeByte('}');
    }
};

const StagePayload = struct {
    p: event.PostProcessStage,

    fn write(x: StagePayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "id", x.p.id);
        try w.writeByte(',');
        try writeField(w, "stage", @tagName(x.p.stage));
        try w.writeByte('}');
    }
};

const QueuePayload = struct {
    reason: []const u8,
    detail: []const u8,

    fn write(p: QueuePayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "reason", p.reason);
        try w.writeByte(',');
        try writeField(w, "detail", p.detail);
        try w.writeByte('}');
    }
};

const ProgressPayload = struct {
    p: event.Progress,

    fn write(x: ProgressPayload, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeByte('{');
        try writeField(w, "id", x.p.id);
        try w.print(",\"done_bytes\":{d},\"total_bytes\":{d},\"bytes_per_second\":{d}}}", .{ x.p.done_bytes, x.p.total_bytes, x.p.bytes_per_second });
    }
};

const DownloadPayload = struct {
    d: dl.Download,

    fn write(p: DownloadPayload, w: *Io.Writer) Io.Writer.Error!void {
        std.json.Stringify.value(download_json.Json{ .d = p.d }, .{}, w) catch return error.WriteFailed;
    }
};

const testing = std.testing;

fn renderString(s: []const u8) ![]u8 {
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    errdefer aw.deinit();
    try writeString(&aw.writer, s);
    return aw.toOwnedSlice();
}

// Expected strings are the output of Go's json.Marshal for the same input.
test "strings are escaped the way Go's encoder does" {
    const cases = [_][2][]const u8{
        .{ "plain", "\"plain\"" },
        .{ "a\x08\x0c\x7f\xffz\xe2\x80\xa9\xc3\xa9\xf0\x9f\x98\x80", "\"a\\b\\f\x7f\xef\xbf\xbdz\\u2029\xc3\xa9\xf0\x9f\x98\x80\"" },
        .{ "12 <a&b> \xe2\x80\xa8 \"q\" \\ \n \x01 \xff end", "\"12 \\u003ca\\u0026b\\u003e \\u2028 \\\"q\\\" \\\\ \\n \\u0001 \xef\xbf\xbd end\"" },
        .{ "", "\"\"" },
        // A truncated sequence at the end is invalid, each byte replaced.
        .{ "\xe2\x80", "\"\xef\xbf\xbd\xef\xbf\xbd\"" },
    };
    for (cases) |c| {
        const out = try renderString(c[0]);
        defer testing.allocator.free(out);
        try testing.expectEqualStrings(c[1], out);
    }
}

test "the postprocess_stage payload is Go's {id, stage}" {
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    try (StagePayload{ .p = .{ .id = "d1", .stage = .verifying } }).write(&aw.writer);
    try testing.expectEqualStrings("{\"id\":\"d1\",\"stage\":\"verifying\"}", aw.written());
    for ([_]event.Stage{ .verifying, .repairing, .unpacking, .finishing }, [_][]const u8{ "verifying", "repairing", "unpacking", "finishing" }) |s, name|
        try testing.expectEqualStrings(name, @tagName(s));
}

fn fuzzString(_: void, smith: *testing.Smith) !void {
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    const out = try renderString(buf[0..n]);
    defer testing.allocator.free(out);
    // Whatever goes in, a JSON string comes out, and it is valid UTF-8.
    try testing.expect(std.unicode.utf8ValidateSlice(out));
    const parsed = try std.json.parseFromSlice([]const u8, testing.allocator, out, .{});
    defer parsed.deinit();
    try testing.expect(std.unicode.utf8ValidateSlice(parsed.value));
}

fn seed(comptime x: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.*);
}

test "fuzz: strings in event payloads" {
    try testing.fuzz({}, fuzzString, .{ .corpus = &.{
        seed("plain"),
        seed("<a&b> \"q\" \\ \n\x01"),
        seed("\xff\xfe\xe2\x80\xa8\xe2\x80"),
        seed("\xf0\x9f\x98\x80 \xf0\x9f"),
        seed(""),
    } });
}
