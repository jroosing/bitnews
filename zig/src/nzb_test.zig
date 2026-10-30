//! Tests of the NZB parser, ported from internal/usenet/nzb/*_test.go (the
//! Go tests are the specification). The fixtures are copies of
//! internal/usenet/nzb/testdata in zig/src/nzb_testdata (@embedFile cannot leave src/).
const std = @import("std");
const testing = std.testing;
const nzb = @import("nzb.zig");

fn nowMs() i96 {
    return @divTrunc(std.Io.Clock.Timestamp.now(testing.io, .awake).raw.nanoseconds, std.time.ns_per_ms);
}

const validBody = "<file subject=\"s\"><segments><segment number=\"1\">a@x</segment></segments></file>";

fn parseOk(arena: std.mem.Allocator, doc: []const u8) !nzb.Nzb {
    return nzb.parse(arena, doc);
}

fn oneFile(a: std.mem.Allocator, segments: []const u8) ![]const u8 {
    return std.fmt.allocPrint(a, "<nzb><file poster=\"p\" date=\"1\" subject=\"s\"><segments>{s}</segments></file></nzb>", .{segments});
}

fn numbers(a: std.mem.Allocator, f: nzb.File) ![]u32 {
    const out = try a.alloc(u32, f.segments.len);
    for (f.segments, out) |s, *o| o.* = s.number;
    return out;
}

fn nFiles(a: std.mem.Allocator, n: usize) ![]const u8 {
    var sb: std.ArrayList(u8) = .empty;
    try sb.appendSlice(a, "<nzb>");
    for (0..n) |_| try sb.appendSlice(a, "<file subject=\"s\"><segments><segment number=\"1\">a@x</segment></segments></file>");
    try sb.appendSlice(a, "</nzb>");
    return sb.items;
}

test "single file, single segment" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(),
        \\<?xml version="1.0" encoding="UTF-8"?>
        \\<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
        \\ <file poster="bob@example.com" date="1700000000" subject="test.bin yEnc (1/1)">
        \\  <groups><group>alt.binaries.test</group></groups>
        \\  <segments><segment bytes="739" number="1">abc@example.com</segment></segments>
        \\ </file>
        \\</nzb>
    );
    try testing.expectEqual(@as(usize, 1), n.files.len);
    const f = n.files[0];
    try testing.expectEqualStrings("test.bin yEnc (1/1)", f.subject);
    try testing.expectEqualStrings("bob@example.com", f.poster);
    try testing.expectEqual(@as(usize, 1), f.segments.len);
    try testing.expectEqual(@as(u32, 1), f.segments[0].number);
    try testing.expectEqual(@as(i64, 739), f.segments[0].bytes);
    try testing.expectEqualStrings("abc@example.com", f.segments[0].message_id);
    try testing.expectEqualStrings("test.bin", f.name);
}

test "file date and groups" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(),
        \\<nzb>
        \\ <file poster="p" date="1700000000" subject="s">
        \\  <groups>
        \\   <group> alt.binaries.test </group>
        \\   <group>alt.binaries.misc</group>
        \\   <group>  </group>
        \\  </groups>
        \\  <segments><segment bytes="1" number="1">a@b</segment></segments>
        \\ </file>
        \\</nzb>
    );
    const f = n.files[0];
    try testing.expectEqual(@as(i64, 1700000000), f.date);
    try testing.expectEqual(@as(usize, 2), f.groups.len);
    try testing.expectEqualStrings("alt.binaries.test", f.groups[0]);
    try testing.expectEqualStrings("alt.binaries.misc", f.groups[1]);
}

test "a missing or bad date is 0" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), "<nzb><file date=\"x\" subject=\"s\"><segments><segment number=\"1\">a@b</segment></segments></file><file date=\" 5 \" subject=\"s\"><segments><segment number=\"1\">a@b</segment></segments></file><file subject=\"s\"><segments><segment number=\"1\">a@b</segment></segments></file></nzb>");
    try testing.expectEqual(@as(i64, 0), n.files[0].date);
    try testing.expectEqual(@as(i64, 5), n.files[1].date);
    try testing.expectEqual(@as(i64, 0), n.files[2].date);
}

