//! Clean.py's name cleaning (internal/postproc/tidy/clean.go, ADR-0009): the
//! suffixes and tags release bots add to an NZB's name, removed in the
//! script's order.
//!
//! Go does this with a chain of regular expressions, all of them anchored at
//! the end of the name and applied with (?i). Every step therefore removes a
//! suffix, so the result is always a prefix of the name: `clean` returns a
//! slice of its argument and allocates nothing. The matching is done by hand
//! and agrees with RE2 on bytes that are not UTF-8:
//!
//!   - a letter matches its case variants, and Go's simple case folding adds
//!     two non-ASCII ones: U+212A (the Kelvin sign) is a K and U+017F (long
//!     s) is an S, so "-NZBGeeK" still ends in "-NZBGeek";
//!   - "." and the classes match one rune, an invalid byte counting as one
//!     (U+FFFD) rune, and "." does not match a newline.
const std = @import("std");
const decodeRune = @import("fsutil_name.zig").decodeRune;

/// One substitution of the script. Most are a literal suffix.
const Step = union(enum) {
    /// A literal suffix, written in lower case.
    lit: []const u8,
    /// "-Rakuv[a-z0-9]*$"
    rakuv,
    /// "\[eztv([ ._-]re)?\]$"
    eztv,
    /// ".mkv-xpost$": the dot is a wildcard in the original too.
    any_then: []const u8,
};

// The patterns of Clean.py, in its order. Literals are lower case because
// matching ignores case.
const steps = [_]Step{
    .{ .lit = "-4p" },
    .{ .lit = "-4planet" },
    .{ .lit = "-alternativetorequested" },
    .{ .lit = "-altezachen" },
    .{ .lit = "-asrequested" },
    .{ .lit = "-asrequested-xpost" },
    .{ .lit = "-buymore" },
    .{ .lit = "-chamele0n" },
    .{ .lit = "-gerov" },
    .{ .lit = "-inc0gnito" },
    .{ .lit = "-nzbgeek" },
    .{ .lit = "-obfuscated" },
    .{ .lit = "-obfuscation" },
    .{ .lit = "-postbot" },
    .rakuv,
    .{ .lit = "-repackpost" },
    .{ .lit = "-scrambled" },
    .{ .lit = "-whiterev" },
    .{ .lit = "-wrteam" },
    .{ .lit = "-captcha" },
    .{ .lit = "-z0ids3n" },
    .eztv,
    .{ .lit = "[tgx]" },
    .{ .lit = "[ettv]" },
    .{ .lit = "[tgx]-xpost" },
    .{ .any_then = "mkv-xpost" },
    .{ .lit = "-xpost" },
};

/// The name without the suffixes and tags release bots add, as Clean.py does,
/// as a prefix of `name`. A name that would become empty is returned
/// unchanged.
pub fn clean(name: []const u8) []const u8 {
    var n = name.len;
    for (steps) |st| {
        if (matchStep(st, name[0..n])) |start| n = start;
    }
    n = trailingGroup(name[0..n]);
    if (n == 0) return name;
    return name[0..n];
}

/// Where the match of `st` at the end of `s` starts, if there is one.
fn matchStep(st: Step, s: []const u8) ?usize {
    return switch (st) {
        .lit => |l| backLit(s, s.len, l),
        .rakuv => rakuv(s),
        .eztv => eztv(s),
        .any_then => |l| blk: {
            const e = backLit(s, s.len, l) orelse break :blk null;
            break :blk backAny(s, e);
        },
    };
}

// ---- case-insensitive literals ----

const kelvin = "\xe2\x84\xaa"; // U+212A
const long_s = "\xc5\xbf"; // U+017F

/// Matches the lower case literal `lit` ending at `end`, case-insensitively,
/// and returns where it starts.
fn backLit(s: []const u8, end: usize, lit: []const u8) ?usize {
    var e = end;
    var i = lit.len;
    while (i > 0) {
        i -= 1;
        const c = lit[i];
        if (e == 0) return null;
        const b = s[e - 1];
        if (b < 0x80) {
            if (std.ascii.toLower(b) != c) return null;
            e -= 1;
        } else if (c == 'k' and std.mem.endsWith(u8, s[0..e], kelvin)) {
            e -= kelvin.len;
        } else if (c == 's' and std.mem.endsWith(u8, s[0..e], long_s)) {
            e -= long_s.len;
        } else return null;
    }
    return e;
}

