//! An NZB document after parsing and normalizing (internal/usenet/nzb). The
//! parser is nzb.zig; these types are shared with the scheduler, the file
//! writer and the runner. Strings belong to the arena the parser was given.
const std = @import("std");

/// The role of a File, guessed from its NAME only (no PAR2 file is ever
/// opened by bitnews: par2 does the verifying and repairing, see ADR-0007).
pub const Kind = enum {
    /// Any file that is not PAR2, including obfuscated names.
    data,
    /// A PAR2 file without recovery blocks ("name.par2").
    par2_index,
    /// A PAR2 recovery volume ("name.volNN+MM.par2").
    par2_volume,
};

/// One Article carrying part of a File.
pub const Segment = struct {
    number: u32,
    bytes: i64,
    message_id: []const u8,
};

/// One file posted to Usenet, split into Segments.
pub const File = struct {
    subject: []const u8 = "",
    poster: []const u8 = "",
    /// Posting time in unix seconds from the "date" attribute; 0 when absent.
    date: i64 = 0,
    groups: []const []const u8 = &.{},
    segments: []const Segment = &.{},
    /// A provisional file name guessed from the subject. Not sanitized: the
    /// yEnc name= header is authoritative once fetched.
    name: []const u8 = "",
    kind: Kind = .data,
    /// The MM of "name.volNN+MM.par2": the number of recovery blocks the
    /// volume claims. 0 for other kinds, and for a volume whose MM does not
    /// fit.
    recovery_blocks: u32 = 0,

    /// Whether the file is left out of a Download's first fetch: PAR2
    /// recovery volumes are fetched only when par2 reports it needs their
    /// blocks. This is the one place that decides it.
    pub fn deferred(f: File) bool {
        return f.kind == .par2_volume;
    }

    /// The sum of the segment sizes, saturating.
    pub fn bytes(f: File) i64 {
        var total: i64 = 0;
        for (f.segments) |s| total = satAdd(total, @max(s.bytes, 0));
        return total;
    }
};

/// The optional values of the NZB <head>; empty means absent.
pub const Meta = struct {
    name: []const u8 = "",
    password: []const u8 = "",
    category: []const u8 = "",
};

pub const Nzb = struct {
    meta: Meta = .{},
    files: []const File = &.{},
    /// Segments dropped because their number or message ID was invalid.
    /// Duplicates are not counted.
    skipped_segments: u32 = 0,

    /// The size of the Articles a Download fetches before any repair: every
    /// file that is not deferred. Saturates instead of overflowing.
    pub fn upFrontBytes(n: Nzb) i64 {
        var total: i64 = 0;
        for (n.files) |f| {
            if (f.deferred()) continue;
            total = satAdd(total, f.bytes());
        }
        return total;
    }
};

fn satAdd(a: i64, b: i64) i64 {
    return std.math.add(i64, a, b) catch std.math.maxInt(i64);
}

const testing = std.testing;

test "only recovery volumes are deferred, and sizes saturate" {
    const seg = [_]Segment{ .{ .number = 1, .bytes = 100, .message_id = "a@b" }, .{ .number = 2, .bytes = 50, .message_id = "c@d" } };
    const files = [_]File{
        .{ .segments = &seg },
        .{ .kind = .par2_index, .segments = &seg },
        .{ .kind = .par2_volume, .recovery_blocks = 4, .segments = &seg },
    };
    const n: Nzb = .{ .files = &files };
    try testing.expect(!files[0].deferred() and !files[1].deferred() and files[2].deferred());
    try testing.expectEqual(@as(i64, 150), files[0].bytes());
    try testing.expectEqual(@as(i64, 300), n.upFrontBytes());

    const huge = [_]Segment{ .{ .number = 1, .bytes = std.math.maxInt(i64), .message_id = "x" }, .{ .number = 2, .bytes = 5, .message_id = "y" } };
    try testing.expectEqual(@as(i64, std.math.maxInt(i64)), (File{ .segments = &huge }).bytes());
}
