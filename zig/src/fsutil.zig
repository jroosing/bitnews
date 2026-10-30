//! Path helpers for hostile names (internal/fsutil): lexical cleaning and
//! containment (`clean`, `within`, `inside`) and the symlink-aware join
//! (`safeJoin`). Unix paths only: '/' separates, and the daemon runs on Linux
//! and macOS.
//!
//! The lexical functions never touch the file system; `safeJoin` does, to
//! resolve symlinks, and its check is not atomic (see there). Callers that
//! delete or write must still open what they act on without following
//! symlinks (the remover does).
//!
//! Strings returned by functions that take an allocator live in it: pass an
//! arena (one per request or step), nothing is freed one by one.
//!
//! SafeName, Unique, MoveDir and the rest of Go's fsutil come with the
//! milestones that need them.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;

pub const sep = '/';

pub const Error = error{
    /// A path that escapes its root, is absolute where a relative one is due,
    /// holds a NUL byte, or resolves outside the root through a symlink
    /// (Go: ErrUnsafePath).
    UnsafePath,
};

/// Whether `path` is absolute.
pub fn isAbs(path: []const u8) bool {
    return path.len > 0 and path[0] == sep;
}

/// The shortest path that names the same place, by lexical rules only (Go's
/// filepath.Clean): repeated and trailing separators go, "." elements go, and
/// ".." elements remove the element before them (at a rooted path's root they
/// are dropped, in a relative path they are kept when nothing is left to
/// remove). The empty path becomes ".". The result is never empty.
pub fn clean(a: Allocator, path: []const u8) Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    errdefer out.deinit(a);
    if (path.len == 0) {
        try out.append(a, '.');
        return out.toOwnedSlice(a);
    }
    const n = path.len;
    const rooted = path[0] == sep;
    var r: usize = 0;
    // The length of out below which ".." cannot backtrack.
    var dotdot: usize = 0;
    if (rooted) {
        try out.append(a, sep);
        r = 1;
        dotdot = 1;
    }
    while (r < n) {
        if (path[r] == sep) {
            r += 1;
        } else if (path[r] == '.' and (r + 1 == n or path[r + 1] == sep)) {
            r += 1;
        } else if (path[r] == '.' and r + 1 < n and path[r + 1] == '.' and (r + 2 == n or path[r + 2] == sep)) {
            r += 2;
            if (out.items.len > dotdot) {
                var w = out.items.len - 1;
                while (w > dotdot and out.items[w] != sep) w -= 1;
                out.shrinkRetainingCapacity(w);
            } else if (!rooted) {
                if (out.items.len > 0) try out.append(a, sep);
                try out.appendSlice(a, "..");
                dotdot = out.items.len;
            }
        } else {
            if ((rooted and out.items.len != 1) or (!rooted and out.items.len != 0)) try out.append(a, sep);
            while (r < n and path[r] != sep) : (r += 1) try out.append(a, path[r]);
        }
    }
    if (out.items.len == 0) try out.append(a, '.');
    return out.toOwnedSlice(a);
}

/// Joins the non-empty parts with separators and cleans the result (Go's
/// filepath.Join); no non-empty part gives "".
pub fn join(a: Allocator, parts: []const []const u8) Allocator.Error![]u8 {
    var raw: std.ArrayList(u8) = .empty;
    defer raw.deinit(a);
    for (parts) |p| {
        if (p.len == 0) continue;
        if (raw.items.len > 0) try raw.append(a, sep);
        try raw.appendSlice(a, p);
    }
    if (raw.items.len == 0) return a.dupe(u8, "");
    return clean(a, raw.items);
}

/// The last element of `path` (Go's filepath.Base): trailing separators are
/// ignored, "" gives "." and only separators give "/". A slice of `path`.
pub fn base(path: []const u8) []const u8 {
    if (path.len == 0) return ".";
    var end = path.len;
    while (end > 0 and path[end - 1] == sep) end -= 1;
    if (end == 0) return path[0..1];
    const start = if (std.mem.lastIndexOfScalar(u8, path[0..end], sep)) |i| i + 1 else 0;
    return path[start..end];
}

