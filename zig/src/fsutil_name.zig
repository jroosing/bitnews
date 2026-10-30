//! Hostile names and collision-free names (internal/fsutil safename.go and
//! unique.go): `safeName` turns an untrusted string (an NZB subject, a yEnc
//! name=, an archive entry, a torrent file name) into one safe file name
//! component, and `unique` finds a free variant of a path.
//!
//! Strings returned by functions that take an allocator live in it: pass an
//! arena (one per Article, request or step), nothing is freed one by one.
//! `fsutil.zig` re-exports the public names.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const unicode_c = @import("fsutil_unicode.zig");

/// The longest file name the common file systems accept, in bytes.
pub const max_name_bytes = 255;
/// What counts as an extension (including the dot) when truncating, so that a
/// hostile ".aaaa..." suffix cannot pin the whole budget.
pub const max_ext_bytes = 32;
/// `unique` counts up to this far; after that it takes a random suffix.
pub const max_unique_tries = 10000;

pub const NameError = error{
    /// The name is empty, ".", ".." or becomes so (Go: ErrInvalidName).
    InvalidName,
};

/// Whether Go's unicode.In(cp, C, Zl, Zp) holds: control and format
/// characters (bidi overrides and isolates, zero width characters, the BOM),
/// surrogates, private use, unassigned code points, and the line and
/// paragraph separators. The table comes from Go's unicode package, so the
/// result is what `SafeName` strips there.
pub fn isStripped(cp: u21) bool {
    var lo: usize = 0;
    var hi: usize = unicode_c.ranges.len;
    while (lo < hi) {
        const mid = lo + (hi - lo) / 2;
        const r = unicode_c.ranges[mid];
        if (cp < r[0]) {
            hi = mid;
        } else if (cp > r[1]) {
            lo = mid + 1;
        } else return true;
    }
    return false;
}

/// One step of Go's utf8.DecodeRuneInString: the code point at s[i..] and its
/// size, or null with size 1 for an invalid byte (overlong forms, surrogates,
/// values above U+10FFFF and truncated sequences included; each byte of an
/// invalid sequence counts on its own).
pub fn decodeRune(s: []const u8, i: usize) struct { cp: ?u21, size: usize } {
    const b = s[i];
    if (b < 0x80) return .{ .cp = b, .size = 1 };
    const n = std.unicode.utf8ByteSequenceLength(b) catch return .{ .cp = null, .size = 1 };
    if (i + n > s.len) return .{ .cp = null, .size = 1 };
    const cp = std.unicode.utf8Decode(s[i..][0..n]) catch return .{ .cp = null, .size = 1 };
    return .{ .cp = cp, .size = n };
}

/// Turns an untrusted string into a single safe file name component.
///
/// It never returns a name that could be a path or that Windows and SMB
/// shares would treat specially:
///
///   - invalid UTF-8 bytes become '_';
///   - control characters (all of Unicode C*: C0/C1, DEL, bidi overrides and
///     isolates, zero-width and format characters, private use) and the line
///     and paragraph separators are removed;
///   - '/', '\' and the SMB-unsafe characters : * ? " < > | become '_';
///   - leading spaces and trailing spaces and dots are trimmed;
///   - Windows device names (CON, PRN, AUX, NUL, COM1-9, LPT1-9, with any
///     extension, case-insensitive) get a '_' prefix;
///   - the result is capped at 255 bytes by shortening the base name on a
///     rune boundary and keeping the extension (an "extension" is the part
///     from the last dot, at most 32 bytes, and never the whole of a dotfile).
///
/// An empty result, "." or ".." is `error.InvalidName`. The result lives in
/// `a`.
///
/// Unicode normalization is deliberately limited to the above, as in Go:
/// names are not converted to NFC, so two canonically equivalent spellings
/// stay distinct names.
pub fn safeName(a: Allocator, s: []const u8) (NameError || Allocator.Error)![]u8 {
    // The size cannot grow: an invalid byte becomes one '_', every other
    // replacement is as long as the original.
    var out: std.ArrayList(u8) = try .initCapacity(a, s.len);
    defer out.deinit(a);

    var i: usize = 0;
    while (i < s.len) {
        const r = decodeRune(s, i);
        defer i += r.size;
        const cp = r.cp orelse {
            out.appendAssumeCapacity('_');
            continue;
        };
        if (isStripped(cp)) continue;
        switch (cp) {
            '/', '\\', ':', '*', '?', '"', '<', '>', '|' => out.appendAssumeCapacity('_'),
            else => out.appendSliceAssumeCapacity(s[i..][0..r.size]),
        }
    }

    var name: []const u8 = std.mem.trimStart(u8, out.items, " ");
    name = std.mem.trimEnd(u8, name, " .");
    if (isBadName(name)) return error.InvalidName;

    const prefix: []const u8 = if (isWindowsReserved(name)) "_" else "";
    var base_part: []const u8 = name;
    var ext: []const u8 = "";
    if (prefix.len + name.len > max_name_bytes) {
        const sp = splitExt(name);
        // The prefix counts toward the cap, as in Go (it is part of the name
        // that gets capped).
        base_part = std.mem.trimEnd(u8, truncateRunes(sp.base, max_name_bytes - prefix.len - sp.ext.len), " .");
        ext = sp.ext;
    }
    const result = try std.mem.concat(a, u8, &.{ prefix, base_part, ext });
    errdefer a.free(result);
    if (isBadName(result)) return error.InvalidName;
    return result;
}

