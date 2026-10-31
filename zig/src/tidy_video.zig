//! SABnzbd's deobfuscation of final file names (internal/postproc/tidy/video.go):
//! the largest file below a directory, when its content is a video but its
//! name has no video extension, is named after the Download.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const fs = @import("tidy_fs.zig");
const fsutil = @import("fsutil.zig");
const Rename = @import("tidy_dots.zig").Rename;

/// How deep the scan goes and how many entries it looks at. A tree from
/// Unpack is far below both; the caps keep a hostile tree from holding the
/// scan (Go has none). Over a cap the scan fails with `error.TooLarge`.
pub const max_depth = 128;
pub const max_scanned = 1 << 20;

pub const Error = fs.ListError || Io.Dir.OpenError || Io.File.OpenError || Io.File.ReadPositionalError ||
    Io.Dir.RenamePreserveError || error{ TooLarge, BadPath };

/// The extensions media managers import as video.
const video_exts = [_][]const u8{
    ".mkv", ".mp4",  ".m4v",  ".avi", ".ts",   ".m2ts", ".mts", ".mov", ".wmv",
    ".mpg", ".mpeg", ".webm", ".vob", ".divx", ".flv",  ".iso", ".img",
};

/// Renames the largest file below `dir` to `name` (made safe) plus the
/// extension its content says, when its own extension is not a video one but
/// its content is a video. It walks subfolders but never follows links (a
/// link is not a file here), never overwrites (a taken name gets a counter)
/// and leaves everything else alone. Null says nothing was renamed; the
/// Rename's paths are relative to `dir`, slash-separated, and live in `a`.
/// The first of several largest files in name order wins.
pub fn nameVideo(a: Allocator, io: Io, dir: []const u8, name: []const u8) Error!?Rename {
    var root = try Io.Dir.cwd().openDir(io, dir, .{ .iterate = true, .follow_symlinks = false });
    defer root.close(io);

    var scan: Scan = .{ .a = a, .io = io };
    try scan.walk(root, "", 0);
    const largest = scan.largest orelse return null;

    const base = fsutil.base(largest);
    if (isVideoExt(extOf(base))) return null;

    const parent_rel = if (std.mem.lastIndexOfScalar(u8, largest, '/')) |i| largest[0..i] else "";
    var parent = try fs.openBelow(io, root, parent_rel, false);
    defer parent.close(io);

    const found = try sniff(io, parent.dir, base);
    if (found.len == 0) return null;
    const wanted = try std.mem.concat(a, u8, &.{ name, found });
    const safe = fsutil.safeName(a, wanted) catch |err| switch (err) {
        error.InvalidName => return null,
        error.OutOfMemory => return error.OutOfMemory,
    };
    const to = try fsutil.unique(a, io, parent.dir, safe);
    try fs.renameNoClobber(io, parent.dir, base, to);
    return .{
        .from = largest,
        .to = if (parent_rel.len == 0) to else try std.mem.concat(a, u8, &.{ parent_rel, "/", to }),
    };
}

const Scan = struct {
    a: Allocator,
    io: Io,
    scanned: usize = 0,
    /// Path of the largest regular file so far, relative to the root.
    largest: ?[]const u8 = null,
    size: u64 = 0,

    /// Go's filepath.WalkDir order: the entries of a directory by name, a
    /// directory entered when it is met.
    fn walk(s: *Scan, d: Io.Dir, rel: []const u8, depth: usize) Error!void {
        if (depth > max_depth) return error.TooLarge;
        const entries = try fs.list(s.a, s.io, d);
        for (entries) |e| {
            s.scanned += 1;
            if (s.scanned > max_scanned) return error.TooLarge;
            switch (e.kind) {
                .directory => {
                    var sub = try d.openDir(s.io, e.name, .{ .iterate = true, .follow_symlinks = false });
                    defer sub.close(s.io);
                    const sub_rel = if (rel.len == 0) e.name else try std.mem.concat(s.a, u8, &.{ rel, "/", e.name });
                    try s.walk(sub, sub_rel, depth + 1);
                },
                .file => {
                    const st = try d.statFile(s.io, e.name, .{ .follow_symlinks = false });
                    if (s.largest == null or st.size > s.size) {
                        s.size = st.size;
                        s.largest = if (rel.len == 0) e.name else try std.mem.concat(s.a, u8, &.{ rel, "/", e.name });
                    }
                },
                else => {},
            }
        }
    }
};

/// Go's strings.ToLower(filepath.Ext(base)) compared with the list. Besides
/// A-Z, Unicode lower-cases the Kelvin sign U+212A to "k" and the dotted
/// capital I U+0130 to "i", so ".mKv" and ".dIvx" in those spellings count.
fn isVideoExt(ext: []const u8) bool {
    var buf: [16]u8 = undefined;
    var n: usize = 0;
    var i: usize = 0;
    while (i < ext.len) {
        if (n >= buf.len) return false;
        const c = ext[i];
        if (c < 0x80) {
            buf[n] = std.ascii.toLower(c);
            n += 1;
            i += 1;
        } else if (std.mem.startsWith(u8, ext[i..], "\xe2\x84\xaa")) {
            buf[n] = 'k';
            n += 1;
            i += 3;
        } else if (std.mem.startsWith(u8, ext[i..], "\xc4\xb0")) {
            buf[n] = 'i';
            n += 1;
            i += 2;
        } else return false;
    }
    for (video_exts) |v| if (std.mem.eql(u8, v, buf[0..n])) return true;
    return false;
}

/// filepath.Ext: from the last dot of the final element, or "".
fn extOf(base: []const u8) []const u8 {
    const i = std.mem.lastIndexOfScalar(u8, base, '.') orelse return "";
    return base[i..];
}

/// The container of the file `name` in `d` by its first bytes, or "".
fn sniff(io: Io, d: Io.Dir, name: []const u8) Error![]const u8 {
    // Not following a link: the scan saw a regular file.
    var f = try d.openFile(io, name, .{ .follow_symlinks = false, .allow_directory = false });
    defer f.close(io);
    var head: [400]u8 = undefined;
    const n = try f.readPositionalAll(io, &head, 0);
    return videoExt(head[0..n]);
}

/// The extension of the video container the bytes start with: Matroska, MP4
/// or QuickTime, AVI, ASF (WMV), MPEG program stream, MPEG transport stream
/// or M2TS; "" for anything else.
pub fn videoExt(head: []const u8) []const u8 {
    if (std.mem.startsWith(u8, head, &.{ 0x1a, 0x45, 0xdf, 0xa3 })) return ".mkv";
    if (head.len >= 12 and std.mem.eql(u8, head[4..8], "ftyp")) {
        return if (std.mem.eql(u8, head[8..12], "qt  ")) ".mov" else ".mp4";
    }
    if (head.len >= 12 and std.mem.eql(u8, head[0..4], "RIFF") and std.mem.eql(u8, head[8..12], "AVI ")) return ".avi";
    if (std.mem.startsWith(u8, head, &.{ 0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11 })) return ".wmv";
    if (std.mem.startsWith(u8, head, &.{ 0x00, 0x00, 0x01, 0xba })) return ".mpg";
    if (syncEvery(head, 0, 188)) return ".ts";
    if (syncEvery(head, 4, 192)) return ".m2ts";
    return "";
}

/// Whether `head` has the transport stream sync byte at `off` and at the
/// next two packets of size `n`.
fn syncEvery(head: []const u8, off: usize, n: usize) bool {
    for (0..3) |i| {
        const p = off + i * n;
        if (p >= head.len or head[p] != 0x47) return false;
    }
    return true;
}
