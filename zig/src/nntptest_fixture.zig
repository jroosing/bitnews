//! Test support: yEnc-encodes files into Article bodies and an NZB that
//! lists them (Go: nntptest encode.go and fixture.go). The encoder does not
//! use the yEnc decoder's code, so a decoder bug cannot hide behind a
//! matching encoder bug.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Encoded characters per data line, as most posters do.
pub const default_line_length = 128;

/// The lines of one Article body, without terminators.
pub const Lines = []const []const u8;

/// yEnc-encodes `file` as Article bodies of at most `part_size` decoded bytes
/// each, with data lines of `line_len` encoded characters (one more when the
/// last character is escaped). A file that fits in one part is a single-part
/// file: no part=, no =ypart, and crc32= in the trailer. An empty file gives
/// one article without data lines. The result is allocated from `arena`.
pub fn encodeArticles(arena: Allocator, name: []const u8, file: []const u8, part_size: usize, line_len: usize) Allocator.Error![]const Lines {
    std.debug.assert(part_size > 0 and line_len > 0);
    if (file.len <= part_size) {
        var lines: std.ArrayList([]const u8) = .empty;
        try lines.append(arena, try std.fmt.allocPrint(arena, "=ybegin line={d} size={d} name={s}", .{ line_len, file.len, name }));
        try encodeLines(arena, &lines, file, line_len);
        try lines.append(arena, try std.fmt.allocPrint(arena, "=yend size={d} crc32={x:0>8}", .{ file.len, crc(file) }));
        const out = try arena.alloc(Lines, 1);
        out[0] = try lines.toOwnedSlice(arena);
        return out;
    }
    const total = (file.len + part_size - 1) / part_size;
    const out = try arena.alloc(Lines, total);
    for (out, 1..) |*article, n| {
        const begin = (n - 1) * part_size;
        const end = @min(begin + part_size, file.len);
        const data = file[begin..end];
        var lines: std.ArrayList([]const u8) = .empty;
        try lines.append(arena, try std.fmt.allocPrint(arena, "=ybegin part={d} total={d} line={d} size={d} name={s}", .{ n, total, line_len, file.len, name }));
        try lines.append(arena, try std.fmt.allocPrint(arena, "=ypart begin={d} end={d}", .{ begin + 1, end }));
        try encodeLines(arena, &lines, data, line_len);
        if (n == total) {
            try lines.append(arena, try std.fmt.allocPrint(arena, "=yend size={d} part={d} pcrc32={x:0>8} crc32={x:0>8}", .{ data.len, n, crc(data), crc(file) }));
        } else {
            try lines.append(arena, try std.fmt.allocPrint(arena, "=yend size={d} part={d} pcrc32={x:0>8}", .{ data.len, n, crc(data) }));
        }
        article.* = try lines.toOwnedSlice(arena);
    }
    return out;
}

fn crc(data: []const u8) u32 {
    return std.hash.Crc32.hash(data);
}

/// NUL, LF, CR and '=' are always escaped; tab and space at the start or end
/// of a line, and '.' at the start of a line, so the lines survive NNTP
/// transport.
fn encodeLines(arena: Allocator, lines: *std.ArrayList([]const u8), data: []const u8, line_len: usize) Allocator.Error!void {
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
            try line.append(arena, '=');
            try line.append(arena, c +% 64);
        } else try line.append(arena, c);
        if (line.items.len >= line_len) {
            try lines.append(arena, try line.toOwnedSlice(arena));
            line = .empty;
        }
    }
    if (line.items.len > 0) try lines.append(arena, try line.toOwnedSlice(arena));
}

/// A copy of a body with one byte changed so that its CRC32 no longer
/// matches while the structure and the decoded length stay valid: a data
/// character is replaced by another plain one. A body without data lines
/// gets a wrong CRC in its trailer instead.
pub fn corruptLines(gpa: Allocator, lines: Lines) Allocator.Error![][]u8 {
    var out = try gpa.alloc([]u8, lines.len);
    var done: usize = 0;
    errdefer {
        for (out[0..done]) |l| gpa.free(l);
        gpa.free(out);
    }
    while (done < lines.len) : (done += 1) out[done] = try gpa.dupe(u8, lines[done]);
    for (out) |l| {
        if (std.mem.startsWith(u8, l, "=y")) continue;
        for (l, 0..) |ch, j| {
            if (ch == '=' or (j > 0 and l[j - 1] == '=')) continue; // part of an escape sequence
            if (plainNeighbor(ch)) |repl| {
                l[j] = repl;
                return out;
            }
        }
    }
    for (out) |l| {
        if (std.mem.startsWith(u8, l, "=yend ")) {
            flipLastHexDigit(l);
            return out;
        }
    }
    return out;
}

