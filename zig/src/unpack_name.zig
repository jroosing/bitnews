//! Names from archives (internal/postproc/unpack guard.go, the name part):
//! `cleanName` turns an entry name into a relative path whose every component
//! went through `fsutil.safeName`, `lowerAlloc` is Go's strings.ToLower (the
//! guard compares names with case ignored), and `truncateForError` keeps
//! hostile text out of messages and logs.
//!
//! Names are hostile, so every function here is bounded: a name longer than
//! `4 * max_path_bytes` is refused before it is looked at, and the result of
//! `cleanName` has at most `max_path_depth` components of 255 bytes.
const std = @import("std");
const Allocator = std.mem.Allocator;
const fsutil = @import("fsutil.zig");
const fsutil_name = @import("fsutil_name.zig");
const lower_table = @import("unpack_lower.zig");
const log_text = @import("log_text.zig");

/// A sanitized entry path is at most this many bytes and this many components
/// deep. Real releases stay far below both.
pub const max_path_bytes = 4096;
pub const max_path_depth = 128;

/// The result of `cleanName`: a safe relative path ("" means the archive root
/// itself, as for "./"), or why the name was refused.
pub const Clean = union(enum) {
    rel: []const u8,
    violation: []const u8,
};

/// Normalizes an archive entry name into a relative path with '/' separators
/// in which every component went through `fsutil.safeName`. It refuses
/// absolute paths, drive letters, "..", NUL bytes and empty names. The result
/// lives in `a`.
pub fn cleanName(a: Allocator, raw: []const u8) Allocator.Error!Clean {
    if (raw.len == 0) return .{ .violation = "empty name" };
    if (std.mem.indexOfScalar(u8, raw, 0) != null) return .{ .violation = "NUL byte in name" };
    if (raw.len > 4 * max_path_bytes) return .{ .violation = "name too long" };
    // A backslash separates, as in a Windows archive; the first byte decides
    // "absolute" after the replacement.
    if (raw[0] == '/' or raw[0] == '\\') return .{ .violation = "absolute path" };
    if (raw.len >= 2 and raw[1] == ':' and isAsciiLetter(raw[0])) return .{ .violation = "drive letter" };

    var comps: std.ArrayList([]const u8) = .empty;
    defer comps.deinit(a);
    var saw_dot = false;
    var total: usize = 0;
    var start: usize = 0;
    var i: usize = 0;
    while (i <= raw.len) : (i += 1) {
        if (i < raw.len and raw[i] != '/' and raw[i] != '\\') continue;
        const c = raw[start..i];
        start = i + 1;
        if (c.len == 0) continue;
        if (std.mem.eql(u8, c, ".")) {
            saw_dot = true;
            continue;
        }
        if (std.mem.eql(u8, c, "..")) return .{ .violation = "parent directory reference" };
        const safe = fsutil.safeName(a, c) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.InvalidName => return .{ .violation = "invalid name component" },
        };
        try comps.append(a, safe);
        total += safe.len + 1;
    }
    if (comps.items.len == 0) {
        if (saw_dot) return .{ .rel = "" };
        return .{ .violation = "empty name" };
    }
    if (comps.items.len > max_path_depth) return .{ .violation = "path too deep" };
    if (total - 1 > max_path_bytes) return .{ .violation = "path too long" };
    return .{ .rel = try std.mem.join(a, "/", comps.items) };
}

fn isAsciiLetter(b: u8) bool {
    return (b | 0x20) >= 'a' and (b | 0x20) <= 'z';
}

/// The text of a refused name, as Go's ErrPathViolation wrap prints it.
pub fn violationText(a: Allocator, why: []const u8, raw: []const u8) Allocator.Error![]u8 {
    const t = try truncateForError(a, raw);
    defer a.free(t);
    return std.fmt.allocPrint(a, "{s}: {s} {f}", .{ @import("unpack_errors.zig").prefix(error.PathViolation), why, quoted(t) });
}