/// All but the last element of `path`, cleaned (Go's filepath.Dir).
pub fn dir(a: Allocator, path: []const u8) Allocator.Error![]u8 {
    const i = if (std.mem.lastIndexOfScalar(u8, path, sep)) |k| k + 1 else 0;
    return clean(a, path[0..i]);
}

/// `p` relative to `root` when both are clean and `p` is at or below `root`:
/// "." for the root itself, else the tail without a leading separator.
fn belowClean(root: []const u8, p: []const u8) ?[]const u8 {
    if (std.mem.eql(u8, root, p)) return ".";
    if (std.mem.eql(u8, root, ".")) {
        // A relative root of the current directory: everything relative that
        // does not climb is below it.
        if (isAbs(p) or std.mem.eql(u8, p, "..") or std.mem.startsWith(u8, p, "../")) return null;
        return p;
    }
    if (std.mem.eql(u8, root, "/")) return if (isAbs(p)) p[1..] else null;
    if (p.len > root.len and std.mem.startsWith(u8, p, root) and p[root.len] == sep) return p[root.len + 1 ..];
    return null;
}

/// `p` relative to `root`, lexically: "." when they are the same place, null
/// when `p` is outside `root`. Both are cleaned first; a relative path paired
/// with an absolute one is never below. The result is allocated in `a`.
pub fn relBelow(a: Allocator, root: []const u8, p: []const u8) Allocator.Error!?[]u8 {
    const cr = try clean(a, root);
    defer a.free(cr);
    const cp = try clean(a, p);
    defer a.free(cp);
    const rel = belowClean(cr, cp) orelse return null;
    return try a.dupe(u8, rel);
}

/// Whether `p` is `root` or lies below it, lexically. It does not touch the
/// file system or resolve symlinks, so callers that need real containment
/// resolve first (`safeJoin` does). "/a/b" is within "/a", "/ab" is not.
/// `scratch` holds temporary copies; an arena is fine.
pub fn within(scratch: Allocator, root: []const u8, p: []const u8) Allocator.Error!bool {
    const r = (try relBelow(scratch, root, p)) orelse return false;
    scratch.free(r);
    return true;
}

/// Whether `p` lies strictly below `root`, lexically: like `within`, but
/// `root` itself is not inside `root`.
pub fn inside(scratch: Allocator, root: []const u8, p: []const u8) Allocator.Error!bool {
    const r = (try relBelow(scratch, root, p)) orelse return false;
    defer scratch.free(r);
    return !std.mem.eql(u8, r, ".");
}

pub const SafeJoinError = Error || Allocator.Error || Io.Cancelable || Io.Dir.RealPathError || Io.Dir.StatFileError;

/// Joins an untrusted relative path onto `root` and guarantees the result
/// stays inside `root`.
///
/// `rel` is cleaned lexically; backslashes count as separators. Absolute
/// paths, NUL bytes and any ".." that climbs above `root` give
/// `error.UnsafePath`. An empty `rel` or "." yields `root`.
///
/// If `root` exists, the deepest existing ancestor of the result is also
/// resolved and must lie inside the resolved `root`; this catches symlinked
/// parent directories that point outside. Dangling or looping symlinks on the
/// way are rejected too, since writing through them could create files
/// anywhere. Components that do not exist yet are allowed. If `root` does not
/// exist, nothing below it can be a symlink and only the lexical check
/// applies.
///
/// The returned path is built from `root` as given (cleaned), not the
/// resolved path. The check is not atomic: a concurrent process able to
/// create symlinks under `root` can still win a race between `safeJoin` and
/// the use of the path.
pub fn safeJoin(a: Allocator, io: Io, root: []const u8, rel_path: []const u8) SafeJoinError![]u8 {
    if (root.len == 0) return error.UnsafePath;
    if (std.mem.indexOfScalar(u8, root, 0) != null or std.mem.indexOfScalar(u8, rel_path, 0) != null) return error.UnsafePath;

    // Backslashes are separators regardless of platform: names from archives
    // and torrents may come from Windows tools.
    const slashed = try a.dupe(u8, rel_path);
    defer a.free(slashed);
    std.mem.replaceScalar(u8, slashed, '\\', '/');
    if (isAbs(slashed)) return error.UnsafePath;
    const cleaned = try clean(a, slashed);
    defer a.free(cleaned);
    if (std.mem.eql(u8, cleaned, "..") or std.mem.startsWith(u8, cleaned, "../")) return error.UnsafePath;
    const joined = try join(a, &.{ root, cleaned });
    errdefer a.free(joined);

    var buf: [Io.Dir.max_path_bytes]u8 = undefined;
    const cwd = Io.Dir.cwd();
    const root_len = cwd.realPathFile(io, root, &buf) catch |err| switch (err) {
        // Nothing exists below a missing root, so there are no symlinks to follow.
        error.FileNotFound => return joined,
        else => |e| return e,
    };
    const real_root = try a.dupe(u8, buf[0..root_len]);
    defer a.free(real_root);

    const anc = try deepestExisting(io, joined);
    const anc_len = cwd.realPathFile(io, anc, &buf) catch |err| switch (err) {
        error.Canceled => return error.Canceled,
        // A dangling or looping symlink: writing through it could land anywhere.
        else => return error.UnsafePath,
    };
    if (!try within(a, real_root, buf[0..anc_len])) return error.UnsafePath;
    return joined;
}