/// A different byte that needs no escaping anywhere in a line.
fn plainNeighbor(c: u8) ?u8 {
    for (0..8) |bit| {
        const r = c ^ (@as(u8, 1) << @intCast(bit));
        switch (r) {
            0, '\n', '\r', '=', ' ', '\t', '.' => continue,
            else => return r,
        }
    }
    return null;
}

/// Changes the last hex digit of the first crc32 value in a =yend line.
fn flipLastHexDigit(l: []u8) void {
    for ([_][]const u8{ "pcrc32=", "crc32=" }) |key| {
        if (std.mem.find(u8, l, key)) |at| {
            const end = at + key.len + 8;
            if (end <= l.len) {
                l[end - 1] = if (l[end - 1] == '0') '1' else '0';
                return;
            }
        }
    }
}

// ---- fixtures ---------------------------------------------------------------------

/// Everything a Fixture owns lives in one arena.
pub const Fixture = struct {
    arena: std.heap.ArenaAllocator,
    /// The document, parseable by the NZB parser.
    nzb: []const u8,
    /// Message id and yEnc body lines of each Article.
    articles: []const ArticleEntry,
    /// The posted files, sorted by name.
    files: []const FixtureFile,

    pub const ArticleEntry = struct { id: []const u8, lines: Lines };

    pub fn deinit(f: *Fixture) void {
        f.arena.deinit();
    }

    /// Every message id, file by file in part order.
    pub fn messageIds(f: *const Fixture, gpa: Allocator) Allocator.Error![][]const u8 {
        var ids: std.ArrayList([]const u8) = .empty;
        for (f.files) |file| try ids.appendSlice(gpa, file.message_ids);
        return ids.toOwnedSlice(gpa);
    }
};

pub const FixtureFile = struct {
    name: []const u8,
    data: []const u8,
    /// The Article of each part, in part order.
    message_ids: []const []const u8,
};

pub const FixtureInput = struct { name: []const u8, data: []const u8 };

pub const FixtureOptions = struct {
    /// The posting time written into the NZB (Unix seconds), which the
    /// scheduler compares with Server retention. Default 2024-01-01 UTC.
    date: i64 = 1704067200,
    line_length: usize = default_line_length,
};

pub const FixtureError = error{ BadArgument, OutOfMemory };

/// yEnc-encodes every file into Articles of at most `part_size` decoded bytes
/// and writes an NZB listing them. Deterministic: the same input gives the
/// same document and message ids. File names must be non-empty and free of
/// quotes and control characters. `inputs` need not be sorted.
pub fn buildFixture(gpa: Allocator, inputs: []const FixtureInput, part_size: usize, options: FixtureOptions) FixtureError!Fixture {
    if (part_size == 0 or options.line_length == 0) return error.BadArgument;
    for (inputs) |in| {
        if (in.name.len == 0 or std.mem.findAny(u8, in.name, "\"\r\n\x00") != null) return error.BadArgument;
    }
    var arena: std.heap.ArenaAllocator = .init(gpa);
    errdefer arena.deinit();
    const a = arena.allocator();

    const sorted = try a.dupe(FixtureInput, inputs);
    std.mem.sort(FixtureInput, sorted, {}, struct {
        fn less(_: void, x: FixtureInput, y: FixtureInput) bool {
            return std.mem.lessThan(u8, x.name, y.name);
        }
    }.less);

    var doc: std.Io.Writer.Allocating = .init(a);
    const w = &doc.writer;
    w.writeAll("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<nzb xmlns=\"http://www.newzbin.com/DTD/2003/nzb\">\n") catch return error.OutOfMemory;
    var entries: std.ArrayList(Fixture.ArticleEntry) = .empty;
    const files = try a.alloc(FixtureFile, sorted.len);
    for (sorted, files) |in, *out| {
        var h: std.crypto.hash.sha2.Sha256 = .init(.{});
        h.update(in.name);
        h.update("\x00");
        h.update(in.data);
        var sum: [32]u8 = undefined;
        h.final(&sum);
        const prefix = std.fmt.bytesToHex(sum[0..8].*, .lower);

        const articles = try encodeArticles(a, in.name, in.data, part_size, options.line_length);
        const ids = try a.alloc([]const u8, articles.len);
        out.* = .{ .name = in.name, .data = in.data, .message_ids = ids };

        var segs: std.Io.Writer.Allocating = .init(a);
        for (articles, ids, 1..) |lines, *id, n| {
            id.* = try std.fmt.allocPrint(a, "{s}.{d}@fixture.test", .{ &prefix, n });
            try entries.append(a, .{ .id = id.*, .lines = lines });
            var size: usize = 0;
            for (lines) |l| size += l.len + 2;
            segs.writer.print("      <segment bytes=\"{d}\" number=\"{d}\">{s}</segment>\n", .{ size, n, id.* }) catch return error.OutOfMemory;
        }
        const subject = try std.fmt.allocPrint(a, "\"{s}\" yEnc (1/{d})", .{ in.name, articles.len });
        w.print("  <file poster=\"poster@fixture.test\" date=\"{d}\" subject=\"", .{options.date}) catch return error.OutOfMemory;
        escapeText(w, subject) catch return error.OutOfMemory;
        w.writeAll("\">\n    <groups><group>alt.binaries.fixture</group></groups>\n    <segments>\n") catch return error.OutOfMemory;
        w.writeAll(segs.written()) catch return error.OutOfMemory;
        w.writeAll("    </segments>\n  </file>\n") catch return error.OutOfMemory;
    }
    w.writeAll("</nzb>\n") catch return error.OutOfMemory;
    return .{ .arena = arena, .nzb = doc.written(), .articles = try entries.toOwnedSlice(a), .files = files };
}