/// Go's %q.
pub fn quoted(s: []const u8) std.fmt.Alt([]const u8, formatQuoted) {
    return .{ .data = s };
}

fn formatQuoted(s: []const u8, w: *std.Io.Writer) std.Io.Writer.Error!void {
    try log_text.writeQuoted(w, s);
}

/// Cuts a hostile string to 80 bytes plus "...", turns control characters
/// into '?' and every invalid UTF-8 byte into U+FFFD, as Go's
/// truncateForError (strings.Map) does. The result lives in `a`.
pub fn truncateForError(a: Allocator, s_in: []const u8) Allocator.Error![]u8 {
    var s = s_in;
    var tail: []const u8 = "";
    if (s.len > 80) {
        s = s[0..80];
        tail = "...";
    }
    var out: std.ArrayList(u8) = try .initCapacity(a, s.len * 3 + tail.len);
    errdefer out.deinit(a);
    var i: usize = 0;
    while (i < s.len) {
        const r = fsutil_name.decodeRune(s, i);
        defer i += r.size;
        const cp = r.cp orelse {
            out.appendSliceAssumeCapacity("\u{fffd}");
            continue;
        };
        if (cp < 0x20 or cp == 0x7f) {
            out.appendAssumeCapacity('?');
        } else {
            out.appendSliceAssumeCapacity(s[i..][0..r.size]);
        }
    }
    out.appendSliceAssumeCapacity(tail);
    return out.toOwnedSlice(a);
}

// ---- strings.ToLower ----

/// Go's unicode.ToLower for one code point.
pub fn lowerRune(cp: u21) u21 {
    if (cp < 0x80) return if (cp >= 'A' and cp <= 'Z') cp + 32 else cp;
    const runs = &lower_table.runs;
    var lo: usize = 0;
    var hi: usize = runs.len;
    while (lo < hi) {
        const mid = lo + (hi - lo) / 2;
        const r = runs[mid];
        if (cp < r.lo) {
            hi = mid;
        } else if (cp > r.hi) {
            lo = mid + 1;
        } else {
            if ((cp - r.lo) % r.stride != 0) return cp;
            return @intCast(@as(i32, cp) + r.delta);
        }
    }
    return cp;
}

/// Go's strings.ToLower: simple per-code-point lower-casing, with every
/// invalid UTF-8 byte becoming U+FFFD. The result lives in `a`.
pub fn lowerAlloc(a: Allocator, s: []const u8) Allocator.Error![]u8 {
    // A lower-case form is never more than 3 bytes per input byte (U+FFFD).
    var out: std.ArrayList(u8) = try .initCapacity(a, s.len);
    errdefer out.deinit(a);
    var i: usize = 0;
    while (i < s.len) {
        const r = fsutil_name.decodeRune(s, i);
        defer i += r.size;
        const cp = r.cp orelse {
            try out.appendSlice(a, "\u{fffd}");
            continue;
        };
        if (cp < 0x80) {
            try out.append(a, @intCast(lowerRune(cp)));
            continue;
        }
        const l = lowerRune(cp);
        var buf: [4]u8 = undefined;
        const n = std.unicode.utf8Encode(l, &buf) catch unreachable;
        try out.appendSlice(a, buf[0..n]);
    }
    return out.toOwnedSlice(a);
}

// ---- spaces ----

/// Go's unicode.IsSpace.
pub fn isSpace(cp: u21) bool {
    return switch (cp) {
        0x09...0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x2000...0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        else => false,
    };
}

/// Go's strings.TrimSpace.
pub fn trimSpace(s: []const u8) []const u8 {
    var start: usize = s.len;
    var end: usize = 0;
    var i: usize = 0;
    while (i < s.len) {
        const r = fsutil_name.decodeRune(s, i);
        if (r.cp == null or !isSpace(r.cp.?)) {
            if (start == s.len) start = i;
            end = i + r.size;
        }
        i += r.size;
    }
    if (end == 0) return "";
    return s[start..end];
}