fn isBadName(name: []const u8) bool {
    return name.len == 0 or std.mem.eql(u8, name, ".") or std.mem.eql(u8, name, "..");
}

/// Whether `name`, ignoring everything from the first dot and trailing spaces
/// before it, is a Windows device name.
fn isWindowsReserved(name: []const u8) bool {
    const dot = std.mem.indexOfScalar(u8, name, '.') orelse name.len;
    const stem = std.mem.trimEnd(u8, name[0..dot], " ");
    for ([_][]const u8{ "CON", "PRN", "AUX", "NUL" }) |dev| {
        if (std.ascii.eqlIgnoreCase(stem, dev)) return true;
    }
    if (stem.len > 3 and (std.ascii.eqlIgnoreCase(stem[0..3], "COM") or std.ascii.eqlIgnoreCase(stem[0..3], "LPT"))) {
        for ([_][]const u8{ "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u{b9}", "\u{b2}", "\u{b3}" }) |d| {
            if (std.mem.eql(u8, stem[3..], d)) return true;
        }
    }
    return false;
}

/// Splits `name` at its last dot into base and extension (with the dot). A
/// leading dot or an overlong extension is not treated as an extension.
pub fn splitExt(name: []const u8) struct { base: []const u8, ext: []const u8 } {
    const i = std.mem.lastIndexOfScalar(u8, name, '.') orelse return .{ .base = name, .ext = "" };
    if (i == 0 or name.len - i > max_ext_bytes) return .{ .base = name, .ext = "" };
    return .{ .base = name[0..i], .ext = name[i..] };
}

/// Cuts `s` to at most `n` bytes without splitting a UTF-8 sequence.
fn truncateRunes(s: []const u8, n_in: usize) []const u8 {
    if (s.len <= n_in) return s;
    var n = n_in;
    while (n > 0 and (s[n] & 0xc0) == 0x80) n -= 1;
    return s[0..n];
}

// ---- unique ----

pub const UniqueError = Allocator.Error || Io.Cancelable;

/// Returns `path` if nothing exists there, otherwise the first free variant
/// with a counter: "file.mkv" becomes "file.1.mkv", "file.2.mkv", ... The
/// counter goes before the last extension of a file, and at the end for
/// directories ("dir" becomes "dir.1") and for names without an extension
/// (including dotfiles such as ".hidden").
///
/// `path` is relative to `dir` (use `Io.Dir.cwd()` for an absolute path). The
/// existence check does not follow symlinks, so a dangling symlink counts as
/// taken, and any error other than "does not exist" counts as taken too. (The
/// one difference to Go: `statFile` reports a path below a regular file as
/// "not found", where Go's Lstat says "not a directory" and so "taken".) The
/// result is shortened on rune boundaries so the final name stays within 255
/// bytes. The result lives in `a`.
///
/// The check is not atomic: a caller that creates the result must still open
/// it exclusively or use a non-clobbering rename.
pub fn unique(a: Allocator, io: Io, dir: Io.Dir, path: []const u8) UniqueError![]u8 {
    return uniqueTries(a, io, dir, path, max_unique_tries);
}