/// Go's xml.EscapeText.
fn escapeText(w: *std.Io.Writer, s: []const u8) std.Io.Writer.Error!void {
    for (s) |c| switch (c) {
        '"' => try w.writeAll("&#34;"),
        '\'' => try w.writeAll("&#39;"),
        '&' => try w.writeAll("&amp;"),
        '<' => try w.writeAll("&lt;"),
        '>' => try w.writeAll("&gt;"),
        '\t' => try w.writeAll("&#x9;"),
        '\n' => try w.writeAll("&#xA;"),
        '\r' => try w.writeAll("&#xD;"),
        else => try w.writeByte(c),
    };
}

// ---- tests -----------------------------------------------------------------------------

const testing = std.testing;

/// A minimal yEnc decoder for the tests only: checks the structure and the
/// CRC, returns the part's data and its begin offset (0-based).
const Decoded = struct { data: []u8, begin: usize, part: usize, crc_ok: bool };

fn decode(gpa: Allocator, lines: Lines) !Decoded {
    var data: std.ArrayList(u8) = .empty;
    errdefer data.deinit(gpa);
    var begin: usize = 0;
    var part: usize = 1;
    var crc_expected: ?u32 = null;
    var size_expected: usize = 0;
    for (lines) |l| {
        if (std.mem.startsWith(u8, l, "=ybegin ")) {
            if (field(l, "part=")) |p| part = try std.fmt.parseInt(usize, p, 10);
        } else if (std.mem.startsWith(u8, l, "=ypart ")) {
            begin = try std.fmt.parseInt(usize, field(l, "begin=").?, 10) - 1;
        } else if (std.mem.startsWith(u8, l, "=yend ")) {
            size_expected = try std.fmt.parseInt(usize, field(l, "size=").?, 10);
            const key = if (field(l, "pcrc32=") != null) "pcrc32=" else "crc32=";
            crc_expected = try std.fmt.parseInt(u32, field(l, key).?, 16);
        } else {
            var i: usize = 0;
            while (i < l.len) : (i += 1) {
                var c = l[i];
                if (c == '=') {
                    i += 1;
                    c = l[i] -% 64;
                }
                try data.append(gpa, c -% 42);
            }
        }
    }
    try testing.expectEqual(size_expected, data.items.len);
    const ok = crc(data.items) == crc_expected.?;
    return .{ .data = try data.toOwnedSlice(gpa), .begin = begin, .part = part, .crc_ok = ok };
}

fn field(line: []const u8, key: []const u8) ?[]const u8 {
    const at = std.mem.find(u8, line, key) orelse return null;
    if (at > 0 and line[at - 1] != ' ') return null;
    const rest = line[at + key.len ..];
    const end = std.mem.findScalar(u8, rest, ' ') orelse rest.len;
    return rest[0..end];
}

fn randomBytes(seed: u64, n: usize, out: []u8) []u8 {
    var prng: std.Random.DefaultPrng = .init(seed);
    prng.random().bytes(out[0..n]);
    return out[0..n];
}

test "encoded articles round-trip through an independent decoder" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var rnd: [50_001]u8 = undefined;
    var all: [256 * 8]u8 = undefined;
    for (&all, 0..) |*b, i| b.* = @truncate(i);
    const Case = struct { file: []const u8, part: usize, line: usize };
    const cases = [_]Case{
        .{ .file = "", .part = 100, .line = 128 },
        .{ .file = randomBytes(2, 500, &rnd), .part = 1000, .line = 128 },
        .{ .file = randomBytes(3, 50_001, &rnd), .part = 10_000, .line = 128 },
        .{ .file = &all, .part = 700, .line = 7 },
        .{ .file = all[0..256], .part = 4096, .line = 1 },
    };
    for (cases) |case| {
        const articles = try encodeArticles(a, "f.bin", case.file, case.part, case.line);
        const got = try a.alloc(u8, case.file.len);
        for (articles, 1..) |lines, i| {
            const d = try decode(a, lines);
            try testing.expect(d.crc_ok);
            try testing.expectEqual(i, d.part);
            @memcpy(got[d.begin..][0..d.data.len], d.data);
        }
        try testing.expectEqualSlices(u8, case.file, got);
    }
}

