//! Tests of the scheduler (internal/usenet/scheduler/*_test.go). The fakes
//! stand in for the NNTP pools; expected values are those of the Go tests.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const sched = @import("scheduler.zig");
const fake = @import("scheduler_fake.zig");
const Fake = fake.Fake;
const Corpus = fake.Corpus;

const gpa = testing.allocator;
const io = testing.io;

/// What the tests keep of a Result (the Part dies after the callback).
const Rec = struct {
    status: sched.Status,
    damaged: bool,
    server: []const u8,
    err: ?anyerror,
    has_part: bool = false,
    crc_match: bool = false,
    has_expected_crc: bool = false,
    name: []const u8 = "",
    size: i64 = 0,
    begin: i64 = 0,
    data: []const u8 = "",
};

/// Gathers Results; safe for concurrent use.
const Collector = struct {
    mu: Io.Mutex = .init,
    arena: std.heap.ArenaAllocator,
    map: std.StringHashMapUnmanaged(Rec) = .empty,
    count: usize = 0,

    fn init() Collector {
        return .{ .arena = .init(gpa) };
    }

    fn deinit(c: *Collector) void {
        c.map.deinit(gpa);
        c.arena.deinit();
    }

    pub fn result(c: *Collector, r: sched.Result) void {
        c.mu.lockUncancelable(io);
        defer c.mu.unlock(io);
        c.count += 1;
        const a = c.arena.allocator();
        var rec: Rec = .{ .status = r.status, .damaged = r.damaged, .server = r.server, .err = r.err };
        if (r.part) |p| {
            rec.has_part = true;
            rec.crc_match = p.crcMatch();
            rec.has_expected_crc = p.has_expected_crc;
            rec.name = a.dupe(u8, p.name) catch @panic("oom");
            rec.size = p.size;
            rec.begin = p.begin;
            rec.data = a.dupe(u8, p.data) catch @panic("oom");
        }
        c.map.put(gpa, r.article.message_id, rec) catch @panic("oom");
    }

    fn n(c: *Collector) usize {
        c.mu.lockUncancelable(io);
        defer c.mu.unlock(io);
        return c.count;
    }

    fn get(c: *Collector, id: []const u8) Rec {
        c.mu.lockUncancelable(io);
        defer c.mu.unlock(io);
        return c.map.get(id) orelse .{ .status = .missing, .damaged = false, .server = "?unresolved", .err = null };
    }
};

const Spec = struct {
    name: []const u8,
    priority: i64 = 0,
    workers: u32 = 1,
    retention_days: u32 = 0,
    fake: *Fake,
};

const Opts = struct {
    now: ?i64 = null,
    backoff: sched.Backoff = .none,
    on_server_down: ?sched.ServerDownSink = null,
};

fn create(specs: []const Spec, arts: []const sched.Article, col: *Collector, opts: Opts) !*sched.Scheduler {
    var servers: [sched.max_servers]sched.Server = undefined;
    for (specs, 0..) |sp, i| servers[i] = .{
        .name = sp.name,
        .priority = sp.priority,
        .workers = sp.workers,
        .retention_days = sp.retention_days,
        .fetcher = sp.fake.fetcher(),
    };
    return sched.Scheduler.create(gpa, .{
        .servers = servers[0..specs.len],
        .articles = arts,
        .on_result = .init(col),
        .on_server_down = opts.on_server_down,
        .now = opts.now,
        .backoff = opts.backoff,
    });
}

/// Runs a Scheduler to completion; every Article resolves exactly once.
fn runAll(specs: []const Spec, arts: []const sched.Article, col: *Collector, opts: Opts) !*sched.Scheduler {
    const s = try create(specs, arts, col, opts);
    errdefer s.destroy();
    try s.run(io);
    try testing.expectEqual(arts.len, col.n());
    return s;
}

fn fixtureFiles(c: *Corpus, count: usize, size: usize, part: usize) ![]const []const u8 {
    const a = c.arena.allocator();
    const files = try a.alloc([]const u8, count);
    for (files, 0..) |*f, i| {
        const data = try a.alloc(u8, size + i * 7);
        for (data, 0..) |*b, j| b.* = @intCast((j * 31 + i * 17 + (j >> 8)) % 251);
        const name = try std.fmt.allocPrint(a, "file{d:0>2}.bin", .{i});
        try c.addFile(gpa, name, data, part);
        f.* = data;
    }
    return files;
}

/// Every `every`th message ID starting at `offset`.
fn pick(a: std.mem.Allocator, arts: []const sched.Article, every: usize, offset: usize) ![]const []const u8 {
    var ids: std.ArrayList([]const u8) = .empty;
    var i = offset;
    while (i < arts.len) : (i += every) try ids.append(a, arts[i].message_id);
    return ids.items;
}

fn contains(ids: []const []const u8, id: []const u8) bool {
    for (ids) |x| if (std.mem.eql(u8, x, id)) return true;
    return false;
}