/// `unique` with its own bound on the counter (for tests).
pub fn uniqueTries(a: Allocator, io: Io, dir: Io.Dir, path: []const u8, max_tries: usize) UniqueError![]u8 {
    const kind = try lstatKind(io, dir, path);
    if (kind == .absent) return a.dupe(u8, path);

    const slash = std.mem.lastIndexOfScalar(u8, path, '/');
    const head = if (slash) |s| path[0 .. s + 1] else "";
    const name = if (slash) |s| path[s + 1 ..] else path;
    var base = name;
    var ext: []const u8 = "";
    if (kind != .directory) {
        const sp = splitExt(name);
        base = sp.base;
        ext = sp.ext;
    }

    var buf: [24]u8 = undefined;
    var n: usize = 1;
    while (n <= max_tries) : (n += 1) {
        const suffix = std.fmt.bufPrint(&buf, ".{d}", .{n}) catch unreachable;
        const cand = try withSuffix(a, head, base, suffix, ext);
        errdefer a.free(cand);
        if ((try lstatKind(io, dir, cand)) == .absent) return cand;
        a.free(cand);
    }
    var rnd: [8]u8 = undefined;
    io.random(&rnd);
    const hex = std.fmt.bytesToHex(rnd, .lower);
    var rsuffix: [17]u8 = undefined;
    const s = std.fmt.bufPrint(&rsuffix, ".{s}", .{hex}) catch unreachable;
    return withSuffix(a, head, base, s, ext);
}

const Presence = enum { absent, directory, other };

/// What is at `path` without following a final symlink: nothing, a directory,
/// or anything else (an error other than "not found" counts as "other").
fn lstatKind(io: Io, dir: Io.Dir, path: []const u8) Io.Cancelable!Presence {
    const st = dir.statFile(io, path, .{ .follow_symlinks = false }) catch |err| switch (err) {
        error.FileNotFound => return .absent,
        error.Canceled => return error.Canceled,
        else => return .other,
    };
    return if (st.kind == .directory) .directory else .other;
}

/// head + base + suffix + ext, shrinking base to respect the byte cap.
fn withSuffix(a: Allocator, head: []const u8, base: []const u8, suffix: []const u8, ext: []const u8) Allocator.Error![]u8 {
    const room = max_name_bytes -| (suffix.len + ext.len);
    return std.mem.concat(a, u8, &.{ head, truncateRunes(base, room), suffix, ext });
}

// ---- tests ----

const testing = std.testing;

fn expectSafe(want: []const u8, in: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try safeName(arena.allocator(), in);
    try testing.expectEqualStrings(want, got);
}

fn expectInvalid(in: []const u8) !void {
    try testing.expectError(error.InvalidName, safeName(testing.allocator, in));
}

test "safeName: ordinary names pass through unchanged" {
    for ([_][]const u8{
        "movie.mkv",
        "My Show S01E01 [1080p].mkv",
        "archive.part01.rar",
        ".hidden",
        "日本語のファイル.txt",
        "no-extension",
    }) |in| try expectSafe(in, in);
}

test "safeName: names that are empty or dots are rejected" {
    for ([_][]const u8{ "", ".", "..", "...", " ", " . ", "\x00", "\u{200b}" }) |in| try expectInvalid(in);
}

test "safeName: strips control and invisible characters" {
    const cases = [_][2][]const u8{
        .{ "a\x00b.txt", "ab.txt" }, // NUL
        .{ "a\x01\x1f\nb\tc\r.txt", "abc.txt" }, // C0 controls
        .{ "a\x7fb", "ab" }, // DEL
        .{ "a\u{85}\u{9f}b", "ab" }, // C1 controls
        .{ "movie\u{202e}txt.exe", "movietxt.exe" }, // bidi override
        .{ "a\u{2066}b\u{2069}c", "abc" }, // bidi isolates
        .{ "a\u{200b}b\u{200d}c\u{feff}d", "abcd" }, // zero width
        .{ "a\u{2028}b\u{2029}c", "abc" }, // line and paragraph separators
    };
    for (cases) |c| try expectSafe(c[1], c[0]);
}

test "safeName: separators and SMB-unsafe characters become underscores" {
    const cases = [_][2][]const u8{
        .{ "a/b", "a_b" },
        .{ "a\\b", "a_b" },
        .{ "../../etc/passwd", ".._.._etc_passwd" },
        .{ "C:\\Windows\\x.dll", "C__Windows_x.dll" },
        .{ "/etc/passwd", "_etc_passwd" },
        .{ "/", "_" },
        .{ "a:b*c?d\"e<f>g|h", "a_b_c_d_e_f_g_h" },
        .{ "name. . ", "name" },
        .{ "  name", "name" },
    };
    for (cases) |c| try expectSafe(c[1], c[0]);
}

