//! Parses an NZB document into the normalized model of nzb_types.zig
//! (internal/usenet/nzb/parse.go). NZBs come from indexers and uploaders and
//! are hostile input: the XML is read by the strict, bounded tokenizer of
//! nzb_xml.zig, input size, file count and segment count are capped by
//! `Limits`, and a segment with an invalid number or message ID is skipped
//! rather than failing the whole NZB.
//!
//! Elements are matched by local name, so the standard namespace, no
//! namespace and prefixed names all work. For each file the segments are
//! sorted by number, duplicates are dropped (the first in the document is
//! kept) and a file left without segments is dropped. File names guessed from
//! subjects are provisional and unsanitized: the yEnc name= header is
//! authoritative, and any name goes through fsutil before it is used on disk.
//!
//! Memory: every string of the result lives in the arena given to `parse`;
//! scratch space (the current file's segments, the text of the current
//! element) is reused, so an arena only grows with the result and with the
//! deepest nesting seen.
const std = @import("std");
const Allocator = std.mem.Allocator;
const types = @import("nzb_types.zig");
const xml = @import("nzb_xml.zig");
const names = @import("nzb_name.zig");

pub const Nzb = types.Nzb;
pub const File = types.File;
pub const Segment = types.Segment;
pub const Meta = types.Meta;
pub const Kind = types.Kind;
pub const Diag = xml.Diag;

/// Bounds the resources `parse` may spend on one NZB. Counts include
/// elements that are later dropped as invalid, so the work done is bounded
/// whatever survives normalization. Zero allows nothing.
pub const Limits = struct {
    /// Input bytes, including anything after </nzb>.
    max_bytes: u64 = 64 << 20,
    /// <file> elements.
    max_files: u32 = 10_000,
    /// <segment> elements, summed over all files.
    max_segments: u32 = 1_000_000,
};

pub const default_limits: Limits = .{};

/// The most decoded text one element or attribute value may have. A message
/// ID is 250 bytes and a subject a few hundred; Go has no such cap but the
/// input size, which would let one NZB hold the whole limit in one string.
pub const max_text_bytes: usize = 1 << 20;

/// Go's sentinel errors (ErrTooLarge, ErrTooManyFiles, ErrTooManySegments,
/// ErrNoFiles, ErrMalformed).
pub const ParseError = error{
    TooLarge,
    TooManyFiles,
    TooManySegments,
    NoFiles,
    Malformed,
    OutOfMemory,
};

/// Parses with the default limits. See `parseWithLimits`.
pub fn parse(arena: Allocator, bytes: []const u8) ParseError!Nzb {
    return parseWithLimits(arena, bytes, default_limits, null);
}

/// Parses `bytes`; every string of the result is allocated in `arena`, which
/// should be an arena (scratch space is left in it too). `diag`, when given,
/// receives the details of a Malformed error for `errorMessage`.
pub fn parseWithLimits(arena: Allocator, bytes: []const u8, lim: Limits, diag: ?*Diag) ParseError!Nzb {
    // The whole input is in memory, so the size is checked before parsing;
    // Go's reader notices when it reads past the limit, which is the same
    // for an input that is not malformed earlier.
    if (bytes.len > lim.max_bytes) return error.TooLarge;
    var p: Parser = .{
        .arena = arena,
        .lim = lim,
        .tz = .init(arena, bytes, 2 * bytes.len),
    };
    defer p.tz.deinit();
    p.run() catch |err| {
        if (err == error.Malformed) {
            if (diag) |d| d.* = p.diag;
        }
        return err;
    };
    if (p.files.items.len == 0) return error.NoFiles;
    return .{
        .meta = p.meta,
        .files = try p.files.toOwnedSlice(arena),
        .skipped_segments = p.skipped,
    };
}

