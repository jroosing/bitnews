//! A strict, bounded XML tokenizer for the NZB subset, standing in for Go's
//! encoding/xml in strict mode (internal/usenet/nzb/parse.go). It accepts and
//! refuses what that decoder does, because the Go parser is the specification:
//!   - elements, attributes (quoted values only), character data, CDATA,
//!     comments, processing instructions and directives are read; a
//!     directive (DOCTYPE and the like) is skipped without being interpreted,
//!     so no entity it defines is ever expanded and nothing outside the input
//!     is read. Only the five predefined entities and numeric character
//!     references are known; any other reference is an error;
//!   - end tags must match their start tags (prefix and local name), and
//!     nothing is checked about namespaces: prefixes are only split off;
//!   - character data is checked to be valid UTF-8 made of XML characters;
//!   - `<?xml version encoding?>`: version other than 1.0 and any encoding
//!     other than UTF-8 and ISO-8859-1 are errors; ISO-8859-1 transcodes the
//!     rest of the input (every time such a declaration is met, like Go).
//! Differences from Go, all in the direction of a hard cap: elements nested
//! deeper than `max_depth` are an error.
//!
//! Tokens point into the input (or into its transcoded copy); nothing is
//! decoded until `appendDecoded` is asked to. Memory is O(depth) plus the
//! transcoded copy, so it is bounded by the input size.
const std = @import("std");
const Allocator = std.mem.Allocator;
const tables = @import("nzb_xml_tables.zig");
const trimSpace = @import("nzb_name.zig").trimSpace;

/// Open elements at once. Go has no limit but the input size; a real NZB is
/// four deep. Above the 1 000 000 levels Go's own test nests.
pub const max_depth: usize = 1 << 20;

pub const Error = error{
    /// Not well-formed (or not accepted); the details are in `Tokenizer.diag`.
    Syntax,
    OutOfMemory,
};

pub const Diag = struct {
    /// The line of an XML syntax error; 0 for an error with no line (an
    /// unsupported version or encoding).
    line: u32 = 0,
    reason: []const u8 = "",
};

pub const Text = struct {
    raw: []const u8,
    /// A CDATA section: no references are expanded.
    cdata: bool,
};

pub const Start = struct {
    /// The element name without its namespace prefix.
    local: []const u8,
    /// The attribute part of the tag, already checked.
    attrs: []const u8,

    /// The raw (not yet decoded) value of the first attribute whose local
    /// name (without prefix) is `name`, like Go's attr helper in parse.go.
    pub fn attr(s: Start, name: []const u8) ?[]const u8 {
        const a = s.attrs;
        var i: usize = 0;
        while (true) {
            while (i < a.len and isSpace(a[i])) i += 1;
            if (i >= a.len) return null;
            const name_start = i;
            while (i < a.len and (a[i] >= 0x80 or isNameByte(a[i]))) i += 1;
            const local = splitName(a[name_start..i]).local;
            while (i < a.len and isSpace(a[i])) i += 1;
            i += 1; // '='
            while (i < a.len and isSpace(a[i])) i += 1;
            const quote = a[i];
            const end = std.mem.indexOfScalarPos(u8, a, i + 1, quote) orelse return null;
            if (std.mem.eql(u8, local, name)) return a[i + 1 .. end];
            i = end + 1;
        }
    }
};

pub const Token = union(enum) {
    start: Start,
    /// An end tag, or the end of a self-closing element. It is already
    /// matched with its start.
    end,
    text: Text,
    /// A processing instruction, comment or directive.
    other,
};