test "safeName: invalid UTF-8 becomes underscores" {
    try expectSafe("a__b_.txt", "a\xff\xfeb\xc3.txt");
    // Overlong forms, an encoded surrogate and a value above U+10FFFF: every
    // byte on its own, as Go's decoder does.
    try expectSafe("a__b", "a\xc0\xafb");
    try expectSafe("a___b", "a\xed\xa0\x80b");
    try expectSafe("a____b", "a\xf4\x90\x80\x80b");
}

test "safeName: Windows device names get a prefix" {
    const cases = [_][2][]const u8{
        .{ "CON", "_CON" },
        .{ "con", "_con" },
        .{ "Prn.txt", "_Prn.txt" },
        .{ "AUX", "_AUX" },
        .{ "nul.tar.gz", "_nul.tar.gz" },
        .{ "COM1", "_COM1" },
        .{ "com9.log", "_com9.log" },
        .{ "LPT1", "_LPT1" },
        .{ "lpt9.txt", "_lpt9.txt" },
        .{ "CON .txt", "_CON .txt" },
        .{ "CONSOLE", "CONSOLE" },
        .{ "COM0", "COM0" },
        .{ "COM10", "COM10" },
        .{ "xCON.txt", "xCON.txt" },
        .{ "a.CON", "a.CON" },
        .{ "COM1:", "COM1_" },
        .{ "LPT\u{b9}.txt", "_LPT\u{b9}.txt" },
    };
    for (cases) |c| try expectSafe(c[1], c[0]);
}

test "safeName: caps the length at 255 bytes and keeps the extension" {
    const a = testing.allocator;
    var arena: std.heap.ArenaAllocator = .init(a);
    defer arena.deinit();
    const al = arena.allocator();
    const long = try al.alloc(u8, 400);
    @memset(long, 'a');

    { // ascii with extension
        const got = try safeName(al, try std.mem.concat(al, u8, &.{ long, ".mkv" }));
        try testing.expectEqual(@as(usize, 255), got.len);
        try testing.expect(std.mem.endsWith(u8, got, "aaa.mkv"));
    }
    { // no extension
        const got = try safeName(al, long);
        try testing.expectEqual(@as(usize, 255), got.len);
        try testing.expect(std.mem.allEqual(u8, got, 'a'));
    }
    { // multibyte truncated on a rune boundary
        const in = try std.mem.concat(al, u8, &.{ try repeat(al, "日", 200), ".mkv" });
        const got = try safeName(al, in);
        try testing.expect(got.len <= 255);
        try testing.expect(std.unicode.utf8ValidateSlice(got));
        try testing.expect(std.mem.endsWith(u8, got, "日.mkv"));
        // 83*3 + 4 = 253; an 84th rune would need 256.
        try testing.expectEqual(@as(usize, 83), std.mem.count(u8, got, "日"));
    }
    { // exactly 255 bytes is untouched
        const in = try std.mem.concat(al, u8, &.{ try repeat(al, "b", 251), ".mkv" });
        try expectSafe(in, in);
    }
    { // an overlong extension is truncated with the name
        const got = try safeName(al, try std.mem.concat(al, u8, &.{ "a.", long }));
        try testing.expectEqual(@as(usize, 255), got.len);
    }
    { // the reserved prefix counts toward the cap
        const got = try safeName(al, try std.mem.concat(al, u8, &.{ "CON.", try repeat(al, "x", 300) }));
        try testing.expect(got.len <= 255);
        try testing.expect(std.mem.startsWith(u8, got, "_CON."));
    }
    { // truncation does not leave a trailing dot or space
        const got = try safeName(al, try std.mem.concat(al, u8, &.{ try repeat(al, "a", 254), " .b", long }));
        try testing.expect(got.len <= 255);
        try testing.expect(got[got.len - 1] != ' ' and got[got.len - 1] != '.');
    }
}

fn repeat(a: Allocator, s: []const u8, n: usize) Allocator.Error![]u8 {
    const out = try a.alloc(u8, s.len * n);
    for (0..n) |i| @memcpy(out[i * s.len ..][0..s.len], s);
    return out;
}