fn hashN(s: []const u8, salt: u64) u64 {
    var h: u64 = 14695981039346656037 +% salt *% 1099511628211;
    for (s) |c| {
        h ^= c;
        h *%= 1099511628211;
    }
    return h >> 33;
}

test "main server has everything and the fill server is never asked" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 3, 5000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;

    const s = try runAll(&.{
        .{ .name = "main", .workers = 4, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 4, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |a| {
        const r = col.get(a.message_id);
        try testing.expectEqual(.done, r.status);
        try testing.expect(!r.damaged);
        try testing.expectEqualStrings("main", r.server);
        try testing.expect(r.has_part);
        try testing.expectEqual(1, main.callsFor(io, a.message_id));
    }
    try testing.expectEqual(0, fill.totalCalls());
    const st = s.stats();
    try testing.expectEqual(arts.len, st.done);
    try testing.expectEqual(0, st.missing);
    try testing.expectEqual(arts.len, st.servers()[0].requests);
    try testing.expectEqual(0, st.servers()[1].requests);
    try testing.expect(st.servers()[0].bytes > 0);
}

test "the fill server is asked exactly for what main lacks" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 4, 20000, 500);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const lacking = try pick(corpus.arena.allocator(), arts, 20, 3);
    try main.ruleAll(lacking, .absent);

    const s = try runAll(&.{
        .{ .name = "main", .workers = 4, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 4, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |a| {
        const r = col.get(a.message_id);
        try testing.expectEqual(.done, r.status);
        try testing.expectEqual(1, main.callsFor(io, a.message_id));
        if (contains(lacking, a.message_id)) {
            try testing.expectEqualStrings("fill", r.server);
            try testing.expectEqual(1, fill.callsFor(io, a.message_id));
        } else {
            try testing.expectEqualStrings("main", r.server);
            try testing.expectEqual(0, fill.callsFor(io, a.message_id));
        }
    }
    try testing.expectEqual(lacking.len, fill.totalCalls());
    try testing.expectEqual(arts.len, s.stats().done);
}

test "an article absent everywhere is missing" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 2, 4000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const gone = arts[2].message_id;
    try main.rule(gone, .{ .behavior = .absent });
    try fill.rule(gone, .{ .behavior = .absent });

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    const r = col.get(gone);
    try testing.expectEqual(.missing, r.status);
    try testing.expect(!r.has_part);
    try testing.expectEqualStrings("", r.server);
    try testing.expectEqual(error.ArticleNotFound, r.err.?);
    try testing.expectEqual(1, main.callsFor(io, gone));
    try testing.expectEqual(1, fill.callsFor(io, gone));
    const st = s.stats();
    try testing.expectEqual(1, st.missing);
    try testing.expectEqual(arts.len - 1, st.done);
}

const day: i64 = 86400;
const now_2024: i64 = 1709251200; // 2024-03-01

fn dated(c: *Corpus, date: i64) void {
    for (c.articles.items) |*a| a.date = date;
}

test "retention shorter than the article age skips the server" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 2, 4000, 1000);
    dated(&corpus, now_2024 - 100 * day);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .retention_days = 30, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{ .now = now_2024 });
    defer s.destroy();

    try testing.expectEqual(0, main.totalCalls());
    try testing.expectEqual(arts.len, fill.totalCalls());
    for (arts) |a| try testing.expectEqualStrings("fill", col.get(a.message_id).server);
}

test "a retention skip on the only server makes the articles missing" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    dated(&corpus, now_2024 - 10 * day);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;

    const s = try runAll(&.{.{ .name = "main", .workers = 2, .retention_days = 5, .fake = &main }}, arts, &col, .{ .now = now_2024 });
    defer s.destroy();

    try testing.expectEqual(0, main.totalCalls());
    try testing.expectEqual(arts.len, s.stats().missing);
}

test "an unknown article age is never skipped" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;

    const s = try runAll(&.{.{ .name = "main", .workers = 2, .retention_days = 1, .fake = &main }}, arts, &col, .{});
    defer s.destroy();

    try testing.expectEqual(arts.len, s.stats().done);
}

test "transient failures are retried on the same server" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const flaky = arts[1].message_id;
    try main.rule(flaky, .{ .behavior = .flaky, .until_call = 2 }); // drops twice, third attempt works

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    try testing.expectEqualStrings("main", col.get(flaky).server);
    try testing.expectEqual(3, main.callsFor(io, flaky));
    try testing.expectEqual(0, fill.totalCalls());
}

test "a server that keeps failing is given up after three attempts" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const flaky = arts[1].message_id;
    try main.rule(flaky, .{ .behavior = .flaky });

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    const r = col.get(flaky);
    try testing.expectEqual(.done, r.status);
    try testing.expectEqualStrings("fill", r.server);
    try testing.expectEqual(sched.max_attempts, main.callsFor(io, flaky));
    try testing.expectEqual(1, fill.callsFor(io, flaky));
}