pub const Tokenizer = struct {
    alloc: Allocator,
    src: []const u8,
    pos: usize = 0,
    /// The line at the start of `src` (it grows when `src` is replaced by a
    /// transcoded copy).
    line_base: u32 = 1,
    /// The most bytes a transcoded copy of the rest of the input may have.
    transcode_limit: usize,
    /// The names of the open elements, as written.
    stack: std.ArrayList([]const u8) = .empty,
    /// A self-closing element's end is still to be returned.
    pending_end: bool = false,
    diag: Diag = .{},

    /// `alloc` should be an arena: transcoded copies and the stack of open
    /// elements are kept until it is freed.
    pub fn init(alloc: Allocator, bytes: []const u8, transcode_limit: usize) Tokenizer {
        return .{ .alloc = alloc, .src = bytes, .transcode_limit = transcode_limit };
    }

    pub fn deinit(t: *Tokenizer) void {
        t.stack.deinit(t.alloc);
    }

    /// The next token, or null at the end of a complete document. Anything
    /// after the first top-level element is tokenized like the rest, as Go's
    /// decoder does (trailing elements are accepted when well-formed).
    pub fn next(t: *Tokenizer) Error!?Token {
        if (t.pending_end) {
            t.pending_end = false;
            return .end;
        }
        if (t.pos >= t.src.len) {
            if (t.stack.items.len > 0) return t.fail("unexpected EOF");
            return null;
        }
        if (t.src[t.pos] != '<') {
            const start = t.pos;
            t.pos = try t.scanText(start, 0);
            return .{ .text = .{ .raw = t.src[start..t.pos], .cdata = false } };
        }
        if (t.pos + 1 >= t.src.len) {
            t.pos = t.src.len;
            return t.fail("unexpected EOF");
        }
        switch (t.src[t.pos + 1]) {
            '/' => return try t.endTag(),
            '?' => return try t.procInst(),
            '!' => return try t.bang(),
            else => return try t.startTag(),
        }
    }

    fn fail(t: *Tokenizer, reason: []const u8) error{Syntax} {
        var line: u32 = t.line_base;
        for (t.src[0..@min(t.pos, t.src.len)]) |c| line += @intFromBool(c == '\n');
        t.diag = .{ .line = line, .reason = reason };
        return error.Syntax;
    }

    fn failNoLine(t: *Tokenizer, reason: []const u8) error{Syntax} {
        t.diag = .{ .line = 0, .reason = reason };
        return error.Syntax;
    }

    /// The byte at `pos`, which must exist.
    fn need(t: *Tokenizer) error{Syntax}!u8 {
        if (t.pos >= t.src.len) return t.fail("unexpected EOF");
        return t.src[t.pos];
    }

    fn skipSpace(t: *Tokenizer) void {
        while (t.pos < t.src.len and isSpace(t.src[t.pos])) t.pos += 1;
    }

    // -- names

    const Name = struct { full: []const u8, local: []const u8 };

    /// A name with an optional single namespace prefix; null when there is no
    /// name here or it has more than one colon.
    fn nsName(t: *Tokenizer) error{Syntax}!?Name {
        const full = (try t.name()) orelse return null;
        var colons: usize = 0;
        for (full) |c| colons += @intFromBool(c == ':');
        if (colons > 1) return null;
        return .{ .full = full, .local = splitName(full).local };
    }

    /// A name, or null when the next byte cannot start one.
    fn name(t: *Tokenizer) error{Syntax}!?[]const u8 {
        const start = t.pos;
        const first = try t.need();
        if (first < 0x80 and !isNameByte(first)) return null;
        t.pos += 1;
        while (true) {
            const b = try t.need();
            if (b < 0x80 and !isNameByte(b)) break;
            t.pos += 1;
        }
        const n = t.src[start..t.pos];
        if (!isName(n)) return t.fail("invalid XML name");
        return n;
    }

    // -- tags

    fn startTag(t: *Tokenizer) Error!Token {
        t.pos += 1; // '<'
        const nm = (try t.nsName()) orelse return t.fail("expected element name after <");
        const attrs_start = t.pos;
        var self_closing = false;
        var attrs_end: usize = undefined;
        while (true) {
            t.skipSpace();
            const b = try t.need();
            if (b == '/') {
                attrs_end = t.pos;
                t.pos += 1;
                if (try t.need() != '>') return t.fail("expected /> in element");
                t.pos += 1;
                self_closing = true;
                break;
            }
            if (b == '>') {
                attrs_end = t.pos;
                t.pos += 1;
                break;
            }
            _ = (try t.nsName()) orelse return t.fail("expected attribute name in element");
            t.skipSpace();
            if (try t.need() != '=') return t.fail("attribute name without = in element");
            t.pos += 1;
            t.skipSpace();
            const quote = try t.need();
            if (quote != '"' and quote != '\'') return t.fail("unquoted or missing attribute value in element");
            t.pos += 1;
            t.pos = (try t.scanText(t.pos, quote)) + 1;
        }
        if (self_closing) {
            t.pending_end = true;
        } else {
            if (t.stack.items.len >= max_depth) return t.fail("elements nested too deeply");
            try t.stack.append(t.alloc, nm.full);
        }
        return .{ .start = .{ .local = nm.local, .attrs = t.src[attrs_start..attrs_end] } };
    }

    fn endTag(t: *Tokenizer) Error!Token {
        t.pos += 2; // "</"
        const nm = (try t.nsName()) orelse return t.fail("expected element name after </");
        t.skipSpace();
        if (try t.need() != '>') return t.fail("invalid characters between end tag name and >");
        t.pos += 1;
        const top = t.stack.pop() orelse return t.fail("unexpected end element");
        if (!std.mem.eql(u8, top, nm.full)) return t.fail("element closed by a different end tag");
        return .end;
    }

    // -- processing instructions, comments, CDATA, directives

    fn procInst(t: *Tokenizer) Error!Token {
        t.pos += 2; // "<?"
        const target = (try t.name()) orelse return t.fail("expected target name after <?");
        t.skipSpace();
        const end = std.mem.indexOfPos(u8, t.src, t.pos, "?>") orelse {
            t.pos = t.src.len;
            return t.fail("unexpected EOF");
        };
        const data = t.src[t.pos..end];
        t.pos = end + 2;
        if (std.mem.eql(u8, target, "xml")) try t.xmlDecl(data);
        return .other;
    }

    /// The version and encoding of an XML declaration, like Go's rawToken.
    fn xmlDecl(t: *Tokenizer, data: []const u8) Error!void {
        const ver = procInstParam("version=", data);
        if (ver.len != 0 and !std.mem.eql(u8, ver, "1.0")) {
            return t.failNoLine("xml: unsupported version; only version 1.0 is supported");
        }
        const enc = procInstParam("encoding=", data);
        if (enc.len == 0 or std.ascii.eqlIgnoreCase(enc, "utf-8")) return;
        const label = trimSpace(enc);
        const supported = [_][]const u8{ "iso-8859-1", "iso8859-1", "latin1", "latin-1" };
        for (supported) |s| {
            if (std.ascii.eqlIgnoreCase(label, s)) return t.switchLatin1();
        }
        return t.failNoLine("xml: opening charset: unsupported encoding");
    }

    /// The rest of the input is ISO-8859-1: replace it by its UTF-8 text. Every
    /// byte maps to the code point of the same value, so it at most doubles.
    fn switchLatin1(t: *Tokenizer) Error!void {
        const rest = t.src[t.pos..];
        var high: usize = 0;
        for (rest) |c| high += @intFromBool(c >= 0x80);
        if (rest.len + high > t.transcode_limit) return t.failNoLine("xml: input too large after transcoding");
        const out = try t.alloc.alloc(u8, rest.len + high);
        var o: usize = 0;
        for (rest) |c| {
            if (c < 0x80) {
                out[o] = c;
                o += 1;
            } else {
                out[o] = 0xC0 | (c >> 6);
                out[o + 1] = 0x80 | (c & 0x3F);
                o += 2;
            }
        }
        for (t.src[0..t.pos]) |c| t.line_base += @intFromBool(c == '\n');
        t.src = out;
        t.pos = 0;
    }

    fn bang(t: *Tokenizer) Error!Token {
        const s = t.src;
        t.pos += 2; // "<!"
        const b = try t.need();
        switch (b) {
            '-' => {
                if (t.pos + 1 >= s.len) {
                    t.pos = s.len;
                    return t.fail("unexpected EOF");
                }
                if (s[t.pos + 1] != '-') return t.fail("invalid sequence <!- not part of <!--");
                // "--" inside a comment must end it.
                const dd = std.mem.indexOfPos(u8, s, t.pos + 2, "--") orelse {
                    t.pos = s.len;
                    return t.fail("unexpected EOF");
                };
                if (dd + 2 >= s.len) {
                    t.pos = s.len;
                    return t.fail("unexpected EOF");
                }
                if (s[dd + 2] != '>') {
                    t.pos = dd + 2;
                    return t.fail("invalid sequence \"--\" not allowed in comments");
                }
                t.pos = dd + 3;
                return .other;
            },
            '[' => {
                const open = "[CDATA[";
                if (s.len - t.pos < open.len) {
                    t.pos = s.len;
                    return t.fail("unexpected EOF");
                }
                if (!std.mem.eql(u8, s[t.pos..][0..open.len], open)) return t.fail("invalid <![ sequence");
                const start = t.pos + open.len;
                const end = std.mem.indexOfPos(u8, s, start, "]]>") orelse {
                    t.pos = s.len;
                    return t.fail("unexpected EOF in CDATA section");
                };
                t.pos = end;
                if (!charsOk(s[start..end])) return t.fail("invalid character in CDATA section");
                t.pos = end + 3;
                return .{ .text = .{ .raw = s[start..end], .cdata = true } };
            },
            else => {
                try t.directive();
                return .other;
            },
        }
    }

    /// <!DOCTYPE ...> and the like, up to the closing '>': quoted angle
    /// brackets do not count, nested <...> and comments are balanced. The
    /// first byte after "<!" is part of it whatever it is (so "<!>" does not
    /// end it), as in Go.
    fn directive(t: *Tokenizer) Error!void {
        const s = t.src;
        var j = t.pos + 1;
        var inquote: u8 = 0;
        var depth: i64 = 0;
        while (true) {
            if (j >= s.len) {
                t.pos = s.len;
                return t.fail("unexpected EOF");
            }
            var b = s[j];
            j += 1;
            if (inquote == 0 and b == '>' and depth == 0) break;
            while (true) { // Go's HandleB: a byte may be handled again
                if (b == inquote) {
                    inquote = 0;
                } else if (inquote != 0) {
                    // in quotes, nothing special
                } else if (b == '\'' or b == '"') {
                    inquote = b;
                } else if (b == '>') {
                    depth -= 1;
                } else if (b == '<') {
                    const comment = "!--";
                    var matched: usize = 0;
                    var again = false;
                    while (matched < comment.len) : (matched += 1) {
                        if (j >= s.len) {
                            t.pos = s.len;
                            return t.fail("unexpected EOF");
                        }
                        const c = s[j];
                        j += 1;
                        if (c != comment[matched]) {
                            depth += 1;
                            b = c;
                            again = true;
                            break;
                        }
                    }
                    if (again) continue;
                    const close = std.mem.indexOfPos(u8, s, j, "-->") orelse {
                        t.pos = s.len;
                        return t.fail("unexpected EOF");
                    };
                    j = close + 3;
                }
                break;
            }
        }
        t.pos = j;
    }

    // -- character data

    /// Checks the character data (or quoted attribute value, when `quote` is
    /// not 0) that starts at `from` and returns where it ends: at the '<' or
    /// end of input for data, at the closing quote for a value. References
    /// and characters are checked here; the text is decoded later.
    fn scanText(t: *Tokenizer, from: usize, quote: u8) error{Syntax}!usize {
        const s = t.src;
        var i = from;
        while (true) {
            while (i < s.len and byte_class[s[i]] == .plain) i += 1;
            if (i >= s.len) {
                t.pos = s.len;
                if (quote != 0) return t.fail("unexpected EOF");
                return s.len;
            }
            const b = s[i];
            switch (byte_class[b]) {
                .plain => unreachable,
                .quote => {
                    if (b == quote) return i;
                    i += 1;
                },
                .lt => {
                    if (quote == 0) return i;
                    t.pos = i;
                    return t.fail("unescaped < inside quoted string");
                },
                .gt => {
                    if (quote == 0 and i - from >= 2 and s[i - 1] == ']' and s[i - 2] == ']') {
                        t.pos = i;
                        return t.fail("unescaped ]]> not in CDATA section");
                    }
                    i += 1;
                },
                .amp => i = try t.entity(i),
                .bad => {
                    t.pos = i;
                    return t.fail("illegal character code");
                },
                .high => {
                    const n = std.unicode.utf8ByteSequenceLength(b) catch {
                        t.pos = i;
                        return t.fail("invalid UTF-8");
                    };
                    if (i + n > s.len) {
                        t.pos = i;
                        return t.fail("invalid UTF-8");
                    }
                    const cp = std.unicode.utf8Decode(s[i..][0..n]) catch {
                        t.pos = i;
                        return t.fail("invalid UTF-8");
                    };
                    if (!charOk(cp)) {
                        t.pos = i;
                        return t.fail("illegal character code");
                    }
                    i += n;
                },
            }
        }
    }

    /// Checks the reference that starts at `i` and returns the index after it.
    fn entity(t: *Tokenizer, i: usize) error{Syntax}!usize {
        const s = t.src;
        var j = i + 1;
        t.pos = j;
        if (j >= s.len) return t.fail("unexpected EOF");
        if (s[j] == '#') {
            j += 1;
            if (j >= s.len) return t.fail("unexpected EOF");
            var base: u32 = 10;
            if (s[j] == 'x') {
                base = 16;
                j += 1;
            }
            var value: u32 = 0;
            var digits: usize = 0;
            while (true) {
                if (j >= s.len) {
                    t.pos = j;
                    return t.fail("unexpected EOF");
                }
                const d = digitValue(s[j], base) orelse break;
                // Past the largest code point it cannot become valid; the
                // clamp keeps the arithmetic from overflowing.
                value = @min(value * base + d, 0x110000);
                digits += 1;
                j += 1;
            }
            if (s[j] != ';' or digits == 0 or value > 0x10FFFF) {
                t.pos = j;
                return t.fail("invalid character entity");
            }
            // A surrogate becomes U+FFFD, which is a valid XML character.
            const cp: u21 = if (value >= 0xD800 and value <= 0xDFFF) 0xFFFD else @intCast(value);
            if (!charOk(cp)) {
                t.pos = j;
                return t.fail("illegal character code");
            }
            return j + 1;
        }
        const name_start = j;
        while (j < s.len and (s[j] >= 0x80 or isNameByte(s[j]))) j += 1;
        if (j >= s.len) {
            t.pos = j;
            return t.fail("unexpected EOF");
        }
        if (s[j] != ';' or predefined(s[name_start..j]) == null) {
            t.pos = j;
            return t.fail("invalid character entity");
        }
        return j + 1;
    }
};

