//! Text helpers shared by the logger and the configuration errors: Go's
//! strconv.Quote, slog's quoting rule, JSON string escaping, float and
//! Duration formatting. All of it matches what Go's log/slog and strconv
//! print (see the expected strings in log.zig's tests, taken from a real Go
//! program), so logs and messages are identical to the Go version.
const std = @import("std");
const Writer = std.Io.Writer;

const hex = "0123456789abcdef";

const Rune = struct {
    /// The code point, or null for an invalid byte (Go's RuneError, width 1).
    cp: ?u21,
    size: u3,
};

fn decodeRune(s: []const u8, i: usize) Rune {
    const len = std.unicode.utf8ByteSequenceLength(s[i]) catch return .{ .cp = null, .size = 1 };
    if (i + len > s.len) return .{ .cp = null, .size = 1 };
    const cp = std.unicode.utf8Decode(s[i..][0..len]) catch return .{ .cp = null, .size = 1 };
    return .{ .cp = cp, .size = len };
}

/// Go's unicode.IsSpace for code points above ASCII.
fn isSpaceRune(cp: u21) bool {
    return switch (cp) {
        0x09...0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x2000...0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000 => true,
        else => false,
    };
}

/// An approximation of Go's unicode.IsPrint without the Unicode tables:
/// controls, spaces, format characters, private use and noncharacters are
/// not printable, everything else is. Unassigned code points count as
/// printable, which only matters for whether a rare string is quoted.
fn isPrintRune(cp: u21) bool {
    if (cp < 0x20 or cp == 0x7f) return false;
    if (cp < 0x7f) return true;
    return switch (cp) {
        0x80...0xa0, 0xad, 0x600...0x605, 0x61c, 0x6dd, 0x70f, 0x1680, 0x180e => false,
        0x2000...0x200f, 0x2028...0x202f, 0x205f...0x206f, 0x3000 => false,
        0xe000...0xf8ff, 0xfeff, 0xfff9...0xfffb, 0xfffe, 0xffff => false,
        0xe0000...0xe0fff, 0xf0000...0x10ffff => false,
        else => true,
    };
}

/// Go's strconv.Quote: a double-quoted string with Go escapes. Invalid
/// UTF-8 bytes become \xNN, non-printable code points \uNNNN or \UNNNNNNNN.
pub fn writeQuoted(w: *Writer, s: []const u8) Writer.Error!void {
    try w.writeByte('"');
    var i: usize = 0;
    while (i < s.len) {
        const r = decodeRune(s, i);
        defer i += r.size;
        const cp = r.cp orelse {
            try writeHexEscape(w, s[i]);
            continue;
        };
        switch (cp) {
            '"' => try w.writeAll("\\\""),
            '\\' => try w.writeAll("\\\\"),
            0x07 => try w.writeAll("\\a"),
            0x08 => try w.writeAll("\\b"),
            0x0c => try w.writeAll("\\f"),
            '\n' => try w.writeAll("\\n"),
            '\r' => try w.writeAll("\\r"),
            '\t' => try w.writeAll("\\t"),
            0x0b => try w.writeAll("\\v"),
            else => if (isPrintRune(cp)) {
                try w.writeAll(s[i..][0..r.size]);
            } else if (cp < 0x20 or cp == 0x7f) {
                try writeHexEscape(w, @intCast(cp));
            } else if (cp < 0x10000) {
                try w.print("\\u{x:0>4}", .{cp});
            } else {
                try w.print("\\U{x:0>8}", .{cp});
            },
        }
    }
    try w.writeByte('"');
}

fn writeHexEscape(w: *Writer, b: u8) Writer.Error!void {
    try w.writeAll(&.{ '\\', 'x', hex[b >> 4], hex[b & 15] });
}

/// slog's text handler rule: a string is quoted when it is empty or holds a
/// space, '=', '"', a control character, invalid UTF-8, or a non-printable
/// or space code point. A backslash alone does not force quoting.
pub fn needsQuoting(s: []const u8) bool {
    if (s.len == 0) return true;
    var i: usize = 0;
    while (i < s.len) {
        const b = s[i];
        if (b < 0x80) {
            if (b == ' ' or b == '=' or b == '"' or b < 0x20) return true;
            i += 1;
            continue;
        }
        const r = decodeRune(s, i);
        const cp = r.cp orelse return true;
        if (isSpaceRune(cp) or !isPrintRune(cp)) return true;
        i += r.size;
    }
    return false;
}

