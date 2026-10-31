//! Tests of the tool runner. The stand-in programs are /bin/sh scripts and
//! coreutils; the runner itself never goes through a shell.
const std = @import("std");
const testing = std.testing;
const posix = std.posix;
const Io = std.Io;
const tool = @import("tool.zig");

const sh = "/bin/sh";

extern "c" fn setenv(name: [*:0]const u8, value: [*:0]const u8, overwrite: c_int) c_int;
extern "c" fn unsetenv(name: [*:0]const u8) c_int;

fn script(body: []const u8) [3][]const u8 {
    return .{ "-c", body, "tool-test" };
}

/// Runs `/bin/sh -c body` with the other fields of `spec`; the caller frees.
fn sh_run(body: []const u8, spec: tool.Spec) tool.Error!tool.Result {
    var s = spec;
    s.path = sh;
    s.args = &script(body);
    return tool.run(testing.allocator, testing.io, s);
}

fn free(r: tool.Result) void {
    tool.freeResult(testing.allocator, r);
}

fn nowMs() i64 {
    return Io.Clock.now(.awake, testing.io).toMilliseconds();
}

fn sleepMs(ms: i64) !void {
    try testing.io.sleep(.fromMilliseconds(ms), .awake);
}

fn openFds() usize {
    var n: usize = 0;
    var fd: c_int = 0;
    while (fd < 1024) : (fd += 1) {
        if (std.c.fcntl(fd, posix.F.GETFD) >= 0) n += 1;
    }
    return n;
}

fn exists(dir: Io.Dir, name: []const u8) bool {
    _ = dir.statFile(testing.io, name, .{}) catch return false;
    return true;
}

// ------------------------------------------------------------ basics

test "reports the exit code without an error" {
    const r = try sh_run("exit 7", .{ .path = "" });
    defer free(r);
    try testing.expectEqual(@as(i32, 7), r.exit_code);
    try testing.expect(!r.truncated);
}

test "a program killed by a signal reports -1" {
    const r = try sh_run("kill -9 $$", .{ .path = "" });
    defer free(r);
    try testing.expectEqual(@as(i32, -1), r.exit_code);
}

test "keeps stdout and stderr apart" {
    const r = try sh_run("echo out; echo err >&2; exit 0", .{ .path = "" });
    defer free(r);
    try testing.expectEqualStrings("out\n", r.stdout);
    try testing.expectEqualStrings("err\n", r.stderr);
}

test "takes the arguments as a list, with no shell between" {
    const r = try tool.run(testing.allocator, testing.io, .{ .path = "/usr/bin/printf", .args = &.{ "%s|", "a b", "$HOME", "x;y", "*" } });
    defer free(r);
    try testing.expectEqualStrings("a b|$HOME|x;y|*|", r.stdout);
}

test "a bare program name is looked up on PATH" {
    const r = try tool.run(testing.allocator, testing.io, .{ .path = "sh", .args = &script("exit 6") });
    defer free(r);
    try testing.expectEqual(@as(i32, 6), r.exit_code);
    try testing.expectError(error.StartFailed, tool.run(testing.allocator, testing.io, .{ .path = "no-such-program-xyz" }));
}

// ------------------------------------------------------------ environment

test "the environment is scrubbed to four variables" {
    _ = setenv("BITNEWS_TOOL_SECRET", "hunter2", 1);
    defer _ = unsetenv("BITNEWS_TOOL_SECRET");
    const old_tmp = std.c.getenv("TMPDIR");
    var saved: [256]u8 = undefined;
    var saved_len: ?usize = null;
    if (old_tmp) |t| {
        const v = std.mem.span(t);
        @memcpy(saved[0..v.len], v);
        saved_len = v.len;
    }
    _ = setenv("TMPDIR", "/var/tmp/daemon", 1);
    defer if (saved_len) |n| {
        saved[n] = 0;
        _ = setenv("TMPDIR", @ptrCast(saved[0..n :0]), 1);
    } else {
        _ = unsetenv("TMPDIR");
    };

    // TMPDIR from the spec wins; the daemon's is used otherwise.
    const r = try tool.run(testing.allocator, testing.io, .{ .path = "/usr/bin/env", .temp_dir = "/work/tmp" });
    defer free(r);
    try testing.expect(std.mem.indexOf(u8, r.stdout, "SECRET") == null);
    try testing.expect(std.mem.indexOf(u8, r.stdout, "PATH") == null);
    var lines: std.ArrayList([]const u8) = .empty;
    defer lines.deinit(testing.allocator);
    var it = std.mem.tokenizeScalar(u8, r.stdout, '\n');
    while (it.next()) |l| try lines.append(testing.allocator, l);
    try testing.expectEqual(@as(usize, 4), lines.items.len);
    for ([_][]const u8{ "LC_ALL=C", "LANG=C", "HOME=/nonexistent", "TMPDIR=/work/tmp" }) |want| {
        var found = false;
        for (lines.items) |l| found = found or std.mem.eql(u8, l, want);
        try testing.expect(found);
    }

    const r2 = try tool.run(testing.allocator, testing.io, .{ .path = "/usr/bin/env" });
    defer free(r2);
    try testing.expect(std.mem.indexOf(u8, r2.stdout, "TMPDIR=/var/tmp/daemon\n") != null);
}