/// The longest prefix of `path` that exists, judged without following the
/// last element so that a dangling symlink counts as existing. A slice of
/// `path`.
fn deepestExisting(io: Io, path: []const u8) (Error || Io.Dir.StatFileError)![]const u8 {
    var cur = path;
    while (true) {
        if (Io.Dir.cwd().statFile(io, cur, .{ .follow_symlinks = false })) |_| {
            return cur;
        } else |err| switch (err) {
            error.FileNotFound, error.NotDir => {},
            // A symlink loop on the way: writing through it could land anywhere.
            error.SymLinkLoop => return error.UnsafePath,
            else => |e| return e,
        }
        // The parent, by cutting the last element; the path is clean.
        const i = std.mem.lastIndexOfScalar(u8, cur, sep) orelse return ".";
        if (i == 0) return if (cur.len > 1) cur[0..1] else cur;
        cur = cur[0..i];
    }
}

// ---- tests ----

const testing = std.testing;

/// Runs every corpus entry once, as `testing.fuzz` does when the test binary
/// is not built for fuzzing (on some backends it does not even do that).
fn runCorpus(ctx: anytype, comptime one: fn (@TypeOf(ctx), *testing.Smith) anyerror!void, entries: []const []const u8) !void {
    for (entries) |input| {
        var smith: testing.Smith = .{ .in = input };
        try one(ctx, &smith);
    }
}

fn expectClean(want: []const u8, path: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const got = try clean(arena.allocator(), path);
    try testing.expectEqualStrings(want, got);
}

// Cases of Go's path_test.go cleantests (unix).
test "clean: lexical cleaning like filepath.Clean" {
    const cases = [_][2][]const u8{
        .{ "", "." },
        .{ "abc", "abc" },
        .{ "abc/def", "abc/def" },
        .{ "a/b/c", "a/b/c" },
        .{ ".", "." },
        .{ "..", ".." },
        .{ "../..", "../.." },
        .{ "../../abc", "../../abc" },
        .{ "/abc", "/abc" },
        .{ "/", "/" },
        .{ "abc/", "abc" },
        .{ "abc/def/", "abc/def" },
        .{ "a/b/c/", "a/b/c" },
        .{ "./", "." },
        .{ "../", ".." },
        .{ "../../", "../.." },
        .{ "/abc/", "/abc" },
        .{ "abc//def//ghi", "abc/def/ghi" },
        .{ "//abc", "/abc" },
        .{ "///abc", "/abc" },
        .{ "//abc//", "/abc" },
        .{ "abc//", "abc" },
        .{ "abc/./def", "abc/def" },
        .{ "/./abc/def", "/abc/def" },
        .{ "abc/.", "abc" },
        .{ "abc/def/ghi/../jkl", "abc/def/jkl" },
        .{ "abc/def/../ghi/../jkl", "abc/jkl" },
        .{ "abc/def/..", "abc" },
        .{ "abc/def/../..", "." },
        .{ "/abc/def/../..", "/" },
        .{ "abc/def/../../..", ".." },
        .{ "/abc/def/../../..", "/" },
        .{ "abc/def/../../../ghi/jkl/../../../mno", "../../mno" },
        .{ "/../abc", "/abc" },
        .{ "a/../b:/../../c", "../c" },
        .{ "a/..b", "a/..b" },
        .{ "a/b..", "a/b.." },
        .{ "...", "..." },
        .{ ".../a", ".../a" },
        .{ "a\x00b/../c", "c" },
    };
    for (cases) |c| try expectClean(c[1], c[0]);
}

