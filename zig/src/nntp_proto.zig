//! The pure parts of the NNTP client (internal/usenet/nntp proto.go, body.go,
//! date.go): limits, the status-line parser, the message-id rule, the DATE
//! timestamp and the sanitising of server text. No I/O here, so every
//! function can be fuzzed on arbitrary bytes.
const std = @import("std");

/// The longest line (without its CRLF) accepted from a Server.
pub const max_line_length = 8 << 10;
/// The most bytes of one body (including line terminators) accepted.
pub const max_body_size = 16 << 20;
/// A reader must hold one full line and its terminator.
pub const min_read_buffer = max_line_length + 2;
/// Server text kept in a diagnostic, after sanitising.
pub const max_error_text = 200;
/// Matches the limit the NZB parser enforces.
pub const max_message_id_length = 250;

/// Raw bytes of a status line's text looked at before sanitising (Go: 4 *
/// maxErrorText), so a hostile 8 KiB line costs nothing.
const max_raw_text = 4 * max_error_text;

pub const Response = struct {
    code: u16,
    /// What follows "NNN ", cut at `max_raw_text`; not yet sanitised.
    text: []const u8,
};

/// A status line: three digits (first one 1 to 5), optionally followed by a
/// space and text. `line` has no terminator.
pub fn parseResponse(line: []const u8) error{Malformed}!Response {
    if (line.len < 3 or line[0] < '1' or line[0] > '5' or !isDigit(line[1]) or !isDigit(line[2]) or
        (line.len > 3 and line[3] != ' ')) return error.Malformed;
    const code: u16 = @as(u16, line[0] - '0') * 100 + @as(u16, line[1] - '0') * 10 + (line[2] - '0');
    const text: []const u8 = if (line.len > 4) line[4..@min(line.len, 4 + max_raw_text)] else "";
    return .{ .code = code, .text = text };
}

pub fn isDigit(b: u8) bool {
    return b >= '0' and b <= '9';
}

/// Makes server text safe to log: every occurrence of a secret becomes "***"
/// (servers sometimes echo credentials back), everything but printable ASCII
/// becomes '?' (one per code point, as Go's range over a string does), at
/// most `max_error_text` bytes, surrounding spaces trimmed.
pub fn sanitize(out: *[max_error_text]u8, raw: []const u8, secrets: []const []const u8) []const u8 {
    var n: usize = 0;
    var i: usize = 0;
    scan: while (i < raw.len and n < out.len) {
        for (secrets) |sec| {
            if (sec.len > 0 and std.mem.startsWith(u8, raw[i..], sec)) {
                for ("***") |c| {
                    if (n == out.len) break;
                    out[n] = c;
                    n += 1;
                }
                i += sec.len;
                continue :scan;
            }
        }
        const b = raw[i];
        if (b < 0x80) {
            out[n] = if (b < 0x20 or b > 0x7e) '?' else b;
            i += 1;
        } else {
            out[n] = '?';
            i += runeWidth(raw[i..]);
        }
        n += 1;
    }
    return std.mem.trim(u8, out[0..n], " ");
}

/// Bytes the code point at the start of `s` (a non-ASCII lead byte) takes; 1
/// for a malformed sequence, which Go's string range also advances by.
fn runeWidth(s: []const u8) usize {
    const len = std.unicode.utf8ByteSequenceLength(s[0]) catch return 1;
    if (len > s.len) return 1;
    _ = std.unicode.utf8Decode(s[0..len]) catch return 1;
    return len;
}

/// Only printable ASCII without space, '<' or '>': nothing that could break
/// out of the BODY command.
pub fn validMessageId(id: []const u8) bool {
    if (id.len == 0 or id.len > max_message_id_length) return false;
    for (id) |b| {
        if (b <= ' ' or b >= 0x7f or b == '<' or b == '>') return false;
    }
    return true;
}

/// The text of a 111 response to DATE: YYYYMMDDhhmmss, UTC, as Unix seconds.
pub fn parseDate(s: []const u8) ?i64 {
    if (s.len != 14) return null;
    for (s) |c| if (!isDigit(c)) return null;
    const year = std.fmt.parseInt(i64, s[0..4], 10) catch return null;
    const month = std.fmt.parseInt(u8, s[4..6], 10) catch return null;
    const day = std.fmt.parseInt(u8, s[6..8], 10) catch return null;
    const hour = std.fmt.parseInt(u8, s[8..10], 10) catch return null;
    const minute = std.fmt.parseInt(u8, s[10..12], 10) catch return null;
    const second = std.fmt.parseInt(u8, s[12..14], 10) catch return null;
    if (month < 1 or month > 12 or hour > 23 or minute > 59 or second > 59) return null;
    if (day < 1 or day > daysInMonth(year, month)) return null;
    return daysFromCivil(year, month, day) * 86400 + @as(i64, hour) * 3600 + @as(i64, minute) * 60 + second;
}

fn isLeap(y: i64) bool {
    return (@mod(y, 4) == 0 and @mod(y, 100) != 0) or @mod(y, 400) == 0;
}

fn daysInMonth(y: i64, m: u8) u8 {
    return switch (m) {
        1, 3, 5, 7, 8, 10, 12 => 31,
        4, 6, 9, 11 => 30,
        else => if (isLeap(y)) 29 else 28,
    };
}