/// Writes `s` as slog's text handler does: bare when it is safe, else quoted.
pub fn writeTextString(w: *Writer, s: []const u8) Writer.Error!void {
    if (needsQuoting(s)) return writeQuoted(w, s);
    try w.writeAll(s);
}

/// Writes `s` as a JSON string the way slog's JSON handler does: quotes,
/// backslash, \n, \r and \t escaped short, other control characters and
/// U+2028/U+2029 as \uNNNN, invalid UTF-8 as U+FFFD. HTML characters and
/// DEL are not escaped.
pub fn writeJsonString(w: *Writer, s: []const u8) Writer.Error!void {
    try w.writeByte('"');
    var i: usize = 0;
    while (i < s.len) {
        const b = s[i];
        if (b < 0x80) {
            i += 1;
            switch (b) {
                '"' => try w.writeAll("\\\""),
                '\\' => try w.writeAll("\\\\"),
                '\n' => try w.writeAll("\\n"),
                '\r' => try w.writeAll("\\r"),
                '\t' => try w.writeAll("\\t"),
                0...8, 0x0b, 0x0c, 0x0e...0x1f => try w.print("\\u{x:0>4}", .{b}),
                else => try w.writeByte(b),
            }
            continue;
        }
        const r = decodeRune(s, i);
        defer i += r.size;
        const cp = r.cp orelse {
            try w.writeAll("\u{fffd}");
            continue;
        };
        if (cp == 0x2028 or cp == 0x2029) {
            try w.print("\\u{x:0>4}", .{cp});
        } else {
            try w.writeAll(s[i..][0..r.size]);
        }
    }
    try w.writeByte('"');
}

/// The shortest decimal digits of a finite, non-zero float: the value is
/// 0.DIGITS * 10^dp, as in Go's strconv.
const Decimal = struct {
    buf: [24]u8 = undefined,
    nd: usize = 0,
    dp: i32 = 0,

    fn digits(d: *const Decimal) []const u8 {
        return d.buf[0..d.nd];
    }
};

fn decompose(abs: f64) Decimal {
    var d: Decimal = .{};
    if (abs == 0) return d;
    var tmp: [std.fmt.float.min_buffer_size]u8 = undefined;
    // The buffer is the documented minimum, so render cannot fail.
    const s = std.fmt.float.render(&tmp, abs, .{}) catch unreachable;
    const e = std.mem.indexOfScalar(u8, s, 'e').?;
    for (s[0..e]) |c| {
        if (c == '.') continue;
        d.buf[d.nd] = c;
        d.nd += 1;
    }
    d.dp = (std.fmt.parseInt(i32, s[e + 1 ..], 10) catch unreachable) + 1;
    return d;
}

/// Go's %e with shortest digits: d[.ddd]e+XX, at least two exponent digits.
fn writeExp(w: *Writer, d: Decimal, clean_exp: bool) Writer.Error!void {
    const ds = d.digits();
    try w.writeByte(ds[0]);
    if (ds.len > 1) {
        try w.writeByte('.');
        try w.writeAll(ds[1..]);
    }
    var exp = d.dp - 1;
    try w.writeByte('e');
    if (exp < 0) {
        try w.writeByte('-');
        exp = -exp;
    } else {
        try w.writeByte('+');
    }
    // encoding/json turns e-09 into e-9 but leaves e+21 alone.
    if (exp < 10 and !(clean_exp and d.dp - 1 < 0)) try w.writeByte('0');
    try w.print("{d}", .{exp});
}

