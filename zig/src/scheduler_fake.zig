//! Test fakes for the scheduler: a corpus of yEnc-encoded Articles and an
//! in-memory Fetcher with scripted behavior and counters (the Go fake_test.go
//! and the nntptest fixture). Tests only.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const usenet_types = @import("usenet_types.zig");
const sched = @import("scheduler.zig");

pub const Lines = []const []const u8;

/// Encoded Articles keyed by message ID, plus the Articles in NZB order. Every
/// string lives in the corpus arena; read-only once built, so fakes on several
/// threads read it without a lock.
pub const Corpus = struct {
    arena: std.heap.ArenaAllocator,
    lines: std.StringHashMapUnmanaged(Lines) = .empty,
    articles: std.ArrayList(sched.Article) = .empty,
    files: u32 = 0,
    /// What a fake serves for an ID the corpus does not know: one good
    /// single-part Article.
    fallback: Lines = &.{},

    pub fn init(gpa: Allocator) !Corpus {
        var c: Corpus = .{ .arena = .init(gpa) };
        errdefer c.deinit(gpa);
        c.fallback = try c.encode("f.bin", "hello, scheduler", 1, 1, 0, 16);
        return c;
    }

    pub fn deinit(c: *Corpus, gpa: Allocator) void {
        c.lines.deinit(gpa);
        c.articles.deinit(gpa);
        c.arena.deinit();
    }

    /// Adds a file as Articles of at most `part_size` decoded bytes. Message
    /// IDs are "<name>.<part>@x"; file_key is the order of the calls.
    pub fn addFile(c: *Corpus, gpa: Allocator, name: []const u8, data: []const u8, part_size: usize) !void {
        const a = c.arena.allocator();
        const total: u32 = @intCast(@max(1, (data.len + part_size - 1) / part_size));
        for (0..total) |k| {
            const begin = k * part_size;
            const end = @min(begin + part_size, data.len);
            const id = try std.fmt.allocPrint(a, "{s}.{d}@x", .{ name, k + 1 });
            const lines = try c.encode(name, data[begin..end], @intCast(k + 1), total, begin, data.len);
            try c.lines.put(gpa, id, lines);
            try c.articles.append(gpa, .{
                .file_key = c.files,
                .segment_number = @intCast(k + 1),
                .message_id = id,
                .expected_bytes = @intCast(end - begin),
            });
        }
        c.files += 1;
    }

    /// `n` Articles that all serve the fallback: 100 per file_key.
    pub fn synthetic(c: *Corpus, n: usize) ![]sched.Article {
        const a = c.arena.allocator();
        const arts = try a.alloc(sched.Article, n);
        for (arts, 0..) |*art, i| art.* = .{
            .file_key = @intCast(i / 100),
            .segment_number = @intCast(i % 100 + 1),
            .message_id = try std.fmt.allocPrint(a, "a{d:0>7}@x", .{i}),
        };
        return arts;
    }

    /// Encodes one Article: single-part when `total` is 1.
    fn encode(c: *Corpus, name: []const u8, part: []const u8, number: u32, total: u32, begin: usize, size: usize) !Lines {
        const a = c.arena.allocator();
        var lines: std.ArrayList([]const u8) = .empty;
        if (total == 1) {
            try lines.append(a, try std.fmt.allocPrint(a, "=ybegin line=128 size={d} name={s}", .{ size, name }));
        } else {
            try lines.append(a, try std.fmt.allocPrint(a, "=ybegin part={d} total={d} line=128 size={d} name={s}", .{ number, total, size, name }));
            try lines.append(a, try std.fmt.allocPrint(a, "=ypart begin={d} end={d}", .{ begin + 1, begin + part.len }));
        }
        try encodeLines(a, &lines, part, 128);
        if (total == 1) {
            try lines.append(a, try std.fmt.allocPrint(a, "=yend size={d} crc32={x:0>8}", .{ part.len, std.hash.Crc32.hash(part) }));
        } else {
            try lines.append(a, try std.fmt.allocPrint(a, "=yend size={d} part={d} pcrc32={x:0>8}", .{ part.len, number, std.hash.Crc32.hash(part) }));
        }
        return lines.items;
    }
};

/// NUL, LF, CR and '=' are always escaped; tab and space at the start or end
/// of a line, '.' at the start, so the lines survive NNTP transport.
fn encodeLines(a: Allocator, lines: *std.ArrayList([]const u8), data: []const u8, line_len: usize) !void {
    var line: std.ArrayList(u8) = .empty;
    for (data, 0..) |b, i| {
        const c = b +% 42;
        const first = line.items.len == 0;
        const last = line.items.len + 1 >= line_len or i == data.len - 1;
        const escape = switch (c) {
            0, '\n', '\r', '=' => true,
            '\t', ' ' => first or last,
            '.' => first,
            else => false,
        };
        if (escape) {
            try line.appendSlice(a, &.{ '=', c +% 64 });
        } else {
            try line.append(a, c);
        }
        if (line.items.len >= line_len) {
            try lines.append(a, line.items);
            line = .empty;
        }
    }
    if (line.items.len > 0) try lines.append(a, line.items);
}

