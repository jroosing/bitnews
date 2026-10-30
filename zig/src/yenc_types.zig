//! One decoded yEnc part (internal/usenet/yenc.Part). The decoder is
//! yenc.zig; the type is shared with the scheduler and the file writer.
const std = @import("std");

pub const Part = struct {
    /// The file name from =ybegin, exactly as posted. Hostile input: sanitize
    /// before using it as a path.
    name: []const u8,
    /// The size of the whole file from =ybegin.
    size: i64,
    /// 1 and 1 for a single-part file.
    part_number: u32 = 1,
    total_parts: u32 = 1,
    /// The 1-based inclusive offsets of `data` within the file. For a
    /// single-part file begin is 1 and end is size.
    begin: i64,
    end: i64,
    /// The decoded bytes; data.len == end - begin + 1. Owned by whoever the
    /// decoder allocated it for (an arena or the writer's buffer).
    data: []const u8,
    /// The IEEE CRC32 computed over `data`.
    crc32: u32,
    /// The CRC32 announced by =yend, valid only when `has_expected_crc`.
    expected_crc32: u32 = 0,
    has_expected_crc: bool = false,

    /// Whether the article announced a CRC32 and it equals the CRC32 of data.
    pub fn crcMatch(p: Part) bool {
        return p.has_expected_crc and p.crc32 == p.expected_crc32;
    }
};

test "crcMatch needs an announced CRC that equals the computed one" {
    var p: Part = .{ .name = "a", .size = 1, .begin = 1, .end = 1, .data = "x", .crc32 = 7 };
    try std.testing.expect(!p.crcMatch());
    p.has_expected_crc = true;
    try std.testing.expect(!p.crcMatch());
    p.expected_crc32 = 7;
    try std.testing.expect(p.crcMatch());
}
