//! What a failed pipeline stage becomes: the FailureReason and the stored,
//! redacted detail text (failureTable, failureDetail, redactDetail in
//! internal/core/postprocess.go).
//!
//! The text is Go's `stageError.Error()`: "<stage>: <error text>". A port
//! reports a `PortError` class and a `Diag` message; the error text is the
//! class's sentinel text (Go's sentinel errors), then the shortfall counts,
//! then the message, joined with ": ". Any path in it is replaced by "<dir>"
//! (a path can reveal the layout of the host or the name of a release) and the
//! result is cut to `max_detail_runes`. A port may vouch for its own text
//! (`Diag.vouched`, Go's `FailureDetail() string`, used by fsutil's permission
//! error, whose text names a folder from the settings): that text is stored
//! as it is, only truncated.
const std = @import("std");
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const ports = @import("postprocess_ports.zig");

pub const max_detail_runes = 200;

pub const Failure = struct {
    reason: dl.FailureReason,
    detail: []const u8,
};

/// Go's failureTable. `Canceled` has no reason: it is not a failure.
pub fn reasonOf(class: ports.PortError) ?dl.FailureReason {
    return switch (class) {
        error.InsufficientRecovery, error.RepairFailed => .repair_insufficient,
        error.MissingArticles => .missing_articles,
        error.ArchiveUnsupported => .archive_unsupported,
        error.ArchivePassword => .archive_password,
        error.ArchiveCorrupt => .archive_corrupt,
        error.PathViolation => .path_violation,
        error.DiskFull => .disk_full,
        error.Canceled => null,
        error.OutOfMemory, error.Failed => .internal,
    };
}

/// The text of Go's sentinel error of the class; empty for `Failed`, whose
/// text is the message alone.
fn classText(class: ports.PortError) []const u8 {
    return switch (class) {
        error.InsufficientRecovery => "not enough recovery data",
        error.RepairFailed => "repair failed",
        error.MissingArticles => "missing articles",
        error.ArchiveUnsupported => "archive unsupported",
        error.ArchivePassword => "archive password missing or wrong",
        error.ArchiveCorrupt => "archive corrupt",
        error.PathViolation => "path violation",
        error.DiskFull => "disk full",
        error.OutOfMemory => "out of memory",
        error.Canceled => "canceled",
        error.Failed => "",
    };
}

/// `Diag.vouched` where the shared ports file has it, else never.
fn vouched(diag: ports.Diag) bool {
    return if (@hasField(ports.Diag, "vouched")) @field(diag, "vouched") else false;
}

/// The reason and the stored detail of a failure of `stage`.
pub fn failure(arena: Allocator, stage: event.Stage, class: ports.PortError, diag: ports.Diag) Allocator.Error!Failure {
    const reason = reasonOf(class) orelse .internal;
    if (vouched(diag) and diag.message.len > 0) return .{ .reason = reason, .detail = try truncate(arena, diag.message) };
    return .{ .reason = reason, .detail = try redact(arena, try describe(arena, stage, class, diag)) };
}

/// The unredacted error text: "<stage>: <message>". The message is the whole
/// of Go's err.Error() (the ports include the sentinel text and the counts
/// themselves, so wrapped errors read exactly as in Go); only a port that
/// gives no message gets the class text and the shortfall counts.
pub fn describe(arena: Allocator, stage: event.Stage, class: ports.PortError, diag: ports.Diag) Allocator.Error![]const u8 {
    var out: std.ArrayList(u8) = .empty;
    try out.appendSlice(arena, @tagName(stage));
    try out.appendSlice(arena, ": ");
    const head = out.items.len;
    if (diag.message.len > 0) {
        try out.appendSlice(arena, diag.message);
        return out.items;
    }
    const ct = classText(class);
    if (ct.len > 0) try out.appendSlice(arena, ct);
    if (class == error.InsufficientRecovery and diag.needed > 0) {
        if (out.items.len > head) try out.appendSlice(arena, ": ");
        try out.print(arena, "need {d} blocks, have {d}", .{ diag.needed, diag.available });
    }
    if (out.items.len == head) try out.appendSlice(arena, "internal error");
    return out.items;
}