test "the layout of single-part and multipart bodies" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const single = try encodeArticles(arena.allocator(), "a b.txt", "hello", 100, 128);
    try testing.expectEqual(@as(usize, 1), single.len);
    try testing.expectEqualStrings("=ybegin line=128 size=5 name=a b.txt", single[0][0]);
    try testing.expectEqualStrings("=yend size=5 crc32=3610a686", single[0][single[0].len - 1]);
    const multi = try encodeArticles(arena.allocator(), "f", "0123456789", 4, 128);
    try testing.expectEqual(@as(usize, 3), multi.len);
    try testing.expectEqualStrings("=ybegin part=3 total=3 line=128 size=10 name=f", multi[2][0]);
    try testing.expectEqualStrings("=ypart begin=9 end=10", multi[2][1]);
    try testing.expect(std.mem.indexOf(u8, multi[2][multi[2].len - 1], " crc32=") != null);
    try testing.expect(std.mem.indexOf(u8, multi[0][multi[0].len - 1], " crc32=") == null);
}

test "corrupted bodies keep their structure and fail the CRC check" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var rnd: [5000]u8 = undefined;
    const file = randomBytes(3, 5000, &rnd);
    const good = (try encodeArticles(a, "f.bin", file, 5000, 128))[0];
    const bad = try corruptLines(a, good);
    const d = try decode(a, bad);
    try testing.expect(!d.crc_ok);
    try testing.expect(!std.mem.eql(u8, file, d.data));
    try testing.expect((try decode(a, good)).crc_ok);

    const empty = (try encodeArticles(a, "f.bin", "", 100, 128))[0];
    try testing.expect(!(try decode(a, try corruptLines(a, empty))).crc_ok);
}

test "a fixture lists the message ids of its NZB, sorted by name, deterministic" {
    var rnd: [3000]u8 = undefined;
    const data = randomBytes(1, 2500, &rnd);
    var fx = try buildFixture(testing.allocator, &.{
        .{ .name = "b.bin", .data = "x" },
        .{ .name = "a.bin", .data = data },
    }, 1000, .{});
    defer fx.deinit();
    try testing.expectEqual(@as(usize, 2), fx.files.len);
    try testing.expectEqualStrings("a.bin", fx.files[0].name);
    try testing.expectEqual(@as(usize, 3), fx.files[0].message_ids.len);
    try testing.expectEqual(@as(usize, 4), fx.articles.len);
    const ids = try fx.messageIds(testing.allocator);
    defer testing.allocator.free(ids);
    try testing.expectEqual(@as(usize, 4), ids.len);
    for (ids) |id| try testing.expect(std.mem.indexOf(u8, fx.nzb, id) != null);
    try testing.expect(std.mem.indexOf(u8, fx.nzb, "date=\"1704067200\"") != null);
    try testing.expect(std.mem.indexOf(u8, fx.nzb, "subject=\"&#34;a.bin&#34; yEnc (1/3)\"") != null);

    var again = try buildFixture(testing.allocator, &.{
        .{ .name = "a.bin", .data = data },
        .{ .name = "b.bin", .data = "x" },
    }, 1000, .{});
    defer again.deinit();
    try testing.expectEqualStrings(fx.nzb, again.nzb);

    var other = try buildFixture(testing.allocator, &.{.{ .name = "a.bin", .data = "different" }}, 1000, .{});
    defer other.deinit();
    for (other.files[0].message_ids) |id| for (ids) |known| try testing.expect(!std.mem.eql(u8, id, known));
}

test "fixture options: posting date and line length; bad arguments are refused" {
    var rnd: [1000]u8 = undefined;
    var fx = try buildFixture(testing.allocator, &.{.{ .name = "a.bin", .data = randomBytes(1, 1000, &rnd) }}, 5000, .{ .date = 1577934245, .line_length = 40 });
    defer fx.deinit();
    try testing.expect(std.mem.indexOf(u8, fx.nzb, "date=\"1577934245\"") != null);
    for (fx.articles) |art| for (art.lines[1 .. art.lines.len - 1]) |l| try testing.expect(l.len <= 41);
    try testing.expectError(error.BadArgument, buildFixture(testing.allocator, &.{.{ .name = "", .data = "" }}, 10, .{}));
    try testing.expectError(error.BadArgument, buildFixture(testing.allocator, &.{.{ .name = "a\"b", .data = "" }}, 10, .{}));
    try testing.expectError(error.BadArgument, buildFixture(testing.allocator, &.{.{ .name = "a", .data = "" }}, 0, .{}));
}