test "join: skips empty parts and cleans" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try testing.expectEqualStrings("/a/b", try join(a, &.{ "/a", "b" }));
    try testing.expectEqualStrings("/a/b", try join(a, &.{ "/a/", "", "./b/" }));
    try testing.expectEqualStrings("b", try join(a, &.{ "", "b" }));
    try testing.expectEqualStrings("", try join(a, &.{ "", "" }));
    try testing.expectEqualStrings("/", try join(a, &.{ "/", "a", ".." }));
    try testing.expectEqualStrings("/b", try join(a, &.{ "/a", "../b" }));
}

test "base and dir like filepath.Base and filepath.Dir" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const cases = [_][3][]const u8{
        .{ "", ".", "." },
        .{ ".", ".", "." },
        .{ "/.", ".", "/" },
        .{ "/", "/", "/" },
        .{ "////", "/", "/" },
        .{ "x/", "x", "x" },
        .{ "abc", "abc", "." },
        .{ "abc/def", "def", "abc" },
        .{ "a/b/.x", ".x", "a/b" },
        .{ "a/b/c.x", "c.x", "a/b" },
        .{ "/abc", "abc", "/" },
        .{ "/a/b", "b", "/a" },
        .{ "/a//b/", "b", "/a/b" },
    };
    for (cases) |c| {
        try testing.expectEqualStrings(c[1], base(c[0]));
        try testing.expectEqualStrings(c[2], try dir(a, c[0]));
    }
}

// The table of Go's TestWithinAndInside.
test "within and inside" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const Case = struct { root: []const u8, p: []const u8, within: bool, inside: bool };
    const cases = [_]Case{
        .{ .root = "/a", .p = "/a", .within = true, .inside = false },
        .{ .root = "/a", .p = "/a/", .within = true, .inside = false },
        .{ .root = "/a/", .p = "/a", .within = true, .inside = false },
        .{ .root = "/a", .p = "/a/b", .within = true, .inside = true },
        .{ .root = "/a/", .p = "/a/b/", .within = true, .inside = true },
        .{ .root = "/a", .p = "/a/b/c", .within = true, .inside = true },
        .{ .root = "/a", .p = "/ab", .within = false, .inside = false },
        .{ .root = "/a", .p = "/ab/c", .within = false, .inside = false },
        .{ .root = "/a/b", .p = "/a", .within = false, .inside = false },
        .{ .root = "/a", .p = "/b", .within = false, .inside = false },
        .{ .root = "/a", .p = "/a/..", .within = false, .inside = false },
        .{ .root = "/a", .p = "/a/../b", .within = false, .inside = false },
        .{ .root = "/a", .p = "/a/b/..", .within = true, .inside = false },
        .{ .root = "/a", .p = "/a/./b", .within = true, .inside = true },
        .{ .root = "/a", .p = "/a/..b", .within = true, .inside = true },
        .{ .root = "/a", .p = "/a//b", .within = true, .inside = true },
        .{ .root = "/", .p = "/", .within = true, .inside = false },
        .{ .root = "/", .p = "/a", .within = true, .inside = true },
        .{ .root = "/", .p = "/a/b", .within = true, .inside = true },
        .{ .root = "/a", .p = "a/b", .within = false, .inside = false },
        .{ .root = "a", .p = "/a/b", .within = false, .inside = false },
        .{ .root = "a", .p = "a/b", .within = true, .inside = true },
        .{ .root = "a", .p = "a", .within = true, .inside = false },
        .{ .root = "a", .p = "../a", .within = false, .inside = false },
        // Beyond Go's table: the current directory as a root, and roots that climb.
        .{ .root = "", .p = "a", .within = true, .inside = true },
        .{ .root = ".", .p = "..", .within = false, .inside = false },
        .{ .root = ".", .p = "/a", .within = false, .inside = false },
        .{ .root = "..", .p = "a", .within = false, .inside = false },
        .{ .root = "../a", .p = "../a/b", .within = true, .inside = true },
        .{ .root = "/", .p = "a", .within = false, .inside = false },
        .{ .root = "/a", .p = "/a/b\x00", .within = true, .inside = true },
    };
    for (cases) |c| {
        try testing.expectEqual(c.within, try within(a, c.root, c.p));
        try testing.expectEqual(c.inside, try inside(a, c.root, c.p));
    }
}