test "a transient failure on the last server leaves the article missing" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const flaky = arts[0].message_id;
    try main.rule(flaky, .{ .behavior = .flaky });

    const s = try runAll(&.{.{ .name = "main", .fake = &main }}, arts, &col, .{});
    defer s.destroy();

    const r = col.get(flaky);
    try testing.expectEqual(.missing, r.status);
    try testing.expectEqual(error.Connection, r.err.?);
    try testing.expectEqual(sched.max_attempts, main.callsFor(io, flaky));
}

test "a corrupt main with a clean fill gives an undamaged part from fill" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 2, 5000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const bad = try pick(corpus.arena.allocator(), arts, 4, 1);
    try main.ruleAll(bad, .corrupt);

    const s = try runAll(&.{
        .{ .name = "main", .workers = 3, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 3, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |a| {
        const r = col.get(a.message_id);
        try testing.expectEqual(.done, r.status);
        try testing.expect(!r.damaged);
        try testing.expect(r.has_part and r.crc_match);
        try testing.expectEqualStrings(if (contains(bad, a.message_id)) "fill" else "main", r.server);
    }
    try testing.expectEqual(0, s.stats().damaged);
    try testing.expectEqual(bad.len, fill.totalCalls());
}

test "corrupt everywhere gives done and damaged with the best part" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const bad = arts[1].message_id;
    try main.rule(bad, .{ .behavior = .corrupt });
    try fill.rule(bad, .{ .behavior = .corrupt });

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    const r = col.get(bad);
    try testing.expectEqual(.done, r.status);
    try testing.expect(r.damaged);
    try testing.expect(r.has_part and !r.crc_match);
    try testing.expectEqual(1000, r.data.len);
    try testing.expectEqualStrings("main", r.server); // the first of equally good copies is kept
    try testing.expectEqual(1, main.callsFor(io, bad));
    try testing.expectEqual(1, fill.callsFor(io, bad));
    const st = s.stats();
    try testing.expectEqual(1, st.damaged);
    try testing.expectEqual(arts.len, st.done);
    try testing.expectEqual(0, st.missing);
}

test "a damaged copy is kept when the other server lacks the article" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 1, 3000, 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const bad = arts[0].message_id;
    try main.rule(bad, .{ .behavior = .corrupt });
    try fill.rule(bad, .{ .behavior = .absent });

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    const r = col.get(bad);
    try testing.expectEqual(.done, r.status);
    try testing.expect(r.damaged);
    try testing.expectEqualStrings("main", r.server);
}

test "the largest damaged copy wins" {
    // Two corrupt copies of different sizes: the longer one is kept.
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    try corpus.addFile(gpa, "small.bin", "0123456789", 1000);
    try corpus.addFile(gpa, "big.bin", "0123456789abcdefghij", 1000);
    // The same Article ID served by two fakes with different corpora.
    var other: Corpus = try .init(gpa);
    defer other.deinit(gpa);
    try other.addFile(gpa, "small.bin", "0123456789abcdefghij", 1000);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &other);
    defer fill.deinit();
    main.default = .corrupt;
    fill.default = .corrupt;
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items[0..1]; // small.bin.1@x

    const s = try runAll(&.{
        .{ .name = "main", .fake = &main },
        .{ .name = "fill", .priority = 1, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    const r = col.get("small.bin.1@x");
    try testing.expect(r.damaged);
    try testing.expectEqualStrings("fill", r.server);
    try testing.expectEqual(20, r.data.len);
}

test "same-level servers share load and try each article at most once" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 4, 30000, 500);
    var a: Fake = .init(gpa, &corpus);
    defer a.deinit();
    var b: Fake = .init(gpa, &corpus);
    defer b.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    for ([_]*Fake{ &a, &b, &fill }) |f| f.latency_ms = 1;
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    // Neither level-0 server has these, so both must try them exactly once.
    const both = try pick(corpus.arena.allocator(), arts, 10, 0);
    try a.ruleAll(both, .absent);
    try b.ruleAll(both, .absent);

    const s = try runAll(&.{
        .{ .name = "a", .workers = 3, .fake = &a },
        .{ .name = "b", .workers = 3, .fake = &b },
        .{ .name = "fill", .priority = 1, .workers = 3, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |art| {
        const id = art.message_id;
        const ca = a.callsFor(io, id);
        const cb = b.callsFor(io, id);
        try testing.expect(ca <= 1 and cb <= 1);
        const r = col.get(id);
        try testing.expectEqual(.done, r.status);
        if (contains(both, id)) {
            try testing.expectEqual(1, ca);
            try testing.expectEqual(1, cb);
            try testing.expectEqualStrings("fill", r.server);
        } else {
            try testing.expectEqual(1, ca + cb); // exactly one of the siblings serves it
            try testing.expectEqual(0, fill.callsFor(io, id));
        }
    }
    try testing.expect(a.totalCalls() > 0);
    try testing.expect(b.totalCalls() > 0);
}

/// Records the servers declared dead.
const Downs = struct {
    mu: Io.Mutex = .init,
    names: std.ArrayList([]const u8) = .empty,
    last: ?sched.FetchError = null,

    pub fn serverDown(d: *Downs, name: []const u8, err: sched.FetchError) void {
        d.mu.lockUncancelable(io);
        defer d.mu.unlock(io);
        d.names.append(gpa, name) catch @panic("oom");
        d.last = err;
    }
};

test "an auth failure on the fill server marks it dead and the run completes" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 2, 8000, 500);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    fill.default = .auth;
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    const lacking = try pick(corpus.arena.allocator(), arts, 5, 0);
    try main.ruleAll(lacking, .absent);
    var downs: Downs = .{};
    defer downs.names.deinit(gpa);

    const s = try runAll(&.{
        .{ .name = "main", .workers = 3, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 3, .fake = &fill },
    }, arts, &col, .{ .on_server_down = .init(&downs) });
    defer s.destroy();

    try testing.expectEqual(1, downs.names.items.len); // it fires once
    try testing.expectEqualStrings("fill", downs.names.items[0]);
    try testing.expectEqual(error.Auth, downs.last.?);
    for (lacking) |id| try testing.expectEqual(.missing, col.get(id).status);
    const st = s.stats();
    try testing.expectEqual(lacking.len, st.missing);
    try testing.expectEqual(arts.len - lacking.len, st.done);
    try testing.expect(st.servers()[1].down);
    try testing.expect(!st.servers()[0].down);
}

test "a dead main server lets the fill server take over" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 2, 4000, 500);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.default = .disabled;
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    var downs: Downs = .{};
    defer downs.names.deinit(gpa);

    const s = try runAll(&.{
        .{ .name = "main", .workers = 3, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 3, .fake = &fill },
    }, arts, &col, .{ .on_server_down = .init(&downs) });
    defer s.destroy();

    try testing.expectEqual(1, downs.names.items.len);
    try testing.expectEqual(error.ServerDisabled, downs.last.?);
    for (arts) |a| try testing.expectEqualStrings("fill", col.get(a.message_id).server);
    try testing.expectEqual(arts.len, s.stats().done);
}