test "runs in the working directory" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir = try tmp.dir.realPathFileAlloc(testing.io, ".", testing.allocator);
    defer testing.allocator.free(dir);

    const r = try sh_run("echo here > marker", .{ .path = "", .dir = dir });
    defer free(r);
    try testing.expectEqual(@as(i32, 0), r.exit_code);
    try testing.expect(exists(tmp.dir, "marker"));
}

// ------------------------------------------------------------ stdin

test "feeds stdin and closes it" {
    const r = try tool.run(testing.allocator, testing.io, .{ .path = "/bin/cat", .stdin = "pw\n" });
    defer free(r);
    try testing.expectEqualStrings("pw\n", r.stdout);
}

test "an empty stdin is an immediate end of input" {
    const r = try tool.run(testing.allocator, testing.io, .{ .path = "/bin/cat" });
    defer free(r);
    try testing.expectEqualStrings("", r.stdout);
    try testing.expectEqual(@as(i32, 0), r.exit_code);
}

test "a program that never reads stdin does not hang or crash us" {
    const big = try testing.allocator.alloc(u8, 4 << 20);
    defer testing.allocator.free(big);
    @memset(big, 'x');
    const r = try sh_run("exit 0", .{ .path = "", .stdin = big });
    defer free(r);
    try testing.expectEqual(@as(i32, 0), r.exit_code);
}

test "a large stdin and a large stdout at once do not deadlock" {
    const big = try testing.allocator.alloc(u8, 3 << 20);
    defer testing.allocator.free(big);
    for (big, 0..) |*b, i| b.* = @truncate(i *% 31);
    const r = try tool.run(testing.allocator, testing.io, .{ .path = "/bin/cat", .stdin = big, .max_output = 4 << 20 });
    defer free(r);
    try testing.expectEqualSlices(u8, big, r.stdout);
}

// ------------------------------------------------------------ output

test "caps the output and keeps draining" {
    const r = try sh_run("head -c 4000000 /dev/zero", .{ .path = "", .max_output = 1000 });
    defer free(r);
    try testing.expectEqual(@as(usize, 1000), r.stdout.len);
    try testing.expect(r.truncated);
    try testing.expectEqual(@as(i32, 0), r.exit_code);
}

test "the cap applies to stderr too, and an exact fit is not truncated" {
    const r = try sh_run("head -c 50 /dev/zero >&2; head -c 10 /dev/zero", .{ .path = "", .max_output = 10 });
    defer free(r);
    try testing.expectEqual(@as(usize, 10), r.stdout.len);
    try testing.expectEqual(@as(usize, 10), r.stderr.len);
    try testing.expect(r.truncated);

    const exact = try sh_run("head -c 10 /dev/zero", .{ .path = "", .max_output = 10 });
    defer free(exact);
    try testing.expect(!exact.truncated);
}

test "large output on both streams at once does not deadlock" {
    const n = 8_000_000;
    const r = try sh_run("head -c 8000000 /dev/zero & head -c 8000000 /dev/zero >&2; wait", .{ .path = "", .max_output = 16 << 20 });
    defer free(r);
    try testing.expectEqual(@as(usize, n), r.stdout.len);
    try testing.expectEqual(@as(usize, n), r.stderr.len);
    try testing.expect(!r.truncated);
}

