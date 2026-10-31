//! The one place that reads PAR2 data ourselves (internal/postproc/par2
//! names.go): the defensive guard that refuses a set with unsafe file names
//! BEFORE par2 writes under them, because par2 versions differ in what they
//! neutralise (ADR-0007). Everything else (verify, repair, rename by hash) is
//! par2's work.
//!
//! WHAT IT PARSES, exactly: the 64-byte PAR2 packet header fields it needs
//! (magic "PAR2\0PKT", the packet length at 8..16, the MD5 at 16..32, the
//! type at 48..64) and, for packets of type "PAR 2.0\0FileDesc" only, the MD5
//! of bytes 32..end and the file name that follows the 56 fixed bytes of the
//! body. It does not follow packet boundaries, does not read Main, IFSC or
//! recovery packets, hashes no file data and builds no set model. To drop the
//! guard, delete this file and its two calls in par2.zig (`inspect`,
//! `removeBackups`); nothing else depends on it.
//!
//! It is deliberately more permissive than par2: it looks for the magic at
//! every byte offset, inside other packets and recovery data too, and accepts
//! every File Description whose MD5 verifies, so it sees every name par2
//! could ever use. Memory (one 256 KiB chunk, one packet of at most 1 MiB)
//! and the hashing done for damaged candidates are bounded; a packet it
//! cannot bound makes it refuse the file.
//!
//! Errors carry their text in the `Diag` passed in, which lives in the arena
//! the caller passes: Go's error strings, the unsafe ones being
//! "par2: unsafe PAR2 set: path violation: ...".
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Md5 = std.crypto.hash.Md5;
const ports = @import("postprocess_ports.zig");
const fsutil = @import("fsutil.zig");
const log_text = @import("log_text.zig");

pub const Error = error{
    /// The set is hostile or unusable as it is (Go: ErrUnsafeSet, a
    /// core.ErrPathViolation); `Diag.message` says why.
    UnsafeSet,
    /// Anything else that failed; `Diag.message` says what.
    Failed,
    Canceled,
    OutOfMemory,
};

pub const packet_header_len = 64;
/// The part of a File Description body before the name.
pub const file_desc_fixed = 56;
/// The longest File Description packet the reader will hash. A candidate that
/// claims more is refused instead of skipped: par2 might accept it.
pub const max_desc_len = 1 << 20;
/// The longest file name (path) a set may carry.
pub const max_name_len = 1024;
/// The longest single path component (a file system limit on every platform
/// we run on).
pub const max_component_len = 255;
/// Bounds how many distinct names one set may carry.
pub const max_names = 100_000;
pub const scan_chunk = 256 << 10;

pub const magic = "PAR2\x00PKT";
pub const type_file_desc = "PAR 2.0\x00FileDesc";

/// Something the reader can read exact ranges from. `readAll` fills `buf`
/// from `off` or fails with `error.ReadFailed` (a short read included).
fn checkSource(comptime S: type) void {
    _ = @as(*const fn (*S, []u8, u64) error{ ReadFailed, Canceled }!void, &S.readAll);
}

/// A `ReaderAt` over memory (tests and fuzzing).
pub const SliceSource = struct {
    data: []const u8,

    pub fn readAll(s: *SliceSource, buf: []u8, off: u64) error{ ReadFailed, Canceled }!void {
        if (off > s.data.len or s.data.len - off < buf.len) return error.ReadFailed;
        @memcpy(buf, s.data[@intCast(off)..][0..buf.len]);
    }
};

/// A `ReaderAt` over an open file. `err` keeps what the last failure was.
pub const FileSource = struct {
    file: Io.File,
    io: Io,
    err: ?anyerror = null,

    pub fn readAll(s: *FileSource, buf: []u8, off: u64) error{ ReadFailed, Canceled }!void {
        const n = s.file.readPositionalAll(s.io, buf, off) catch |e| {
            s.err = e;
            return if (e == error.Canceled) error.Canceled else error.ReadFailed;
        };
        if (n != buf.len) {
            s.err = error.UnexpectedEof;
            return error.ReadFailed;
        }
    }
};

/// The buffers of the scanner, shared by every file of a set so that a set of
/// a hundred volumes costs one chunk and one packet buffer in the arena.
pub const Scanner = struct {
    chunk: []u8,
    pkt: []u8 = &.{},

    pub fn init(arena: Allocator) Allocator.Error!Scanner {
        return .{ .chunk = try arena.alloc(u8, scan_chunk + magic.len - 1) };
    }
};

/// The distinct names found, in the order they were first seen. The strings
/// live in the arena passed to `readNames`.
pub const NameSet = struct {
    map: std.StringArrayHashMapUnmanaged(void) = .empty,

    pub fn names(s: *const NameSet) []const []const u8 {
        return s.map.keys();
    }
};