/// Go's `\s`: [\t\n\f\r ].
fn isSpace(c: u8) bool {
    return c == ' ' or c == '\t' or c == '\n' or c == '\x0c' or c == '\r';
}

/// `[^\s"'()<>|;,]`, which ends a Windows path.
fn windowsRunChar(c: u8) bool {
    return !isSpace(c) and std.mem.indexOfScalar(u8, "\"'()<>|;,", c) == null;
}

/// `[^\s"'()\[\]:;,]`, which ends a Unix path.
fn unixRunChar(c: u8) bool {
    return !isSpace(c) and std.mem.indexOfScalar(u8, "\"'()[]:;,", c) == null;
}

/// `[\s"'(\[=]`, which may come before a Unix path.
fn unixPrefixChar(c: u8) bool {
    return isSpace(c) or std.mem.indexOfScalar(u8, "\"'([=", c) != null;
}

/// The length of the run of `ok` bytes at the start of `s`.
fn runLen(s: []const u8, comptime ok: fn (u8) bool) usize {
    var n: usize = 0;
    while (n < s.len and ok(s[n])) n += 1;
    return n;
}

/// The length of a Windows path at the start of `s`, or 0:
/// `[A-Za-z]:\\[^\s"'()<>|;,]*|\\\\[^\s"'()<>|;,]+`.
fn windowsPathLen(s: []const u8) usize {
    if (s.len >= 3 and std.ascii.isAlphabetic(s[0]) and s[1] == ':' and s[2] == '\\')
        return 3 + runLen(s[3..], windowsRunChar);
    if (s.len >= 3 and s[0] == '\\' and s[1] == '\\') {
        const n = runLen(s[2..], windowsRunChar);
        if (n > 0) return 2 + n;
    }
    return 0;
}

/// Replaces drive-letter and UNC paths by "<dir>".
fn redactWindows(arena: Allocator, s: []const u8) Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) {
        const n = windowsPathLen(s[i..]);
        if (n > 0) {
            try out.appendSlice(arena, "<dir>");
            i += n;
        } else {
            try out.append(arena, s[i]);
            i += 1;
        }
    }
    return out.items;
}

/// Replaces an absolute Unix path that starts a word by "<dir>", keeping the
/// character before it: `(^|[\s"'(\[=])/[^\s"'()\[\]:;,]+`.
fn redactUnix(arena: Allocator, s: []const u8) Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    var i: usize = 0;
    while (i < s.len) {
        // The `^` alternative comes first, as in a leftmost-first match.
        if (i == 0 and s[0] == '/') {
            const n = runLen(s[1..], unixRunChar);
            if (n > 0) {
                try out.appendSlice(arena, "<dir>");
                i = 1 + n;
                continue;
            }
        }
        if (unixPrefixChar(s[i]) and i + 1 < s.len and s[i + 1] == '/') {
            const n = runLen(s[i + 2 ..], unixRunChar);
            if (n > 0) {
                try out.append(arena, s[i]);
                try out.appendSlice(arena, "<dir>");
                i += 2 + n;
                continue;
            }
        }
        try out.append(arena, s[i]);
        i += 1;
    }
    return out.items;
}

/// Makes an error text safe to store and show: absolute paths become "<dir>"
/// and the result is cut to `max_detail_runes`.
pub fn redact(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    const win = try redactWindows(arena, s);
    return truncate(arena, try redactUnix(arena, win));
}

/// The width of the rune at the start of `s`, and whether it is valid UTF-8
/// (an invalid byte is one rune wide, as in Go).
fn runeAt(s: []const u8) struct { width: usize, valid: bool } {
    const w = std.unicode.utf8ByteSequenceLength(s[0]) catch return .{ .width = 1, .valid = false };
    if (w > s.len) return .{ .width = 1, .valid = false };
    _ = std.unicode.utf8Decode(s[0..w]) catch return .{ .width = 1, .valid = false };
    return .{ .width = w, .valid = true };
}