/// Matches the lower case literal `lit` starting at `start` and returns where
/// it ends.
fn fwdLit(s: []const u8, start: usize, lit: []const u8) ?usize {
    var p = start;
    for (lit) |c| {
        if (p >= s.len) return null;
        const b = s[p];
        if (b < 0x80) {
            if (std.ascii.toLower(b) != c) return null;
            p += 1;
        } else if (c == 'k' and std.mem.startsWith(u8, s[p..], kelvin)) {
            p += kelvin.len;
        } else if (c == 's' and std.mem.startsWith(u8, s[p..], long_s)) {
            p += long_s.len;
        } else return null;
    }
    return p;
}

// ---- runes ----

/// The start of the rune that ends at `end` (not a newline), as Go decodes
/// the name from its start: a complete valid sequence, else one byte. A
/// lead byte is always the start of a rune, so the backwards guess is the
/// forward truth.
fn backAny(s: []const u8, end: usize) ?usize {
    if (end == 0) return null;
    const b = s[end - 1];
    if (b < 0x80) return if (b == '\n') null else end - 1;
    var n: usize = 2;
    while (n <= 4 and n <= end) : (n += 1) {
        const lead = s[end - n];
        const len = std.unicode.utf8ByteSequenceLength(lead) catch continue;
        if (len != n) continue;
        _ = std.unicode.utf8Decode(s[end - n .. end]) catch continue;
        return end - n;
    }
    return end - 1;
}

/// "[a-z0-9]" with the case folding of RE2, going backwards from `end`.
fn classBack(s: []const u8, end: usize) ?usize {
    const b = s[end - 1];
    if (std.ascii.isAlphanumeric(b)) return end - 1;
    if (std.mem.endsWith(u8, s[0..end], kelvin)) return end - kelvin.len;
    if (std.mem.endsWith(u8, s[0..end], long_s)) return end - long_s.len;
    return null;
}

// ---- the steps that are not a literal ----

/// "-Rakuv[a-z0-9]*$": the class has no '-', so the match starts at the '-'
/// before the last run of class characters, and that run must begin with
/// "rakuv".
fn rakuv(s: []const u8) ?usize {
    var i = s.len;
    while (i > 0) i = classBack(s, i) orelse break;
    if (i == 0 or s[i - 1] != '-') return null;
    _ = fwdLit(s, i, "rakuv") orelse return null;
    return i - 1;
}

fn eztv(s: []const u8) ?usize {
    if (s.len == 0 or s[s.len - 1] != ']') return null;
    const e = s.len - 1;
    // With the optional separator and "re": "[eztv-re]".
    if (e >= 3 and std.mem.indexOfScalar(u8, " ._-", s[e - 3]) != null) {
        if (backLit(s, e, "re") == e - 2) {
            if (backLit(s, e - 3, "[eztv")) |start| return start;
        }
    }
    return backLit(s, e, "[eztv");
}

// ---- the group name ----

/// The script's last step: after a group name (or the literal -D-Z0N3), a
/// further "-" and exactly four characters are removed. The pattern is
/// "(-D-Z0N3|-[^-.\n]*)(-.{4})?$", replaced by its first group. It matches at
/// the leftmost '-' from which the rest of the name fits; this returns where
/// the group ends, which is the length that stays.
fn trailingGroup(s: []const u8) usize {
    var from: usize = 0;
    while (std.mem.indexOfScalarPos(u8, s, from, '-')) |p| : (from = p + 1) {
        // The first alternative has priority.
        if (fwdLit(s, p, "-d-z0n3")) |e| {
            if (tailFits(s, e)) return e;
        }
        var q = p + 1;
        while (q < s.len) {
            const r = decodeRune(s, q);
            if (r.cp) |cp| {
                if (cp == '-' or cp == '.' or cp == '\n') break;
            }
            q += r.size;
        }
        if (tailFits(s, q)) return q;
    }
    return s.len;
}

/// Whether the rest of `s` from `q` is empty or "-" and four more runes (any
/// but a newline).
fn tailFits(s: []const u8, q: usize) bool {
    if (q == s.len) return true;
    if (s[q] != '-') return false;
    var i = q + 1;
    var k: usize = 0;
    while (k < 4) : (k += 1) {
        if (i >= s.len) return false;
        const r = decodeRune(s, i);
        if (r.cp != null and r.cp.? == '\n') return false;
        i += r.size;
    }
    return i == s.len;
}