/// The text Go's errors have, for the message of a client error ("not a
/// valid NZB: ..."). Allocated in `a`.
pub fn errorMessage(a: Allocator, err: ParseError, lim: Limits, diag: Diag) Allocator.Error![]const u8 {
    return switch (err) {
        error.OutOfMemory => error.OutOfMemory,
        error.TooLarge => std.fmt.allocPrint(a, "nzb: input too large: limit is {d} bytes", .{lim.max_bytes}),
        error.TooManyFiles => std.fmt.allocPrint(a, "nzb: too many files: more than {d}", .{lim.max_files}),
        error.TooManySegments => std.fmt.allocPrint(a, "nzb: too many segments: more than {d}", .{lim.max_segments}),
        error.NoFiles => "nzb: no usable files",
        error.Malformed => if (diag.line > 0)
            std.fmt.allocPrint(a, "nzb: malformed document: XML syntax error on line {d}: {s}", .{ diag.line, diag.reason })
        else if (diag.reason.len > 0)
            std.fmt.allocPrint(a, "nzb: malformed document: {s}", .{diag.reason})
        else
            "nzb: malformed document",
    };
}

const Parser = struct {
    arena: Allocator,
    lim: Limits,
    tz: xml.Tokenizer,
    diag: Diag = .{},
    files: std.ArrayList(File) = .empty,
    meta: Meta = .{},
    seen_meta: [3]bool = @splat(false),
    nfiles: u32 = 0,
    nsegs: u32 = 0,
    skipped: u32 = 0,
    // Scratch, reused: the segments and groups of the file being read, the
    // text of the element being read, and a decoded attribute value.
    segs: std.ArrayList(Segment) = .empty,
    groups: std.ArrayList([]const u8) = .empty,
    text_buf: std.ArrayList(u8) = .empty,
    attr_buf: std.ArrayList(u8) = .empty,

    fn run(p: *Parser) ParseError!void {
        // The root element is the first start element.
        const root = while (try p.tok()) |t| {
            if (t == .start) break t.start;
        } else return p.malformed("no root element");
        if (!std.mem.eql(u8, root.local, "nzb")) {
            const reason = std.fmt.allocPrint(p.arena, "root element is <{s}>, want <nzb>", .{root.local}) catch return error.OutOfMemory;
            return p.malformed(reason);
        }
        while (try p.tok()) |t| switch (t) {
            .start => |s| {
                if (std.mem.eql(u8, s.local, "head")) {
                    try p.parseHead();
                } else if (std.mem.eql(u8, s.local, "file")) {
                    try p.parseFile(s);
                } else try p.skip();
            },
            .end => break,
            else => {},
        } else return p.malformed("unexpected EOF");
        // The rest is read too, so trailing bytes are checked as well.
        while (try p.tok()) |_| {}
    }

    fn malformed(p: *Parser, reason: []const u8) error{Malformed} {
        p.diag = .{ .line = 0, .reason = reason };
        return error.Malformed;
    }

    fn tok(p: *Parser) ParseError!?xml.Token {
        return p.tz.next() catch |err| switch (err) {
            error.Syntax => {
                p.diag = p.tz.diag;
                return error.Malformed;
            },
            error.OutOfMemory => return error.OutOfMemory,
        };
    }

    /// Consumes the rest of the element just opened.
    fn skip(p: *Parser) ParseError!void {
        var depth: usize = 0;
        while (try p.tok()) |t| switch (t) {
            .start => depth += 1,
            .end => {
                if (depth == 0) return;
                depth -= 1;
            },
            else => {},
        };
        return p.malformed("unexpected EOF");
    }

    const Collected = struct { text: []const u8, too_long: bool };

    /// The character data of the element just opened, up to its end, without
    /// that of its children. Valid until the next call.
    fn collectText(p: *Parser) ParseError!Collected {
        p.text_buf.clearRetainingCapacity();
        var depth: usize = 0;
        var too_long = false;
        while (try p.tok()) |t| switch (t) {
            .text => |x| {
                if (depth == 0 and !too_long) {
                    xml.appendDecoded(&p.text_buf, p.arena, x.raw, x.cdata, max_text_bytes) catch |err| switch (err) {
                        error.TextTooLong => too_long = true,
                        error.OutOfMemory => return error.OutOfMemory,
                    };
                }
            },
            .start => depth += 1,
            .end => {
                if (depth == 0) return .{ .text = p.text_buf.items, .too_long = too_long };
                depth -= 1;
            },
            .other => {},
        };
        return p.malformed("unexpected EOF");
    }

    /// The decoded value of an attribute, "" when absent; valid until the
    /// next call. null when it is longer than `max_text_bytes`.
    fn attr(p: *Parser, s: xml.Start, name: []const u8) Allocator.Error!?[]const u8 {
        const raw = s.attr(name) orelse return "";
        return xml.decodeValue(&p.attr_buf, p.arena, raw, max_text_bytes) catch |err| switch (err) {
            error.TextTooLong => null,
            error.OutOfMemory => error.OutOfMemory,
        };
    }

    fn parseHead(p: *Parser) ParseError!void {
        while (try p.tok()) |t| switch (t) {
            .start => |s| {
                if (std.mem.eql(u8, s.local, "meta")) try p.parseMeta(s) else try p.skip();
            },
            .end => return,
            else => {},
        };
        return p.malformed("unexpected EOF");
    }

    fn parseMeta(p: *Parser, s: xml.Start) ParseError!void {
        const c = try p.collectText();
        if (c.too_long) return p.malformed("meta value too long");
        const typ_raw = (try p.attr(s, "type")) orelse return;
        const typ = names.trimSpace(typ_raw);
        const kinds = [_][]const u8{ "name", "password", "category" };
        const idx = for (kinds, 0..) |k, i| {
            if (std.ascii.eqlIgnoreCase(typ, k)) break i;
        } else return;
        if (p.seen_meta[idx]) return;
        p.seen_meta[idx] = true;
        const value = try p.arena.dupe(u8, names.trimSpace(c.text));
        switch (idx) {
            0 => p.meta.name = value,
            1 => p.meta.password = value,
            else => p.meta.category = value,
        }
    }

    fn parseFile(p: *Parser, s: xml.Start) ParseError!void {
        p.nfiles += 1;
        if (p.nfiles > p.lim.max_files) return error.TooManyFiles;
        const subject_raw = (try p.attr(s, "subject")) orelse return p.malformed("subject too long");
        const subject = try p.arena.dupe(u8, subject_raw);
        const poster_raw = (try p.attr(s, "poster")) orelse return p.malformed("poster too long");
        const poster = try p.arena.dupe(u8, poster_raw);
        const date_raw = (try p.attr(s, "date")) orelse "";
        const date = parseDecimal(names.trimSpace(date_raw)) orelse 0;

        p.segs.clearRetainingCapacity();
        p.groups.clearRetainingCapacity();
        while (try p.tok()) |t| switch (t) {
            .start => |c| {
                if (std.mem.eql(u8, c.local, "groups")) {
                    try p.parseGroups();
                } else if (std.mem.eql(u8, c.local, "segments")) {
                    try p.parseSegments();
                } else try p.skip();
            },
            .end => break,
            else => {},
        } else return p.malformed("unexpected EOF");

        if (p.segs.items.len == 0) return;
        const segments = normalize(p.segs.items);
        const name = names.guessName(subject);
        const class = names.classify(name);
        try p.files.append(p.arena, .{
            .subject = subject,
            .poster = poster,
            .date = date,
            .groups = try p.arena.dupe([]const u8, p.groups.items),
            .segments = try p.arena.dupe(Segment, segments),
            .name = name,
            .kind = class.kind,
            .recovery_blocks = class.recovery_blocks,
        });
    }

    fn parseGroups(p: *Parser) ParseError!void {
        while (try p.tok()) |t| switch (t) {
            .start => |s| {
                if (!std.mem.eql(u8, s.local, "group")) {
                    try p.skip();
                    continue;
                }
                const c = try p.collectText();
                if (c.too_long) return p.malformed("group too long");
                const g = names.trimSpace(c.text);
                if (g.len > 0) try p.groups.append(p.arena, try p.arena.dupe(u8, g));
            },
            .end => return,
            else => {},
        };
        return p.malformed("unexpected EOF");
    }

    fn parseSegments(p: *Parser) ParseError!void {
        while (try p.tok()) |t| switch (t) {
            .start => |s| {
                if (!std.mem.eql(u8, s.local, "segment")) {
                    try p.skip();
                    continue;
                }
                if (try p.parseSegment(s)) |seg| {
                    try p.segs.append(p.arena, seg);
                } else p.skipped += 1;
            },
            .end => return,
            else => {},
        };
        return p.malformed("unexpected EOF");
    }

    /// One <segment>; null when it is invalid and must be skipped.
    fn parseSegment(p: *Parser, s: xml.Start) ParseError!?Segment {
        p.nsegs += 1;
        if (p.nsegs > p.lim.max_segments) return error.TooManySegments;
        const c = try p.collectText();

        // A number that does not fit a u32 is as invalid as one below 1: no
        // NZB has 4 billion segments in a file.
        const number_raw = (try p.attr(s, "number")) orelse return null;
        const number = parseDecimal(names.trimSpace(number_raw)) orelse return null;
        if (number < 1 or number > std.math.maxInt(u32)) return null;

        // The size is informational: unknown is allowed.
        const bytes_raw = (try p.attr(s, "bytes")) orelse "";
        var bytes = parseDecimal(names.trimSpace(bytes_raw)) orelse 0;
        if (bytes < 0) bytes = 0;

        if (c.too_long) return null;
        var id = names.trimSpace(c.text);
        if (std.mem.startsWith(u8, id, "<")) id = id[1..];
        if (std.mem.endsWith(u8, id, ">")) id = id[0 .. id.len - 1];
        if (!validMessageId(id)) return null;
        return .{
            .number = @intCast(number),
            .bytes = bytes,
            .message_id = try p.arena.dupe(u8, id),
        };
    }
};