/// Cuts `s` to `max_detail_runes` runes. A text that is short enough comes
/// back as it is; a cut one has U+FFFD for every invalid byte, as Go's
/// `string([]rune(s)[:n])`.
pub fn truncate(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    var count: usize = 0;
    var i: usize = 0;
    while (i < s.len) : (count += 1) i += runeAt(s[i..]).width;
    if (count <= max_detail_runes) return s;
    var out: std.ArrayList(u8) = .empty;
    i = 0;
    var kept: usize = 0;
    while (kept < max_detail_runes) : (kept += 1) {
        const r = runeAt(s[i..]);
        if (r.valid) try out.appendSlice(arena, s[i..][0..r.width]) else try out.appendSlice(arena, "\u{FFFD}");
        i += r.width;
    }
    return out.items;
}

// ---- tests ----

const testing = std.testing;

/// `s` repeated `n` times (there is no `**` in this Zig).
fn rep(comptime s: []const u8, comptime n: usize) [s.len * n]u8 {
    var out: [s.len * n]u8 = undefined;
    for (0..n) |i| @memcpy(out[i * s.len ..][0..s.len], s);
    return out;
}

const RedactCase = struct { in: []const u8, want: []const u8 };

// Expected values are the output of Go's redactDetail (the two regexps and the
// rune cut of internal/core/postprocess.go) for the same input.
const redact_cases = [_]RedactCase{
    .{ .in = "unpacking: open /srv/incomplete/Secret.Name/a.rar: permission denied; also \"/etc/passwd\" and C:\\Users\\me\\x.rar failed", .want = "unpacking: open <dir>: permission denied; also \"<dir>\" and <dir> failed" },
    .{ .in = "no marker in /work/x", .want = "no marker in <dir>" },
    .{ .in = "/a /b //c /", .want = "<dir> <dir> <dir> /" },
    .{ .in = "x=/etc/passwd,/b (/c) [/d] /e:f", .want = "x=<dir>,/b (<dir>) [<dir>] <dir>:f" },
    .{ .in = "\\\\server\\share\\file ok \\\\ x C:\\ y D:\\a\\b;c", .want = "<dir> ok \\\\ x <dir> y <dir>;c" },
    .{ .in = "a/b/c not a path", .want = "a/b/c not a path" },
    .{ .in = "tab\t/x\n/y\r/z\x0b/w", .want = "tab\t<dir>\n<dir>\r<dir>" },
    .{ .in = "é /é/ü ü/", .want = "é <dir> ü/" },
    .{ .in = "\xff/x \xfe /y\xff", .want = "\xff/x \xfe <dir>" },
    .{ .in = "C:/not/win d:\\x", .want = "C:/not/win <dir>" },
    .{ .in = "'/q' \"/r\" =/s", .want = "'<dir>' \"<dir>\" =<dir>" },
    .{ .in = "/", .want = "/" },
    .{ .in = "", .want = "" },
    .{ .in = "one /two\u{2028}/three \u{a0}/four", .want = "one <dir> \u{a0}/four" },
};

test "redaction matches Go's regexps on paths, edges and non-ASCII" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    for (redact_cases) |c| try testing.expectEqualStrings(c.want, try redact(arena.allocator(), c.in));
}

test "a long text is cut to 200 runes, not bytes, and invalid bytes become U+FFFD when cut" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    const long = "x " ++ rep("é", 300);
    try testing.expectEqualStrings("x " ++ rep("é", 198), try redact(a, long));

    const slash = "/" ++ rep("a", 300);
    try testing.expectEqualStrings("<dir>", try redact(a, slash));

    // 199 runes, then two invalid bytes and more: the cut keeps one of them.
    try testing.expectEqualStrings(rep("é", 199) ++ "\u{FFFD}", try redact(a, rep("é", 199) ++ "\xff\xffzz"));

    // Not over the limit: left alone, invalid bytes and all.
    try testing.expectEqualStrings("ab\xffcd", try redact(a, "ab\xffcd"));
    const cut = try redact(a, "ab\xffcd" ++ rep("\xff", 300));
    try testing.expect(std.unicode.utf8ValidateSlice(cut));
    try testing.expectEqualStrings("ab\u{FFFD}cd" ++ rep("\u{FFFD}", 195), cut);
}

