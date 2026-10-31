//! The tool runner: the one place that starts a process (ADR-0007, Go
//! internal/postproc/tool). It runs one of the pinned post-processing
//! programs (7zz, par2) with an argument list, no shell, a scrubbed
//! environment and its own process group, so a cancel or a timeout kills the
//! program and everything it started. Output is captured up to a cap; the
//! rest is read and dropped so the program never blocks on a full pipe.
//!
//! Design: `std.process.spawn` starts the program (pipes, process group,
//! working directory, environment), then ONE thread drives the program with
//! poll(2): it feeds stdin, drains stdout and stderr, reaps the program and
//! watches the deadline. Using `Io.concurrent` tasks for the three streams
//! would make a run fail with ConcurrencyUnavailable when the thread pool is
//! busy, and lose a cancel that arrives early (see the Z3 findings). poll is
//! not an Io operation, so the loop wakes every `tick_ms` and calls
//! `io.checkCancel()`: a cancel is noticed within one tick. It relies on
//! `Io.Threaded` having made SIGPIPE a no-op, so a program that never reads
//! stdin gives EPIPE (ignored) instead of killing the daemon.
const std = @import("std");
const builtin = @import("builtin");
const posix = std.posix;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const types = @import("tool_types.zig");

pub const Spec = types.Spec;
pub const Result = types.Result;
pub const Error = types.Error;
pub const Runner = types.Runner;
pub const default_max_output = types.default_max_output;
pub const default_timeout_ms = types.default_timeout_ms;

/// How long a killed program, or one that exited while something it started
/// still holds its output pipes, has to let go of them before the pipes are
/// closed (Go's WaitDelay). A variable only so tests can shorten it.
pub var kill_grace_ms: i64 = 5000;
/// How often the loop looks for a cancel, a deadline or a program that
/// exited while a pipe is still held.
const tick_ms: i32 = 25;
const read_chunk = 64 << 10;

// ---------------------------------------------------------------- locate

pub const LocateError = error{ NotFound, OutOfMemory };

/// The text of a `NotFound`, as Go's Locate words it.
pub fn notFoundMessage(gpa: Allocator, name: []const u8) Allocator.Error![]u8 {
    return std.fmt.allocPrint(gpa, "tool: program not found: {s} (install it, or run `make tools` and add bin/tools to PATH; see docs/deploy.md)", .{name});
}

/// The absolute path of the program `name`, searched on the daemon's PATH.
/// The caller frees it. A relative PATH entry (including an empty one) is
/// skipped, as Go refuses a result found relative to the working directory.
pub fn locate(gpa: Allocator, io: Io, name: []const u8) LocateError![]u8 {
    const path = if (std.c.getenv("PATH")) |p| std.mem.span(p) else "";
    return locateIn(gpa, io, path, name);
}

/// `locate` over an explicit PATH value (colon separated).
pub fn locateIn(gpa: Allocator, io: Io, path_list: []const u8, name: []const u8) LocateError![]u8 {
    if (name.len == 0) return error.NotFound;
    if (std.mem.indexOfScalar(u8, name, '/') != null) {
        if (name[0] != '/' or !isExecutable(io, name)) return error.NotFound;
        return gpa.dupe(u8, name);
    }
    var it = std.mem.splitScalar(u8, path_list, ':');
    while (it.next()) |dir| {
        if (dir.len == 0 or dir[0] != '/') continue;
        const full = try std.fs.path.join(gpa, &.{ dir, name });
        if (isExecutable(io, full)) return full;
        gpa.free(full);
    }
    return error.NotFound;
}

fn isExecutable(io: Io, path: []const u8) bool {
    const st = Io.Dir.cwd().statFile(io, path, .{}) catch return false;
    if (st.kind != .file) return false;
    Io.Dir.cwd().access(io, path, .{ .execute = true }) catch return false;
    return true;
}

// ---------------------------------------------------------------- runner

/// The real runner, as a `Runner` for the par2 and unpack slices (their
/// tests pass a fake instead). Stateless.
pub const SystemRunner = struct {
    pub fn runner(self: *SystemRunner) Runner {
        return .{ .ctx = self, .run_fn = runFn };
    }

    fn runFn(_: *anyopaque, gpa: Allocator, io: Io, spec: Spec) Error!Result {
        return run(gpa, io, spec);
    }
};

