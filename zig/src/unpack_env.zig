//! What one Unpack run needs: the options (internal/postproc/unpack
//! options.go), the allocators and the seams. No global state.
//!
//! Memory: `arena` holds what outlives the run (the result, the message of a
//! failure); the caller owns it. `gpa` is for scratch: each step (listing,
//! walk) makes an arena of its own on it and drops it at the end, so the
//! memory of a hostile listing is returned as soon as the guard is done.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const fsutil_space = @import("fsutil_space.zig");
const ports = @import("postprocess_ports.zig");
const tool = @import("tool_types.zig");
const log = @import("log.zig");

/// Caps the number of entries in one archive.
pub const default_max_entries: usize = 1_000_000;

/// A compression bomb is more than `bomb_min_bytes` of output at more than
/// `bomb_ratio` times the archive size.
pub const bomb_min_bytes: i64 = 1 << 30;
pub const bomb_ratio: i64 = 100;

/// Sizes the cap on the output of `7zz l`: the listing of an archive with
/// `max_entries` entries is expected to fit.
pub const list_bytes_per_entry = 600;
pub const list_bytes_floor = 4 << 20;
pub const list_bytes_ceiling = 512 << 20;

/// The free disk space probe (Go: WithFreeSpace). The default is statfs.
pub const FreeSpace = struct {
    ctx: ?*anyopaque = null,
    probe_fn: *const fn (ctx: ?*anyopaque, path: []const u8) anyerror!u64 = defaultProbe,

    pub fn probe(f: FreeSpace, path: []const u8) anyerror!u64 {
        return f.probe_fn(f.ctx, path);
    }

    fn defaultProbe(_: ?*anyopaque, path: []const u8) anyerror!u64 {
        return fsutil_space.freeBytes(path);
    }
};

pub const Config = struct {
    /// The 7zz program, as `tool.locate` found it.
    seven_zip: []const u8 = "",
    /// Handed to 7zz on its standard input, never on the command line, and
    /// never part of an error. Empty means none.
    password: []const u8 = "",
    /// Bytes to keep free: the declared output must fit into free minus this.
    min_free: u64 = 0,
    /// Total bytes written, nested archives included; zero is no cap.
    max_bytes: i64 = 0,
    max_entries: usize = default_max_entries,
    /// Replace an existing destination instead of failing with Exists.
    replace: bool = false,
    free_space: FreeSpace = .{},
    /// Tests lower these so a bomb need not be a gigabyte.
    bomb_min: i64 = bomb_min_bytes,
    bomb_ratio: i64 = bomb_ratio,
};

pub const Env = struct {
    io: Io,
    gpa: Allocator,
    arena: Allocator,
    runner: tool.Runner,
    cfg: Config,
    diag: *ports.Diag,
    logger: ?log.Logger = null,
};

/// a + b, saturating at 2^62 (Go: addSat); a declared size cannot overflow.
pub fn addSat(a: i64, b: i64) i64 {
    const cap: i64 = 1 << 62;
    if (a > cap - b) return cap;
    return a + b;
}
