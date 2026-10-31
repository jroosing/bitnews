//! Helpers of the unpack tests: an `Env` on the test allocators, a temporary
//! directory with file helpers, a scripted fake of 7zz, and the real runner
//! with the pinned 7zz (bin/tools/7zz). Only tests import this file.
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const Io = std.Io;
const Dir = Io.Dir;
const ports = @import("postprocess_ports.zig");
const tool = @import("tool.zig");
const tool_types = @import("tool_types.zig");
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;

pub const gpa = testing.allocator;
pub const io = testing.io;

pub const Rig = struct {
    arena_state: std.heap.ArenaAllocator,
    diag: ports.Diag = .{},
    env: Env,
    tmp: testing.TmpDir,
    /// The absolute path of the temporary directory.
    root: []const u8,

    /// A rig on the real 7zz (`bin/tools/7zz`).
    pub fn real(cfg_in: env_mod.Config) !*Rig {
        const z = try tool.pinned(gpa, io, "7zz");
        defer gpa.free(z);
        var cfg = cfg_in;
        const rig = try init(tool.system(), cfg);
        cfg.seven_zip = try rig.arena().dupe(u8, z);
        rig.env.cfg = cfg;
        return rig;
    }

    pub fn init(runner: tool_types.Runner, cfg: env_mod.Config) !*Rig {
        const rig = try gpa.create(Rig);
        rig.* = .{
            .arena_state = .init(gpa),
            .env = undefined,
            .tmp = testing.tmpDir(.{ .iterate = true }),
            .root = undefined,
        };
        var buf: [Dir.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(io, &buf);
        rig.root = try rig.arena().dupe(u8, buf[0..n]);
        rig.env = .{ .io = io, .gpa = gpa, .arena = rig.arena(), .runner = runner, .cfg = cfg, .diag = &rig.diag };
        return rig;
    }

    pub fn deinit(rig: *Rig) void {
        rig.tmp.cleanup();
        rig.arena_state.deinit();
        gpa.destroy(rig);
    }

    pub fn arena(rig: *Rig) Allocator {
        return rig.arena_state.allocator();
    }

    /// root/rel as an absolute path.
    pub fn path(rig: *Rig, rel: []const u8) []const u8 {
        return std.fs.path.join(rig.arena(), &.{ rig.root, rel }) catch @panic("oom");
    }

    pub fn write(rig: *Rig, rel: []const u8, bytes: []const u8) !void {
        if (std.fs.path.dirname(rel)) |d| try rig.tmp.dir.createDirPath(io, d);
        try rig.tmp.dir.writeFile(io, .{ .sub_path = rel, .data = bytes });
    }

    pub fn mkdir(rig: *Rig, rel: []const u8) !void {
        try rig.tmp.dir.createDirPath(io, rel);
    }

    pub fn read(rig: *Rig, rel: []const u8) ![]u8 {
        return rig.tmp.dir.readFileAlloc(io, rel, rig.arena(), .limited(64 << 20));
    }

    pub fn exists(rig: *Rig, rel: []const u8) bool {
        _ = rig.tmp.dir.statFile(io, rel, .{ .follow_symlinks = false }) catch return false;
        return true;
    }

    /// Copies testdata/unpack/<src> (the tests run in zig/) to root/<dst>.
    pub fn fixture(rig: *Rig, src: []const u8, dst: []const u8) !void {
        const p = try std.fmt.allocPrint(rig.arena(), "testdata/unpack/{s}", .{src});
        const bytes = try Dir.cwd().readFileAlloc(io, p, rig.arena(), .limited(8 << 20));
        try rig.write(dst, bytes);
    }

    /// Every path below root/rel (directories included), relative to it,
    /// sorted.
    pub fn tree(rig: *Rig, rel: []const u8) ![][]const u8 {
        const a = rig.arena();
        var d = try rig.tmp.dir.openDir(io, rel, .{ .iterate = true });
        defer d.close(io);
        var w = try d.walk(a);
        defer w.deinit();
        var out: std.ArrayList([]const u8) = .empty;
        while (try w.next(io)) |e| try out.append(a, try a.dupe(u8, e.path));
        std.mem.sort([]const u8, out.items, {}, lessString);
        return out.items;
    }

    /// The names directly inside root/rel, sorted ("" is the root).
    pub fn children(rig: *Rig, rel: []const u8) ![][]const u8 {
        const a = rig.arena();
        var d = try rig.tmp.dir.openDir(io, if (rel.len == 0) "." else rel, .{ .iterate = true });
        defer d.close(io);
        var out: std.ArrayList([]const u8) = .empty;
        var it = d.iterate();
        while (try it.next(io)) |e| try out.append(a, try a.dupe(u8, e.name));
        std.mem.sort([]const u8, out.items, {}, lessString);
        return out.items;
    }

    /// Runs the real 7zz in root/dir to build a test archive; fails the test
    /// when it does not succeed. `stdin` is the password dialogue.
    pub fn run7zz(rig: *Rig, dir: []const u8, stdin: []const u8, args: []const []const u8) !void {
        const res = try tool.run(rig.arena(), io, .{
            .path = rig.env.cfg.seven_zip,
            .args = args,
            .dir = rig.path(dir),
            .stdin = stdin,
        });
        if (res.exit_code != 0) {
            std.debug.print("7zz {any}: exit {d}\n{s}\n{s}\n", .{ args, res.exit_code, res.stdout, res.stderr });
            return error.SevenZipFailed;
        }
    }
};

extern "c" fn mkfifo(path: [*:0]const u8, mode: std.posix.mode_t) c_int;

/// Makes a FIFO at root/rel.
pub fn makeFifo(rig: *Rig, rel: []const u8) !void {
    const p = try std.fmt.allocPrintSentinel(rig.arena(), "{s}", .{rig.path(rel)}, 0);
    if (mkfifo(p, 0o600) != 0) return error.MkfifoFailed;
}

pub fn lessString(_: void, x: []const u8, y: []const u8) bool {
    return std.mem.order(u8, x, y) == .lt;
}

pub fn expectStrings(want: []const []const u8, got: []const []const u8) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g);
}