var system_runner: SystemRunner = .{};

/// A runner for the real programs; no setup, nothing to free.
pub fn system() Runner {
    return system_runner.runner();
}

/// Frees the buffers of a Result made by `run`.
pub fn freeResult(gpa: Allocator, r: Result) void {
    gpa.free(r.stdout);
    gpa.free(r.stderr);
}

/// Starts the program and waits for it. A non-zero exit status is not an
/// error: the caller reads `exit_code` (-1 when a signal killed the program).
/// The buffers of the Result belong to `gpa`; see `freeResult`.
///
/// Errors: `StartFailed` (the program or the working directory cannot be
/// used), `Timeout` and `Canceled` (the whole process group was killed). A
/// program that exited but left something holding its output pipes for more
/// than `kill_grace_ms` still gives a Result, with the output read so far
/// (Go reports an error there; the exit status is the useful answer).
pub fn run(gpa: Allocator, io: Io, spec: Spec) Error!Result {
    if (spec.path.len == 0) return error.StartFailed;
    const limit = if (spec.max_output == 0) default_max_output else spec.max_output;
    const timeout_ms: i64 = @intCast(@min(if (spec.timeout_ms == 0) default_timeout_ms else spec.timeout_ms, std.math.maxInt(i62)));

    // Check what a failed exec would report before forking: on Linux
    // `std.process.spawn` 0.17.0 leaks the three pipe ends and the zombie
    // child when the exec fails (processSpawnPosix returns without closing or
    // reaping), so a start failure must not get that far.
    const resolved: ?[]u8 = if (std.mem.indexOfScalar(u8, spec.path, '/') == null) locate(gpa, io, spec.path) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.NotFound => return error.StartFailed,
    } else null;
    defer if (resolved) |r| gpa.free(r);
    const program = resolved orelse spec.path;
    if (!isExecutable(io, program)) return error.StartFailed;
    if (spec.dir.len > 0) {
        const st = Io.Dir.cwd().statFile(io, spec.dir, .{}) catch return error.StartFailed;
        if (st.kind != .directory) return error.StartFailed;
    }

    var env = try scrubbedEnv(gpa, spec.temp_dir);
    defer env.deinit();
    const argv = try gpa.alloc([]const u8, spec.args.len + 1);
    defer gpa.free(argv);
    argv[0] = program;
    @memcpy(argv[1..], spec.args);

    var child = std.process.spawn(io, .{
        .argv = argv,
        .cwd = if (spec.dir.len == 0) .inherit else .{ .path = spec.dir },
        .environ_map = &env,
        .stdin = .pipe,
        .stdout = .pipe,
        .stderr = .pipe,
        // Its own process group (pgid = pid): the spawn returns after exec,
        // so the group exists when we kill it.
        .pgid = 0,
    }) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.Canceled => return error.Canceled,
        else => return error.StartFailed,
    };

    var proc: Proc = .{
        .pid = child.id.?,
        .stdin = child.stdin.?.handle,
        .stdout = child.stdout.?.handle,
        .stderr = child.stderr.?.handle,
    };
    // We own the pipes and the reaping from here: `child` is only a record.
    child.id = null;
    child.stdin = null;
    child.stdout = null;
    child.stderr = null;
    defer proc.finish();
    setNonBlocking(proc.stdin);

    var out: Sink = .{ .limit = limit };
    defer out.buf.deinit(gpa);
    var err_sink: Sink = .{ .limit = limit };
    defer err_sink.buf.deinit(gpa);

    const outcome = try drive(gpa, io, &proc, spec.stdin, timeout_ms, &out, &err_sink);
    switch (outcome) {
        .canceled => return error.Canceled,
        .timeout => return error.Timeout,
        .done => {},
    }

    const stdout = try out.buf.toOwnedSlice(gpa);
    errdefer gpa.free(stdout);
    const stderr = try err_sink.buf.toOwnedSlice(gpa);
    return .{
        .exit_code = proc.exit_code,
        .stdout = stdout,
        .stderr = stderr,
        .truncated = out.dropped or err_sink.dropped,
    };
}