// -- decoding

pub const DecodeError = error{ TextTooLong, OutOfMemory };

/// Appends the text of `raw` (a checked text token, or an attribute value
/// when `cdata` is false) with references expanded and line ends turned into
/// "\n", as Go's decoder returns it. Fails with TextTooLong rather than let
/// `list` pass `limit` bytes.
pub fn appendDecoded(list: *std.ArrayList(u8), a: Allocator, raw: []const u8, cdata: bool, limit: usize) DecodeError!void {
    var i: usize = 0;
    while (i < raw.len) {
        var j = i;
        while (j < raw.len and raw[j] != '\r' and (cdata or raw[j] != '&')) j += 1;
        if (j > i) {
            if (list.items.len + (j - i) > limit) return error.TextTooLong;
            try list.appendSlice(a, raw[i..j]);
            i = j;
            continue;
        }
        if (list.items.len + 4 > limit) return error.TextTooLong;
        if (raw[i] == '\r') {
            // \r and \r\n are one \n.
            try list.append(a, '\n');
            i += 1;
            if (i < raw.len and raw[i] == '\n') i += 1;
        } else {
            // '&': the reference was checked when the token was scanned.
            const end = std.mem.indexOfScalarPos(u8, raw, i, ';').?;
            const ref = raw[i + 1 .. end];
            var buf: [4]u8 = undefined;
            const n = if (ref[0] == '#') blk: {
                const hex = ref.len > 1 and ref[1] == 'x';
                const v = std.fmt.parseInt(u32, ref[if (hex) 2 else 1..], if (hex) 16 else 10) catch unreachable;
                const cp: u21 = if (v >= 0xD800 and v <= 0xDFFF) 0xFFFD else @intCast(v);
                break :blk std.unicode.utf8Encode(cp, &buf) catch unreachable;
            } else blk: {
                buf[0] = predefined(ref).?;
                break :blk 1;
            };
            try list.appendSlice(a, buf[0..n]);
            i = end + 1;
        }
    }
}

