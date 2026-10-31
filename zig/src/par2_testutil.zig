//! Test only: what the par2 tests share. A directory in std.testing.tmpDir
//! (nothing is written outside it), a scripted fake runner, and
//! sets made by the real `par2 c` (the pure-Go PAR2 builder of the Go tests
//! has no counterpart: par2 makes the sets).
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const Io = std.Io;
const tool = @import("tool.zig");
const ports = @import("postprocess_ports.zig");
const par2 = @import("par2.zig");

// ---- finding the pinned program ----

/// The pinned par2 (bin/tools/par2 at the repository root, what `make tools`
/// fills for the host platform). When it cannot run here (a macOS binary in
/// the Debian container, or `make tools` not run) the real-par2 tests skip
/// with a message; the lead runs them where the binary fits. The result lives
/// in `a`.
pub fn par2Path(a: Allocator) ![]const u8 {
    const skip = "SKIP: no runnable bin/tools/par2 on this platform (run `make tools`, or scripts/get-postproc-tools.sh in Debian)\n";
    const path = tool.pinned(a, testing.io, "par2") catch {
        std.debug.print(skip, .{});
        return error.SkipZigTest;
    };
    // A binary of another platform is found but cannot start.
    const res = tool.system().run(a, testing.io, .{ .path = path, .args = &.{"-V"} }) catch {
        std.debug.print(skip, .{});
        return error.SkipZigTest;
    };
    if (res.exit_code != 0) {
        std.debug.print(skip, .{});
        return error.SkipZigTest;
    }
    return path;
}

// ---- a scripted fake runner ----

/// Returns what the test says and records what it was asked. The recorded
/// strings are copies in the Fake's own arena.
pub const Fake = struct {
    arena: std.heap.ArenaAllocator,
    /// What the next call returns; `fail` wins when set.
    exit_code: i32 = 0,
    stdout: []const u8 = "",
    stderr: []const u8 = "",
    fail: ?tool.Error = null,
    calls: usize = 0,
    path: []const u8 = "",
    args: []const []const u8 = &.{},
    dir: []const u8 = "",
    stdin: []const u8 = "",

    pub fn init() Fake {
        return .{ .arena = .init(testing.allocator) };
    }

    pub fn deinit(f: *Fake) void {
        f.arena.deinit();
    }

    pub fn runner(f: *Fake) tool.Runner {
        return .{ .ctx = f, .run_fn = run };
    }

    fn run(ctx: *anyopaque, gpa: Allocator, _: Io, spec: tool.Spec) tool.Error!tool.Result {
        const f: *Fake = @ptrCast(@alignCast(ctx));
        f.calls += 1;
        const a = f.arena.allocator();
        f.path = a.dupe(u8, spec.path) catch return error.OutOfMemory;
        const args = a.alloc([]const u8, spec.args.len) catch return error.OutOfMemory;
        for (spec.args, args) |s, *d| d.* = a.dupe(u8, s) catch return error.OutOfMemory;
        f.args = args;
        f.dir = a.dupe(u8, spec.dir) catch return error.OutOfMemory;
        f.stdin = a.dupe(u8, spec.stdin) catch return error.OutOfMemory;
        if (f.fail) |e| return e;
        return .{
            .exit_code = f.exit_code,
            .stdout = gpa.dupe(u8, f.stdout) catch return error.OutOfMemory,
            .stderr = gpa.dupe(u8, f.stderr) catch return error.OutOfMemory,
        };
    }
};

// ---- a directory under test ----

