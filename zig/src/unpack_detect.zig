//! Finding the archive sets in a directory (internal/postproc/unpack
//! detect.go, volumes.go). Files are classified by their magic bytes, never
//! by extension alone; the extension only decides which volumes belong
//! together. Symbolic links, directories and special files are ignored. The
//! volume names are matched by hand: the Go code uses four regular
//! expressions, and `matchSevenZ`, `matchNewStyle`, `matchOldFirst` and
//! `matchOldCont` accept exactly what those do (golden test).
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const name_mod = @import("unpack_name.zig");
const fsutil = @import("fsutil.zig");
const ufs = @import("unpack_fs.zig");

/// The archive format of a set; the order is the order of the result.
pub const Format = enum(u8) {
    rar = 1,
    seven_z = 2,

    pub fn text(f: Format) []const u8 {
        return switch (f) {
            .rar => "rar",
            .seven_z => "7z",
        };
    }
};

/// One archive: a single file or an ordered list of volumes.
pub const ArchiveSet = struct {
    format: Format,
    /// The name without volume suffixes, "movie" for movie.part01.rar.
    name: []const u8,
    /// Full paths, first volume first.
    volumes: []const []const u8,
    /// Set when the set cannot be unpacked as found, for example when a
    /// middle volume is missing: the text of Go's ErrMissingVolume chain.
    /// Unpack refuses such a set.
    err: ?[]const u8 = null,
};

const magic_rar4 = "Rar!\x1a\x07\x00";
const magic_rar5 = "Rar!\x1a\x07\x01\x00";
const magic_7z = "7z\xbc\xaf\x27\x1c";

// ---- volume names ----

/// `(?i)^(.+\.7z)\.(\d{3,9})$`
pub const SevenZ = struct { base: []const u8, digits: []const u8 };

pub fn matchSevenZ(name: []const u8) ?SevenZ {
    const dot = std.mem.lastIndexOfScalar(u8, name, '.') orelse return null;
    const digits = name[dot + 1 ..];
    if (digits.len < 3 or digits.len > 9 or !allDigits(digits)) return null;
    const base = name[0..dot];
    if (base.len < 4 or !endsWithFold(base, ".7z") or hasNewline(base)) return null;
    return .{ .base = base, .digits = digits };
}

/// `(?i)^(.+)\.part(\d+)\.rar$`
pub const NewStyle = struct { base: []const u8, digits: []const u8 };

pub fn matchNewStyle(name: []const u8) ?NewStyle {
    if (!endsWithFold(name, ".rar")) return null;
    const head = name[0 .. name.len - 4];
    var q = head.len;
    while (q > 0 and std.ascii.isDigit(head[q - 1])) q -= 1;
    if (q == head.len) return null;
    if (q < 6 or !endsWithFold(head[0..q], ".part")) return null;
    const base = head[0 .. q - 5];
    if (base.len == 0 or hasNewline(base)) return null;
    return .{ .base = base, .digits = head[q..] };
}

/// `(?i)^(.+)\.rar$`
pub fn matchOldFirst(name: []const u8) ?[]const u8 {
    if (!endsWithFold(name, ".rar")) return null;
    const base = name[0 .. name.len - 4];
    if (base.len == 0 or hasNewline(base)) return null;
    return base;
}

/// `(?i)^(.+)\.([r-z])(\d\d)$`; the letter is also matched by U+017F, which
/// Go's case folding makes equal to 's'.
pub const OldCont = struct { base: []const u8, letter: []const u8, digits: []const u8 };

pub fn matchOldCont(name: []const u8) ?OldCont {
    if (name.len < 4) return null;
    const digits = name[name.len - 2 ..];
    if (!allDigits(digits)) return null;
    var head = name[0 .. name.len - 2];
    var letter_len: usize = 1;
    const last = head[head.len - 1];
    if (std.mem.endsWith(u8, head, "\xc5\xbf")) {
        letter_len = 2;
    } else if ((last | 0x20) < 'r' or (last | 0x20) > 'z' or !std.ascii.isAlphabetic(last)) {
        return null;
    }
    if (head.len < letter_len + 2) return null;
    const letter = head[head.len - letter_len ..];
    head = head[0 .. head.len - letter_len];
    if (head[head.len - 1] != '.') return null;
    const base = head[0 .. head.len - 1];
    if (base.len == 0 or hasNewline(base)) return null;
    return .{ .base = base, .letter = letter, .digits = digits };
}

fn allDigits(s: []const u8) bool {
    for (s) |c| if (!std.ascii.isDigit(c)) return false;
    return true;
}