/// The decoded text of `raw`: `raw` itself when nothing needs expanding,
/// else a copy in `scratch` (valid until the next use of `scratch`).
pub fn decodeValue(scratch: *std.ArrayList(u8), a: Allocator, raw: []const u8, limit: usize) DecodeError![]const u8 {
    if (raw.len > limit) return error.TextTooLong;
    if (std.mem.indexOfAny(u8, raw, "&\r") == null) return raw;
    scratch.clearRetainingCapacity();
    try appendDecoded(scratch, a, raw, false, limit);
    return scratch.items;
}

// -- character classes

const Class = enum(u8) { plain, quote, lt, gt, amp, bad, high };

const byte_class: [256]Class = blk: {
    var t: [256]Class = @splat(.plain);
    for (0..0x20) |c| {
        if (c != '\t' and c != '\n' and c != '\r') t[c] = .bad;
    }
    t['"'] = .quote;
    t['\''] = .quote;
    t['<'] = .lt;
    t['>'] = .gt;
    t['&'] = .amp;
    for (0x80..0x100) |c| t[c] = .high;
    break :blk t;
};

/// Whether `cp` is in the XML Character Range (the Char production).
fn charOk(cp: u21) bool {
    return cp == 0x09 or cp == 0x0A or cp == 0x0D or
        (cp >= 0x20 and cp <= 0xD7FF) or
        (cp >= 0xE000 and cp <= 0xFFFD) or
        (cp >= 0x10000 and cp <= 0x10FFFF);
}