/// What a fake does for one request.
pub const Behavior = enum {
    ok,
    /// 430.
    absent,
    /// A good Article whose announced CRC is wrong.
    corrupt,
    /// A connection error.
    flaky,
    /// error.ServerDisabled.
    disabled,
    /// error.Auth.
    auth,
    /// Not yEnc at all.
    malformed,
    /// A good Article whose trailer announces no CRC.
    no_crc,
};

/// A behavior that applies to calls 1..until_call of one ID, then the fake
/// serves the Article normally.
pub const Rule = struct {
    behavior: Behavior,
    until_call: u32 = std.math.maxInt(u32),
};

pub const Script = struct {
    ctx: ?*anyopaque = null,
    call: *const fn (ctx: ?*anyopaque, id: []const u8, call: u32) Behavior,
};

pub const Fake = struct {
    gpa: Allocator,
    corpus: *const Corpus,
    /// Decides first when set.
    script: ?Script = null,
    default: Behavior = .ok,
    latency_ms: i64 = 0,
    /// Count calls per ID and keep their order. Off for the perf test.
    track: bool = true,

    mu: Io.Mutex = .init,
    rules: std.StringHashMapUnmanaged(Rule) = .empty,
    calls: std.StringHashMapUnmanaged(u32) = .empty,
    order: std.ArrayList([]const u8) = .empty,
    total: std.atomic.Value(u64) = .init(0),

    pub fn init(gpa: Allocator, corpus: *const Corpus) Fake {
        return .{ .gpa = gpa, .corpus = corpus };
    }

    pub fn deinit(f: *Fake) void {
        f.rules.deinit(f.gpa);
        f.calls.deinit(f.gpa);
        f.order.deinit(f.gpa);
    }

    pub fn fetcher(f: *Fake) usenet_types.Fetcher {
        return .{ .ctx = f, .body_fn = body };
    }

    /// Setup only, before the run.
    pub fn rule(f: *Fake, id: []const u8, r: Rule) !void {
        try f.rules.put(f.gpa, id, r);
    }

    pub fn ruleAll(f: *Fake, ids: []const []const u8, b: Behavior) !void {
        for (ids) |id| try f.rule(id, .{ .behavior = b });
    }

    pub fn callsFor(f: *Fake, io: Io, id: []const u8) u32 {
        f.mu.lockUncancelable(io);
        defer f.mu.unlock(io);
        return f.calls.get(id) orelse 0;
    }

    pub fn totalCalls(f: *const Fake) u64 {
        return f.total.load(.monotonic);
    }

    fn behaviorFor(f: *Fake, id: []const u8, call: u32) Behavior {
        if (f.script) |s| return s.call(s.ctx, id, call);
        if (f.rules.get(id)) |r| if (call <= r.until_call) return r.behavior;
        return f.default;
    }

    fn body(ctx: *anyopaque, io: Io, id: []const u8, sink: usenet_types.Sink) usenet_types.FetchError!void {
        const f: *Fake = @ptrCast(@alignCast(ctx));
        io.checkCancel() catch return error.Canceled;
        _ = f.total.fetchAdd(1, .monotonic);
        var call: u32 = 1;
        if (f.track) {
            f.mu.lockUncancelable(io);
            defer f.mu.unlock(io);
            const gop = f.calls.getOrPut(f.gpa, id) catch return error.OutOfMemory;
            if (!gop.found_existing) gop.value_ptr.* = 0;
            gop.value_ptr.* += 1;
            call = gop.value_ptr.*;
            f.order.append(f.gpa, id) catch return error.OutOfMemory;
        }
        if (f.latency_ms > 0) io.sleep(.fromMilliseconds(f.latency_ms), .awake) catch return error.Canceled;

        var scratch: std.heap.ArenaAllocator = .init(f.gpa);
        defer scratch.deinit();
        const a = scratch.allocator();
        const base = f.corpus.lines.get(id) orelse f.corpus.fallback;
        const lines: Lines = switch (f.behaviorFor(id, call)) {
            .absent => return error.ArticleNotFound,
            .flaky => return error.Connection,
            .disabled => return error.ServerDisabled,
            .auth => return error.Auth,
            .malformed => &.{ "=ybegin garbage", "no part" },
            .ok => base,
            .corrupt => try withTrailer(a, base, .corrupt),
            .no_crc => try withTrailer(a, base, .no_crc),
        };
        for (lines) |l| sink.line(l) catch return error.Aborted;
    }

    fn withTrailer(a: Allocator, base: Lines, how: enum { corrupt, no_crc }) Allocator.Error!Lines {
        const out = try a.dupe([]const u8, base);
        var last = try a.dupe(u8, out[out.len - 1]);
        // The CRC the decoder compares: pcrc32 of a multipart trailer, else
        // crc32.
        const key_at = std.mem.indexOf(u8, last, "pcrc32=") orelse std.mem.indexOf(u8, last, "crc32=").?;
        const eq = std.mem.indexOfScalarPos(u8, last, key_at, '=').?;
        switch (how) {
            .corrupt => last[eq + 1] = if (last[eq + 1] == '0') '1' else '0',
            .no_crc => {
                const sp = std.mem.lastIndexOfScalar(u8, last[0..key_at], ' ').?;
                last = last[0..sp];
            },
        }
        out[out.len - 1] = last;
        return out;
    }
};
