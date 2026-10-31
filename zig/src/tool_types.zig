//! The seam to the external post-processing programs, 7-Zip and
//! par2cmdline-turbo (ADR-0007; internal/postproc/tool). `tool.zig` is the
//! only place that starts processes; the par2 and unpack slices take a
//! `Runner` so their tests use a fake. Strings and buffers of a Result live
//! in the allocator passed to `run`.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;

pub const default_max_output: usize = 4 << 20;
pub const default_timeout_ms: u64 = 12 * 60 * 60 * 1000;

pub const Spec = struct {
    /// The program, from `locate`. `args` does not include it.
    path: []const u8,
    args: []const []const u8 = &.{},
    /// The working directory.
    dir: []const u8 = "",
    /// Fed to the program (passwords go here, never into `args`); empty means
    /// an empty stdin.
    stdin: []const u8 = "",
    /// Becomes TMPDIR; empty uses the daemon's.
    temp_dir: []const u8 = "",
    /// Caps the kept stdout and stderr each; the rest is read and dropped.
    max_output: usize = default_max_output,
    timeout_ms: u64 = default_timeout_ms,
};

pub const Result = struct {
    /// The exit status, or -1 when a signal killed the program.
    exit_code: i32,
    stdout: []u8,
    stderr: []u8,
    /// stdout or stderr was longer than `max_output`.
    truncated: bool = false,
};

pub const Error = error{
    /// The program could not be started.
    StartFailed,
    /// `timeout_ms` passed; the whole process group was killed.
    Timeout,
    /// The task was cancelled; the whole process group was killed.
    Canceled,
    OutOfMemory,
};

/// A non-zero exit status is not an error: the caller reads `exit_code`.
pub const Runner = struct {
    ctx: *anyopaque,
    run_fn: *const fn (ctx: *anyopaque, gpa: Allocator, io: Io, spec: Spec) Error!Result,

    pub fn run(r: Runner, gpa: Allocator, io: Io, spec: Spec) Error!Result {
        return r.run_fn(r.ctx, gpa, io, spec);
    }
};