/// Adds the distinct file names that the File Description packets of one
/// .par2 stream (`src`, `size` bytes) carry to `set`, trailing NULs removed.
///
/// It must accept at least every packet par2 would accept, so it does not
/// follow packet boundaries: it looks for the magic at every byte offset and
/// takes every candidate whose type is File Description and whose MD5
/// verifies. Names are added exactly as written (possibly hostile); run them
/// through `checkNames`.
///
/// The work done for candidates that fail verification is bounded to a small
/// multiple of `size`; when it is exceeded, for a candidate longer than 1 MiB
/// and for more than `max_names` names the stream is refused with
/// `error.UnsafeSet`. A failed read is `error.ReadFailed` from the source,
/// reported here as `Failed` with no text (the caller knows the file).
pub fn readNames(arena: Allocator, sc: *Scanner, src: anytype, size: u64, set: *NameSet, diag: *ports.Diag) (Error || error{ReadFailed})!void {
    comptime checkSource(@typeInfo(@TypeOf(src)).pointer.child);
    var rd: NameReader(@TypeOf(src)) = .{
        .arena = arena,
        .sc = sc,
        .src = src,
        .size = size,
        // 4*size + 1 MiB of failed hashing is allowed.
        .budget = @intCast(@min(size *| 4 +| (1 << 20), std.math.maxInt(i64))),
        .set = set,
        .diag = diag,
    };
    try rd.run();
}

fn NameReader(comptime SrcPtr: type) type {
    return struct {
        const Self = @This();
        arena: Allocator,
        sc: *Scanner,
        src: SrcPtr,
        size: u64,
        budget: i64,
        set: *NameSet,
        diag: *ports.Diag,

        fn run(n: *Self) (Error || error{ReadFailed})!void {
            const buf = n.sc.chunk;
            var pos: u64 = 0;
            while (pos < n.size) {
                const want: usize = @intCast(@min(buf.len, n.size - pos));
                try n.src.readAll(buf[0..want], pos);
                const window = buf[0..want];
                var base: usize = 0;
                while (std.mem.indexOfPos(u8, window, base, magic)) |at| {
                    // A magic in the last len(magic)-1 bytes of a full chunk
                    // is cut and not found here; the next chunk starts early
                    // enough to see it whole.
                    try n.candidate(pos + at);
                    base = at + 1;
                }
                if (pos + want >= n.size) break;
                pos += want - magic.len + 1;
            }
        }

        /// Looks at the bytes at `off`, where the magic was found.
        fn candidate(n: *Self, off: u64) (Error || error{ReadFailed})!void {
            if (n.size - off < packet_header_len) return;
            var hdr: [packet_header_len]u8 = undefined;
            try n.src.readAll(&hdr, off);
            if (!std.mem.eql(u8, hdr[48..64], type_file_desc)) return;
            const length = std.mem.readInt(u64, hdr[8..16], .little);
            // par2 refuses a packet like this too.
            if (length % 4 != 0 or length < packet_header_len + file_desc_fixed or length > n.size - off) return;
            if (length > max_desc_len) {
                return unsafe(n.arena, n.diag, "a file description packet claims {d} bytes", .{length});
            }
            const len: usize = @intCast(length);
            if (n.sc.pkt.len < len) {
                // Grows by doubling to at most the cap: the old buffer stays
                // in the arena, which costs at most the same again.
                const grown = @min(@max(len, n.sc.pkt.len * 2), max_desc_len);
                n.sc.pkt = try n.arena.alloc(u8, grown);
            }
            const pkt = n.sc.pkt[0..len];
            try n.src.readAll(pkt, off);
            var sum: [16]u8 = undefined;
            Md5.hash(pkt[32..], &sum, .{});
            if (!std.mem.eql(u8, &sum, pkt[16..32])) {
                n.budget -= @as(i64, @intCast(len)) + 4096;
                if (n.budget < 0) return unsafe(n.arena, n.diag, "too many damaged file description packets", .{});
                return;
            }
            const name = std.mem.trimEnd(u8, pkt[packet_header_len + file_desc_fixed ..], "\x00");
            if (n.set.map.contains(name)) return;
            if (n.set.map.count() >= max_names) return unsafe(n.arena, n.diag, "more than {d} file names", .{max_names});
            try n.set.map.put(n.arena, try n.arena.dupe(u8, name), {});
        }
    };
}

/// `readNames` for a file on disk. Read errors come back as `Failed` with the
/// text "par2: read <file>: <error>".
pub fn readNamesFile(arena: Allocator, io: Io, sc: *Scanner, dir: Io.Dir, name: []const u8, set: *NameSet, diag: *ports.Diag) Error!void {
    const file = dir.openFile(io, name, .{ .allow_directory = false }) catch |e| return readFailure(arena, diag, name, e);
    defer file.close(io);
    const size = file.length(io) catch |e| return readFailure(arena, diag, name, e);
    var src: FileSource = .{ .file = file, .io = io };
    readNames(arena, sc, &src, size, set, diag) catch |e| switch (e) {
        error.ReadFailed => return readFailure(arena, diag, name, src.err orelse error.ReadFailed),
        else => |x| return x,
    };
}