fn hasNewline(s: []const u8) bool {
    return std.mem.indexOfScalar(u8, s, '\n') != null;
}

fn endsWithFold(s: []const u8, suffix: []const u8) bool {
    return s.len >= suffix.len and std.ascii.eqlIgnoreCase(s[s.len - suffix.len ..], suffix);
}

/// Go's strconv.Atoi for a run of ASCII digits: null when it overflows int.
fn atoi(digits: []const u8) ?i64 {
    return std.fmt.parseInt(i64, digits, 10) catch null;
}

// ---- grouping ----

const Family = enum(u8) {
    rar_new = 1, // name.partNN.rar
    rar_old = 2, // name.rar, name.r00, name.r01, ...
    seven_z = 3, // name.7z.001, name.7z.002, ...
};

const Group = struct {
    fam: Family,
    name: []const u8,
    /// Volume number to the files for it, normally one.
    vols: std.AutoArrayHashMapUnmanaged(i64, std.ArrayList([]const u8)) = .empty,
    /// Volume numbers whose file starts with archive magic.
    has_mag: std.AutoArrayHashMapUnmanaged(i64, void) = .empty,

    fn add(g: *Group, a: Allocator, num: i64, path: []const u8, magic: bool) Allocator.Error!void {
        const gop = try g.vols.getOrPut(a, num);
        if (!gop.found_existing) gop.value_ptr.* = .empty;
        try gop.value_ptr.append(a, path);
        if (magic) try g.has_mag.put(a, num, {});
    }

    /// Turns the group into a set, recording gaps as the set's error.
    fn toSet(g: *Group, a: Allocator, sa: Allocator) Allocator.Error!?ArchiveSet {
        var first: i64 = 1;
        var format: Format = .rar;
        switch (g.fam) {
            .rar_old => first = 0,
            .seven_z => format = .seven_z,
            .rar_new => {},
        }
        var set: ArchiveSet = .{ .format = format, .name = g.name, .volumes = &.{} };
        if (g.fam == .seven_z) {
            set.name = trimExt(g.name);
            if (!g.has_mag.contains(1)) return null; // a lone continuation volume is not an archive
        }
        const nums = try sa.dupe(i64, g.vols.keys());
        std.mem.sort(i64, nums, {}, std.sort.asc(i64));
        var vols: std.ArrayList([]const u8) = .empty;
        var err: std.ArrayList(u8) = .empty;
        const p = errors.prefix(error.MissingVolume);
        for (nums, 0..) |n, i| {
            const files = g.vols.get(n).?.items;
            if (files.len > 1) {
                if (err.items.len > 0) try err.append(a, '\n');
                try err.print(a, "{s}: {s} has several files for volume {d}", .{ p, g.name, n });
            }
            try vols.appendSlice(a, files);
            const want = first + @as(i64, @intCast(i));
            if (n != want and err.items.len == 0) {
                try err.print(a, "{s}: {s} is missing volume {d}", .{ p, g.name, want });
            }
        }
        if (nums.len > 0 and nums[0] != first and err.items.len == 0) {
            try err.print(a, "{s}: {s} has no first volume", .{ p, g.name });
        }
        set.volumes = try vols.toOwnedSlice(a);
        set.err = if (err.items.len > 0) err.items else null;
        return set;
    }
};

/// strings.TrimSuffix(name, filepath.Ext(name)).
fn trimExt(name: []const u8) []const u8 {
    const dot = std.mem.lastIndexOfScalar(u8, name, '.') orelse return name;
    return name[0..dot];
}

fn sniff(env: *Env, path: []const u8) ?Format {
    var f = Io.Dir.cwd().openFile(env.io, path, .{}) catch return null;
    defer f.close(env.io);
    var buf: [8]u8 = undefined;
    const n = f.readPositionalAll(env.io, &buf, 0) catch return null;
    if (n == 0) return null;
    const b = buf[0..n];
    if (std.mem.startsWith(u8, b, magic_rar5) or std.mem.startsWith(u8, b, magic_rar4)) return .rar;
    if (std.mem.startsWith(u8, b, magic_7z)) return .seven_z;
    return null;
}

const Sortable = struct { set: ArchiveSet, key: []const u8 };

fn lessThan(_: void, x: Sortable, y: Sortable) bool {
    if (x.set.format != y.set.format) return @backingInt(x.set.format) < @backingInt(y.set.format);
    const c = std.mem.order(u8, x.key, y.key);
    if (c != .eq) return c == .lt;
    return std.mem.order(u8, x.set.volumes[0], y.set.volumes[0]) == .lt;
}