test "every class maps to Go's reason, and cancellation to none" {
    try testing.expectEqual(dl.FailureReason.repair_insufficient, reasonOf(error.InsufficientRecovery).?);
    try testing.expectEqual(dl.FailureReason.repair_insufficient, reasonOf(error.RepairFailed).?);
    try testing.expectEqual(dl.FailureReason.missing_articles, reasonOf(error.MissingArticles).?);
    try testing.expectEqual(dl.FailureReason.archive_unsupported, reasonOf(error.ArchiveUnsupported).?);
    try testing.expectEqual(dl.FailureReason.archive_password, reasonOf(error.ArchivePassword).?);
    try testing.expectEqual(dl.FailureReason.archive_corrupt, reasonOf(error.ArchiveCorrupt).?);
    try testing.expectEqual(dl.FailureReason.path_violation, reasonOf(error.PathViolation).?);
    try testing.expectEqual(dl.FailureReason.disk_full, reasonOf(error.DiskFull).?);
    try testing.expectEqual(dl.FailureReason.internal, reasonOf(error.Failed).?);
    try testing.expectEqual(dl.FailureReason.internal, reasonOf(error.OutOfMemory).?);
    try testing.expect(reasonOf(error.Canceled) == null);
}

test "the detail is the stage and the port's whole message, or the class text and counts without one" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    const f = try failure(a, .repairing, error.InsufficientRecovery, .{ .needed = 12, .available = 5 });
    try testing.expectEqualStrings("repairing: not enough recovery data: need 12 blocks, have 5", f.detail);
    try testing.expectEqual(dl.FailureReason.repair_insufficient, f.reason);

    const g = try failure(a, .verifying, error.MissingArticles, .{ .message = "missing articles: 2 missing, 1 damaged and no PAR2 set" });
    try testing.expectEqualStrings("verifying: missing articles: 2 missing, 1 damaged and no PAR2 set", g.detail);

    const h = try failure(a, .unpacking, error.Failed, .{ .message = "open /srv/incomplete/x/a.rar: permission denied" });
    try testing.expectEqualStrings("unpacking: open <dir>: permission denied", h.detail);

    const bare = try failure(a, .finishing, error.Failed, .{});
    try testing.expectEqualStrings("finishing: internal error", bare.detail);

    const oom = try failure(a, .verifying, error.OutOfMemory, .{});
    try testing.expectEqualStrings("verifying: out of memory", oom.detail);
}

test "a port that vouches for its text keeps the folder path in it" {
    if (!@hasField(ports.Diag, "vouched")) return error.SkipZigTest; // waits for Diag.vouched
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var diag: ports.Diag = .{ .message = "cannot write to folder \"/srv/complete\": permission denied for uid 1000" };
    @field(diag, "vouched") = true;
    const f = try failure(arena.allocator(), .finishing, error.Failed, diag);
    try testing.expectEqualStrings("cannot write to folder \"/srv/complete\": permission denied for uid 1000", f.detail);
}

fn fuzzRedact(_: void, smith: *testing.Smith) !void {
    var buf: [1024]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const out = try redact(arena.allocator(), buf[0..n]);
    // Never longer than the cap, whatever went in, and a cut text is valid UTF-8.
    var runes: usize = 0;
    var i: usize = 0;
    while (i < out.len) : (runes += 1) i += runeAt(out[i..]).width;
    try testing.expect(runes <= max_detail_runes);
    if (std.unicode.utf8ValidateSlice(buf[0..n])) try testing.expect(std.unicode.utf8ValidateSlice(out));
}

fn seed(comptime x: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, x.len)) ++ x.*);
}

test "fuzz: redacting an error text" {
    try testing.fuzz({}, fuzzRedact, .{ .corpus = &.{
        seed("open /srv/incomplete/Secret.Name/a.rar: denied; C:\\Users\\me\\x.rar \\\\host\\share"),
        seed("x=/etc/passwd,/b (/c) [/d] /e:f //g /"),
        seed("\xff/x \xfe /y\xff \xe2\x80"),
        seed(&rep("é", 150)),
        seed(""),
    } });
}
