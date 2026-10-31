//! Test only: a PAR2 packet builder (internal/postproc/par2 builder_test.go).
//! It writes packets and small sets from scratch, computing every ID and MD5
//! itself, so the name reader is tested against an independent implementation
//! of the packet layout and hostile file names can be put into otherwise
//! valid sets. Recovery slice payloads are arbitrary bytes, not Reed-Solomon
//! data. Everything lives in the arena passed in.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Md5 = std.crypto.hash.Md5;

pub const type_main = "PAR 2.0\x00Main\x00\x00\x00\x00";
pub const type_file_desc = "PAR 2.0\x00FileDesc";
pub const type_ifsc = "PAR 2.0\x00IFSC\x00\x00\x00\x00";
pub const type_recv_slic = "PAR 2.0\x00RecvSlic";

pub fn md5(parts: []const []const u8) [16]u8 {
    var h = Md5.init(.{});
    for (parts) |p| h.update(p);
    var out: [16]u8 = undefined;
    h.final(&out);
    return out;
}

fn le64(a: Allocator, v: u64) ![]u8 {
    const b = try a.alloc(u8, 8);
    std.mem.writeInt(u64, b[0..8], v, .little);
    return b;
}

/// A packet with a correct MD5. `body` must be a multiple of 4.
pub fn rawPacket(a: Allocator, set_id: [16]u8, typ: *const [16]u8, body: []const u8) ![]u8 {
    const p = try a.alloc(u8, 64 + body.len);
    @memset(p, 0);
    @memcpy(p[0..8], "PAR2\x00PKT");
    std.mem.writeInt(u64, p[8..16], p.len, .little);
    @memcpy(p[32..48], &set_id);
    @memcpy(p[48..64], typ);
    @memcpy(p[64..], body);
    const sum = md5(&.{p[32..]});
    @memcpy(p[16..32], &sum);
    return p;
}

pub const FileSpec = struct {
    name: []const u8,
    data: []const u8,

    fn md516k(f: FileSpec) [16]u8 {
        return md5(&.{f.data[0..@min(f.data.len, 16 * 1024)]});
    }

    pub fn id(f: FileSpec, a: Allocator) ![16]u8 {
        const h = f.md516k();
        return md5(&.{ &h, try le64(a, f.data.len), f.name });
    }

    fn fileDescBody(f: FileSpec, a: Allocator) ![]u8 {
        const fid = try f.id(a);
        const full = md5(&.{f.data});
        const h16 = f.md516k();
        var b: std.ArrayList(u8) = .empty;
        try b.appendSlice(a, &fid);
        try b.appendSlice(a, &full);
        try b.appendSlice(a, &h16);
        try b.appendSlice(a, try le64(a, f.data.len));
        try b.appendSlice(a, f.name);
        while (b.items.len % 4 != 0) try b.append(a, 0);
        return b.items;
    }

    fn ifscBody(f: FileSpec, a: Allocator, slice_size: usize) ![]u8 {
        const fid = try f.id(a);
        var b: std.ArrayList(u8) = .empty;
        try b.appendSlice(a, &fid);
        var off: usize = 0;
        while (off < f.data.len) : (off += slice_size) {
            const slice = try a.alloc(u8, slice_size);
            @memset(slice, 0);
            const part = f.data[off..@min(off + slice_size, f.data.len)];
            @memcpy(slice[0..part.len], part);
            const sum = md5(&.{slice});
            try b.appendSlice(a, &sum);
            var crc: [4]u8 = undefined;
            std.mem.writeInt(u32, &crc, std.hash.Crc32.hash(slice), .little);
            try b.appendSlice(a, &crc);
        }
        return b.items;
    }
};

/// A structurally valid recovery set whose file names are whatever the test says.
pub const TestSet = struct {
    a: Allocator,
    slice_size: usize,
    files: []const FileSpec,
    set_id: [16]u8,
    main_body: []const u8,

    pub fn init(a: Allocator, slice_size: usize, files: []const FileSpec) !TestSet {
        var body: std.ArrayList(u8) = .empty;
        try body.appendSlice(a, try le64(a, slice_size));
        var n: [4]u8 = undefined;
        std.mem.writeInt(u32, &n, @intCast(files.len), .little);
        try body.appendSlice(a, &n);
        for (files) |f| try body.appendSlice(a, &(try f.id(a)));
        return .{ .a = a, .slice_size = slice_size, .files = files, .set_id = md5(&.{body.items}), .main_body = body.items };
    }

    pub fn mainPacket(s: TestSet) ![]u8 {
        return rawPacket(s.a, s.set_id, type_main, s.main_body);
    }

    pub fn fileDescPacket(s: TestSet, i: usize) ![]u8 {
        return rawPacket(s.a, s.set_id, type_file_desc, try s.files[i].fileDescBody(s.a));
    }

    /// Main, then FileDesc and IFSC per file.
    pub fn index(s: TestSet) ![]u8 {
        var out: std.ArrayList(u8) = .empty;
        try out.appendSlice(s.a, try s.mainPacket());
        for (s.files, 0..) |f, i| {
            try out.appendSlice(s.a, try s.fileDescPacket(i));
            try out.appendSlice(s.a, try rawPacket(s.a, s.set_id, type_ifsc, try f.ifscBody(s.a, s.slice_size)));
        }
        return out.items;
    }

    /// The Main packet followed by recovery packets with the given payloads (a
    /// multiple of 4 bytes each).
    pub fn volume(s: TestSet, payloads: []const []const u8) ![]u8 {
        var out: std.ArrayList(u8) = .empty;
        try out.appendSlice(s.a, try s.mainPacket());
        for (payloads, 0..) |p, i| {
            var body: std.ArrayList(u8) = .empty;
            var ex: [4]u8 = undefined;
            std.mem.writeInt(u32, &ex, @intCast(i), .little);
            try body.appendSlice(s.a, &ex);
            try body.appendSlice(s.a, p);
            try out.appendSlice(s.a, try rawPacket(s.a, s.set_id, type_recv_slic, body.items));
        }
        return out.items;
    }
};

pub fn concat(a: Allocator, parts: []const []const u8) ![]u8 {
    return std.mem.concat(a, u8, parts);
}