/// Finds the archive sets among the regular files of `dir_path`. The result,
/// ordered by format and name, lives in `env.arena`. Detect does not recurse.
pub fn detect(env: *Env, dir_path: []const u8) Error![]ArchiveSet {
    const a = env.arena;
    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    const sa = step.allocator();

    var dir = Io.Dir.cwd().openDir(env.io, dir_path, .{ .iterate = true }) catch |err| {
        return errors.fail(a, env.diag, error.Internal, "detect archives: open {s}: {s}", .{ dir_path, @errorName(err) });
    };
    defer dir.close(env.io);
    const names = ufs.regularNames(env, sa, dir) catch |err| switch (err) {
        error.Canceled => return error.Canceled,
        error.OutOfMemory => return error.OutOfMemory,
        else => return errors.fail(a, env.diag, error.Internal, "detect archives: read {s}: {s}", .{ dir_path, @errorName(err) }),
    };

    var sets: std.ArrayList(ArchiveSet) = .empty;
    var groups: std.StringArrayHashMapUnmanaged(*Group) = .empty;

    for (names) |name| {
        try env.io.checkCancel();
        const p = try fsutil.join(a, &.{ dir_path, name });
        if (matchSevenZ(name)) |m| {
            // Volumes after the first have no magic of their own.
            const num = atoi(m.digits) orelse 0;
            if (num == 1 and sniff(env, p) != .seven_z) continue;
            const g = try groupFor(a, sa, &groups, .seven_z, m.base);
            try g.add(sa, num, p, num == 1);
            continue;
        }
        const format = sniff(env, p) orelse continue;
        if (format == .seven_z) {
            try sets.append(a, .{ .format = .seven_z, .name = try a.dupe(u8, trimExt(name)), .volumes = try dupeOne(a, p) });
            continue;
        }
        if (matchNewStyle(name)) |m| {
            const num = atoi(m.digits) orelse -1;
            const g = try groupFor(a, sa, &groups, .rar_new, m.base);
            try g.add(sa, num, p, true);
        } else if (matchOldFirst(name)) |base| {
            const g = try groupFor(a, sa, &groups, .rar_old, base);
            try g.add(sa, 0, p, true);
        } else if (matchOldCont(name)) |m| {
            const nn = atoi(m.digits).?;
            const letter = lowerLatin1(m.letter[0]);
            const g = try groupFor(a, sa, &groups, .rar_old, m.base);
            try g.add(sa, (@as(i64, letter) - 'r') * 100 + nn + 1, p, true);
        } else {
            // Obfuscated name: a single-volume set of its own.
            try sets.append(a, .{ .format = .rar, .name = try a.dupe(u8, name), .volumes = try dupeOne(a, p) });
        }
    }
    for (groups.values()) |g| {
        if (try g.toSet(a, sa)) |s| try sets.append(a, s);
    }

    const sortable = try sa.alloc(Sortable, sets.items.len);
    for (sets.items, sortable) |s, *o| o.* = .{ .set = s, .key = try name_mod.lowerAlloc(sa, s.name) };
    std.mem.sort(Sortable, sortable, {}, lessThan);
    const out = try a.alloc(ArchiveSet, sortable.len);
    for (sortable, out) |s, *o| o.* = s.set;
    return out;
}

/// unicode.ToLower of a Latin-1 code point, for the letter of an old-style
/// volume extension (Go converts the first byte of the match to a rune).
fn lowerLatin1(b: u8) u8 {
    if ((b >= 'A' and b <= 'Z') or (b >= 0xc0 and b <= 0xde and b != 0xd7)) return b + 0x20;
    return b;
}

fn dupeOne(a: Allocator, p: []const u8) Allocator.Error![]const []const u8 {
    const out = try a.alloc([]const u8, 1);
    out[0] = p;
    return out;
}

fn groupFor(a: Allocator, sa: Allocator, groups: *std.StringArrayHashMapUnmanaged(*Group), fam: Family, name: []const u8) Allocator.Error!*Group {
    const lowered = try name_mod.lowerAlloc(sa, name);
    const key = try std.fmt.allocPrint(sa, "{d}/{s}", .{ @backingInt(fam), lowered });
    const gop = try groups.getOrPut(sa, key);
    if (!gop.found_existing) {
        const g = try sa.create(Group);
        g.* = .{ .fam = fam, .name = try a.dupe(u8, name) };
        gop.value_ptr.* = g;
    }
    return gop.value_ptr.*;
}