test "every server dead makes the remaining articles missing" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 2, 4000, 500);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.default = .auth;
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    fill.default = .auth;
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;

    const s = try runAll(&.{
        .{ .name = "main", .workers = 3, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 3, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    try testing.expectEqual(arts.len, s.stats().missing);
}

test "reassembly from the delivered parts is byte-exact" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const files = try fixtureFiles(&corpus, 5, 25000, 700);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;
    try main.ruleAll(try pick(corpus.arena.allocator(), arts, 7, 2), .absent);
    try main.ruleAll(try pick(corpus.arena.allocator(), arts, 11, 5), .corrupt);

    const s = try runAll(&.{
        .{ .name = "main", .workers = 4, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 4, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (files, 0..) |want, fi| {
        const got = try gpa.alloc(u8, want.len);
        defer gpa.free(got);
        @memset(got, 0);
        var name_buf: [16]u8 = undefined;
        const name = try std.fmt.bufPrint(&name_buf, "file{d:0>2}.bin", .{fi});
        for (arts) |a| {
            if (a.file_key != fi) continue;
            const r = col.get(a.message_id);
            try testing.expectEqual(.done, r.status);
            try testing.expectEqualStrings(name, r.name);
            try testing.expectEqual(@as(i64, @intCast(want.len)), r.size);
            @memcpy(got[@intCast(r.begin - 1)..][0..r.data.len], r.data);
        }
        try testing.expectEqualSlices(u8, want, got);
    }
}

test "dispatch follows file and segment order at every level" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = [_]sched.Article{
        .{ .file_key = 1, .segment_number = 2, .message_id = "b2@x" },
        .{ .file_key = 0, .segment_number = 2, .message_id = "a2@x" },
        .{ .file_key = 1, .segment_number = 1, .message_id = "b1@x" },
        .{ .file_key = 0, .segment_number = 1, .message_id = "a1@x" },
    };
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    try main.rule("a2@x", .{ .behavior = .absent });
    try main.rule("b2@x", .{ .behavior = .absent });
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{
        .{ .name = "main", .fake = &main },
        .{ .name = "fill", .priority = 5, .fake = &fill },
    }, &arts, &col, .{});
    defer s.destroy();

    const want_main = [_][]const u8{ "a1@x", "a2@x", "b1@x", "b2@x" };
    const want_fill = [_][]const u8{ "a2@x", "b2@x" };
    try testing.expectEqual(want_main.len, main.order.items.len);
    for (want_main, main.order.items) |w, g| try testing.expectEqualStrings(w, g);
    try testing.expectEqual(want_fill.len, fill.order.items.len);
    for (want_fill, fill.order.items) |w, g| try testing.expectEqualStrings(w, g);
}

test "higher-level workers only take articles promoted to them" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(500);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 8, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    try testing.expectEqual(500, main.totalCalls());
    try testing.expectEqual(0, fill.totalCalls());
}

test "a malformed part counts as absent for that server" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(3);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.default = .malformed;
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{
        .{ .name = "main", .fake = &main },
        .{ .name = "fill", .priority = 1, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |a| {
        try testing.expectEqualStrings("fill", col.get(a.message_id).server);
        try testing.expectEqual(1, main.callsFor(io, a.message_id)); // garbage is not retried
    }
}

test "a malformed part on the only server is missing with the decode error" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(1);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.default = .malformed;
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{.{ .name = "main", .fake = &main }}, arts, &col, .{});
    defer s.destroy();

    const r = col.get(arts[0].message_id);
    try testing.expectEqual(.missing, r.status);
    try testing.expect(r.err != null);
    try testing.expect(r.err.? != error.ArticleNotFound);
}