/// Go's golden files hex-encode every field, because names are hostile bytes.
pub fn unhex(a: Allocator, s: []const u8) ![]u8 {
    const out = try a.alloc(u8, s.len / 2);
    _ = try std.fmt.hexToBytes(out, s);
    return out;
}

// ---- writing into an absolute directory (what a scripted 7zz does) ----

pub fn putFile(dir: []const u8, rel: []const u8, bytes: []const u8) !void {
    var d = try Dir.cwd().openDir(io, dir, .{});
    defer d.close(io);
    if (std.fs.path.dirname(rel)) |p| try d.createDirPath(io, p);
    try d.writeFile(io, .{ .sub_path = rel, .data = bytes });
}

pub fn putDir(dir: []const u8, rel: []const u8) !void {
    var d = try Dir.cwd().openDir(io, dir, .{});
    defer d.close(io);
    try d.createDirPath(io, rel);
}

pub fn putSymlink(dir: []const u8, target: []const u8, name: []const u8) !void {
    var d = try Dir.cwd().openDir(io, dir, .{});
    defer d.close(io);
    try d.symLink(io, target, name, .{});
}

pub fn putHardlink(dir: []const u8, existing: []const u8, name: []const u8) !void {
    var d = try Dir.cwd().openDir(io, dir, .{});
    defer d.close(io);
    try d.hardLink(existing, d, name, io, .{});
}

pub fn putMode(dir: []const u8, rel: []const u8, mode: u32) !void {
    var d = try Dir.cwd().openDir(io, dir, .{});
    defer d.close(io);
    try d.setFilePermissions(io, rel, .fromMode(@intCast(mode)), .{});
}

pub fn modeOf(rig: *Rig, rel: []const u8) !u32 {
    const st = try rig.tmp.dir.statFile(io, rel, .{ .follow_symlinks = false });
    return @intCast(@backingInt(st.permissions) & 0o7777);
}