fn readFailure(arena: Allocator, diag: *ports.Diag, name: []const u8, err: anyerror) Error {
    if (err == error.Canceled) return error.Canceled;
    if (err == error.OutOfMemory) return error.OutOfMemory;
    diag.message = std.fmt.allocPrint(arena, "par2: read {f}: {s}", .{ quoted(name), @errorName(err) }) catch return error.OutOfMemory;
    return error.Failed;
}

/// Go's %q for `{f}`.
fn quoted(s: []const u8) std.fmt.Alt([]const u8, formatQuoted) {
    return .{ .data = s };
}

fn formatQuoted(s: []const u8, w: *Io.Writer) Io.Writer.Error!void {
    try log_text.writeQuoted(w, s);
}

/// Sets the text of an unsafe set (Go: ErrUnsafeSet wrapped with detail).
fn unsafe(arena: Allocator, diag: *ports.Diag, comptime fmt: []const u8, args: anytype) Error {
    diag.message = std.fmt.allocPrint(arena, "par2: unsafe PAR2 set: path violation: " ++ fmt, args) catch return error.OutOfMemory;
    return error.UnsafeSet;
}

/// At most `n` bytes of `s` for an error message.
fn truncate(arena: Allocator, s: []const u8, n: usize) Allocator.Error![]const u8 {
    if (s.len <= n) return s;
    return std.mem.concat(arena, u8, &.{ s[0..n], "..." });
}

fn bad(arena: Allocator, diag: *ports.Diag, name: []const u8, why: []const u8) Error {
    const shown = try truncate(arena, name, 80);
    return unsafe(arena, diag, "file name {f}: {s}", .{ quoted(shown), why });
}

/// Whether `name`, taken from a PAR2 file, is a plain relative path that is
/// safe to give to par2: slash separated, no empty, "." or ".." component, not
/// absolute, no drive letter, no backslash, no NUL or other control
/// character, not a PAR2 file itself, and within the length limits. Control
/// characters are refused because par2 prints names, and a name with a line
/// break could fake lines of the output we parse.
pub fn checkName(arena: Allocator, name: []const u8, diag: *ports.Diag) Error!void {
    if (name.len == 0) return bad(arena, diag, name, "empty");
    if (name.len > max_name_len) return bad(arena, diag, name, "too long");
    if (name[0] == '/') return bad(arena, diag, name, "absolute path");
    if (name.len >= 2 and name[1] == ':' and std.ascii.isAlphabetic(name[0])) return bad(arena, diag, name, "drive letter");
    for (name) |c| {
        if (c == 0) return bad(arena, diag, name, "NUL byte");
        if (c == '\\') return bad(arena, diag, name, "backslash");
        if (c < 0x20 or c == 0x7f) return bad(arena, diag, name, "control character");
    }
    var it = std.mem.splitScalar(u8, name, '/');
    while (it.next()) |comp| {
        if (comp.len == 0) return bad(arena, diag, name, "empty path component");
        if (std.mem.eql(u8, comp, ".") or std.mem.eql(u8, comp, "..")) return bad(arena, diag, name, "relative path component");
        if (comp.len > max_component_len) return bad(arena, diag, name, "path component too long");
    }
    if (std.ascii.endsWithIgnoreCase(name, ".par2")) return bad(arena, diag, name, "a PAR2 file cannot be a target");
}

/// Runs `checkName` on every name and then `fsutil.safeJoin` on `dir` and the
/// name, which also refuses a name whose parent directory in `dir` is a
/// symlink that leaves `dir`. A target that already exists in `dir` as a
/// symlink or as anything but a regular file is refused too: par2 would read
/// it or replace it.
pub fn checkNames(arena: Allocator, io: Io, dir: []const u8, names: []const []const u8, diag: *ports.Diag) Error!void {
    // safeJoin allocates its result; a fixed buffer, reset per name, keeps a
    // set of many names from filling the arena with joined paths.
    var buf: [64 << 10]u8 = undefined;
    var fba: std.heap.FixedBufferAllocator = .init(&buf);
    for (names) |name| {
        try checkName(arena, name, diag);
        fba.reset();
        const p = fsutil.safeJoin(fba.allocator(), io, dir, name) catch |e| switch (e) {
            error.Canceled => return error.Canceled,
            else => {
                const shown = try truncate(arena, name, 80);
                return unsafe(arena, diag, "file name {f}: {s}", .{ quoted(shown), @errorName(e) });
            },
        };
        const st = Io.Dir.cwd().statFile(io, p, .{ .follow_symlinks = false }) catch |e| switch (e) {
            error.FileNotFound => continue,
            error.Canceled => return error.Canceled,
            else => {
                diag.message = std.fmt.allocPrint(arena, "par2: {s}", .{@errorName(e)}) catch return error.OutOfMemory;
                return error.Failed;
            },
        };
        if (st.kind != .file) {
            const shown = try truncate(arena, name, 80);
            return unsafe(arena, diag, "target {f} exists and is not a regular file", .{quoted(shown)});
        }
    }
}