test "a part without an announced CRC is good, not damaged" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(5);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.default = .no_crc;
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{
        .{ .name = "main", .workers = 2, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 2, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |a| {
        const r = col.get(a.message_id);
        try testing.expectEqual(.done, r.status);
        try testing.expect(!r.damaged);
        try testing.expectEqualStrings("main", r.server);
        try testing.expect(r.has_part and !r.has_expected_crc);
    }
    try testing.expectEqual(0, fill.totalCalls()); // no fallback for a part that announces no CRC
    try testing.expectEqual(0, s.stats().damaged);
}

test "a wrong announced CRC still falls back and is damaged if alone" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(2);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.default = .corrupt;
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    fill.default = .absent;
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{
        .{ .name = "main", .fake = &main },
        .{ .name = "fill", .priority = 1, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();

    for (arts) |a| {
        try testing.expect(col.get(a.message_id).damaged);
        try testing.expectEqual(1, fill.callsFor(io, a.message_id));
    }
    try testing.expectEqual(2, s.stats().damaged);
}

test "the context cancel stops the run and leaves no task behind" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    _ = try fixtureFiles(&corpus, 4, 50000, 500);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.latency_ms = 5;
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const arts = corpus.articles.items;

    const s = try create(&.{
        .{ .name = "main", .workers = 4, .fake = &main },
        .{ .name = "fill", .priority = 1, .workers = 4, .fake = &fill },
    }, arts, &col, .{});
    defer s.destroy();
    var fut = try io.concurrent(sched.Scheduler.run, .{ s, io });
    while (col.n() < 20) try io.sleep(.fromMilliseconds(1), .awake);

    try testing.expectError(error.Canceled, fut.cancel(io));

    const delivered = col.n();
    try testing.expect(delivered >= 20);
    try testing.expect(delivered < arts.len); // the run stopped early
    try io.sleep(.fromMilliseconds(30), .awake);
    try testing.expectEqual(delivered, col.n()); // nothing is delivered after run returned
}

test "a cancel while every worker is inside a fetch returns at once" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(10);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.latency_ms = 10_000; // a fetch that would outlast the test
    var col: Collector = .init();
    defer col.deinit();

    const s = try create(&.{.{ .name = "main", .workers = 2, .fake = &main }}, arts, &col, .{});
    defer s.destroy();
    var fut = try io.concurrent(sched.Scheduler.run, .{ s, io });
    // Cancelling a task that has not started yet can be lost in std 0.17, so
    // let the workers get into their fetches first.
    while (main.totalCalls() < 2) try io.sleep(.fromMilliseconds(1), .awake);

    try testing.expectError(error.Canceled, fut.cancel(io));
    try testing.expectEqual(0, col.n());
}

test "create rejects bad configs" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    var f: Fake = .init(gpa, &corpus);
    defer f.deinit();
    try testing.expectError(error.NoServers, sched.Scheduler.create(gpa, .{ .servers = &.{}, .articles = &.{} }));
    const one: sched.Server = .{ .name = "a", .fetcher = f.fetcher() };
    var many: [sched.max_servers + 1]sched.Server = undefined;
    @memset(&many, one);
    try testing.expectError(error.TooManyServers, sched.Scheduler.create(gpa, .{ .servers = &many, .articles = &.{} }));
    const s = try sched.Scheduler.create(gpa, .{ .servers = many[0..sched.max_servers], .articles = &.{} });
    s.destroy();
}

test "create frees everything when an allocation fails" {
    const T = struct {
        fn go(a: std.mem.Allocator, fetcher: sched.Fetcher) !void {
            const arts = [_]sched.Article{
                .{ .file_key = 1, .segment_number = 1, .message_id = "a@x" },
                .{ .file_key = 0, .segment_number = 1, .message_id = "b@x" },
            };
            const servers = [_]sched.Server{
                .{ .name = "a", .fetcher = fetcher },
                .{ .name = "b", .priority = 3, .fetcher = fetcher },
                .{ .name = "c", .priority = 3, .fetcher = fetcher },
            };
            const s = try sched.Scheduler.create(a, .{ .servers = &servers, .articles = &arts });
            s.destroy();
        }
    };
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    var f: Fake = .init(gpa, &corpus);
    defer f.deinit();
    try testing.checkAllAllocationFailures(gpa, T.go, .{f.fetcher()});
}