test "a program that closes its output and keeps running is waited for" {
    const r = try sh_run("exec >&- 2>&-; sleep 0.2; exit 4", .{ .path = "" });
    defer free(r);
    try testing.expectEqual(@as(i32, 4), r.exit_code);
}

// ------------------------------------------------------------ timeout and cancel

test "a timeout kills the program and everything it started" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir = try tmp.dir.realPathFileAlloc(testing.io, ".", testing.allocator);
    defer testing.allocator.free(dir);

    const t0 = nowMs();
    // The grandchild would write the file after one second if it lived.
    try testing.expectError(error.Timeout, sh_run("(sleep 1; echo alive > grandchild) & sleep 60", .{ .path = "", .dir = dir, .timeout_ms = 200 }));
    try testing.expect(nowMs() - t0 < 3000);
    try sleepMs(1500);
    try testing.expect(!exists(tmp.dir, "grandchild"));
}

const Job = struct {
    fn go(spec: tool.Spec) tool.Error!tool.Result {
        return tool.run(testing.allocator, testing.io, spec);
    }
};

test "cancelling kills the whole process group" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir = try tmp.dir.realPathFileAlloc(testing.io, ".", testing.allocator);
    defer testing.allocator.free(dir);

    const spec: tool.Spec = .{ .path = sh, .args = &script("(sleep 1; echo alive > grandchild) & sleep 60"), .dir = dir, .timeout_ms = 30_000 };
    var fut = try testing.io.concurrent(Job.go, .{spec});
    try sleepMs(200); // let it start; an early cancel can be lost (Z3 findings)
    const t0 = nowMs();
    const res = fut.cancel(testing.io);
    try testing.expectError(error.Canceled, res);
    try testing.expect(nowMs() - t0 < 2000);
    try sleepMs(1500);
    try testing.expect(!exists(tmp.dir, "grandchild"));
}

test "a grandchild holding the pipes cannot hang the run" {
    const old = tool.kill_grace_ms;
    tool.kill_grace_ms = 300;
    defer tool.kill_grace_ms = old;

    const t0 = nowMs();
    // The sleep keeps stdout open after the leader exits.
    const r = try sh_run("sleep 4 & echo hi; exit 3", .{ .path = "" });
    defer free(r);
    const took = nowMs() - t0;
    try testing.expectEqual(@as(i32, 3), r.exit_code);
    try testing.expectEqualStrings("hi\n", r.stdout);
    try testing.expect(took >= 250 and took < 3000);
}

test "a program that ignores SIGTERM is still killed on timeout" {
    try testing.expectError(error.Timeout, sh_run("trap '' TERM; while :; do sleep 1; done", .{ .path = "", .timeout_ms = 200 }));
}

// ------------------------------------------------------------ start failure

test "reports a program that cannot be started" {
    try testing.expectError(error.StartFailed, tool.run(testing.allocator, testing.io, .{ .path = "/nonexistent/prog" }));
    try testing.expectError(error.StartFailed, tool.run(testing.allocator, testing.io, .{ .path = "" }));
    try testing.expectError(error.StartFailed, tool.run(testing.allocator, testing.io, .{ .path = "/bin/sh", .dir = "/nonexistent/dir" }));
    try testing.expectError(error.StartFailed, tool.run(testing.allocator, testing.io, .{ .path = "/etc/passwd" }));
}

// ------------------------------------------------------------ leaks and stress

test "1000 sequential runs leak no descriptors and no zombies" {
    // Warm up so lazily opened descriptors (/dev/null cache) are counted.
    free(try tool.run(testing.allocator, testing.io, .{ .path = "/usr/bin/true" }));
    const before = openFds();
    for (0..1000) |i| {
        const r = if (i % 10 == 0)
            try sh_run("echo x; echo y >&2; exit 2", .{ .path = "" })
        else
            try tool.run(testing.allocator, testing.io, .{ .path = "/usr/bin/true" });
        free(r);
    }
    try testing.expectEqual(before, openFds());
    // No child of ours is left, running or zombie.
    var status: c_int = 0;
    const rc = std.c.waitpid(-1, &status, posix.W.NOHANG);
    try testing.expectEqual(@as(posix.pid_t, -1), rc);
    try testing.expectEqual(posix.E.CHILD, posix.errno(rc));
}