/// A temp directory, its absolute path and an arena for the test's strings.
/// Does not move after `init`.
pub const Env = struct {
    tmp: testing.TmpDir,
    arena: std.heap.ArenaAllocator,
    /// The absolute path of the temp directory.
    root: []const u8,
    /// Original contents by name, for sets made by `makeSet`.
    files: std.StringArrayHashMapUnmanaged([]const u8) = .empty,
    par2: []const u8 = "",

    pub fn init(env: *Env) !void {
        env.* = .{ .tmp = testing.tmpDir(.{ .iterate = true }), .arena = .init(testing.allocator), .root = "" };
        errdefer env.tmp.cleanup();
        errdefer env.arena.deinit();
        var buf: [Io.Dir.max_path_bytes]u8 = undefined;
        env.root = try env.arena.allocator().dupe(u8, buf[0..try env.tmp.dir.realPath(testing.io, &buf)]);
    }

    pub fn deinit(env: *Env) void {
        env.arena.deinit();
        env.tmp.cleanup();
    }

    pub fn a(env: *Env) Allocator {
        return env.arena.allocator();
    }

    /// The absolute path of `rel` below the temp directory.
    pub fn path(env: *Env, rel: []const u8) ![]const u8 {
        return std.fmt.allocPrint(env.a(), "{s}/{s}", .{ env.root, rel });
    }

    pub fn write(env: *Env, rel: []const u8, data: []const u8) !void {
        if (std.fs.path.dirname(rel)) |parent| try env.tmp.dir.createDirPath(testing.io, parent);
        try env.tmp.dir.writeFile(testing.io, .{ .sub_path = rel, .data = data });
    }

    pub fn read(env: *Env, rel: []const u8) ![]u8 {
        return env.tmp.dir.readFileAlloc(testing.io, rel, env.a(), .limited(64 << 20));
    }

    pub fn remove(env: *Env, rel: []const u8) !void {
        try env.tmp.dir.deleteFile(testing.io, rel);
    }

    pub fn rename(env: *Env, from: []const u8, to: []const u8) !void {
        try env.tmp.dir.rename(from, env.tmp.dir, to, testing.io);
    }

    pub fn exists(env: *Env, rel: []const u8) bool {
        _ = env.tmp.dir.statFile(testing.io, rel, .{ .follow_symlinks = false }) catch return false;
        return true;
    }

    /// Flips every bit of the byte at each offset.
    pub fn corrupt(env: *Env, rel: []const u8, offsets: []const usize) !void {
        const b = try env.read(rel);
        for (offsets) |o| b[o] ^= 0xff;
        try env.write(rel, b);
    }

    pub fn truncate(env: *Env, rel: []const u8, size: usize) !void {
        const b = try env.read(rel);
        try env.write(rel, b[0..size]);
    }

    pub fn fileSize(env: *Env, rel: []const u8) !u64 {
        return (try env.tmp.dir.statFile(testing.io, rel, .{})).size;
    }

    /// The names (slash separated, relative) of the regular files below the
    /// temp directory, sorted.
    pub fn listing(env: *Env) ![]const []const u8 {
        var out: std.ArrayList([]const u8) = .empty;
        var w = try env.tmp.dir.walk(env.a());
        defer w.deinit();
        while (try w.next(testing.io)) |ent| {
            if (ent.kind == .directory) continue;
            try out.append(env.a(), try env.a().dupe(u8, ent.path));
        }
        std.mem.sort([]const u8, out.items, {}, lessString);
        return out.items;
    }

    /// Every entry below the temp directory with its content: equal snapshots
    /// mean nothing was created, moved or changed.
    pub fn snapshot(env: *Env) ![]const u8 {
        var out: std.ArrayList(u8) = .empty;
        var entries: std.ArrayList([]const u8) = .empty;
        var w = try env.tmp.dir.walk(env.a());
        defer w.deinit();
        while (try w.next(testing.io)) |ent| {
            const e = switch (ent.kind) {
                .directory => try std.fmt.allocPrint(env.a(), "{s}/", .{ent.path}),
                .file => try std.fmt.allocPrint(env.a(), "{s}\x00{s}", .{ ent.path, try env.read(ent.path) }),
                else => try std.fmt.allocPrint(env.a(), "{s}\x00(special)", .{ent.path}),
            };
            try entries.append(env.a(), e);
        }
        std.mem.sort([]const u8, entries.items, {}, lessString);
        for (entries.items) |e| {
            try out.appendSlice(env.a(), e);
            try out.append(env.a(), '\n');
        }
        return out.items;
    }

    pub fn expectListing(env: *Env, want: []const []const u8) !void {
        const sorted = try env.a().dupe([]const u8, want);
        std.mem.sort([]const u8, sorted, {}, lessString);
        const got = try env.listing();
        testing.expectEqual(sorted.len, got.len) catch |e| {
            printLists(sorted, got);
            return e;
        };
        for (sorted, got) |w, g| testing.expectEqualStrings(w, g) catch |e| {
            printLists(sorted, got);
            return e;
        };
    }

    // ---- sets made by par2 c ----

    pub const Spec = struct { name: []const u8, size: usize };

    /// Writes the files (sizes seed pseudo-random contents), runs
    /// `par2 c -s1000 -c<recovery> -n2` as set.par2 with two volumes, and
    /// remembers the contents. Names may hold a subdirectory.
    pub fn makeSet(env: *Env, recovery: usize, specs: []const Spec) !void {
        env.par2 = try par2Path(env.a());
        var args: std.ArrayList([]const u8) = .empty;
        try args.appendSlice(env.a(), &.{
            "c",
            try std.fmt.allocPrint(env.a(), "-B{s}", .{env.root}),
            try std.fmt.allocPrint(env.a(), "-s{d}", .{block_size}),
            try std.fmt.allocPrint(env.a(), "-c{d}", .{recovery}),
            "-n2",
            "-q",
            "--",
            "set.par2",
        });
        var seed: u64 = 1;
        for (specs) |s| {
            const data = payload(env.a(), seed, s.size);
            seed += 1;
            try env.write(s.name, data);
            try env.files.put(env.a(), s.name, data);
            try args.append(env.a(), try env.path(s.name));
        }
        const res = try tool.system().run(env.a(), testing.io, .{ .path = env.par2, .args = args.items, .dir = env.root });
        if (res.exit_code != 0) std.debug.print("par2 c: {s}\n", .{res.stdout});
        try testing.expectEqual(0, res.exit_code);
    }

    /// Every original file has its original bytes.
    pub fn requireOriginals(env: *Env) !void {
        var it = env.files.iterator();
        while (it.next()) |kv| {
            const got = env.read(kv.key_ptr.*) catch |e| {
                std.debug.print("original {s}: {s}\n", .{ kv.key_ptr.*, @errorName(e) });
                return e;
            };
            testing.expectEqualSlices(u8, kv.value_ptr.*, got) catch |e| {
                std.debug.print("original {s} differs\n", .{kv.key_ptr.*});
                return e;
            };
        }
    }

    /// A Repairer that runs the pinned par2 with the real runner.
    pub fn repairer(env: *Env) !par2.Repairer {
        if (env.par2.len == 0) env.par2 = try par2Path(env.a());
        return .{ .runner = tool.system(), .path = env.par2, .threads = 2, .memory_mb = 64 };
    }
};

