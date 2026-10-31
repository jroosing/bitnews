//! What every step of the finisher shares: the arena of the call, the Io and
//! the Diag the failure text goes to, and the way errors become PortErrors.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const ports = @import("postprocess_ports.zig");
const fsutil = @import("fsutil.zig");
const log_text = @import("log_text.zig");

pub const PortError = ports.PortError;
pub const Diag = ports.Diag;

pub const Ctx = struct {
    arena: Allocator,
    io: Io,
    diag: *Diag,

    /// Records Go's err.Error() text and gives the class of an error the
    /// orchestrator reports as internal.
    pub fn fail(c: Ctx, comptime fmt: []const u8, args: anytype) PortError {
        c.diag.message = std.fmt.allocPrint(c.arena, fmt, args) catch return error.OutOfMemory;
        return error.Failed;
    }

    /// Maps an error of an operation. Cancellation and memory pass through; a
    /// refused permission names `folder` and the user id when it is known
    /// (Go: fsutil.WriteError); anything else says what failed.
    pub fn ioFail(c: Ctx, err: anyerror, comptime what: []const u8, folder: ?[]const u8) PortError {
        switch (err) {
            error.Canceled => return error.Canceled,
            error.OutOfMemory => return error.OutOfMemory,
            error.AccessDenied, error.PermissionDenied => if (folder) |f| {
                c.diag.message = fsutil.writeErrorDetail(c.arena, f, fsutil.effectiveUid()) catch return error.OutOfMemory;
                // Go's FailureDetail(): the text names a folder from the
                // settings and the user id on purpose, so it is shown as is.
                c.diag.vouched = true;
                return error.Failed;
            },
            else => {},
        }
        return c.fail("finalize: " ++ what ++ ": {s}", .{@errorName(err)});
    }
};

/// Go's %q for `{f}`.
pub fn q(s: []const u8) std.fmt.Alt([]const u8, formatQ) {
    return .{ .data = s };
}

fn formatQ(s: []const u8, w: *Io.Writer) Io.Writer.Error!void {
    try log_text.writeQuoted(w, s);
}