fn seed(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

fn seed2(comptime x: []const u8, comptime y: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.* ++ std.mem.toBytes(@as(u32, y.len)) ++ y.*);
}

fn fuzzClean(_: void, smith: *testing.Smith) !void {
    var buf: [128]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const c = try clean(a, buf[0..n]);
    try testing.expect(c.len > 0);
    // Idempotent, no empty or "." elements, ".." only leading in a relative path.
    try testing.expectEqualStrings(c, try clean(a, c));
    try testing.expect(std.mem.indexOf(u8, c, "//") == null);
    if (c.len > 1) try testing.expect(c[c.len - 1] != '/');
    var seen_normal = false;
    var it = std.mem.splitScalar(u8, c, '/');
    while (it.next()) |el| {
        if (el.len == 0) continue;
        try testing.expect(c.len == 1 or !std.mem.eql(u8, el, "."));
        if (std.mem.eql(u8, el, "..")) {
            try testing.expect(!isAbs(c));
            try testing.expect(!seen_normal);
        } else seen_normal = true;
    }
    if (isAbs(buf[0..n])) try testing.expect(isAbs(c));
    if (c.len == 1 and c[0] == '.') try testing.expect(!isAbs(buf[0..n]));
}

test "fuzz: clean is idempotent and normal for any bytes" {
    const corpus = [_][]const u8{ seed("a/../.."), seed("/../a//b/./c/"), seed("..."), seed("a/b/../../.."), seed(""), seed("/"), seed("./."), seed("\x00/..") };
    try runCorpus({}, fuzzClean, &corpus);
    try testing.fuzz({}, fuzzClean, .{ .corpus = &corpus });
}

fn fuzzWithin(_: void, smith: *testing.Smith) !void {
    var rb: [64]u8 = undefined;
    var pb: [64]u8 = undefined;
    const root = rb[0..smith.slice(&rb)];
    const p = pb[0..smith.slice(&pb)];
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const w = try within(a, root, p);
    const in = try inside(a, root, p);
    if (!w) {
        try testing.expect(!in);
        return;
    }
    const cr = try clean(a, root);
    const cp = try clean(a, p);
    try testing.expectEqual(!std.mem.eql(u8, cr, cp), in);
    // Within means p is root or root plus a separator-bounded tail.
    if (!std.mem.eql(u8, cr, cp) and !std.mem.eql(u8, cr, "/") and !std.mem.eql(u8, cr, ".")) {
        try testing.expect(std.mem.startsWith(u8, cp, cr));
        try testing.expectEqual(@as(u8, '/'), cp[cr.len]);
    }
    try testing.expectEqual(isAbs(root), isAbs(p));
    // The tail, when joined back, names p.
    const r = (try relBelow(a, root, p)).?;
    try testing.expectEqualStrings(cp, try join(a, &.{ cr, r }));
}

test "fuzz: within and inside agree with each other and with clean" {
    const corpus = [_][]const u8{
        seed2("/a", "/a"),
        seed2("/a", "/ab"),
        seed2("/", "/x"),
        seed2("a", "a/b"),
        seed2("", ""),
        seed2("/a/", "/a/../b"),
        seed2("a", "/a"),
        seed2(".", "../a"),
    };
    try runCorpus({}, fuzzWithin, &corpus);
    try testing.fuzz({}, fuzzWithin, .{ .corpus = &corpus });
}

// ---- safeJoin on a real tree ----