/// The whole environment of a tool: nothing of the daemon's leaks in, and
/// the output language is fixed so the parsers see English.
fn scrubbedEnv(gpa: Allocator, temp_dir: []const u8) Allocator.Error!std.process.Environ.Map {
    var env = std.process.Environ.Map.init(gpa);
    errdefer env.deinit();
    try env.put("LC_ALL", "C");
    try env.put("LANG", "C");
    try env.put("HOME", "/nonexistent");
    if (temp_dir.len > 0) {
        try env.put("TMPDIR", temp_dir);
    } else if (std.c.getenv("TMPDIR")) |t| {
        const v = std.mem.span(t);
        if (v.len > 0) try env.put("TMPDIR", v);
    }
    return env;
}

/// The child and our ends of its pipes; -1 is closed.
const Proc = struct {
    pid: posix.pid_t,
    stdin: posix.fd_t,
    stdout: posix.fd_t,
    stderr: posix.fd_t,
    reaped: bool = false,
    exit_code: i32 = -1,

    fn closeFd(fd: *posix.fd_t) void {
        if (fd.* >= 0) _ = std.c.close(fd.*);
        fd.* = -1;
    }

    /// SIGKILL to the whole group. Only while the leader is not reaped: after
    /// that the pid may belong to something else.
    fn killGroup(p: *Proc) void {
        if (p.reaped) return;
        _ = posix.system.kill(-p.pid, .KILL);
    }

    /// Reaps the leader; with `block` waits for it. No-op when reaped.
    fn reap(p: *Proc, block: bool) void {
        while (!p.reaped) {
            var status: c_int = 0;
            const rc = posix.system.waitpid(p.pid, &status, if (block) 0 else posix.W.NOHANG);
            switch (posix.errno(rc)) {
                .SUCCESS => {
                    if (rc == 0) return; // still running
                    p.reaped = true;
                    const st: u32 = @bitCast(status);
                    p.exit_code = if (posix.W.IFEXITED(st)) posix.W.EXITSTATUS(st) else -1;
                },
                .INTR => continue,
                else => p.reaped = true, // ECHILD: nothing left to wait for
            }
        }
    }

    /// Leaves nothing behind on any path: pipes closed, leader reaped (a
    /// leader still alive here was never reaped because of an error, and
    /// dies with its group).
    fn finish(p: *Proc) void {
        closeFd(&p.stdin);
        closeFd(&p.stdout);
        closeFd(&p.stderr);
        if (!p.reaped) {
            p.killGroup();
            p.reap(true);
        }
    }
};

const Outcome = enum { done, timeout, canceled };

/// Keeps the first `limit` bytes and drops the rest.
const Sink = struct {
    buf: std.ArrayList(u8) = .empty,
    limit: usize,
    dropped: bool = false,

    fn add(s: *Sink, gpa: Allocator, data: []const u8) Allocator.Error!void {
        const room = s.limit -| s.buf.items.len;
        const n = @min(room, data.len);
        if (n > 0) try s.buf.appendSlice(gpa, data[0..n]);
        if (n < data.len) s.dropped = true;
    }
};

fn nowMs(io: Io) i64 {
    return Io.Clock.now(.awake, io).toMilliseconds();
}