test "isStripped agrees with Go on the edges of the classes" {
    for ([_]u21{ 0, 0x1f, 0x7f, 0x9f, 0xad, 0x61c, 0x200b, 0x200f, 0x2028, 0x2029, 0x202a, 0x202e, 0x2066, 0x2069, 0xd800, 0xdfff, 0xe000, 0xf8ff, 0xfeff, 0xfff9, 0xfffe, 0x10ffff, 0x378 }) |cp| {
        try testing.expect(isStripped(cp));
    }
    for ([_]u21{ 0x20, 0x21, 0x7e, 0xa0, 0xa1, 0xac, 0xae, 0x2027, 0x202f, 0x2060 - 1 + 0x1000, 0xfffd, 0x10000, 'A', 0x65e5 }) |cp| {
        try testing.expect(!isStripped(cp));
    }
}

test "decodeRune follows Go's decoder" {
    try testing.expectEqual(@as(?u21, 'a'), decodeRune("a", 0).cp);
    try testing.expectEqual(@as(usize, 3), decodeRune("日", 0).size);
    try testing.expectEqual(@as(?u21, null), decodeRune("\xe6\x97", 0).cp); // truncated
    try testing.expectEqual(@as(usize, 1), decodeRune("\xe6\x97", 0).size);
    try testing.expectEqual(@as(?u21, null), decodeRune("\xff", 0).cp);
}

// The property checks of Go's FuzzSafeName.
fn checkSafeName(in: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = safeName(arena.allocator(), in) catch |err| switch (err) {
        error.InvalidName => return,
        error.OutOfMemory => return err,
    };
    try testing.expect(got.len > 0 and got.len <= max_name_bytes);
    try testing.expect(!std.mem.eql(u8, got, ".") and !std.mem.eql(u8, got, ".."));
    try testing.expect(std.unicode.utf8ValidateSlice(got));
    try testing.expect(std.mem.indexOfAny(u8, got, "/\\\x00:*?\"<>|") == null);
    try testing.expect(got[got.len - 1] != ' ' and got[got.len - 1] != '.' and got[0] != ' ');
    var it = (try std.unicode.Utf8View.init(got)).iterator();
    while (it.nextCodepoint()) |cp| try testing.expect(!isStripped(cp));
}

fn fuzzSafeName(_: void, smith: *testing.Smith) !void {
    var buf: [512]u8 = undefined;
    const n = smith.slice(&buf);
    try checkSafeName(buf[0..n]);
}

/// `s` repeated `n` times (Zig 0.17 has no `**`).
fn rep(comptime s: []const u8, comptime n: usize) [s.len * n]u8 {
    var out: [s.len * n]u8 = undefined;
    for (0..n) |i| @memcpy(out[i * s.len ..][0..s.len], s);
    return out;
}

fn seedOf(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

test "fuzz: safeName always yields one safe name component" {
    const corpus = [_][]const u8{
        seedOf(""),                    seedOf("."),                     seedOf(".."),               seedOf("..."),
        seedOf("a/b"),                 seedOf("a\\b"),                  seedOf("../../etc/passwd"), seedOf("a\x00b"),
        seedOf("movie.mkv"),           seedOf("CON"),                   seedOf("nul.txt"),          seedOf("COM1.log"),
        seedOf("a\xffb"),              seedOf("x\u{202e}exe.txt"),      seedOf("\u{200b}"),         seedOf(" . "),
        seedOf("a:b*c?d\"e<f>g|h"),    seedOf("trailing. . "),          seedOf(".hidden"),          seedOf("\xed\xa0\x80"),
        seedOf("LPT\u{b9}.txt"),       seedOf(rep("a", 300) ++ ".mkv"),
        seedOf(rep("日", 100) ++ ".mkv"),
        seedOf("a." ++ rep("b", 300)),
    };
    for (corpus) |input| {
        var smith: testing.Smith = .{ .in = input };
        try fuzzSafeName({}, &smith);
    }
    try testing.fuzz({}, fuzzSafeName, .{ .corpus = &corpus });
}

// Cases made by a throwaway Go program from the real fsutil.SafeName: every
// edge of the table above and random mixtures. A line is the hex of the
// input, a tab and the hex of the result, or "-" for ErrInvalidName.
test "safeName gives the same names as Go" {
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();
    const text = try Io.Dir.cwd().readFileAlloc(testing.io, "testdata/compat/safename.txt", arena, .limited(8 << 20));
    var lines = std.mem.splitScalar(u8, text, '\n');
    var count: usize = 0;
    while (lines.next()) |line| {
        if (line.len == 0) continue;
        const tab = std.mem.indexOfScalar(u8, line, '\t').?;
        const in = try unhex(arena, line[0..tab]);
        const want = line[tab + 1 ..];
        count += 1;
        if (std.mem.eql(u8, want, "-")) {
            try testing.expectError(error.InvalidName, safeName(arena, in));
        } else {
            const got = try safeName(arena, in);
            const want_bytes = try unhex(arena, want);
            testing.expectEqualStrings(want_bytes, got) catch |err| {
                std.debug.print("input: {x}\n", .{in});
                return err;
            };
        }
    }
    try testing.expect(count > 1000);
}

fn unhex(a: Allocator, hex: []const u8) ![]u8 {
    const out = try a.alloc(u8, hex.len / 2);
    _ = try std.fmt.hexToBytes(out, hex);
    return out;
}

fn touch(dir: Io.Dir, path: []const u8) !void {
    try dir.writeFile(testing.io, .{ .sub_path = path, .data = "" });
}

fn expectUnique(dir: Io.Dir, want: []const u8, path: []const u8) !void {
    const got = try unique(testing.allocator, testing.io, dir, path);
    defer testing.allocator.free(got);
    try testing.expectEqualStrings(want, got);
}

test "unique: returns the path unchanged when free" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try expectUnique(tmp.dir, "file.mkv", "file.mkv");
}

