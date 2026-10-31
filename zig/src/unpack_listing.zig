//! The listing of an archive as `7zz l -slt -ba` prints it (internal/postproc/
//! unpack listing.go and entry.go): one block of "Key = Value" lines per
//! entry, blocks separated by an empty line, every block starting with its
//! "Path" line.
//!
//! The output is attacker-influenced, because entry names and comments are
//! printed into it. The parser is therefore strict about the shape: after the
//! first block starts, a line that is not "Key = Value" or "Path = ...", and
//! a key that appears twice in one block, fail the listing with
//! `error.Corrupt`. A name or comment that contains a line break, which could
//! forge fields of the entry (a smaller "Size", a missing "Encrypted"), is
//! caught by that rule. Lines before the first block (the "Enter password:"
//! prompt, which goes to stdout) are ignored.
//!
//! Entries are slices of the listing buffer: it must outlive them.
const std = @import("std");
const Allocator = std.mem.Allocator;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const name_mod = @import("unpack_name.zig");
const fsutil_name = @import("fsutil_name.zig");

/// The type of an archive entry. Only files and directories are ever
/// extracted; every other kind fails the Unpack. The numbering is the one of
/// the Go constants (the golden files use it).
pub const Kind = enum(u8) {
    file,
    dir,
    symlink,
    hardlink,
    device,
    fifo,
    other,

    pub fn text(k: Kind) []const u8 {
        return switch (k) {
            .file => "file",
            .dir => "directory",
            .symlink => "symlink",
            .hardlink => "hardlink",
            .device => "device",
            .fifo => "fifo",
            .other => "special entry",
        };
    }
};

/// One archive member as the listing describes it. Everything about it is
/// hostile until the guards have looked at it.
pub const Entry = struct {
    name: []const u8,
    kind: Kind = .file,
    /// The declared uncompressed size, zero for directories.
    size: i64 = 0,
    /// The content (or the name) is encrypted.
    encrypted: bool = false,
};

/// The largest size a listing may declare for one entry; more is a lie.
const max_declared_size: i64 = 1 << 62;
const max_key_bytes = 64;

const Fields = struct {
    path: []const u8,
    size: ?[]const u8 = null,
    attributes: ?[]const u8 = null,
    encrypted: ?[]const u8 = null,
    folder: ?[]const u8 = null,
    symbolic_link: ?[]const u8 = null,
    hard_link: ?[]const u8 = null,
    link: ?[]const u8 = null,
};

/// Parses a listing. `scratch` holds the entries and a set of the keys of
/// the entry being read. At most `keep` entries are returned, but the whole
/// output is checked: the guard stops at the entry that exceeds the cap
/// anyway, so a listing with millions of entries costs no more memory than
/// the cap allows (the caller passes max_entries + 1).
pub fn parse(env: *Env, scratch: Allocator, out: []const u8, keep: usize) Error![]Entry {
    var entries: std.ArrayList(Entry) = .empty;
    var seen: std.StringHashMapUnmanaged(void) = .empty;
    var cur: ?Fields = null;
    var total: usize = 0;

    var it = std.mem.splitScalar(u8, out, '\n');
    while (it.next()) |raw_line| {
        // The text after the last newline is a line only when it is not empty.
        if (it.peek() == null and raw_line.len == 0) break;
        const s = if (std.mem.endsWith(u8, raw_line, "\r")) raw_line[0 .. raw_line.len - 1] else raw_line;
        if (s.len == 0) {
            try finish(env, scratch, &entries, &cur, &total, keep);
            continue;
        }
        if (std.mem.startsWith(u8, s, "Path = ")) {
            if (cur != null) return corrupt(env, "listing has a second Path line inside one entry", .{});
            cur = .{ .path = s["Path = ".len..] };
            seen.clearRetainingCapacity();
            try seen.put(scratch, "Path", {});
            continue;
        }
        if (cur == null) {
            if (total == 0) continue; // prompt or banner before the first entry
            return corrupt(env, "unexpected output after the entries: {f}", .{try quotedTrunc(env, s)});
        }
        var key: []const u8 = undefined;
        var val: []const u8 = undefined;
        if (std.mem.indexOf(u8, s, " = ")) |i| {
            key = s[0..i];
            val = s[i + 3 ..];
        } else if (std.mem.endsWith(u8, s, " =")) {
            key = s[0 .. s.len - 2];
            val = "";
        } else {
            return corrupt(env, "malformed listing line {f}", .{try quotedTrunc(env, s)});
        }
        if (key.len == 0 or key.len > max_key_bytes) {
            return corrupt(env, "malformed listing line {f}", .{try quotedTrunc(env, s)});
        }
        const gop = try seen.getOrPut(scratch, key);
        if (gop.found_existing) {
            return corrupt(env, "listing repeats the field {f} within one entry", .{try quotedTrunc(env, key)});
        }
        const f = &cur.?;
        if (std.mem.eql(u8, key, "Size")) {
            f.size = val;
        } else if (std.mem.eql(u8, key, "Attributes")) {
            f.attributes = val;
        } else if (std.mem.eql(u8, key, "Encrypted")) {
            f.encrypted = val;
        } else if (std.mem.eql(u8, key, "Folder")) {
            f.folder = val;
        } else if (std.mem.eql(u8, key, "Symbolic Link")) {
            f.symbolic_link = val;
        } else if (std.mem.eql(u8, key, "Hard Link")) {
            f.hard_link = val;
        } else if (std.mem.eql(u8, key, "Link")) {
            f.link = val;
        }
    }
    try finish(env, scratch, &entries, &cur, &total, keep);
    return entries.items;
}

