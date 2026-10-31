//! The errors of the unpack slice (internal/postproc/unpack errors.go). Go
//! wraps sentinels; here an error is one of these tags and the text Go's
//! err.Error() would print goes into a `Diag`. Each tag maps to the port
//! error class the orchestrator turns into a FailureReason.
const std = @import("std");
const Allocator = std.mem.Allocator;
const ports = @import("postprocess_ports.zig");

pub const Error = error{
    /// The set's volumes are not all present (class: archive corrupt).
    MissingVolume,
    /// Encrypted, and the password is missing or wrong.
    Password,
    /// A compression method or feature 7zz does not support.
    Unsupported,
    /// Damaged, truncated or lying about itself.
    Corrupt,
    /// A name tries to escape, or an entry is a link or special file.
    PathViolation,
    /// The output does not fit into the free disk space.
    DiskFull,
    /// The archive expands far beyond what its size justifies.
    Bomb,
    /// The output exceeds the configured byte cap.
    TooLarge,
    TooManyEntries,
    /// The destination exists and Replace was not set.
    Exists,
    /// Anything that is not the archive's fault (7zz missing, I/O errors).
    Internal,
    Canceled,
    OutOfMemory,
};

/// The text Go prints for the error's sentinel chain.
pub fn prefix(e: Error) []const u8 {
    return switch (e) {
        error.MissingVolume => "unpack: missing archive volume: archive corrupt",
        error.Password => "unpack: archive password missing or wrong",
        error.Unsupported => "unpack: unsupported archive feature: archive unsupported",
        error.Corrupt => "unpack: archive corrupt",
        error.PathViolation => "unpack: unsafe path in archive: path violation",
        error.DiskFull => "unpack: resource limit exceeded: not enough free disk space: disk full",
        error.Bomb => "unpack: resource limit exceeded: compression ratio too high: archive corrupt",
        error.TooLarge => "unpack: resource limit exceeded: output too large: archive corrupt",
        error.TooManyEntries => "unpack: resource limit exceeded: too many entries: archive corrupt",
        error.Exists => "unpack: destination exists",
        error.Internal, error.Canceled, error.OutOfMemory => "",
    };
}

pub fn toPort(e: Error) ports.PortError {
    return switch (e) {
        error.MissingVolume, error.Corrupt, error.Bomb, error.TooLarge, error.TooManyEntries => error.ArchiveCorrupt,
        error.Password => error.ArchivePassword,
        error.Unsupported => error.ArchiveUnsupported,
        error.PathViolation => error.PathViolation,
        error.DiskFull => error.DiskFull,
        error.Exists, error.Internal => error.Failed,
        error.Canceled => error.Canceled,
        error.OutOfMemory => error.OutOfMemory,
    };
}

/// Sets the message to `<prefix of e>: <formatted detail>` (Go:
/// fmt.Errorf("%w: ...", ErrX, ...)) and returns `e`.
pub fn fail(a: Allocator, diag: *ports.Diag, e: Error, comptime fmt: []const u8, args: anytype) Error {
    const p = prefix(e);
    diag.message = if (p.len == 0)
        std.fmt.allocPrint(a, fmt, args) catch return error.OutOfMemory
    else
        std.fmt.allocPrint(a, "{s}: " ++ fmt, .{p} ++ args) catch return error.OutOfMemory;
    return e;
}

/// Puts `<text>: ` in front of the message already set (Go:
/// fmt.Errorf("nested archive %q: %w", ...)).
pub fn wrap(a: Allocator, diag: *ports.Diag, comptime fmt: []const u8, args: anytype) void {
    diag.message = std.fmt.allocPrint(a, fmt ++ ": {s}", args ++ .{diag.message}) catch diag.message;
}

/// Maps an I/O error to the unpack error classes: a full disk is DiskFull
/// (Go: diskErr), a cancellation stays one, anything else is Internal with
/// `what` and the error name in the message.
pub fn io(a: Allocator, diag: *ports.Diag, err: anyerror, comptime what: []const u8) Error {
    switch (err) {
        error.Canceled => return error.Canceled,
        error.OutOfMemory => return error.OutOfMemory,
        error.NoSpaceLeft => return fail(a, diag, error.DiskFull, what ++ ": {s}", .{@errorName(err)}),
        else => return fail(a, diag, error.Internal, "unpack: " ++ what ++ ": {s}", .{@errorName(err)}),
    }
}