/// Valid UTF-8 made of XML characters only.
fn charsOk(s: []const u8) bool {
    var i: usize = 0;
    while (i < s.len) {
        const b = s[i];
        if (b < 0x80) {
            if (byte_class[b] == .bad) return false;
            i += 1;
            continue;
        }
        const n = std.unicode.utf8ByteSequenceLength(b) catch return false;
        if (i + n > s.len) return false;
        const cp = std.unicode.utf8Decode(s[i..][0..n]) catch return false;
        if (!charOk(cp)) return false;
        i += n;
    }
    return true;
}

fn digitValue(b: u8, base: u32) ?u32 {
    return switch (b) {
        '0'...'9' => b - '0',
        'a'...'f' => if (base == 16) b - 'a' + 10 else null,
        'A'...'F' => if (base == 16) b - 'A' + 10 else null,
        else => null,
    };
}

fn predefined(name: []const u8) ?u8 {
    if (std.mem.eql(u8, name, "lt")) return '<';
    if (std.mem.eql(u8, name, "gt")) return '>';
    if (std.mem.eql(u8, name, "amp")) return '&';
    if (std.mem.eql(u8, name, "apos")) return '\'';
    if (std.mem.eql(u8, name, "quot")) return '"';
    return null;
}

fn isSpace(b: u8) bool {
    return b == ' ' or b == '\r' or b == '\n' or b == '\t';
}

