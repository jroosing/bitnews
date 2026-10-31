//! What we read from the English text par2 prints (internal/postproc/par2
//! parse.go; the runner sets LC_ALL=C). Every pattern matches a whole line and
//! begins with a fixed word, never with a file name, so a name in the output
//! (par2 prints them, and the name check refuses names with line breaks)
//! cannot fake one of these lines.
//!
//! Go uses regular expressions; here each pattern is a small hand-written
//! matcher with the same language. Numbers are ASCII digits; one that does not
//! fit an i64 saturates at `saturated` instead of failing.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Go's 1<<31-1: what an unparsable (too long) number becomes.
pub const saturated: i64 = (1 << 31) - 1;

/// Hard cap on the partial-copy names kept from one report. The runner caps
/// the output itself; this bounds the list it can produce.
pub const max_partials = 100_000;

pub const Output = struct {
    /// "You have X out of Y data blocks available."; `have_avail` says it was seen.
    total: i64 = 0,
    available: i64 = 0,
    have_avail: bool = false,
    /// "You have Z recovery blocks available."; absent means `loaded`.
    recovery: i64 = 0,
    have_recov: bool = false,
    /// Sum of "Loaded N new packets including M recovery blocks."
    loaded: i64 = 0,
    /// "You need N more recovery blocks to be able to repair."
    need_more: i64 = 0,
    /// "N recovery blocks will be used to repair."
    will_use: i64 = 0,
    /// Names of extra files par2 found parts of a target in:
    /// `File: "x" - found 9 of 10 data blocks from "a.bin".` Untrusted until
    /// checked against the directory listing. Slices of the parsed text.
    partials: []const []const u8 = &.{},

    /// The number of recovery blocks par2 found. A clean verify does not print
    /// "You have Z recovery blocks available.", so the blocks that the loaded
    /// files announced are the answer then (par2 loads each packet once, so a
    /// duplicate volume is not counted twice).
    pub fn recoveryBlocks(o: Output) i64 {
        return if (o.have_recov) o.recovery else o.loaded;
    }

    /// The number of data blocks that are missing or damaged, which is the
    /// number of recovery blocks a repair consumes.
    pub fn damaged(o: Output) i64 {
        if (!o.have_avail) return 0;
        return @max(o.total - o.available, 0);
    }
};

/// Reads par2's stdout. Progress lines end in a carriage return, so both \r
/// and \n end a line. `partials` and its strings live in `a` (the strings are
/// slices of `b`).
pub fn parse(a: Allocator, b: []const u8) Allocator.Error!Output {
    var o: Output = .{};
    var partials: std.ArrayList([]const u8) = .empty;
    var it = std.mem.tokenizeAny(u8, b, "\r\n");
    while (it.next()) |line| {
        if (matchAvailable(line)) |m| {
            o.available, o.total, o.have_avail = .{ m[0], m[1], true };
        } else if (matchOne(line, "You have ", " recovery blocks available.")) |n| {
            o.recovery, o.have_recov = .{ n, true };
        } else if (matchLoaded(line)) |n| {
            o.loaded = satAdd(o.loaded, n);
        } else if (matchOne(line, "You need ", " more recovery blocks to be able to repair.")) |n| {
            o.need_more = n;
        } else if (matchOne(line, "", " recovery blocks will be used to repair.")) |n| {
            o.will_use = n;
        } else if (matchPartial(line)) |name| {
            if (partials.items.len < max_partials) try partials.append(a, name);
        }
    }
    o.partials = try partials.toOwnedSlice(a);
    return o;
}

/// Digits at the start of `s`: their value (saturating) and the rest, or null
/// when `s` does not start with a digit.
fn number(s: []const u8) ?struct { i64, []const u8 } {
    var i: usize = 0;
    while (i < s.len and s[i] >= '0' and s[i] <= '9') i += 1;
    if (i == 0) return null;
    const n = std.fmt.parseInt(i64, s[0..i], 10) catch saturated;
    return .{ n, s[i..] };
}