test "meta: the first of each type wins" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(),
        \\<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
        \\ <head>
        \\  <meta type="name">Some.Release.2024</meta>
        \\  <meta type="password"> s3cret </meta>
        \\  <meta type="category">TV &gt; HD</meta>
        \\  <meta type="name">ignored</meta>
        \\  <meta type="tag">unrelated</meta>
        \\ </head>
        \\ <file poster="p" date="1" subject="s"><segments><segment number="1">a@b</segment></segments></file>
        \\</nzb>
    );
    try testing.expectEqualStrings("Some.Release.2024", n.meta.name);
    try testing.expectEqualStrings("s3cret", n.meta.password);
    try testing.expectEqualStrings("TV > HD", n.meta.category);
}

test "segments are sorted by number" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, try oneFile(a,
        \\<segment number="3">c@x</segment>
        \\<segment number="1">a@x</segment>
        \\<segment number="2">b@x</segment>
    ));
    try testing.expectEqualSlices(u32, &.{ 1, 2, 3 }, try numbers(a, n.files[0]));
    try testing.expectEqualStrings("a@x", n.files[0].segments[0].message_id);
}

test "duplicate segment numbers keep the first" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, try oneFile(a,
        \\<segment number="2">first-two@x</segment>
        \\<segment number="1">a@x</segment>
        \\<segment number="2">second-two@x</segment>
        \\<segment number="2">third-two@x</segment>
    ));
    try testing.expectEqualSlices(u32, &.{ 1, 2 }, try numbers(a, n.files[0]));
    try testing.expectEqualStrings("first-two@x", n.files[0].segments[1].message_id);
    try testing.expectEqual(@as(u32, 0), n.skipped_segments);
}

test "angle brackets around a message ID are trimmed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, try oneFile(a, "<segment number=\"1\"> &lt;part1of1.abc@news.example&gt; </segment>"));
    try testing.expectEqualStrings("part1of1.abc@news.example", n.files[0].segments[0].message_id);
}

test "an invalid message ID skips the segment" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const over_250 = try std.mem.concat(a, u8, &.{ &@as([249]u8, @splat('a')), "@x" });
    const ids = [_][]const u8{
        "", // empty
        "noatsign.example",
        "a b@x",
        "a\tb@x",
        "é@x",
        "a\x7f@x",
        over_250,
        "&lt;&gt;",
        "a&gt;b@x",
        "a&lt;b@x",
    };
    for (ids) |id| {
        const doc = try oneFile(a, try std.fmt.allocPrint(a, "<segment number=\"1\">ok@x</segment><segment number=\"2\">{s}</segment>", .{id}));
        const n = try parseOk(a, doc);
        try testing.expectEqualSlices(u32, &.{1}, try numbers(a, n.files[0]));
        try testing.expectEqual(@as(u32, 1), n.skipped_segments);
    }
}

test "a message ID of 250 characters is accepted" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const id = try std.mem.concat(a, u8, &.{ &@as([248]u8, @splat('a')), "@x" });
    const n = try parseOk(a, try oneFile(a, try std.fmt.allocPrint(a, "<segment number=\"1\">{s}</segment>", .{id})));
    try testing.expectEqualStrings(id, n.files[0].segments[0].message_id);
}

test "an invalid segment number skips the segment" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const segs = [_][]const u8{
        "<segment>b@x</segment>",
        "<segment number=\"\">b@x</segment>",
        "<segment number=\"0\">b@x</segment>",
        "<segment number=\"-2\">b@x</segment>",
        "<segment number=\"two\">b@x</segment>",
        "<segment number=\"99999999999999999999999\">b@x</segment>",
        "<segment number=\"4294967296\">b@x</segment>",
        "<segment number=\"1_0\">b@x</segment>",
        "<segment number=\"0x10\">b@x</segment>",
    };
    for (segs) |seg| {
        const n = try parseOk(a, try oneFile(a, try std.mem.concat(a, u8, &.{ "<segment number=\"1\">ok@x</segment>", seg })));
        try testing.expectEqualSlices(u32, &.{1}, try numbers(a, n.files[0]));
        try testing.expectEqual(@as(u32, 1), n.skipped_segments);
    }
}