test "unique: inserts the counter before the extension" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try touch(tmp.dir, "file.mkv");
    try expectUnique(tmp.dir, "file.1.mkv", "file.mkv");
    try touch(tmp.dir, "file.1.mkv");
    try expectUnique(tmp.dir, "file.2.mkv", "file.mkv");
}

test "unique: directories get the counter appended" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.createDir(testing.io, "Show.S01.1080p", .default_dir);
    try expectUnique(tmp.dir, "Show.S01.1080p.1", "Show.S01.1080p");
    try tmp.dir.createDir(testing.io, "Show.S01.1080p.1", .default_dir);
    try expectUnique(tmp.dir, "Show.S01.1080p.2", "Show.S01.1080p");
}

test "unique: dotfiles and names without an extension" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try touch(tmp.dir, ".hidden");
    try touch(tmp.dir, "README");
    try expectUnique(tmp.dir, ".hidden.1", ".hidden");
    try expectUnique(tmp.dir, "README.1", "README");
}

test "unique: a dangling symlink counts as taken" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.symLink(testing.io, "nowhere", "file.txt", .{});
    try expectUnique(tmp.dir, "file.1.txt", "file.txt");
}

test "unique: keeps the name within 255 bytes" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const name = rep("a", 251) ++ ".mkv";
    try touch(tmp.dir, name);
    const got = try unique(testing.allocator, testing.io, tmp.dir, name);
    defer testing.allocator.free(got);
    try testing.expect(got.len <= 255);
    try testing.expect(std.mem.endsWith(u8, got, ".1.mkv"));
    try testing.expectError(error.FileNotFound, tmp.dir.statFile(testing.io, got, .{ .follow_symlinks = false }));
}

test "unique: a multibyte base is cut on a rune boundary" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const name = rep("日", 83) ++ ".mkv"; // 253 bytes
    try touch(tmp.dir, name);
    const got = try unique(testing.allocator, testing.io, tmp.dir, name);
    defer testing.allocator.free(got);
    try testing.expect(got.len <= 255);
    try testing.expect(std.unicode.utf8ValidateSlice(got));
    try testing.expect(std.mem.endsWith(u8, got, ".1.mkv"));
}

test "unique: keeps the directory part of the path" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.createDir(testing.io, "d", .default_dir);
    try touch(tmp.dir, "d/f.txt");
    try expectUnique(tmp.dir, "d/f.1.txt", "d/f.txt");
}

test "unique: after the counter runs out a random suffix is used" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try touch(tmp.dir, "f.txt");
    try touch(tmp.dir, "f.1.txt");
    try touch(tmp.dir, "f.2.txt");
    const got = try uniqueTries(testing.allocator, testing.io, tmp.dir, "f.txt", 2);
    defer testing.allocator.free(got);
    // "f." + 16 hex digits + ".txt"
    try testing.expectEqual(@as(usize, 2 + 16 + 4), got.len);
    try testing.expect(std.mem.startsWith(u8, got, "f.") and std.mem.endsWith(u8, got, ".txt"));
}

test "unique: allocation failures leave nothing behind" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try touch(tmp.dir, "f.txt");
    try touch(tmp.dir, "f.1.txt");
    var fa: testing.FailingAllocator = .init(testing.allocator, .{ .fail_index = 1 });
    try testing.expectError(error.OutOfMemory, unique(fa.allocator(), testing.io, tmp.dir, "f.txt"));
}