const Tree = struct {
    tmp: testing.TmpDir,
    /// Absolute and symlink-free, so `real_base` equals what the tests build.
    base: []u8,

    fn init() !Tree {
        var t: Tree = .{ .tmp = testing.tmpDir(.{}), .base = undefined };
        errdefer t.tmp.cleanup();
        var buf: [Io.Dir.max_path_bytes]u8 = undefined;
        const n = try t.tmp.dir.realPath(testing.io, &buf);
        t.base = try testing.allocator.dupe(u8, buf[0..n]);
        return t;
    }

    fn deinit(t: *Tree) void {
        testing.allocator.free(t.base);
        t.tmp.cleanup();
    }

    fn mkdir(t: *Tree, rel_path: []const u8) !void {
        try t.tmp.dir.createDirPath(testing.io, rel_path);
    }

    fn symlink(t: *Tree, target: []const u8, rel_path: []const u8) !void {
        try t.tmp.dir.symLink(testing.io, target, rel_path, .{});
    }

    fn path(t: *Tree, a: Allocator, rel_path: []const u8) ![]u8 {
        return join(a, &.{ t.base, rel_path });
    }
};

test "safeJoin joins relative paths under the root" {
    var tree = try Tree.init();
    defer tree.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const cases = [_][2][]const u8{
        .{ "file.txt", "file.txt" },
        .{ "a/b/c.txt", "a/b/c.txt" },
        .{ "./a//b/", "a/b" },
        .{ "a/../b", "b" },
        .{ "a/b/../../c", "c" },
        .{ "", "" },
        .{ ".", "" },
        .{ "windows\\style\\path", "windows/style/path" },
    };
    for (cases) |c| {
        const got = try safeJoin(a, testing.io, tree.base, c[0]);
        try testing.expectEqualStrings(try tree.path(a, c[1]), got);
    }
}

test "safeJoin rejects lexical escapes, NUL bytes and an empty or NUL root" {
    var tree = try Tree.init();
    defer tree.deinit();
    const rejected = [_][]const u8{
        "..",           "../x",         "a/../../x",   "a/b/../../../x",
        "..\\x",        "a\\..\\..\\x", "/etc/passwd", "\\etc\\passwd",
        "//host/share", "a\x00b",       "\x00",
    };
    for (rejected) |rel_path| {
        try testing.expectError(error.UnsafePath, safeJoin(testing.allocator, testing.io, tree.base, rel_path));
    }
    try testing.expectError(error.UnsafePath, safeJoin(testing.allocator, testing.io, "", "x"));
    try testing.expectError(error.UnsafePath, safeJoin(testing.allocator, testing.io, "a\x00b", "x"));
}

test "safeJoin: a sibling that has the root as a prefix is not under it" {
    var tree = try Tree.init();
    defer tree.deinit();
    try tree.mkdir("data");
    try tree.mkdir("data-evil");
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const root = try tree.path(a, "data");
    try testing.expectError(error.UnsafePath, safeJoin(testing.allocator, testing.io, root, "../data-evil/x"));
}

test "safeJoin resolves symlinks in existing parents" {
    var tree = try Tree.init();
    defer tree.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try tree.mkdir("root/real/sub");
    try tree.mkdir("outside");
    try tree.symlink(try tree.path(a, "outside"), "root/escape");
    try tree.symlink("real", "root/inside");
    try tree.symlink(try tree.path(a, "does-not-exist"), "root/dangling");
    try tree.symlink("../../outside", "root/real/rel-escape");
    try tree.symlink("../../..", "root/real/sub/up");
    try tree.symlink("loop", "root/loop");
    const root = try tree.path(a, "root");

    const rejected = [_][]const u8{
        "escape",            "escape/file.txt",          "escape/new/deeper/file.txt", "dangling",
        "dangling/file.txt", "real/rel-escape/file.txt", "real/sub/up/outside/x",      "loop",
        "loop/x",
    };
    for (rejected) |rel_path| {
        try testing.expectError(error.UnsafePath, safeJoin(testing.allocator, testing.io, root, rel_path));
    }

    // A symlink that stays inside is fine, and the unresolved path comes back.
    const inside_path = try safeJoin(a, testing.io, root, "inside/sub/new.txt");
    try testing.expectEqualStrings(try tree.path(a, "root/inside/sub/new.txt"), inside_path);
    // A tail that does not exist yet is fine.
    const tail = try safeJoin(a, testing.io, root, "real/a/b/c.txt");
    try testing.expectEqualStrings(try tree.path(a, "root/real/a/b/c.txt"), tail);
}

