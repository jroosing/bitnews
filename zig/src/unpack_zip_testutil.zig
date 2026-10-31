//! A writer of stored zip files for the tests: Go's tests use archive/zip to
//! build hostile archives (names with "..", symlink and device modes), and
//! this does the same with a few lines, because no zip tool lets one choose
//! such names. Only tests import this file. The format is the one of the
//! zip APPNOTE: local headers, data, central directory, end record; every
//! entry is stored (no compression).
const std = @import("std");
const Allocator = std.mem.Allocator;
const tu = @import("unpack_testutil.zig");

pub const Ent = struct {
    name: []const u8,
    data: []const u8 = "",
    /// The Unix mode including the type bits; zero is a plain 0644 file.
    mode: u32 = 0,
};

/// Builds the archive and writes it to root/rel.
pub fn write(rig: *tu.Rig, rel: []const u8, ents: []const Ent) !void {
    const bytes = try build(rig.arena(), ents);
    try rig.write(rel, bytes);
}

pub fn build(a: Allocator, ents: []const Ent) ![]u8 {
    var out: std.ArrayList(u8) = .empty;
    var central: std.ArrayList(u8) = .empty;
    for (ents) |e| {
        const offset: u32 = @intCast(out.items.len);
        const crc = std.hash.Crc32.hash(e.data);
        // Local file header.
        try out.appendSlice(a, "PK\x03\x04");
        try le(a, &out, u16, 20); // version needed
        try le(a, &out, u16, 0x0800); // flags: UTF-8 names
        try le(a, &out, u16, 0); // stored
        try le(a, &out, u16, 0); // time
        try le(a, &out, u16, 0x21); // date
        try le(a, &out, u32, crc);
        try le(a, &out, u32, @intCast(e.data.len));
        try le(a, &out, u32, @intCast(e.data.len));
        try le(a, &out, u16, @intCast(e.name.len));
        try le(a, &out, u16, 0);
        try out.appendSlice(a, e.name);
        try out.appendSlice(a, e.data);
        // Central directory header.
        const mode: u32 = if (e.mode != 0) e.mode else if (std.mem.endsWith(u8, e.name, "/")) 0o040755 else 0o100644;
        try central.appendSlice(a, "PK\x01\x02");
        try le(a, &central, u16, 3 << 8 | 20); // made by Unix
        try le(a, &central, u16, 20);
        try le(a, &central, u16, 0x0800);
        try le(a, &central, u16, 0);
        try le(a, &central, u16, 0);
        try le(a, &central, u16, 0x21);
        try le(a, &central, u32, crc);
        try le(a, &central, u32, @intCast(e.data.len));
        try le(a, &central, u32, @intCast(e.data.len));
        try le(a, &central, u16, @intCast(e.name.len));
        try le(a, &central, u16, 0); // extra
        try le(a, &central, u16, 0); // comment
        try le(a, &central, u16, 0); // disk
        try le(a, &central, u16, 0); // internal attributes
        try le(a, &central, u32, mode << 16);
        try le(a, &central, u32, offset);
        try central.appendSlice(a, e.name);
    }
    const cd_offset: u32 = @intCast(out.items.len);
    try out.appendSlice(a, central.items);
    try out.appendSlice(a, "PK\x05\x06");
    try le(a, &out, u16, 0);
    try le(a, &out, u16, 0);
    try le(a, &out, u16, @intCast(ents.len));
    try le(a, &out, u16, @intCast(ents.len));
    try le(a, &out, u32, @intCast(central.items.len));
    try le(a, &out, u32, cd_offset);
    try le(a, &out, u16, 0);
    return out.items;
}

fn le(a: Allocator, list: *std.ArrayList(u8), comptime T: type, v: T) !void {
    var buf: [@sizeOf(T)]u8 = undefined;
    std.mem.writeInt(T, &buf, v, .little);
    try list.appendSlice(a, &buf);
}

/// Sets the uncompressed size in the first local header and in the central
/// directory entry of a one-file zip made by 7zz, so the header lies.
pub fn patchSizes(zip: []u8, size: u32) !void {
    const local = std.mem.indexOf(u8, zip, "PK\x03\x04") orelse return error.NoLocalHeader;
    const cd = std.mem.lastIndexOf(u8, zip, "PK\x01\x02") orelse return error.NoCentralHeader;
    std.mem.writeInt(u32, zip[local + 22 ..][0..4], size, .little);
    std.mem.writeInt(u32, zip[cd + 24 ..][0..4], size, .little);
}