test "a segment number may have a sign and spaces" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, try oneFile(a, "<segment number=\" +7 \">a@x</segment><segment number=\"4294967295\">b@x</segment>"));
    try testing.expectEqualSlices(u32, &.{ 7, 4294967295 }, try numbers(a, n.files[0]));
}

test "a missing or invalid size becomes 0" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const segs = [_][]const u8{
        "<segment number=\"1\">a@x</segment>",
        "<segment bytes=\"lots\" number=\"1\">a@x</segment>",
        "<segment bytes=\"-5\" number=\"1\">a@x</segment>",
        "<segment bytes=\"99999999999999999999\" number=\"1\">a@x</segment>",
    };
    for (segs) |seg| {
        const n = try parseOk(a, try oneFile(a, seg));
        try testing.expectEqual(@as(usize, 1), n.files[0].segments.len);
        try testing.expectEqual(@as(i64, 0), n.files[0].segments[0].bytes);
    }
}

test "a file without valid segments is dropped" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(),
        \\<nzb>
        \\ <file poster="p" date="1" subject="empty"><segments></segments></file>
        \\ <file poster="p" date="1" subject="all bad"><segments><segment number="1">nope</segment></segments></file>
        \\ <file poster="p" date="1" subject="good"><segments><segment number="1">a@x</segment></segments></file>
        \\</nzb>
    );
    try testing.expectEqual(@as(usize, 1), n.files.len);
    try testing.expectEqualStrings("good", n.files[0].subject);
    try testing.expectEqual(@as(u32, 1), n.skipped_segments);
}

test "no usable files is NoFiles" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const docs = [_][]const u8{
        "<nzb xmlns=\"http://www.newzbin.com/DTD/2003/nzb\"></nzb>",
        "<nzb><head><meta type=\"name\">x</meta></head></nzb>",
        try oneFile(a, "<segment number=\"1\">bad</segment>"),
    };
    for (docs) |doc| try testing.expectError(error.NoFiles, nzb.parse(a, doc));
}

test "malformed XML is Malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const docs = [_][]const u8{
        "<nzb><file subject=\"s\"><segments><segment number=\"1\">a@x",
        "<nzb><file subject=\"s\"></segments></file></nzb>",
        "this is not xml <<<",
        "",
        "<nzb><file subject=s></file></nzb>",
        "<html><body><file subject=\"s\"><segments><segment number=\"1\">a@x</segment></segments></file></body></html>",
        "<nzb>" ++ validBody ++ "</nzb></nzb>",
        "<nzb>" ++ validBody ++ "</nzb><x>",
    };
    for (docs) |doc| try testing.expectError(error.Malformed, nzb.parse(a, doc));
}

test "the details of a syntax error" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var diag: nzb.Diag = .{};
    try testing.expectError(error.Malformed, nzb.parseWithLimits(a, "<nzb>\n<file>", .{}, &diag));
    try testing.expectEqualStrings(
        "nzb: malformed document: XML syntax error on line 2: unexpected EOF",
        try nzb.errorMessage(a, error.Malformed, .{}, diag),
    );
    try testing.expectEqualStrings("nzb: too many files: more than 2", try nzb.errorMessage(a, error.TooManyFiles, .{ .max_files = 2 }, .{}));
}

test "trailing content after the root is checked but not used" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, "<nzb>" ++ validBody ++ "</nzb>\n<!-- end --><extra/>text\n");
    try testing.expectEqual(@as(usize, 1), n.files.len);
}

test "file limit" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const lim: nzb.Limits = .{ .max_files = 2 };
    _ = try nzb.parseWithLimits(a, try nFiles(a, 2), lim, null);
    try testing.expectError(error.TooManyFiles, nzb.parseWithLimits(a, try nFiles(a, 3), lim, null));
}

