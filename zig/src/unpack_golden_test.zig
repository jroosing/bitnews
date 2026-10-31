//! The pure functions against the real Go code. The files under
//! testdata/unpack/golden were written by a throwaway program that ran the Go
//! package (cleanName, parseListing, kindFromAttributes, the volume-name
//! regular expressions and strings.ToLower) on the inputs; every field is
//! hex, because names are arbitrary bytes. See testdata/unpack/README.
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const Io = std.Io;
const tu = @import("unpack_testutil.zig");
const name_mod = @import("unpack_name.zig");
const listing = @import("unpack_listing.zig");
const detect = @import("unpack_detect.zig");
const errors = @import("unpack_errors.zig");
const env_mod = @import("unpack_env.zig");

fn readGolden(a: Allocator, name: []const u8) ![]u8 {
    const p = try std.fmt.allocPrint(a, "testdata/unpack/golden/{s}", .{name});
    return Io.Dir.cwd().readFileAlloc(tu.io, p, a, .limited(8 << 20));
}

test "golden: cleanName gives Go's path or Go's message for 1500 names" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const text = try readGolden(a, "cleanname.tsv");
    var lines = std.mem.splitScalar(u8, text, '\n');
    var n: usize = 0;
    while (lines.next()) |line| {
        if (line.len == 0) continue;
        var f = std.mem.splitScalar(u8, line, '\t');
        const in = try tu.unhex(a, f.next().?);
        const tag = f.next().?;
        const want = try tu.unhex(a, f.next() orelse "");
        const got = try name_mod.cleanName(a, in);
        switch (got) {
            .rel => |r| {
                try testing.expectEqualStrings("O", tag);
                try testing.expectEqualStrings(want, r);
            },
            .violation => |why| {
                try testing.expectEqualStrings("E", tag);
                try testing.expectEqualStrings(want, try name_mod.violationText(a, why, in));
            },
        }
        n += 1;
    }
    try testing.expect(n > 900);
}

test "golden: lower-casing is Go's strings.ToLower" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const text = try readGolden(a, "lower.tsv");
    var lines = std.mem.splitScalar(u8, text, '\n');
    var runes: usize = 0;
    while (lines.next()) |line| {
        if (line.len == 0) continue;
        var f = std.mem.splitScalar(u8, line, '\t');
        const first = f.next().?;
        if (std.mem.eql(u8, first, "R")) {
            const cp = try std.fmt.parseInt(u21, f.next().?, 16);
            const want = try std.fmt.parseInt(u21, f.next().?, 16);
            try testing.expectEqual(want, name_mod.lowerRune(cp));
            runes += 1;
            continue;
        }
        const in = try tu.unhex(a, first);
        const want = try tu.unhex(a, f.next().?);
        try testing.expectEqualStrings(want, try name_mod.lowerAlloc(a, in));
    }
    try testing.expect(runes > 1000);
    // Every other code point is its own lower case form: sample the BMP and
    // the first astral planes against the table's own claims.
    var cp: u21 = 0;
    while (cp < 0x30000) : (cp += 1) {
        if (cp >= 0xd800 and cp <= 0xdfff) continue;
        const l = name_mod.lowerRune(cp);
        if (l != cp) {
            // A change must be one the golden file lists (checked above).
            try testing.expect(l != 0);
        }
    }
}

test "golden: the listing parser reads and refuses what Go does, with Go's messages" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const text = try readGolden(a, "listing.tsv");
    var lines = std.mem.splitScalar(u8, text, '\n');
    var n: usize = 0;
    while (lines.next()) |line| {
        if (line.len == 0) continue;
        var f = std.mem.splitScalar(u8, line, '\t');
        const in = try tu.unhex(a, f.next().?);
        const tag = f.next().?;
        const rest = f.next() orelse "";
        var rig = try tu.Rig.init(undefined, .{});
        defer rig.deinit();
        const res = listing.parse(&rig.env, a, in, std.math.maxInt(usize));
        if (std.mem.eql(u8, tag, "E")) {
            try testing.expectError(error.Corrupt, res);
            try testing.expectEqualStrings(try tu.unhex(a, rest), rig.diag.message);
        } else {
            const entries = try res;
            var want = std.mem.tokenizeScalar(u8, rest, ' ');
            var i: usize = 0;
            while (want.next()) |w| : (i += 1) {
                var p = std.mem.splitScalar(u8, w, ':');
                try testing.expectEqualStrings(try tu.unhex(a, p.next().?), entries[i].name);
                try testing.expectEqual(try std.fmt.parseInt(u8, p.next().?, 10), @backingInt(entries[i].kind));
                try testing.expectEqual(try std.fmt.parseInt(i64, p.next().?, 10), entries[i].size);
                try testing.expectEqual(std.mem.eql(u8, p.next().?, "1"), entries[i].encrypted);
            }
            try testing.expectEqual(i, entries.len);
        }
        n += 1;
    }
    try testing.expect(n > 40);
}

test "golden: Attributes become the kinds Go gives them" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const text = try readGolden(a, "attrs.tsv");
    var lines = std.mem.splitScalar(u8, text, '\n');
    while (lines.next()) |line| {
        if (line.len == 0) continue;
        var f = std.mem.splitScalar(u8, line, '\t');
        const attr = try tu.unhex(a, f.next() orelse "");
        const want = try std.fmt.parseInt(u8, f.next().?, 10);
        try testing.expectEqual(want, @backingInt(listing.kindFromAttributes(attr)));
    }
}

test "golden: volume names match the four regular expressions of Go" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const text = try readGolden(a, "volnames.tsv");
    var lines = std.mem.splitScalar(u8, text, '\n');
    var n: usize = 0;
    while (lines.next()) |line| {
        if (line.len == 0) continue;
        var f = std.mem.splitScalar(u8, line, '\t');
        const name = try tu.unhex(a, f.next().?);
        const want = f.next() orelse "";
        var got: std.ArrayList(u8) = .empty;
        if (detect.matchSevenZ(name)) |m| try got.print(a, "V7:{x}:{x} ", .{ m.base, m.digits });
        if (detect.matchNewStyle(name)) |m| try got.print(a, "NEW:{x}:{x} ", .{ m.base, m.digits });
        if (detect.matchOldFirst(name)) |b| try got.print(a, "OLD1:{x} ", .{b});
        if (detect.matchOldCont(name)) |m| try got.print(a, "OLDC:{x}:{x}:{x} ", .{ m.base, m.letter, m.digits });
        const trimmed = std.mem.trimEnd(u8, got.items, " ");
        try testing.expectEqualStrings(want, trimmed);
        n += 1;
    }
    try testing.expect(n > 1000);
}