/// Go's %f with shortest digits: no exponent, no trailing zeros.
fn writeFixed(w: *Writer, d: Decimal) Writer.Error!void {
    const ds = d.digits();
    if (d.dp > 0) {
        const dp: usize = @intCast(d.dp);
        var i: usize = 0;
        while (i < dp) : (i += 1) try w.writeByte(if (i < ds.len) ds[i] else '0');
    } else {
        try w.writeByte('0');
    }
    const frac: i32 = @max(@as(i32, @intCast(ds.len)) - d.dp, 0);
    if (frac > 0) {
        try w.writeByte('.');
        var i: i32 = 0;
        while (i < frac) : (i += 1) {
            const j = d.dp + i;
            try w.writeByte(if (j >= 0 and j < ds.len) ds[@intCast(j)] else '0');
        }
    }
}

/// strconv.FormatFloat(f, 'g', -1, 64), which is what slog's text handler
/// prints: the exponent form for exponents below -4 or from 6 up.
pub fn writeFloatText(w: *Writer, f: f64) Writer.Error!void {
    if (std.math.isNan(f)) return w.writeAll("NaN");
    if (std.math.isInf(f)) return w.writeAll(if (f > 0) "+Inf" else "-Inf");
    if (std.math.signbit(f)) try w.writeByte('-');
    const d = decompose(@abs(f));
    if (d.nd == 0) return w.writeByte('0');
    const exp = d.dp - 1;
    if (exp < -4 or exp >= 6) return writeExp(w, d, false);
    try writeFixed(w, d);
}

/// What encoding/json prints for a float64, which is what slog's JSON
/// handler does. NaN and infinities are not JSON numbers: slog writes an
/// error string in their place, and so do we.
pub fn writeFloatJson(w: *Writer, f: f64) Writer.Error!void {
    if (std.math.isNan(f)) return w.writeAll("\"!ERROR:json: unsupported value: NaN\"");
    if (std.math.isInf(f)) return w.writeAll(if (f > 0) "\"!ERROR:json: unsupported value: +Inf\"" else "\"!ERROR:json: unsupported value: -Inf\"");
    if (std.math.signbit(f)) try w.writeByte('-');
    const abs = @abs(f);
    const d = decompose(abs);
    if (d.nd == 0) return w.writeByte('0');
    if (abs < 1e-6 or abs >= 1e21) return writeExp(w, d, true);
    try writeFixed(w, d);
}

/// Go's time.Duration.String, for example 1.5s, 1m0s, 1h2m3.5s, 150ms.
pub fn writeDuration(w: *Writer, ns: i64) Writer.Error!void {
    if (ns == 0) return w.writeAll("0s");
    if (ns < 0) try w.writeByte('-');
    // Negating minInt(i64) overflows, so go through the unsigned value.
    const abs: u64 = if (ns < 0) @as(u64, @intCast(-(ns + 1))) + 1 else @intCast(ns);

    if (abs < std.time.ns_per_s) {
        // Sub-second: the largest of ns, µs, ms that gives a whole part.
        if (abs < std.time.ns_per_us) {
            try w.print("{d}ns", .{abs});
        } else if (abs < std.time.ns_per_ms) {
            try writeWithFraction(w, abs, 3);
            try w.writeAll("µs");
        } else {
            try writeWithFraction(w, abs, 6);
            try w.writeAll("ms");
        }
        return;
    }
    const secs = abs / std.time.ns_per_s;
    const mins = secs / 60;
    const hours = mins / 60;
    if (hours > 0) try w.print("{d}h", .{hours});
    if (mins > 0) try w.print("{d}m", .{mins % 60});
    try writeWithFraction(w, abs % (60 * std.time.ns_per_s), 9);
    try w.writeByte('s');
}

/// Writes `v / 10^prec` with the fraction's trailing zeros dropped.
fn writeWithFraction(w: *Writer, v: u64, prec: u6) Writer.Error!void {
    var div: u64 = 1;
    for (0..prec) |_| div *= 10;
    try w.print("{d}", .{v / div});
    var frac = v % div;
    if (frac == 0) return;
    var digits: [9]u8 = undefined;
    var i: usize = prec;
    while (i > 0) {
        i -= 1;
        digits[i] = '0' + @as(u8, @intCast(frac % 10));
        frac /= 10;
    }
    var n: usize = prec;
    while (digits[n - 1] == '0') n -= 1;
    try w.writeByte('.');
    try w.writeAll(digits[0..n]);
}