test "run twice is an error" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(2);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{.{ .name = "a", .fake = &main }}, arts, &col, .{});
    defer s.destroy();

    try testing.expectError(error.AlreadyRun, s.run(io));
}

test "no articles finishes at once" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(&.{.{ .name = "a", .fake = &main }}, &.{}, &col, .{});
    defer s.destroy();

    try testing.expectEqual(0, main.totalCalls());
}

fn independentRun() !void {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(300);
    var f: Fake = .init(gpa, &corpus);
    defer f.deinit();
    var col: Collector = .init();
    defer col.deinit();
    const s = try runAll(&.{.{ .name = "a", .workers = 3, .fake = &f }}, arts, &col, .{});
    defer s.destroy();
    try testing.expectEqual(300, col.n());
}

test "two schedulers run concurrently without shared state" {
    var g: Io.Group = .init;
    defer g.cancel(io);
    var errs: [2]?anyerror = .{ null, null };
    const T = struct {
        fn go(slot: *?anyerror) void {
            independentRun() catch |e| {
                slot.* = e;
            };
        }
    };
    for (&errs) |*e| try g.concurrent(io, T.go, .{e});
    try g.await(io);
    try testing.expectEqual(null, errs[0]);
    try testing.expectEqual(null, errs[1]);
}

/// Records the attempt numbers the scheduler waits for.
const Waits = struct {
    seen: std.ArrayList(u32) = .empty,

    pub fn backoff(w: *Waits, attempt: u32) Io.Duration {
        w.seen.append(gpa, attempt) catch @panic("oom");
        return .fromMilliseconds(0);
    }
};

test "the backoff grows with the attempt number" {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(1);
    var f: Fake = .init(gpa, &corpus);
    defer f.deinit();
    f.default = .flaky;
    var col: Collector = .init();
    defer col.deinit();
    var waits: Waits = .{};
    defer waits.seen.deinit(gpa);

    const s = try runAll(&.{.{ .name = "a", .fake = &f }}, arts, &col, .{ .backoff = .init(&waits) });
    defer s.destroy();

    try testing.expectEqualSlices(u32, &.{ 1, 2 }, waits.seen.items); // waits between the three attempts
}

test "the default backoff is 200 ms times the attempt" {
    const b = sched.Backoff.default;
    try testing.expectEqual(200, b.call(null, 1).toMilliseconds());
    try testing.expectEqual(600, b.call(null, 3).toMilliseconds());
}

test "a body larger than the wire cap is absent, not a hang" {
    // A server that never sends =ybegin: endless junk lines.
    const Junk = struct {
        fn body(_: *anyopaque, _: Io, _: []const u8, sink: sched.usenet_types.Sink) sched.FetchError!void {
            const line: [1024]u8 = @splat('x');
            while (true) sink.line(&line) catch return error.Aborted;
        }
    };
    var dummy: u8 = 0;
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(1);
    var col: Collector = .init();
    defer col.deinit();
    const s = try sched.Scheduler.create(gpa, .{
        .servers = &.{.{ .name = "junk", .fetcher = .{ .ctx = &dummy, .body_fn = Junk.body } }},
        .articles = arts,
        .on_result = .init(&col),
        .backoff = .none,
    });
    defer s.destroy();

    try s.run(io);

    const r = col.get(arts[0].message_id);
    try testing.expectEqual(.missing, r.status);
    try testing.expectEqual(error.TooLarge, r.err.?);
    try testing.expect(s.stats().servers()[0].bytes > 32 << 20);
    try testing.expectEqual(1, s.stats().servers()[0].requests); // not retried
}

// Stress and performance

const Server4 = struct { server: u64 };

/// Per (server, article) behavior; the 4th server dies after a while.
fn stressScript(ctx: ?*anyopaque, id: []const u8, call: u32) fake.Behavior {
    const server: *const Server4 = @ptrCast(@alignCast(ctx.?));
    return switch (hashN(id, server.server) % 100) {
        0...29 => .absent,
        30...39 => .corrupt,
        40...46 => if (call < 3) .flaky else .ok,
        47...49 => .flaky, // never recovers
        else => .ok,
    };
}

const Dying = struct {
    served: std.atomic.Value(u32) = .init(0),
    inner: Server4 = .{ .server = 3 },
    died: std.atomic.Value(bool) = .init(false),

    fn script(ctx: ?*anyopaque, id: []const u8, call: u32) fake.Behavior {
        const d: *Dying = @ptrCast(@alignCast(ctx.?));
        if (d.served.fetchAdd(1, .monotonic) + 1 > 60) {
            d.died.store(true, .monotonic);
            return .disabled;
        }
        return stressScript(&d.inner, id, call);
    }
};