fn finish(env: *Env, scratch: Allocator, entries: *std.ArrayList(Entry), cur: *?Fields, total: *usize, keep: usize) Error!void {
    const f = cur.* orelse return;
    cur.* = null;
    const e = try entryFromFields(env, f);
    total.* += 1;
    if (entries.items.len < keep) try entries.append(scratch, e);
}

fn nonEmpty(v: ?[]const u8) bool {
    return v != null and v.?.len > 0;
}

fn entryFromFields(env: *Env, f: Fields) Error!Entry {
    var e: Entry = .{ .name = f.path, .encrypted = std.mem.eql(u8, f.encrypted orelse "", "+") };
    if (nonEmpty(f.size)) {
        const v = f.size.?;
        const n = std.fmt.parseInt(i64, v, 10) catch -1;
        // Go's ParseInt also refuses an underscore with an explicit base.
        if (n < 0 or n > max_declared_size or std.mem.indexOfScalar(u8, v, '_') != null) {
            return corrupt(env, "bad size {f} for {f}", .{ try quotedTrunc(env, v), try quotedTrunc(env, e.name) });
        }
        e.size = n;
    }
    e.kind = kindFromAttributes(f.attributes orelse "");
    if (std.mem.eql(u8, f.folder orelse "", "+") and e.kind == .file) e.kind = .dir;
    if (nonEmpty(f.symbolic_link)) e.kind = .symlink;
    if (nonEmpty(f.hard_link)) e.kind = .hardlink;
    if (nonEmpty(f.link) and e.kind != .hardlink) e.kind = .symlink;
    if (e.kind == .dir) e.size = 0;
    return e;
}

/// Reads the Attributes field: optional Windows attribute letters, then
/// optionally the Unix mode string.
///
///     "A -rw-r--r--"   file          "D drwxr-xr-x"  directory
///     " lrwxrwxrwx"    symlink       "A"             Windows file
///     "RAL"            reparse point (a Windows symlink or junction)
pub fn kindFromAttributes(attr: []const u8) Kind {
    var unix: []const u8 = "";
    var has_l = false;
    var has_d = false; // device
    var has_dir = false;
    var i: usize = 0;
    while (i < attr.len) {
        // Go's strings.FieldsSeq splits on unicode.IsSpace.
        while (i < attr.len) {
            const r = fsutil_name.decodeRune(attr, i);
            if (r.cp == null or !name_mod.isSpace(r.cp.?)) break;
            i += r.size;
        }
        const start = i;
        while (i < attr.len) {
            const r = fsutil_name.decodeRune(attr, i);
            if (r.cp != null and name_mod.isSpace(r.cp.?)) break;
            i += r.size;
        }
        if (i == start) break;
        const field = attr[start..i];
        if (isUnixMode(field)) {
            unix = field;
        } else {
            if (std.mem.indexOfScalar(u8, field, 'L') != null) has_l = true;
            if (std.mem.indexOfScalar(u8, field, 'd') != null) has_d = true;
            if (std.mem.indexOfScalar(u8, field, 'D') != null) has_dir = true;
        }
    }
    if (unix.len > 0) {
        return switch (unix[0]) {
            '-' => .file,
            'd' => .dir,
            'l' => .symlink,
            'c', 'b' => .device,
            'p' => .fifo,
            else => .other,
        };
    }
    if (has_l) return .symlink; // reparse point
    if (has_d) return .device;
    if (has_dir) return .dir;
    return .file;
}

/// An "ls -l" style mode such as "-rwxr-xr-x".
fn isUnixMode(s: []const u8) bool {
    if (s.len != 10 or std.mem.indexOfScalar(u8, "-dlcbpsh?", s[0]) == null) return false;
    for (s[1..]) |c| {
        if (std.mem.indexOfScalar(u8, "rwxsStT-", c) == null) return false;
    }
    return true;
}

fn corrupt(env: *Env, comptime fmt: []const u8, args: anytype) Error {
    return errors.fail(env.arena, env.diag, error.Corrupt, fmt, args);
}

fn quotedTrunc(env: *Env, s: []const u8) Error!@TypeOf(name_mod.quoted("")) {
    return name_mod.quoted(try name_mod.truncateForError(env.arena, s));
}