fn drive(gpa: Allocator, io: Io, p: *Proc, stdin: []const u8, timeout_ms: i64, out: *Sink, err_sink: *Sink) Error!Outcome {
    var outcome: Outcome = .done;
    var stdin_off: usize = 0;
    if (stdin.len == 0) Proc.closeFd(&p.stdin);
    const deadline = nowMs(io) +| timeout_ms;
    var grace_end: ?i64 = null;
    var buf: [read_chunk]u8 = undefined;
    // With no pipe left only the leader's exit is awaited: look often at
    // first (it is usually a few microseconds away), then settle at a tick.
    var idle_ms: i32 = 1;

    while (true) {
        p.reap(false);
        if (p.reaped and p.stdout < 0 and p.stderr < 0) break;
        const now = nowMs(io);

        if (outcome == .done) {
            if (io.checkCancel()) {} else |_| {
                outcome = .canceled;
                p.killGroup();
            }
        }
        if (outcome == .done and !p.reaped and now >= deadline) {
            outcome = .timeout;
            p.killGroup();
        }
        if (p.reaped and outcome == .canceled) break;
        if (grace_end == null and (p.reaped or outcome != .done)) grace_end = now + kill_grace_ms;
        if (grace_end) |g| if (now >= g) break;

        var fds: [3]posix.pollfd = undefined;
        var which: [3]enum { stdin, stdout, stderr } = undefined;
        var n: usize = 0;
        if (p.stdin >= 0) {
            fds[n] = .{ .fd = p.stdin, .events = posix.POLL.OUT, .revents = 0 };
            which[n] = .stdin;
            n += 1;
        }
        if (p.stdout >= 0) {
            fds[n] = .{ .fd = p.stdout, .events = posix.POLL.IN, .revents = 0 };
            which[n] = .stdout;
            n += 1;
        }
        if (p.stderr >= 0) {
            fds[n] = .{ .fd = p.stderr, .events = posix.POLL.IN, .revents = 0 };
            which[n] = .stderr;
            n += 1;
        }
        // With no pipe left this is a sleep until the leader exits.
        const wait_ms = if (n == 0) idle_ms else tick_ms;
        if (n == 0) idle_ms = @min(idle_ms * 2, tick_ms) else idle_ms = 1;
        _ = posix.poll(fds[0..n], wait_ms) catch return error.OutOfMemory;

        for (fds[0..n], which[0..n]) |f, w| {
            if (f.revents == 0) continue;
            switch (w) {
                .stdin => feed(p, stdin, &stdin_off),
                .stdout => try drain(gpa, &p.stdout, &buf, out),
                .stderr => try drain(gpa, &p.stderr, &buf, err_sink),
            }
        }
    }
    return outcome;
}

/// One write of the rest of stdin; closes the pipe when it is all written or
/// the program is gone (EPIPE is normal for a program that ignores stdin).
fn feed(p: *Proc, data: []const u8, off: *usize) void {
    while (off.* < data.len) {
        const rc = posix.system.write(p.stdin, data[off.*..].ptr, data.len - off.*);
        switch (posix.errno(rc)) {
            .SUCCESS => off.* += @intCast(rc),
            .INTR => continue,
            .AGAIN => return,
            else => {
                Proc.closeFd(&p.stdin);
                return;
            },
        }
    }
    Proc.closeFd(&p.stdin);
}

/// One read into the sink; closes the pipe at the end of the stream.
fn drain(gpa: Allocator, fd: *posix.fd_t, buf: []u8, sink: *Sink) Allocator.Error!void {
    const n = posix.read(fd.*, buf) catch |err| switch (err) {
        error.WouldBlock => return,
        else => {
            Proc.closeFd(fd);
            return;
        },
    };
    if (n == 0) {
        Proc.closeFd(fd);
        return;
    }
    try sink.add(gpa, buf[0..n]);
}

fn setNonBlocking(fd: posix.fd_t) void {
    const flags = std.c.fcntl(fd, posix.F.GETFL);
    if (flags < 0) return;
    var o: posix.O = @bitCast(@as(@typeInfo(posix.O).@"struct".backing_integer.?, @intCast(flags)));
    o.NONBLOCK = true;
    _ = std.c.fcntl(fd, posix.F.SETFL, @as(c_int, @intCast(@as(@typeInfo(posix.O).@"struct".backing_integer.?, @bitCast(o)))));
}

// ---------------------------------------------------------------- tests

/// Finds a pinned tool (`7zz`, `par2`) for a test: `bin/tools/<name>` at the
/// repository root (what `make tools` fills for the host platform), searched
/// upwards from the working directory, else PATH. A missing program fails the
/// test instead of skipping it. The caller frees the absolute path.
pub fn pinned(gpa: Allocator, io: Io, name: []const u8) ![]u8 {
    if (!builtin.is_test) @compileError("pinned is a test helper");
    var rel: std.ArrayList(u8) = .empty;
    defer rel.deinit(gpa);
    var depth: usize = 0;
    while (depth < 8) : (depth += 1) {
        rel.clearRetainingCapacity();
        for (0..depth) |_| try rel.appendSlice(gpa, "../");
        try rel.appendSlice(gpa, "bin/tools/");
        try rel.appendSlice(gpa, name);
        const real = Io.Dir.cwd().realPathFileAlloc(io, rel.items, gpa) catch continue;
        defer gpa.free(real);
        if (isExecutable(io, real)) return gpa.dupe(u8, real);
    }
    return locate(gpa, io, name) catch {
        std.debug.print("{s} not found: run `make tools` (or put it on PATH)\n", .{name});
        return error.ToolMissing;
    };
}