test "segment limit is the total across files, invalid segments count" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const lim: nzb.Limits = .{ .max_segments = 2 };
    _ = try nzb.parseWithLimits(a, try nFiles(a, 2), lim, null);
    try testing.expectError(error.TooManySegments, nzb.parseWithLimits(a, try nFiles(a, 3), lim, null));
    const doc = try oneFile(a, "<segment number=\"1\">a@x</segment><segment number=\"x\">b@x</segment><segment number=\"2\">c@x</segment>");
    try testing.expectError(error.TooManySegments, nzb.parseWithLimits(a, doc, lim, null));
}

test "a limit is hit before a later syntax error" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const doc = try std.mem.concat(a, u8, &.{ "<nzb>", validBody, validBody, "<oops" });
    try testing.expectError(error.TooManyFiles, nzb.parseWithLimits(a, doc, .{ .max_files = 1 }, null));
}

test "input size limit" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const doc = try nFiles(a, 1);
    _ = try nzb.parseWithLimits(a, doc, .{ .max_bytes = doc.len }, null);
    try testing.expectError(error.TooLarge, nzb.parseWithLimits(a, doc, .{ .max_bytes = doc.len - 1 }, null));
    try testing.expectError(error.TooLarge, nzb.parseWithLimits(a, doc, .{ .max_bytes = 0 }, null));
    _ = try nzb.parseWithLimits(a, doc, .{ .max_bytes = std.math.maxInt(u64) }, null);
    // Trailing bytes count too, even when they are only blanks.
    const padded = try std.mem.concat(a, u8, &.{ doc, &@as([100]u8, @splat(' ')) });
    try testing.expectError(error.TooLarge, nzb.parseWithLimits(a, padded, .{ .max_bytes = doc.len + 99 }, null));
}

test "ISO-8859-1 is decoded, other encodings are Malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, "<?xml version=\"1.0\" encoding=\"iso-8859-1\" ?>\n" ++
        "<nzb><file subject=\"Caf\xe9.mkv\"><segments><segment number=\"1\">a@x</segment></segments></file></nzb>");
    try testing.expectEqualStrings("Café.mkv", n.files[0].subject);
    try testing.expectError(error.Malformed, nzb.parse(a, "<?xml version=\"1.0\" encoding=\"shift_jis\"?><nzb>" ++ validBody ++ "</nzb>"));
}

test "deeply nested unknown elements are skipped without recursion" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const depth = 1_000_000;
    const open = try a.alloc(u8, depth * 3);
    for (0..depth) |i| @memcpy(open[i * 3 ..][0..3], "<x>");
    const close = try a.alloc(u8, depth * 4);
    for (0..depth) |i| @memcpy(close[i * 4 ..][0..4], "</x>");
    const nest = try std.mem.concat(a, u8, &.{ open, close });
    const docs = [_][]const u8{
        try std.mem.concat(a, u8, &.{ "<nzb>", nest, validBody, "</nzb>" }),
        try oneFile(a, try std.mem.concat(a, u8, &.{ "<segment number=\"1\">a@x", nest, "</segment>" })),
        try std.mem.concat(a, u8, &.{ "<nzb><head>", nest, "</head>", validBody, "</nzb>" }),
    };
    for (docs) |doc| {
        const n = try nzb.parseWithLimits(a, doc, .{ .max_bytes = 1 << 30 }, null);
        try testing.expectEqual(@as(usize, 1), n.files.len);
        try testing.expectEqualStrings("a@x", n.files[0].segments[0].message_id);
    }
}

test "a prefixed namespace is accepted" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(),
        \\<n:nzb xmlns:n="http://www.newzbin.com/DTD/2003/nzb">
        \\ <n:head><n:meta type="name">x</n:meta></n:head>
        \\ <n:file subject="s"><n:segments><n:segment number="1">a@x</n:segment></n:segments></n:file>
        \\</n:nzb>
    );
    try testing.expectEqualStrings("x", n.meta.name);
    try testing.expectEqual(@as(usize, 1), n.files.len);
}

test "text: CDATA and nested elements" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a, try oneFile(a, "<segment number=\"1\">a<![CDATA[b@]]><i>zz</i>x</segment>"));
    try testing.expectEqualStrings("ab@x", n.files[0].segments[0].message_id);
}