test "failed runs leak no descriptors either" {
    free(try tool.run(testing.allocator, testing.io, .{ .path = "/usr/bin/true" }));
    const before = openFds();
    for (0..100) |_| {
        try testing.expectError(error.StartFailed, tool.run(testing.allocator, testing.io, .{ .path = "/nonexistent/prog" }));
        try testing.expectError(error.Timeout, sh_run("sleep 5", .{ .path = "", .timeout_ms = 20 }));
    }
    try testing.expectEqual(before, openFds());
}

const Stress = struct {
    failures: std.atomic.Value(u32) = .init(0),

    fn one(s: *Stress, i: usize) void {
        var num: [16]u8 = undefined;
        const text = std.fmt.bufPrint(&num, "{d}", .{i}) catch unreachable;
        const args = [_][]const u8{ "-c", "echo \"run-$0\"; head -c 200000 /dev/zero >&2; cat", text };
        const r = tool.run(testing.allocator, testing.io, .{ .path = sh, .args = &args, .stdin = "in", .max_output = 100_000 }) catch {
            _ = s.failures.fetchAdd(1, .monotonic);
            return;
        };
        defer free(r);
        var want: [32]u8 = undefined;
        const w = std.fmt.bufPrint(&want, "run-{d}\nin", .{i}) catch unreachable;
        if (!std.mem.eql(u8, r.stdout, w) or r.stderr.len != 100_000 or !r.truncated or r.exit_code != 0)
            _ = s.failures.fetchAdd(1, .monotonic);
    }
};

test "50 concurrent runs" {
    var stress: Stress = .{};
    var threads: [50]std.Thread = undefined;
    for (&threads, 0..) |*t, i| t.* = try std.Thread.spawn(.{}, Stress.one, .{ &stress, i });
    for (threads) |t| t.join();
    try testing.expectEqual(@as(u32, 0), stress.failures.load(.monotonic));
}

// ------------------------------------------------------------ the Runner seam

test "the system runner runs through the Runner interface" {
    var sys: tool.SystemRunner = .{};
    const runner = sys.runner();
    const r = try runner.run(testing.allocator, testing.io, .{ .path = "/bin/cat", .stdin = "via seam" });
    defer free(r);
    try testing.expectEqualStrings("via seam", r.stdout);

    const r2 = try tool.system().run(testing.allocator, testing.io, .{ .path = sh, .args = &script("exit 5") });
    defer free(r2);
    try testing.expectEqual(@as(i32, 5), r2.exit_code);
}

// ------------------------------------------------------------ locate

test "locate finds a program on PATH" {
    const p = try tool.locateIn(testing.allocator, testing.io, "/nonexistent:/bin:/usr/bin", "sh");
    defer testing.allocator.free(p);
    try testing.expect(std.mem.endsWith(u8, p, "/sh"));
    try testing.expect(std.fs.path.isAbsolute(p));

    const real = try tool.locate(testing.allocator, testing.io, "sh");
    testing.allocator.free(real);
}

test "locate skips relative PATH entries, directories and non-programs" {
    try testing.expectError(error.NotFound, tool.locateIn(testing.allocator, testing.io, "bin:.::", "sh"));
    try testing.expectError(error.NotFound, tool.locateIn(testing.allocator, testing.io, "/", "etc")); // a directory
    try testing.expectError(error.NotFound, tool.locateIn(testing.allocator, testing.io, "/etc", "passwd")); // not executable
    try testing.expectError(error.NotFound, tool.locateIn(testing.allocator, testing.io, "/bin", ""));
}

test "locate names the missing program" {
    try testing.expectError(error.NotFound, tool.locateIn(testing.allocator, testing.io, "", "7zz"));
    const msg = try tool.notFoundMessage(testing.allocator, "7zz");
    defer testing.allocator.free(msg);
    try testing.expectEqualStrings("tool: program not found: 7zz (install it, or run `make tools` and add bin/tools to PATH; see docs/deploy.md)", msg);
}

test "locate takes an absolute path as is" {
    const p = try tool.locateIn(testing.allocator, testing.io, "", "/bin/sh");
    defer testing.allocator.free(p);
    try testing.expectEqualStrings("/bin/sh", p);
    try testing.expectError(error.NotFound, tool.locateIn(testing.allocator, testing.io, "/bin", "./sh"));
}

test "pinned finds the tools make tools installs" {
    const p = try tool.pinned(testing.allocator, testing.io, "par2");
    defer testing.allocator.free(p);
    try testing.expect(std.fs.path.isAbsolute(p));
}