test "stress: four servers, many workers, a mix of errors" {
    const n = 2000;
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(n);
    var ids: [4]Server4 = .{ .{ .server = 0 }, .{ .server = 1 }, .{ .server = 2 }, .{ .server = 3 } };
    var dying: Dying = .{};
    var fakes: [4]Fake = undefined;
    for (&fakes, 0..) |*f, i| {
        f.* = .init(gpa, &corpus);
        f.script = .{ .ctx = &ids[i], .call = stressScript };
    }
    defer for (&fakes) |*f| f.deinit();
    fakes[3].script = .{ .ctx = &dying, .call = Dying.script };
    var col: Collector = .init();
    defer col.deinit();
    var downs: Downs = .{};
    defer downs.names.deinit(gpa);

    const s = try runAll(&.{
        .{ .name = "a0", .workers = 4, .fake = &fakes[0] },
        .{ .name = "a1", .workers = 4, .fake = &fakes[1] },
        .{ .name = "b", .priority = 1, .workers = 6, .fake = &fakes[2] },
        .{ .name = "c", .priority = 2, .workers = 6, .fake = &fakes[3] },
    }, arts, &col, .{ .on_server_down = .init(&downs) });
    defer s.destroy();

    try testing.expect(downs.names.items.len <= 1); // it fires once
    for (arts) |a| {
        const id = a.message_id;
        for (&fakes) |*f| try testing.expect(f.callsFor(io, id) <= sched.max_attempts);
        const r = col.get(id);
        switch (r.status) {
            .done => {
                try testing.expect(r.has_part);
                try testing.expectEqual(r.damaged, !r.crc_match);
            },
            .missing => try testing.expect(!r.has_part),
        }
        // Among the servers that cannot die the first level with a good copy
        // wins.
        const good = struct {
            fn at(i: u64, sid: []const u8) bool {
                const h = hashN(sid, i) % 100;
                return h >= 50 or (h >= 40 and h < 47);
            }
        }.at;
        if (good(0, id) or good(1, id)) {
            try testing.expectEqual(.done, r.status);
            try testing.expect(!r.damaged);
            try testing.expect(std.mem.eql(u8, r.server, "a0") or std.mem.eql(u8, r.server, "a1"));
            try testing.expectEqual(0, fakes[2].callsFor(io, id));
            try testing.expectEqual(0, fakes[3].callsFor(io, id));
        } else if (good(2, id)) {
            try testing.expect(!r.damaged);
            try testing.expectEqualStrings("b", r.server);
            try testing.expectEqual(0, fakes[3].callsFor(io, id));
        }
    }
    const st = s.stats();
    try testing.expectEqual(n, st.done + st.missing);
    var total: u64 = 0;
    for (&fakes) |*f| total += f.totalCalls();
    var reqs: u64 = 0;
    for (st.servers()) |sv| reqs += sv.requests;
    try testing.expectEqual(total, reqs); // stats count every request
    try testing.expect(dying.died.load(.monotonic)); // the dying server was exercised
    try testing.expect(st.servers()[3].down);
}

/// The behavior table of the fuzz target: one byte per (server, article).
const Table = struct {
    bytes: []const u8,
    server: usize,

    fn kind(t: Table, index: usize) u8 {
        return t.bytes[(t.server * 41 + index) % t.bytes.len] % 8;
    }

    fn script(ctx: ?*anyopaque, id: []const u8, call: u32) fake.Behavior {
        const t: *const Table = @ptrCast(@alignCast(ctx.?));
        const index = std.fmt.parseInt(usize, id[1..8], 10) catch unreachable;
        return switch (t.kind(index)) {
            0, 1 => .ok,
            2, 3 => .absent,
            4 => .corrupt,
            5 => .flaky, // never recovers
            6 => .no_crc,
            else => if (call < 3) .flaky else .ok, // recovers on the last attempt
        };
    }

    fn good(t: Table, index: usize) bool {
        return switch (t.kind(index)) {
            0, 1, 6, 7 => true,
            else => false,
        };
    }
};