test "attributes: entities, the first match by local name" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), "<nzb><file x:subject=\"first\" subject=\"second\" poster=\"a &amp; b\"><segments><segment number=\"1\">a@x</segment></segments></file></nzb>");
    try testing.expectEqualStrings("first", n.files[0].subject);
    try testing.expectEqualStrings("a & b", n.files[0].poster);
}

test "BillionLaughs and external entities are not expanded" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const dtd =
        \\<?xml version="1.0"?>
        \\<!DOCTYPE nzb [
        \\ <!ENTITY lol "lol">
        \\ <!ENTITY lol1 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
        \\ <!ENTITY xxe SYSTEM "file:///etc/passwd">
        \\]>
        \\
    ;
    try testing.expectError(error.Malformed, nzb.parse(a, dtd ++ "<nzb><head><meta type=\"name\">&lol1;</meta></head>" ++ validBody ++ "</nzb>"));
    try testing.expectError(error.Malformed, nzb.parse(a, dtd ++ "<nzb><file subject=\"&xxe;\"><segments><segment number=\"1\">a@x</segment></segments></file></nzb>"));
    // Defined but not referenced: ignored.
    const n = try nzb.parse(a, dtd ++ "<nzb><head><meta type=\"name\">plain</meta></head>" ++ validBody ++ "</nzb>");
    try testing.expectEqualStrings("plain", n.meta.name);
}

test "files are classified, with block counts" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const n = try parseOk(a,
        \\<nzb>
        \\ <file subject="&quot;movie.part01.rar&quot; yEnc (1/50)"><segments><segment number="1">a@x</segment></segments></file>
        \\ <file subject="&quot;MOVIE.PAR2&quot; yEnc (1/1)"><segments><segment number="1">a@x</segment></segments></file>
        \\ <file subject="&quot;Movie.VOL07+08.PAR2&quot; yEnc (1/5)"><segments><segment number="1">a@x</segment></segments></file>
        \\ <file subject="&quot;movie.vol00+99999999999999999999.par2&quot;"><segments><segment number="1">a@x</segment></segments></file>
        \\ <file subject="&quot;a8f3e9c2b1d4e5f6&quot; yEnc (1/1)"><segments><segment number="1">a@x</segment></segments></file>
        \\</nzb>
    );
    const kinds = [_]nzb.Kind{ .data, .par2_index, .par2_volume, .par2_volume, .data };
    const blocks = [_]u32{ 0, 0, 8, 0, 0 };
    for (n.files, kinds, blocks) |f, k, b| {
        try testing.expectEqual(k, f.kind);
        try testing.expectEqual(b, f.recovery_blocks);
    }
    try testing.expect(n.files[2].deferred() and !n.files[1].deferred());
}

// -- fixtures

const fx_newznab = @embedFile("nzb_testdata/newznab_meta.nzb");
const fx_no_meta = @embedFile("nzb_testdata/no_meta.nzb");
const fx_obfuscated = @embedFile("nzb_testdata/obfuscated.nzb");
const fx_duplicates = @embedFile("nzb_testdata/duplicate_segments.nzb");
const fx_missing_bytes = @embedFile("nzb_testdata/missing_bytes.nzb");
const fx_par2 = @embedFile("nzb_testdata/par2_set.nzb");

test "fixture: newznab with meta" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), fx_newznab);
    try testing.expectEqualStrings("Some.Show.S02E05.1080p.WEB.h264-GRP", n.meta.name);
    try testing.expectEqualStrings("hunter2", n.meta.password);
    try testing.expectEqualStrings("TV > HD", n.meta.category);
    const want_names = [_][]const u8{
        "some.show.s02e05.1080p.web.h264-grp.nfo",
        "some.show.s02e05.1080p.web.h264-grp.rar",
        "some.show.s02e05.1080p.web.h264-grp.par2",
        "some.show.s02e05.1080p.web.h264-grp.vol00+02.par2",
    };
    try testing.expectEqual(want_names.len, n.files.len);
    for (n.files, want_names) |f, w| try testing.expectEqualStrings(w, f.name);
    const kinds = [_]nzb.Kind{ .data, .data, .par2_index, .par2_volume };
    for (n.files, kinds) |f, k| try testing.expectEqual(k, f.kind);
    try testing.expectEqual(@as(u32, 2), n.files[3].recovery_blocks);
    const nfo = n.files[0];
    try testing.expectEqualStrings("GRP <grp@poster.example>", nfo.poster);
    try testing.expectEqual(@as(i64, 1712345678), nfo.date);
    try testing.expectEqual(@as(usize, 2), nfo.groups.len);
    try testing.expectEqualStrings("alt.binaries.teevee", nfo.groups[0]);
    try testing.expectEqualStrings("alt.binaries.hdtv.x264", nfo.groups[1]);
    try testing.expectEqual(@as(usize, 3), n.files[1].segments.len);
    try testing.expectEqual(@as(i64, 739948 + 739912 + 412088), n.files[1].bytes());
    try testing.expectEqual(@as(u32, 0), n.skipped_segments);
}