test "safeJoin: a root behind a symlinked ancestor is allowed" {
    var tree = try Tree.init();
    defer tree.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try tree.mkdir("real");
    try tree.symlink(try tree.path(a, "real"), "link");
    const link = try tree.path(a, "link");
    const got = try safeJoin(a, testing.io, link, "a/b.txt");
    try testing.expectEqualStrings(try tree.path(a, "link/a/b.txt"), got);
}

test "safeJoin: a root that does not exist has nothing to resolve" {
    var tree = try Tree.init();
    defer tree.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const root = try tree.path(a, "not/yet");
    const got = try safeJoin(a, testing.io, root, "a/b.txt");
    try testing.expectEqualStrings(try tree.path(a, "not/yet/a/b.txt"), got);
}

test "safeJoin: a file in the way is not a directory to climb out of" {
    var tree = try Tree.init();
    defer tree.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try tree.mkdir("root");
    try tree.tmp.dir.writeFile(testing.io, .{ .sub_path = "root/f", .data = "x" });
    const root = try tree.path(a, "root");
    // Below a regular file nothing exists; the result is still inside.
    const got = try safeJoin(a, testing.io, root, "f/x/y");
    try testing.expectEqualStrings(try tree.path(a, "root/f/x/y"), got);
}

const FuzzJoinCtx = struct { root: []const u8, outside: []const u8 };

fn fuzzSafeJoin(ctx: FuzzJoinCtx, smith: *testing.Smith) !void {
    var buf: [96]u8 = undefined;
    const n = smith.slice(&buf);
    const rel_path = buf[0..n];
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const got = safeJoin(a, testing.io, ctx.root, rel_path) catch return;

    // Lexical containment: no NUL, and the root is a path prefix.
    try testing.expect(std.mem.indexOfScalar(u8, got, 0) == null);
    try testing.expect(try within(a, ctx.root, got));
    const tail = (try relBelow(a, ctx.root, got)).?;
    try testing.expect(!std.mem.eql(u8, tail, "..") and !std.mem.startsWith(u8, tail, "../"));

    // Real containment: whatever exists of the result resolves inside the root.
    var anc: []const u8 = got;
    while (true) {
        if (Io.Dir.cwd().statFile(testing.io, anc, .{ .follow_symlinks = false })) |_| break else |_| {}
        const i = std.mem.lastIndexOfScalar(u8, anc, '/') orelse return;
        if (i == 0) return;
        anc = anc[0..i];
    }
    var rb: [Io.Dir.max_path_bytes]u8 = undefined;
    const rn = try Io.Dir.cwd().realPathFile(testing.io, anc, &rb);
    try testing.expect(try within(a, ctx.root, rb[0..rn]));
    try testing.expect(!try within(a, ctx.outside, rb[0..rn]));
}

test "fuzz: safeJoin never returns a path that resolves outside the root" {
    var tree = try Tree.init();
    defer tree.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try tree.mkdir("root/dir");
    try tree.mkdir("root/inside");
    try tree.mkdir("outside");
    try tree.symlink(try tree.path(a, "outside"), "root/link");
    try tree.symlink(try tree.path(a, "missing"), "root/dangling");
    try tree.symlink("../outside", "root/rel");
    try tree.symlink("inside", "root/ok");
    try tree.symlink("loop", "root/loop");
    const ctx: FuzzJoinCtx = .{ .root = try tree.path(a, "root"), .outside = try tree.path(a, "outside") };
    const corpus = [_][]const u8{
        seed(""),            seed("."),               seed(".."),         seed("../x"),          seed("a/../../x"),
        seed("/etc/passwd"), seed("..\\x"),           seed("a/b/c"),      seed("a\x00b"),        seed("link"),
        seed("link/x"),      seed("inside/new/file"), seed("dangling/x"), seed("dir/../link/x"), seed("//host/share"),
        seed("a/./b//c/"),   seed("a\\..\\..\\b"),    seed("rel/x"),      seed("ok/x"),          seed("loop/x"),
    };
    try runCorpus(ctx, fuzzSafeJoin, &corpus);
    try testing.fuzz(ctx, fuzzSafeJoin, .{ .corpus = &corpus });
}