/// Go's strconv.ParseInt(s, 10, 64): an optional sign and ASCII digits only
/// (std.fmt.parseInt also takes underscores), null on overflow.
fn parseDecimal(s: []const u8) ?i64 {
    var digits = s;
    if (digits.len > 0 and (digits[0] == '+' or digits[0] == '-')) digits = digits[1..];
    if (digits.len == 0) return null;
    for (digits) |c| if (c < '0' or c > '9') return null;
    return std.fmt.parseInt(i64, s, 10) catch null;
}

/// The most bytes a message ID has (without angle brackets).
pub const max_message_id_len = 250;

/// Whether `id` is safe to send in an NNTP command: 1..250 printable ASCII
/// characters (0x21-0x7E) including an '@', and no angle brackets, which
/// frame the ID in "BODY <id>".
pub fn validMessageId(id: []const u8) bool {
    if (id.len == 0 or id.len > max_message_id_len) return false;
    if (std.mem.indexOfScalar(u8, id, '@') == null) return false;
    for (id) |c| {
        if (c < 0x21 or c > 0x7e or c == '<' or c == '>') return false;
    }
    return true;
}

/// Orders segments by number and drops duplicate numbers, keeping the first
/// in the document (the sort is stable). In place; returns the kept prefix.
fn normalize(segs: []Segment) []Segment {
    var sorted = true;
    for (segs[1..], 0..) |s, i| {
        if (s.number <= segs[i].number) {
            sorted = false;
            break;
        }
    }
    if (sorted) return segs;
    std.mem.sort(Segment, segs, {}, struct {
        fn less(_: void, a: Segment, b: Segment) bool {
            return a.number < b.number;
        }
    }.less);
    var n: usize = 1;
    for (segs[1..]) |s| {
        if (s.number == segs[n - 1].number) continue;
        segs[n] = s;
        n += 1;
    }
    return segs[0..n];
}