/// Days since 1970-01-01 of a civil date (H. Hinnant's algorithm).
fn daysFromCivil(year: i64, month: u8, day: u8) i64 {
    const y = if (month <= 2) year - 1 else year;
    const era = @divFloor(y, 400);
    const yoe = y - era * 400;
    const mp: i64 = if (month > 2) month - 3 else @as(i64, month) + 9;
    const doy = @divFloor(153 * mp + 2, 5) + day - 1;
    const doe = yoe * 365 + @divFloor(yoe, 4) - @divFloor(yoe, 100) + doy;
    return era * 146097 + doe - 719468;
}

const testing = std.testing;

test "status line: code, text and the shapes that are refused" {
    const ok = try parseResponse("200 welcome");
    try testing.expectEqual(@as(u16, 200), ok.code);
    try testing.expectEqualStrings("welcome", ok.text);
    try testing.expectEqual(@as(u16, 430), (try parseResponse("430")).code);
    try testing.expectEqualStrings("", (try parseResponse("430 ")).text);
    for ([_][]const u8{ "", "20", "99 x", "2000 x", "200hi", "600 x", "abc", "2a0 x", "hello there", "000 x" }) |bad| {
        try testing.expectError(error.Malformed, parseResponse(bad));
    }
}

test "status text is cut before sanitising" {
    var line: [9000]u8 = undefined;
    @memset(&line, 'x');
    @memcpy(line[0..4], "200 ");
    const r = try parseResponse(&line);
    try testing.expectEqual(@as(usize, 800), r.text.len);
}

test "sanitize: printable ASCII only, one '?' per code point, truncated, trimmed, secrets redacted" {
    var out: [max_error_text]u8 = undefined;
    try testing.expectEqualStrings("bad??[31m", sanitize(&out, "bad\x01\x1b[31m", &.{}));
    try testing.expectEqualStrings("caf?", sanitize(&out, "caf\xc3\xa9", &.{}));
    try testing.expectEqualStrings("a?b", sanitize(&out, "a\xffb", &.{}));
    try testing.expectEqualStrings("a?b", sanitize(&out, "a\xef\xbf\xbdb", &.{})); // U+FFFD itself
    try testing.expectEqualStrings("a??", sanitize(&out, "a\xe2\x82", &.{})); // truncated sequence: byte by byte
    try testing.expectEqualStrings("hi", sanitize(&out, "  hi  ", &.{}));
    try testing.expectEqualStrings("", sanitize(&out, "", &.{}));
    var long: [1000]u8 = undefined;
    @memset(&long, 'y');
    try testing.expectEqual(@as(usize, 200), sanitize(&out, &long, &.{}).len);
    try testing.expectEqualStrings("wrong password *** for ***", sanitize(&out, "wrong password hunter2 for alice", &.{ "alice", "hunter2" }));
    try testing.expectEqualStrings("same", sanitize(&out, "same", &.{ "", "" }));
}

test "message ids: printable ASCII without space or angle brackets, 1 to 250 bytes" {
    try testing.expect(validMessageId("a@b"));
    try testing.expect(validMessageId("part1of2.abc$def@news.example"));
    try testing.expect(validMessageId(&@as([250]u8, @splat('a'))));
    for ([_][]const u8{ "", &@as([251]u8, @splat('a')), "a b", "a\tb", "a\r\nQUIT", "<a@b>", "a>b", "a\x00b", "a\x7fb", "caf\xc3\xa9@x" }) |bad| {
        try testing.expect(!validMessageId(bad));
    }
}

test "DATE: UTC timestamps, calendar checked" {
    try testing.expectEqual(@as(?i64, 1767366245), parseDate("20260102150405"));
    try testing.expectEqual(@as(?i64, 0), parseDate("19700101000000"));
    try testing.expectEqual(@as(?i64, 951782400), parseDate("20000229000000")); // leap day
    try testing.expectEqual(@as(?i64, -86400), parseDate("19691231000000"));
    for ([_][]const u8{
        "",                 "notadate",       "2026010215040",  "202601021504055", "20261301000000", "20260230000000",
        "19000229000000",   "20260100000000", "20260132000000", "20260102240000",  "20260102156000", "20260102150460",
        "2026-01-02T15:04", "+0260102150405",
    }) |bad| {
        try testing.expectEqual(@as(?i64, null), parseDate(bad));
    }
}

test "fuzz: the status-line parser and sanitiser never crash and keep their promises" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [10_000]u8 = undefined;
            const n = smith.slice(&buf);
            const r = parseResponse(buf[0..n]) catch return;
            try testing.expect(r.code >= 100 and r.code <= 599);
            var out: [max_error_text]u8 = undefined;
            const text = sanitize(&out, r.text, &.{ "alice", "pw" });
            try testing.expect(text.len <= max_error_text);
            for (text) |c| try testing.expect(c >= 0x20 and c <= 0x7e);
            try testing.expect(std.mem.indexOf(u8, text, "alice") == null);
            _ = parseDate(buf[0..@min(n, 20)]);
            _ = validMessageId(buf[0..n]);
        }
    }.one, .{ .corpus = &.{
        "\x08\x00\x00\x00200 hi\r\n",
        "\x0b\x00\x00\x00222 0 <a@b>",
        "\x03\x00\x00\x00430",
        "\x0e\x00\x00\x00502 \xff\x00 alice\r\n",
        "\x0e\x00\x00\x00111 2026010215",
    } });
}