fn isNameByte(c: u8) bool {
    return (c >= 'A' and c <= 'Z') or (c >= 'a' and c <= 'z') or (c >= '0' and c <= '9') or
        c == '_' or c == ':' or c == '.' or c == '-';
}

/// An XML name: a letter, '_' or ':' then name characters; non-ASCII
/// characters are looked up in Go's tables.
fn isName(s: []const u8) bool {
    if (s.len == 0) return false;
    var i: usize = 0;
    var is_first = true;
    while (i < s.len) {
        const b = s[i];
        if (b < 0x80) {
            const letter = (b >= 'A' and b <= 'Z') or (b >= 'a' and b <= 'z') or b == '_' or b == ':';
            if (!(letter or (!is_first and (isNameByte(b))))) return false;
            i += 1;
        } else {
            const n = std.unicode.utf8ByteSequenceLength(b) catch return false;
            if (i + n > s.len) return false;
            const cp = std.unicode.utf8Decode(s[i..][0..n]) catch return false;
            if (cp > 0xFFFF) return false;
            const c: u16 = @intCast(cp);
            if (!inTable(&tables.first, c) and (is_first or !inTable(&tables.second, c))) return false;
            i += n;
        }
        is_first = false;
    }
    return true;
}

fn inTable(table: []const tables.Range, c: u16) bool {
    var lo: usize = 0;
    var hi: usize = table.len;
    while (lo < hi) { // the last range that starts at or before c
        const mid = lo + (hi - lo) / 2;
        if (table[mid].lo <= c) lo = mid + 1 else hi = mid;
    }
    if (lo == 0) return false;
    const r = table[lo - 1];
    return c <= r.hi and (c - r.lo) % r.stride == 0;
}

/// Splits "prefix:local" when it has exactly one colon with text on both
/// sides; any other name is all local.
fn splitName(full: []const u8) struct { prefix: []const u8, local: []const u8 } {
    if (std.mem.indexOfScalar(u8, full, ':')) |c| {
        if (c > 0 and c + 1 < full.len and std.mem.indexOfScalarPos(u8, full, c + 1, ':') == null) {
            return .{ .prefix = full[0..c], .local = full[c + 1 ..] };
        }
    }
    return .{ .prefix = "", .local = full };
}

/// The value of `param` ("version=") in the data of a processing
/// instruction: a port of encoding/xml's lenient procInst.
fn procInstParam(param: []const u8, s: []const u8) []const u8 {
    var i: usize = 0;
    var sep: u8 = 0;
    while (i < s.len) {
        const sub = s[i..];
        const k = std.mem.indexOf(u8, sub, param) orelse return "";
        if (param.len + k >= sub.len) return "";
        i += param.len + k + 1;
        const c = sub[param.len + k];
        if (c == '\'' or c == '"') {
            sep = c;
            break;
        }
    }
    if (sep == 0) return "";
    const j = std.mem.indexOfScalar(u8, s[i..], sep) orelse return "";
    return s[i .. i + j];
}

// -- tests

const testing = std.testing;