test "fixture: without meta" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), fx_no_meta);
    try testing.expectEqualStrings("", n.meta.name);
    try testing.expectEqual(@as(usize, 1), n.files.len);
    const f = n.files[0];
    try testing.expectEqualStrings("abc-mr2a.r01", f.name);
    try testing.expectEqual(nzb.Kind.data, f.kind);
    try testing.expectEqualStrings("Joe Bloggs (bloggs@nowhere.example)", f.poster);
    try testing.expectEqual(@as(usize, 2), f.segments.len);
}

test "fixture: obfuscated subjects" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), fx_obfuscated);
    const want_names = [_][]const u8{
        "9f86d081884c7d659a2feaa0c55ad015",
        "9f86d081884c7d659a2feaa0c55ad015.par2",
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    };
    try testing.expectEqual(want_names.len, n.files.len);
    for (n.files, want_names) |f, w| try testing.expectEqualStrings(w, f.name);
    const kinds = [_]nzb.Kind{ .data, .par2_index, .data };
    for (n.files, kinds) |f, k| try testing.expectEqual(k, f.kind);
}

test "fixture: duplicate segments" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), fx_duplicates);
    const segs = n.files[0].segments;
    const ids = [_][]const u8{ "part1of4.XyZ@upload.example", "part2of4.XyZ@upload.example", "part3of4.XyZ@reposted.example" };
    try testing.expectEqual(ids.len, segs.len);
    for (segs, ids, 1..) |s, id, num| {
        try testing.expectEqualStrings(id, s.message_id);
        try testing.expectEqual(@as(u32, @intCast(num)), s.number);
        try testing.expectEqual(@as(i64, 716800), s.bytes);
    }
    try testing.expectEqual(@as(u32, 1), n.skipped_segments);
    try testing.expectEqualStrings("album.2020.flac-grp.7z.001", n.files[0].name);
}

test "fixture: missing bytes" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), fx_missing_bytes);
    try testing.expectEqual(@as(usize, 2), n.files[0].segments.len);
    for (n.files[0].segments) |s| try testing.expectEqual(@as(i64, 0), s.bytes);
}

test "fixture: PAR2 index and volumes" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const n = try parseOk(arena.allocator(), fx_par2);
    const kinds = [_]nzb.Kind{ .data, .par2_index, .par2_volume, .par2_volume, .par2_volume };
    const blocks = [_]u32{ 0, 0, 1, 2, 4 };
    try testing.expectEqual(kinds.len, n.files.len);
    for (n.files, kinds, blocks) |f, k, b| {
        try testing.expectEqual(k, f.kind);
        try testing.expectEqual(b, f.recovery_blocks);
    }
}

// -- scale and hostile input