fn skip(s: []const u8, lit: []const u8) ?[]const u8 {
    return if (std.mem.startsWith(u8, s, lit)) s[lit.len..] else null;
}

/// `^<head>(\d+)<tail>$`.
fn matchOne(line: []const u8, head: []const u8, tail: []const u8) ?i64 {
    const s = skip(line, head) orelse return null;
    const n, const rest = number(s) orelse return null;
    return if (std.mem.eql(u8, rest, tail)) n else null;
}

/// `^You have (\d+) out of (\d+) data blocks available\.$`
fn matchAvailable(line: []const u8) ?[2]i64 {
    var s = skip(line, "You have ") orelse return null;
    const x, s = number(s) orelse return null;
    s = skip(s, " out of ") orelse return null;
    const y, s = number(s) orelse return null;
    if (!std.mem.eql(u8, s, " data blocks available.")) return null;
    return .{ x, y };
}

/// `^Loaded \d+ new packets? including (\d+) recovery blocks?$`
fn matchLoaded(line: []const u8) ?i64 {
    var s = skip(line, "Loaded ") orelse return null;
    _, s = number(s) orelse return null;
    s = skip(s, " new packet") orelse return null;
    s = skip(s, "s") orelse s;
    s = skip(s, " including ") orelse return null;
    const n, s = number(s) orelse return null;
    s = skip(s, " recovery block") orelse return null;
    s = skip(s, "s") orelse s;
    return if (s.len == 0) n else null;
}

/// `^File: "(.*?)" - found \d+ of \d+ data blocks from ".*"\.$`: the name is
/// the shortest one that lets the rest match.
fn matchPartial(line: []const u8) ?[]const u8 {
    const rest = skip(line, "File: \"") orelse return null;
    const sep = "\" - found ";
    var from: usize = 0;
    while (std.mem.indexOfPos(u8, rest, from, sep)) |at| : (from = at + 1) {
        var s = rest[at + sep.len ..];
        _, s = number(s) orelse continue;
        s = skip(s, " of ") orelse continue;
        _, s = number(s) orelse continue;
        s = skip(s, " data blocks from \"") orelse continue;
        if (s.len >= 2 and std.mem.endsWith(u8, s, "\".")) return rest[0..at];
    }
    return null;
}

fn satAdd(a: i64, b: i64) i64 {
    if (a > saturated - b) return saturated;
    return a + b;
}

// ---- tests (internal/postproc/par2/parse_test.go) ----

const testing = std.testing;

// Real output of par2cmdline-turbo 1.5.0 (progress lines cut).
const out_damaged = "Loaded 6 new packets\r\nLoaded 4 new packets including 4 recovery blocks\r\n" ++
    "There are a total of 17 data blocks.\r\n" ++
    "Target: \"b.bin\" - damaged. Found 5 of 7 data blocks.\r\n\r\n" ++
    "Repair is required.\r\n2 file(s) exist but are damaged.\r\n" ++
    "You have 14 out of 17 data blocks available.\r\nYou have 8 recovery blocks available.\r\n" ++
    "Repair is possible.\r\nYou have an excess of 5 recovery blocks.\r\n3 recovery blocks will be used to repair.\r\n";
const out_impossible = "Repair is required.\n1 file(s) are missing.\nYou have 7 out of 17 data blocks available.\n" ++
    "You have 8 recovery blocks available.\nRepair is not possible.\nYou need 2 more recovery blocks to be able to repair.\n";
const out_clean = "Loaded 4 new packets including 4 recovery blocks\nLoaded 4 new packets including 4 recovery blocks\n" ++
    "There are a total of 17 data blocks.\nAll files are correct, repair is not required.\n";
const out_renamed = "File: \"x1f3\" - is a match for \"a.bin\".\n\nRepair is required.\n1 file(s) have the wrong name.\n" ++
    "You have 17 out of 17 data blocks available.\nRepair is possible.\n";