fn collect(arena: Allocator, doc: []const u8) ![]const u8 {
    var tz = Tokenizer.init(arena, doc, doc.len * 2);
    var out: std.ArrayList(u8) = .empty;
    while (try tz.next()) |tok| switch (tok) {
        .start => |s| try out.print(arena, "<{s}>", .{s.local}),
        .end => try out.appendSlice(arena, "</>"),
        .text => |x| {
            try out.append(arena, '[');
            try appendDecoded(&out, arena, x.raw, x.cdata, 1 << 20);
            try out.append(arena, ']');
        },
        .other => try out.append(arena, '?'),
    };
    return out.items;
}

fn expectTokens(doc: []const u8, want: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqualStrings(want, try collect(arena.allocator(), doc));
}

fn expectSyntaxError(doc: []const u8) !void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectError(error.Syntax, collect(arena.allocator(), doc));
}

test "elements, attributes, text, comments and declarations" {
    try expectTokens("<?xml version=\"1.0\"?><a x='1'>hi<!-- c --><b/></a>", "?<a>[hi]?<b></></>");
    try expectTokens("<p:a xmlns:p=\"u\"><p:b></p:b></p:a>", "<a><b></></>");
}

test "predefined entities, character references and line ends" {
    try expectTokens("<a>&lt;&gt;&amp;&apos;&quot;&#65;&#x42;&#xD800;a\r\nb\rc</a>", "<a>[<>&'\"AB\u{FFFD}a\nb\nc]</>");
    try expectTokens("<a>&#13;</a>", "<a>[\r]</>");
}

test "CDATA is not expanded" {
    try expectTokens("<a><![CDATA[<&amp;>]]></a>", "<a>[<&amp;>]</>");
    try expectTokens("<a><![CDATA[]]></a>", "<a>[]</>");
}

test "a directive is skipped, entities it defines are not known" {
    try expectTokens("<!DOCTYPE a [ <!ENTITY x \"y>\"> <!-- > --> ]><a/>", "?<a></>");
    try expectSyntaxError("<!DOCTYPE a [ <!ENTITY x \"y\"> ]><a>&x;</a>");
    try expectSyntaxError("<!DOCTYPE a [ <!ENTITY x SYSTEM \"file:///etc/passwd\"> ]><a>&x;</a>");
    // "<!>" does not end a directive.
    try expectTokens("<!>x><a/>", "?<a></>");
}

test "malformed documents are refused" {
    const bad = [_][]const u8{
        "<a>",
        "<a></b>",
        "</a>",
        "<a></a></a>",
        "<a b=c></a>",
        "<a b></a>",
        "<a b='1\"></a>",
        "<a b='<'></a>",
        "<a>&bogus;</a>",
        "<a>&amp</a>",
        "<a>&#;</a>",
        "<a>&#xZ;</a>",
        "<a>&#1114112;</a>",
        "<a>&#0;</a>",
        "<a>&#xFFFE;</a>",
        "<a>]]></a>",
        "<a>\x01</a>",
        "<a>\xff</a>",
        "<a>\xed\xa0\x80</a>",
        "<a><!-- a -- b --></a>",
        "<a><!- x></a>",
        "<a><![CDAT[x]]></a>",
        "<a><![CDATA[x</a>",
        "<1a/>",
        "<a:b:c/>",
        "<?xml version=\"1.1\"?><a/>",
        "<?xml version=\"1.0\" encoding=\"shift_jis\"?><a/>",
        "<a",
        "<",
        "<a ",
        "<a b=",
        "<a b='x",
        "<a/ >",
        "<>",
    };
    for (bad) |doc| expectSyntaxError(doc) catch |e| {
        std.debug.print("accepted or wrong error: {s}\n", .{doc});
        return e;
    };
}

test "trailing content is tokenized like the rest" {
    try expectTokens("<a/><b/>text<!-- c -->", "<a></><b></>[text]?");
    try expectSyntaxError("<a/></a>");
}

test "names: prefixes, non-ASCII letters, ASCII digits at the start" {
    try expectTokens("<é/>", "<é></>");
    try expectTokens("<a.b-c_d:e/>", "<e></>");
    try expectTokens("<:a/>", "<:a></>");
    try expectSyntaxError("<-a/>");
    try expectSyntaxError("<a\u{2000}b/>");
}