test "a 5 MB NZB parses in linear time" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var sb: std.ArrayList(u8) = .empty;
    try sb.appendSlice(a, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><nzb>");
    var f: usize = 0;
    // 40 files of 1000 segments, written in reverse order to make the sort work.
    while (f < 40) : (f += 1) {
        try sb.print(a, "<file poster=\"p\" date=\"1\" subject=\"&quot;f{d}.rar&quot; yEnc\"><groups><group>alt.test</group></groups><segments>", .{f});
        var i: usize = 1000;
        while (i > 0) : (i -= 1) {
            try sb.print(a, "<segment bytes=\"{d}\" number=\"{d}\">part{d}of1000.f{d}@news.example.com</segment>", .{ 700000 + i, i, i, f });
        }
        try sb.appendSlice(a, "</segments></file>");
    }
    try sb.appendSlice(a, "</nzb>");
    try testing.expect(sb.items.len > 3_000_000);

    const t0 = nowMs();
    const n = try nzb.parse(a, sb.items);
    const ms = nowMs() - t0;
    try testing.expectEqual(@as(usize, 40), n.files.len);
    try testing.expectEqual(@as(usize, 1000), n.files[39].segments.len);
    try testing.expectEqual(@as(u32, 1), n.files[39].segments[0].number);
    // Generous even on the debug allocator: a quadratic parser takes minutes.
    try testing.expect(ms < 20_000);
}

test "many attributes, entities and tiny elements stay linear" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    var sb: std.ArrayList(u8) = .empty;
    try sb.appendSlice(a, "<nzb><file subject=\"s\" ");
    for (0..100_000) |i| try sb.print(a, "a{d}=\"&amp;&#65;\" ", .{i});
    try sb.appendSlice(a, "><segments><segment number=\"1\">a@x</segment>");
    for (0..200_000) |_| try sb.appendSlice(a, "<u/>");
    try sb.appendSlice(a, "</segments></file></nzb>");
    const t0 = nowMs();
    const n = try nzb.parse(a, sb.items);
    try testing.expectEqual(@as(usize, 1), n.files.len);
    try testing.expect(nowMs() - t0 < 20_000);
}

test "text longer than the cap: a segment is skipped, a meta is Malformed" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const big = try a.alloc(u8, nzb.max_text_bytes + 1);
    @memset(big, 'a');
    const n = try nzb.parse(a, try oneFile(a, try std.mem.concat(a, u8, &.{ "<segment number=\"1\">ok@x</segment><segment number=\"2\">", big, "@x</segment>" })));
    try testing.expectEqual(@as(usize, 1), n.files[0].segments.len);
    try testing.expectEqual(@as(u32, 1), n.skipped_segments);
    try testing.expectError(error.Malformed, nzb.parse(a, try std.mem.concat(a, u8, &.{ "<nzb><head><meta type=\"name\">", big, "</meta></head>", validBody, "</nzb>" })));
}

fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}

test "fuzz: parse never crashes or leaks, and results are well formed" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [4096]u8 = undefined;
            const len = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            // Small limits so the fuzzer can reach every cap.
            const lim: nzb.Limits = .{ .max_bytes = 1 << 16, .max_files = 20, .max_segments = 100 };
            const n = nzb.parseWithLimits(arena.allocator(), buf[0..len], lim, null) catch |err| switch (err) {
                error.OutOfMemory => return error.OutOfMemory,
                else => return,
            };
            if (n.files.len == 0) return error.SuccessWithoutFiles;
            for (n.files) |f| {
                if (f.segments.len == 0) return error.FileWithoutSegments;
                var prev: u32 = 0;
                for (f.segments) |s| {
                    if (s.number <= prev) return error.SegmentsNotIncreasing;
                    prev = s.number;
                    if (!nzb.validMessageId(s.message_id)) return error.InvalidMessageId;
                    if (s.bytes < 0) return error.NegativeBytes;
                }
            }
        }
    }.one, .{ .corpus = &.{
        seed(fx_duplicates),
        seed(fx_missing_bytes),
        seed(fx_no_meta),
        seed(fx_obfuscated),
        seed(fx_par2),
        seed("<nzb><file subject=\"s\"><segments><segment number=\"2\">&lt;b@x&gt;</segment><segment number=\"2\">c@x</segment><segment number=\"1\">a b</segment></segments></file></nzb>"),
        seed("<!DOCTYPE nzb [ <!ENTITY lol \"lol\"> ]><nzb><head><meta type=\"name\">&lol;</meta></head></nzb>"),
        seed("<?xml encoding=\"latin1\"?><nzb><file subject=\"Caf\xe9\"><segments><segment number=\"1\">a@x</segment></segments></file></nzb>"),
    } });
}
