//! replace_for.py (internal/postproc/tidy/dots.go): "_" becomes "." in the
//! names directly inside a directory.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const fs = @import("tidy_fs.zig");

/// One entry that was renamed. Names live in the arena of the call.
pub const Rename = struct { from: []const u8, to: []const u8 };

pub const Error = fs.ListError || Io.Dir.OpenError || Io.Dir.RenamePreserveError;

/// Replaces "_" with "." in the name of every entry (file or folder)
/// directly inside `dir`. It does not recurse and never overwrites: an entry
/// whose new name is taken is left as it is. A link is renamed itself, never
/// followed. `dir` is opened without following a symlink.
///
/// What was renamed is appended to `done`, also when a later rename fails, so
/// the caller can report the work that was done together with the error. The
/// strings and the list live in `a` (use an arena).
pub fn dotUnderscores(a: Allocator, io: Io, dir: []const u8, done: *std.ArrayList(Rename)) Error!void {
    var d = try Io.Dir.cwd().openDir(io, dir, .{ .iterate = true, .follow_symlinks = false });
    defer d.close(io);
    const entries = try fs.list(a, io, d);
    for (entries) |e| {
        if (std.mem.indexOfScalar(u8, e.name, '_') == null) continue;
        const to = try a.dupe(u8, e.name);
        std.mem.replaceScalar(u8, to, '_', '.');
        if (fs.exists(io, d, to)) continue; // taken: never overwrite
        fs.renameNoClobber(io, d, e.name, to) catch |err| switch (err) {
            // Taken by someone between the check and the rename.
            error.PathAlreadyExists => continue,
            else => |x| return x,
        };
        try done.append(a, .{ .from = e.name, .to = to });
    }
}