// ---- the scripted 7zz ----

/// A stand-in for 7zz: prints a canned listing for "l" and runs `extract_fn`
/// for "x". It lets the tests make "7zz" do what a buggy or compromised one
/// could (plant a symlink, write too much, fail with a given status and
/// message) without needing an archive that provokes it.
pub const Script = struct {
    list_out: []const u8 = "",
    list_exit: i32 = 0,
    list_stderr: []const u8 = "",
    /// Called with the directory after -o; may write anything there.
    extract_fn: ?*const fn (dir: []const u8) anyerror!void = null,
    extract_exit: i32 = 0,
    extract_stderr: []const u8 = "",
    /// How many times each command ran, and the last arguments and stdin.
    lists: usize = 0,
    extracts: usize = 0,
    last_args: []const []const u8 = &.{},
    last_stdin: []const u8 = "",
    last_dir: []const u8 = "",

    /// The record of the last call is kept in the test allocator.
    fn record(s: *Script, spec: tool_types.Spec) !void {
        s.deinit();
        const args = try gpa.alloc([]const u8, spec.args.len);
        for (spec.args, args) |src, *dst| dst.* = try gpa.dupe(u8, src);
        s.last_args = args;
        s.last_stdin = try gpa.dupe(u8, spec.stdin);
        s.last_dir = try gpa.dupe(u8, spec.dir);
    }

    pub fn deinit(s: *Script) void {
        for (s.last_args) |arg| gpa.free(arg);
        gpa.free(s.last_args);
        gpa.free(s.last_stdin);
        gpa.free(s.last_dir);
        s.last_args = &.{};
        s.last_stdin = "";
        s.last_dir = "";
    }

    pub fn runner(s: *Script) tool_types.Runner {
        return .{ .ctx = s, .run_fn = runFn };
    }

    fn runFn(ctx: *anyopaque, a: Allocator, _: Io, spec: tool_types.Spec) tool_types.Error!tool_types.Result {
        const s: *Script = @ptrCast(@alignCast(ctx));
        s.record(spec) catch return error.OutOfMemory;
        if (spec.args.len == 0) return .{ .exit_code = 7, .stdout = &.{}, .stderr = &.{} };
        if (std.mem.eql(u8, spec.args[0], "l")) {
            s.lists += 1;
            const out = a.dupe(u8, s.list_out) catch return error.OutOfMemory;
            const err = a.dupe(u8, s.list_stderr) catch return error.OutOfMemory;
            const truncated = out.len > spec.max_output;
            return .{ .exit_code = s.list_exit, .stdout = if (truncated) out[0..spec.max_output] else out, .stderr = err, .truncated = truncated };
        }
        if (std.mem.eql(u8, spec.args[0], "x")) {
            s.extracts += 1;
            var dir: []const u8 = "";
            for (spec.args) |arg| if (std.mem.startsWith(u8, arg, "-o")) {
                dir = arg[2..];
            };
            if (s.extract_fn) |f| f(dir) catch |err| switch (err) {
                error.Canceled => return error.Canceled,
                else => {
                    std.debug.print("scripted extract failed: {s}\n", .{@errorName(err)});
                    return .{ .exit_code = 7, .stdout = &.{}, .stderr = &.{} };
                },
            };
            const err = a.dupe(u8, s.extract_stderr) catch return error.OutOfMemory;
            return .{ .exit_code = s.extract_exit, .stdout = &.{}, .stderr = err };
        }
        return .{ .exit_code = 7, .stdout = &.{}, .stderr = &.{} };
    }
};

/// One listing block, as 7zz prints it.
pub fn block(a: Allocator, name: []const u8, size: []const u8, attrs: []const u8) ![]u8 {
    return std.fmt.allocPrint(a, "Path = {s}\nSize = {s}\nAttributes = {s}\nEncrypted = -\n\n", .{ name, size, attrs });
}
