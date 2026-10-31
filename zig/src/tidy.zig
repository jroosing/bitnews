//! The two TRaSH SABnzbd scripts, ported (ADR-0009), and the deobfuscation of
//! final file names (internal/postproc/tidy). bitnews runs no scripts; these
//! are the same rules in the same order.
//!
//!   - `clean`: Clean.py, the suffixes and tags release bots add to an NZB's
//!     name. A pure function; the result is a prefix of its argument.
//!   - `dotUnderscores`: replace_for.py, "_" to "." in the names at the top
//!     level of a directory.
//!   - `nameVideo`: the largest file, when its content is a video but its
//!     name has no video extension, is named after the Download.
//!
//! Memory: nothing is kept. Functions that allocate take the arena of the
//! request or step and put every string they return in it.
const std = @import("std");
const Allocator = std.mem.Allocator;
const add_service = @import("add_service.zig");

pub const clean = @import("tidy_clean.zig").clean;
pub const Rename = @import("tidy_dots.zig").Rename;
pub const dotUnderscores = @import("tidy_dots.zig").dotUnderscores;
pub const nameVideo = @import("tidy_video.zig").nameVideo;
pub const videoExt = @import("tidy_video.zig").videoExt;

/// `clean` as the add service's NameCleaner (the CleanNames option). The
/// result is a copy in the arena of the add.
pub fn nameCleaner() add_service.NameCleaner {
    return .{ .ctx = null, .clean_fn = cleanName };
}

fn cleanName(_: ?*anyopaque, arena: Allocator, name: []const u8) anyerror![]const u8 {
    return arena.dupe(u8, clean(name));
}