/// One case of the mix: `b` decides the servers, priorities, workers and what
/// each server does with each article. At least 12 bytes.
fn mixCase(b: []const u8) !void {
    const n_servers = 1 + b[0] % 4;
    const n = 1 + b[1] % 40;

    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(n);
    var fakes: [4]Fake = undefined;
    var tables: [4]Table = undefined;
    var specs: [4]Spec = undefined;
    var names: [4][2]u8 = undefined;
    for (0..n_servers) |i| {
        tables[i] = .{ .bytes = b[2..], .server = i };
        fakes[i] = .init(gpa, &corpus);
        fakes[i].script = .{ .ctx = &tables[i], .call = Table.script };
        names[i] = .{ 's', @intCast('0' + i) };
        specs[i] = .{ .name = &names[i], .priority = b[2 + i] % 3, .workers = 1 + b[6 + i] % 3, .fake = &fakes[i] };
    }
    defer for (fakes[0..n_servers]) |*f| f.deinit();
    var col: Collector = .init();
    defer col.deinit();

    const s = try runAll(specs[0..n_servers], arts, &col, .{});
    defer s.destroy();

    for (arts, 0..) |a, idx| {
        const r = col.get(a.message_id);
        var best: ?i64 = null; // lowest priority holding a good copy
        var any_damaged = false;
        for (0..n_servers) |i| {
            if (tables[i].good(idx)) best = @min(best orelse specs[i].priority, specs[i].priority);
            if (tables[i].kind(idx) == 4) any_damaged = true;
            const calls = fakes[i].callsFor(io, a.message_id);
            try testing.expect(calls <= sched.max_attempts);
        }
        if (best) |lvl| {
            try testing.expectEqual(.done, r.status);
            try testing.expect(!r.damaged);
            for (0..n_servers) |i| {
                if (specs[i].priority > lvl) try testing.expectEqual(0, fakes[i].callsFor(io, a.message_id));
            }
        } else if (any_damaged) {
            try testing.expectEqual(.done, r.status);
            try testing.expect(r.damaged);
            for (0..n_servers) |i| try testing.expect(fakes[i].callsFor(io, a.message_id) >= 1);
        } else {
            try testing.expectEqual(.missing, r.status);
            try testing.expect(r.err != null);
        }
    }
    const st = s.stats();
    try testing.expectEqual(n, st.done + st.missing);
}

test "fuzz: any mix of servers, priorities and errors resolves every article once" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [300]u8 = undefined;
            const len = smith.slice(&buf);
            if (len < 12) return;
            try mixCase(buf[0..len]);
        }
    }.one, .{
        .corpus = &.{
            // Three servers on three levels and a spread of behaviors.
            seed("\x02\x1e\x00\x01\x02\x00\x01\x02\x03\x03\x00\x01\x02\x03\x04\x05\x06\x07\x02\x03\x04\x05\x02\x02\x02\x05\x05\x04"),
            // Two servers on one level: they share the load.
            seed("\x01\x14\x00\x00\x00\x00\x00\x00\x01\x01\x02\x03\x04\x05\x06\x07\x00\x01\x02\x03"),
        },
    });
}

/// A fuzz seed: a Smith slice is a u32 little-endian length and the bytes.
fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}

test "random mixes of servers and errors resolve every article once" {
    var prng: std.Random.DefaultPrng = .init(20260101);
    var buf: [200]u8 = undefined;
    for (0..60) |_| {
        prng.random().bytes(&buf);
        try mixCase(&buf);
    }
}

fn perfScript(_: ?*anyopaque, id: []const u8, _: u32) fake.Behavior {
    return if (hashN(id, 1) % 10 == 0) .absent else .ok;
}

const Counter = struct {
    n: std.atomic.Value(u64) = .init(0),

    pub fn result(c: *Counter, _: sched.Result) void {
        _ = c.n.fetchAdd(1, .monotonic);
    }
};

/// Runs `n` synthetic articles over a main and a fill server where main lacks
/// 10% and returns the duration in nanoseconds.
fn runLarge(n: usize) !i96 {
    var corpus: Corpus = try .init(gpa);
    defer corpus.deinit(gpa);
    const arts = try corpus.synthetic(n);
    var main: Fake = .init(gpa, &corpus);
    defer main.deinit();
    main.track = false;
    main.script = .{ .call = perfScript };
    var fill: Fake = .init(gpa, &corpus);
    defer fill.deinit();
    fill.track = false;
    var counter: Counter = .{};
    const s = try sched.Scheduler.create(gpa, .{
        .servers = &.{
            .{ .name = "main", .workers = 8, .fetcher = main.fetcher() },
            .{ .name = "fill", .priority = 1, .workers = 8, .fetcher = fill.fetcher() },
        },
        .articles = arts,
        .on_result = .init(&counter),
    });
    defer s.destroy();
    const t0 = Io.Clock.awake.now(io);
    try s.run(io);
    const dt = t0.durationTo(Io.Clock.awake.now(io)).nanoseconds;
    try testing.expectEqual(n, counter.n.load(.monotonic));
    try testing.expectEqual(n, s.stats().done);
    return dt;
}

test "perf: large NZB dispatch is not quadratic" {
    const small = try runLarge(25_000);
    const large = try runLarge(200_000);
    const secs = @as(f64, @floatFromInt(large)) / 1e9;
    std.debug.print("scheduler perf: 25k in {d} ms, 200k in {d} ms = {d:.0} articles/s\n", .{
        @divTrunc(small, 1_000_000), @divTrunc(large, 1_000_000), 200_000.0 / secs,
    });
    try testing.expect(large < 60 * std.time.ns_per_s);
    // 8x the articles: linear work is about 8x, quadratic about 64x.
    try testing.expect(@as(f64, @floatFromInt(large)) / @as(f64, @floatFromInt(@max(small, std.time.ns_per_ms))) < 30.0);
}