/// Block size of the test sets: small, so par2 is fast.
pub const block_size = 1000;

fn lessString(_: void, a: []const u8, b: []const u8) bool {
    return std.mem.lessThan(u8, a, b);
}

fn printLists(want: []const []const u8, got: []const []const u8) void {
    std.debug.print("want:", .{});
    for (want) |w| std.debug.print(" {s}", .{w});
    std.debug.print("\ngot: ", .{});
    for (got) |g| std.debug.print(" {s}", .{g});
    std.debug.print("\n", .{});
}

/// `n` deterministic pseudo-random bytes in `a`.
pub fn payload(a: Allocator, seed: u64, n: usize) []u8 {
    var prng: std.Random.DefaultPrng = .init(seed);
    const b = a.alloc(u8, n) catch @panic("oom");
    prng.random().bytes(b);
    return b;
}

// ---- one repair ----

/// The result of one `repair` call with the arena its strings live in.
pub const Run = struct {
    arena: std.heap.ArenaAllocator,
    diag: ports.Diag = .{},
    result: ports.PortError!ports.RepairOutcome = error.Failed,

    pub fn deinit(r: *Run) void {
        r.arena.deinit();
    }

    pub fn outcome(r: *Run) !ports.RepairOutcome {
        return r.result;
    }
};

/// Runs the repairer on `dir` in a fresh arena (over the leak-checking
/// allocator). Always `defer run.deinit()`.
pub fn repairOf(rep: *const par2.Repairer, dir: []const u8) Run {
    var r: Run = .{ .arena = .init(testing.allocator) };
    r.result = rep.repair(r.arena.allocator(), testing.io, dir, &r.diag);
    return r;
}