test "ISO-8859-1 declarations transcode the rest of the input" {
    try expectTokens("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><a>Caf\xe9</a>", "?<a>[Café]</>");
    try expectTokens("<?xml encoding='latin1'?><a>\xe9</a>", "?<a>[é]</>");
    // UTF-8 bytes in a Latin-1 document are read as Latin-1 characters.
    try expectTokens("<?xml encoding='latin-1'?><a>\xc3\xa9</a>", "?<a>[Ã©]</>");
    // A second declaration transcodes again, as in Go.
    try expectTokens("<?xml encoding='latin1'?><?xml encoding='latin1'?><a>\xe9</a>", "??<a>[Ã©]</>");
}

test "the encoding label is trimmed of Unicode white space" {
    try expectTokens("<?xml encoding=\"\u{a0}latin1 \"?><a>\xe9</a>", "?<a>[é]</>");
}

test "a deep nesting is refused past the cap, not before" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const depth = max_depth;
    const open = try a.alloc(u8, depth * 3);
    for (0..depth) |i| @memcpy(open[i * 3 ..][0..3], "<x>");
    var tz = Tokenizer.init(a, open, 0);
    var n: usize = 0;
    while (tz.next()) |tok| {
        if (tok == null) break;
        n += 1;
    } else |e| try testing.expectEqual(error.Syntax, e);
    // all levels were read, then the missing end tags are an unexpected EOF
    try testing.expectEqual(depth, n);
    try testing.expectEqualStrings("unexpected EOF", tz.diag.reason);
    const more = try std.mem.concat(a, u8, &.{ open, "<x>" });
    var tz2 = Tokenizer.init(a, more, 0);
    var m: usize = 0;
    while (tz2.next()) |tok| {
        if (tok == null) break;
        m += 1;
    } else |e| try testing.expectEqual(error.Syntax, e);
    try testing.expectEqual(depth, m);
    try testing.expectEqualStrings("elements nested too deeply", tz2.diag.reason);
}

test "attributes: the first match by local name wins, prefixes are ignored" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var tz = Tokenizer.init(arena.allocator(), "<a x:k='1' k=\"2\"  m = '3' /><b/>", 100);
    const s = (try tz.next()).?.start;
    try testing.expectEqualStrings("1", s.attr("k").?);
    try testing.expectEqualStrings("3", s.attr("m").?);
    try testing.expect(s.attr("x") == null);
    try testing.expect(s.attr("") == null);
}

test "error lines" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var tz = Tokenizer.init(arena.allocator(), "<a>\n\n</b>", 100);
    _ = try tz.next();
    _ = try tz.next();
    try testing.expectError(error.Syntax, tz.next());
    try testing.expectEqual(@as(u32, 3), tz.diag.line);
}

test "decoding stops at the limit" {
    var list: std.ArrayList(u8) = .empty;
    defer list.deinit(testing.allocator);
    try testing.expectError(error.TextTooLong, appendDecoded(&list, testing.allocator, "abcdef&amp;", false, 5));
    try testing.expect(list.items.len <= 5);
}

test "fuzz: the tokenizer never crashes, leaks or runs away" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [2048]u8 = undefined;
            const n = smith.slice(&buf);
            var arena: std.heap.ArenaAllocator = .init(testing.allocator);
            defer arena.deinit();
            var tz = Tokenizer.init(arena.allocator(), buf[0..n], 2 * n);
            var scratch: std.ArrayList(u8) = .empty;
            var tokens: usize = 0;
            while (tz.next() catch |err| switch (err) {
                error.Syntax => return,
                error.OutOfMemory => return error.OutOfMemory,
            }) |tok| {
                tokens += 1;
                // Every token consumes input, so there are at most as many as bytes.
                if (tokens > 2 * n + 2) return error.NoProgress;
                switch (tok) {
                    .text => |x| try appendDecoded(&scratch, arena.allocator(), x.raw, x.cdata, 1 << 20),
                    .start => |s| _ = s.attr("a"),
                    else => {},
                }
            }
        }
    }.one, .{ .corpus = &.{
        seed("<?xml version=\"1.0\"?><a x='1'>hi&amp;<!-- c --><b/></a>"),
        seed("<?xml encoding='latin1'?><a>\xe9<![CDATA[x]]></a>"),
        seed("<!DOCTYPE a [ <!ENTITY x \"y\"> ]><a>&x;</a>"),
        seed("<a b='&#x41;"),
    } });
}

/// A fuzz seed: a Smith slice is a u32 little-endian length and the bytes.
fn seed(comptime s: []const u8) []const u8 {
    return std.mem.asBytes(&std.mem.nativeToLittle(u32, s.len)) ++ s;
}