fn parseT(a: Allocator, text: []const u8) !Output {
    return parse(a, text);
}

test "parse: damaged" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), out_damaged);
    try testing.expectEqual(3, o.damaged());
    try testing.expectEqual(8, o.recoveryBlocks());
    try testing.expectEqual(3, o.will_use);
}

test "parse: impossible" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), out_impossible);
    try testing.expectEqual(10, o.damaged());
    try testing.expectEqual(8, o.recoveryBlocks());
    try testing.expectEqual(2, o.need_more);
}

test "parse: clean takes the recovery blocks from the loaded files" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), out_clean);
    try testing.expectEqual(8, o.recoveryBlocks());
    try testing.expectEqual(0, o.damaged());
}

test "parse: renamed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), out_renamed);
    try testing.expectEqual(0, o.damaged());
}

test "parse: no recovery blocks line means none" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), "You have 16 out of 17 data blocks available.\nRepair is not possible.\n");
    try testing.expectEqual(1, o.damaged());
    try testing.expectEqual(0, o.recoveryBlocks());
}

test "parse: a huge number saturates instead of failing" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), "You have 0 recovery blocks available.\nYou have 99999999999999999999 out of 99999999999999999999 data blocks available.\n");
    try testing.expectEqual(0, o.damaged());
    try testing.expectEqual(saturated, o.total);
}

test "parse: a file name cannot fake a line" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    // Names come first on their line, after a fixed word and a quote.
    const o = try parseT(arena.allocator(), "Target: \"x\" - missing.\nTarget: \"You have 1 out of 1 data blocks available.\" - found.\n");
    try testing.expect(!o.have_avail);
}

test "parse: partial matches" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), "File: \"x7f3a9\" - found 9 of 10 data blocks from \"a.bin\".\nFile: \"junk.dat\" - no data found.\n");
    try testing.expectEqual(1, o.partials.len);
    try testing.expectEqualStrings("x7f3a9", o.partials[0]);
}

test "parse: a partial name takes the shortest match, like the lazy group" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), "File: \"a\" - found 1 of 2 data blocks from \"b\" - found 3 of 4 data blocks from \"c\".\n" ++
        "File: \"\" - found 1 of 2 data blocks from \"\".\n" ++
        "File: \"n\" - found 1 of 2 data blocks from \"x\"\n" ++
        "File: \"n\" - found 1 of x data blocks from \"x\".\n");
    try testing.expectEqual(2, o.partials.len);
    try testing.expectEqualStrings("a", o.partials[0]);
    try testing.expectEqualStrings("", o.partials[1]);
}

test "parse: singular forms and lines that do not match whole" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const o = try parseT(arena.allocator(), "Loaded 1 new packet including 1 recovery block\n" ++
        "Loaded 2 new packets including 3 recovery blocks \n" ++ // trailing space: no match
        "x You need 5 more recovery blocks to be able to repair.\n");
    try testing.expectEqual(1, o.loaded);
    try testing.expectEqual(0, o.need_more);
}

test "fuzz: parse never crashes or leaks on any text" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [2048]u8 = undefined;
            const len = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            const o = try parse(arena.allocator(), buf[0..len]);
            try testing.expect(o.damaged() >= 0);
            try testing.expect(o.loaded <= saturated);
            for (o.partials) |p| try testing.expect(std.mem.indexOf(u8, buf[0..len], p) != null);
        }
    }.one, .{ .corpus = &.{
        seed(out_damaged),
        seed(out_impossible),
        seed(out_clean),
        seed(out_renamed),
        seed("File: \"x7f3a9\" - found 9 of 10 data blocks from \"a.bin\".\n"),
        seed("You have 99999999999999999999 out of 1 data blocks available.\n"),
    } });
}

fn seed(comptime s: []const u8) []const u8 {
    return &std.mem.toBytes(@as(u32, s.len)) ++ s;
}
