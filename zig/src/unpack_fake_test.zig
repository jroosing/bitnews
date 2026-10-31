//! Tests with a scripted 7zz (internal/postproc/unpack fake_test.go): the
//! exit status and message mapping, what is checked in the tree 7zz wrote,
//! the arguments, the password on stdin and the listing cap. The "7zz" here
//! can do what a buggy or compromised one could, without an archive that
//! provokes it.
const std = @import("std");
const testing = std.testing;
const Dir = std.Io.Dir;
const tu = @import("unpack_testutil.zig");
const unpack = @import("unpack.zig");
const detect = @import("unpack_detect.zig");
const sevenzip = @import("unpack_sevenzip.zig");
const env_mod = @import("unpack_env.zig");
const tool_types = @import("tool_types.zig");

const one_file = "Path = a.txt\nSize = 6\nAttributes = A -rw-r--r--\nEncrypted = -\n\n";

/// A rig, a scripted 7zz and the jail of Go's tests: the destination is two
/// levels below the temporary directory, so an escape by "../" lands where
/// the test can see it.
const Fixture = struct {
    rig: *tu.Rig,
    script: *tu.Script,
    set: detect.ArchiveSet,
    dst: []const u8,

    fn init(list: []const u8, extract: ?*const fn ([]const u8) anyerror!void) !Fixture {
        const script = try testing.allocator.create(tu.Script);
        script.* = .{ .list_out = list, .extract_fn = extract };
        const rig = try tu.Rig.init(script.runner(), .{ .seven_zip = "fake" });
        try rig.write("arch/x.7z", "not read");
        const vols = try rig.arena().alloc([]const u8, 1);
        vols[0] = rig.path("arch/x.7z");
        return .{
            .rig = rig,
            .script = script,
            .set = .{ .format = .seven_z, .name = "x.7z", .volumes = vols },
            .dst = rig.path("base/jail/out"),
        };
    }

    fn deinit(f: *Fixture) void {
        f.script.deinit();
        testing.allocator.destroy(f.script);
        f.rig.deinit();
    }

    fn run(f: *Fixture) !unpack.Result {
        return unpack.unpack(&f.rig.env, f.set, f.dst);
    }

    /// Nothing may be next to the jail, and nothing may be left in it: no
    /// destination and no staging directory.
    fn requireContained(f: *Fixture) !void {
        try testing.expect(!f.rig.exists("base/out"));
        const top = try f.rig.children("base");
        try testing.expect(top.len == 0 or (top.len == 1 and std.mem.eql(u8, top[0], "jail")));
        if (top.len == 1) try testing.expectEqual(@as(usize, 0), (try f.rig.children("base/jail")).len);
    }
};

fn extractHello(dir: []const u8) anyerror!void {
    try tu.putFile(dir, "a.txt", "hello\n");
}

test "exit status and message are mapped to the error classes" {
    const Case = struct { status: i32, stderr: []const u8, want: ?anyerror };
    const cases = [_]Case{
        .{ .status = 0, .stderr = "WARNING: x", .want = null },
        .{ .status = 2, .stderr = "ERROR: Wrong password : a.txt", .want = error.Password },
        .{ .status = 2, .stderr = "ERROR: x : Cannot open encrypted archive. Wrong password?", .want = error.Password },
        .{ .status = 2, .stderr = "ERROR: Data Error in encrypted file. Wrong password? : a.txt", .want = error.Password },
        .{ .status = 2, .stderr = "ERROR: Unsupported Method : a.txt", .want = error.Unsupported },
        .{ .status = 2, .stderr = "ERROR: Data Error : a.txt", .want = error.Corrupt },
        .{ .status = 2, .stderr = "ERROR: CRC Failed : a.txt", .want = error.Corrupt },
        .{ .status = 2, .stderr = "ERRORS:\nHeaders Error", .want = error.Corrupt },
        .{ .status = 2, .stderr = "ERRORS:\nUnexpected end of archive", .want = error.Corrupt },
        .{ .status = 2, .stderr = "ERRORS:\nIs not archive", .want = error.Corrupt },
        .{ .status = 1, .stderr = "WARNING: Some files were not extracted", .want = error.Corrupt },
        .{ .status = 2, .stderr = "ERROR: No space left on device : a.txt", .want = error.DiskFull },
        .{ .status = 2, .stderr = "ERROR: There is not enough space on the disk.", .want = error.DiskFull },
        .{ .status = 8, .stderr = "", .want = error.TooLarge },
        .{ .status = 2, .stderr = "ERROR: Can't allocate required memory!", .want = error.TooLarge },
        .{ .status = 2, .stderr = "", .want = error.Corrupt },
        .{ .status = 255, .stderr = "Break signaled", .want = error.Internal },
        .{ .status = 3, .stderr = "", .want = error.Corrupt },
        .{ .status = 7, .stderr = "Command Line Error", .want = error.Internal },
    };
    for (cases) |c| {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        f.script.extract_exit = c.status;
        f.script.extract_stderr = c.stderr;
        if (c.want) |want| {
            try testing.expectError(want, f.run());
            try f.requireContained();
        } else {
            _ = try f.run();
        }
    }
}

test "messages carry the class prefix of Go and one bounded line of 7zz" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.script.extract_exit = 2;
    f.script.extract_stderr = "\nWARNINGS:\nsomething\nERROR: Data Error : a.txt\n";
    try testing.expectError(error.Corrupt, f.run());
    try testing.expectEqualStrings("unpack: archive corrupt: ERROR: Data Error : a.txt", f.rig.diag.message);
}

test "a failing listing is classified too" {
    var f = try Fixture.init("", null);
    defer f.deinit();
    f.script.list_exit = 2;
    f.script.list_stderr = "ERROR: x : Cannot open encrypted archive. Wrong password?";
    try testing.expectError(error.Password, f.run());
    try f.requireContained();
}

test "the encrypted-header prompt without a password is a password error" {
    var f = try Fixture.init("\nEnter password:\n", null);
    defer f.deinit();
    f.script.list_exit = 255;
    f.script.list_stderr = "Break signaled";
    try testing.expectError(error.Password, f.run());
}

test "what 7zz writes is checked before it is published" {
    const Sym = struct {
        fn abs(dir: []const u8) anyerror!void {
            try extractHello(dir);
            try tu.putSymlink(dir, "/etc", "evil");
        }
        fn parent(dir: []const u8) anyerror!void {
            try extractHello(dir);
            try tu.putSymlink(dir, "../..", "evil");
        }
        fn dangling(dir: []const u8) anyerror!void {
            try extractHello(dir);
            try tu.putSymlink(dir, "nothing", "evil");
        }
        fn hard(dir: []const u8) anyerror!void {
            try extractHello(dir);
            try tu.putHardlink(dir, "a.txt", "b.txt");
        }
        fn fifo(dir: []const u8) anyerror!void {
            try extractHello(dir);
            const p = try std.fmt.allocPrintSentinel(testing.allocator, "{s}/pipe", .{dir}, 0);
            defer testing.allocator.free(p);
            if (mkfifo(p, 0o600) != 0) return error.Mkfifo;
        }
        fn more(dir: []const u8) anyerror!void {
            try tu.putFile(dir, "a.txt", "hello-and-more\n");
        }
        fn extra(dir: []const u8) anyerror!void {
            try extractHello(dir);
            try tu.putFile(dir, "extra", "x\n");
        }
        extern "c" fn mkfifo(path: [*:0]const u8, mode: std.posix.mode_t) c_int;
    };
    const Case = struct { f: *const fn ([]const u8) anyerror!void, want: anyerror };
    const cases = [_]Case{
        .{ .f = Sym.abs, .want = error.PathViolation },
        .{ .f = Sym.parent, .want = error.PathViolation },
        .{ .f = Sym.dangling, .want = error.PathViolation },
        .{ .f = Sym.hard, .want = error.PathViolation },
        .{ .f = Sym.fifo, .want = error.PathViolation },
        .{ .f = Sym.more, .want = error.Corrupt },
        .{ .f = Sym.extra, .want = error.Corrupt },
    };
    for (cases) |c| {
        var f = try Fixture.init(one_file, c.f);
        defer f.deinit();
        try testing.expectError(c.want, f.run());
        try f.requireContained();
    }
}

test "permissions of what 7zz wrote are normalized" {
    const E = struct {
        fn run(dir: []const u8) anyerror!void {
            try tu.putFile(dir, "d/f", "x\n");
            try tu.putFile(dir, "g", "y\n");
            try tu.putMode(dir, "d/f", 0o4755);
            try tu.putMode(dir, "g", 0o777);
            try tu.putMode(dir, "d", 0o500);
        }
    };
    const list = "Path = d\nSize = 0\nAttributes = D drwxr-xr-x\n\nPath = d/f\nSize = 2\nAttributes = A -rwsr-xr-x\n\nPath = g\nSize = 2\nAttributes = A -rwxrwxrwx\n\n";
    var f = try Fixture.init(list, E.run);
    defer f.deinit();
    const res = try f.run();
    try tu.expectStrings(&.{ "d/f", "g" }, res.files);
    try testing.expectEqual(@as(u32, 0o755), try tu.modeOf(f.rig, "base/jail/out/d"));
    try testing.expectEqual(@as(u32, 0o644), try tu.modeOf(f.rig, "base/jail/out/d/f"));
    try testing.expectEqual(@as(u32, 0o644), try tu.modeOf(f.rig, "base/jail/out/g"));
}

test "names that were not safe are renamed and collisions refused" {
    const list = "Path = x\nSize = 2\nAttributes = A\n\nPath = y\nSize = 2\nAttributes = A\n\n";
    const E = struct {
        fn renamed(dir: []const u8) anyerror!void {
            try tu.putFile(dir, "a:b", "x\n");
            try tu.putFile(dir, "con.txt", "y\n");
        }
        fn collide(dir: []const u8) anyerror!void {
            try tu.putFile(dir, "a:b", "x\n");
            try tu.putFile(dir, "a_b", "y\n");
        }
        fn control(dir: []const u8) anyerror!void {
            try tu.putFile(dir, "ev\x1bil", "x\n");
            try tu.putFile(dir, "y", "y\n");
        }
    };
    {
        var f = try Fixture.init(list, E.renamed);
        defer f.deinit();
        const res = try f.run();
        try tu.expectStrings(&.{ "_con.txt", "a_b" }, res.files);
    }
    {
        var f = try Fixture.init(list, E.collide);
        defer f.deinit();
        try testing.expectError(error.PathViolation, f.run());
        try f.requireContained();
    }
    {
        var f = try Fixture.init(list, E.control);
        defer f.deinit();
        const res = try f.run();
        try tu.expectStrings(&.{ "evil", "y" }, res.files);
    }
}

test "the arguments are fixed words and absolute paths, the password only on stdin" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.rig.env.cfg.password = "s3cr3t-pw";
    // An archive named like a switch and like a shell injection.
    try f.rig.write("arch/-so $(touch pwned).7z", "x");
    const vols = try f.rig.arena().alloc([]const u8, 1);
    vols[0] = f.rig.path("arch/-so $(touch pwned).7z");
    f.set.volumes = vols;
    _ = try f.run();
    const args = f.script.last_args;
    try testing.expectEqualStrings("x", args[0]);
    var dashdash = false;
    for (args) |a| {
        if (std.mem.eql(u8, a, "--")) dashdash = true;
        try testing.expect(std.mem.indexOf(u8, a, "s3cr3t-pw") == null);
        try testing.expect(!std.mem.startsWith(u8, a, "-p"));
    }
    try testing.expect(dashdash);
    try testing.expectEqualStrings(vols[0], args[args.len - 1]);
    var out_arg: []const u8 = "";
    for (args) |a| if (std.mem.startsWith(u8, a, "-o")) {
        out_arg = a;
    };
    try testing.expectEqualStrings(try std.fmt.allocPrint(f.rig.arena(), "-o{s}", .{f.rig.path("base/jail/out.tmp")}), out_arg);
    try testing.expectEqualStrings("s3cr3t-pw\n", f.script.last_stdin);
    try testing.expect(!f.rig.exists("arch/pwned"));
}

test "without a password stdin is empty" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    _ = try f.run();
    try testing.expectEqualStrings("", f.script.last_stdin);
}

test "a password with a line break, NUL or invalid UTF-8 is refused" {
    for ([_][]const u8{ "pass\nword", "pass\rword", "pa\x00ss", "\xff\xfe" }) |pw| {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        f.rig.env.cfg.password = pw;
        try testing.expectError(error.Password, f.run());
        try testing.expectEqual(@as(usize, 0), f.script.lists);
    }
}

test "the password is not in any error message" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.rig.env.cfg.password = "hunter2-guess";
    f.script.extract_exit = 2;
    f.script.extract_stderr = "ERROR: Wrong password : a.txt";
    try testing.expectError(error.Password, f.run());
    try testing.expect(std.mem.indexOf(u8, f.rig.diag.message, "hunter2") == null);
}

test "a listing with more entries than allowed is refused before extraction" {
    var list: std.ArrayList(u8) = .empty;
    defer list.deinit(testing.allocator);
    for (0..50) |i| try list.print(testing.allocator, "Path = f{d}\nSize = 1\nAttributes = A\n\n", .{i});
    var f = try Fixture.init(list.items, null);
    defer f.deinit();
    f.rig.env.cfg.max_entries = 5;
    try testing.expectError(error.TooManyEntries, f.run());
    try testing.expectEqual(@as(usize, 0), f.script.extracts);
    try f.requireContained();
}

test "a huge listing is cut off by the output cap and refused" {
    // Far more output than max_entries entries can explain: the runner stops
    // keeping it and the unpack fails instead of parsing a partial listing.
    var list: std.ArrayList(u8) = .empty;
    defer list.deinit(testing.allocator);
    const long: [200]u8 = @splat('n');
    for (0..30000) |i| try list.print(testing.allocator, "Path = {d}{s}\nSize = 1\nAttributes = A\n\n", .{ i, &long });
    var f = try Fixture.init(list.items, null);
    defer f.deinit();
    f.rig.env.cfg.max_entries = 5;
    try testing.expectError(error.TooManyEntries, f.run());
    try f.requireContained();
}

test "the list cap grows with the entry cap and is bounded" {
    try testing.expectEqual(@as(usize, 1 * 600 + (4 << 20)), sevenzip.listLimit(0));
    try testing.expectEqual(@as(usize, 5 * 600 + (4 << 20)), sevenzip.listLimit(5));
    try testing.expectEqual(@as(usize, 512 << 20), sevenzip.listLimit(env_mod.default_max_entries));
}

test "an empty program path is an internal error, not an archive problem" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.rig.env.cfg.seven_zip = "";
    try testing.expectError(error.Internal, f.run());
    try f.requireContained();
}

test "a set with an error or without volumes is refused" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.set.err = "unpack: missing archive volume: archive corrupt: x";
    try testing.expectError(error.MissingVolume, f.run());
    f.set.err = null;
    f.set.volumes = &.{};
    try testing.expectError(error.Corrupt, f.run());
}

test "a volume that became a symlink is refused" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    try f.rig.write("real.7z", "x");
    try f.rig.tmp.dir.deleteFile(tu.io, "arch/x.7z");
    try f.rig.tmp.dir.symLink(tu.io, f.rig.path("real.7z"), "arch/x.7z", .{});
    try testing.expectError(error.Corrupt, f.run());
}

test "the byte cap applies to the declared sizes, the bomb check to the ratio" {
    {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        f.rig.env.cfg.max_bytes = 5;
        try testing.expectError(error.TooLarge, f.run());
        try f.requireContained();
    }
    {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        f.rig.env.cfg.max_bytes = 6;
        _ = try f.run();
    }
    {
        // 100 declared bytes in an 8-byte volume: a ratio of 12.
        var f = try Fixture.init("Path = a.txt\nSize = 100\nAttributes = A\n\n", extractHello);
        defer f.deinit();
        f.rig.env.cfg.bomb_min = 1;
        f.rig.env.cfg.bomb_ratio = 5;
        try testing.expectError(error.Bomb, f.run());
        try f.requireContained();
    }
}

fn free(comptime n: u64) env_mod.FreeSpace {
    return .{ .probe_fn = struct {
        fn f(_: ?*anyopaque, _: []const u8) anyerror!u64 {
            return n;
        }
    }.f };
}

test "the declared size must fit into the free disk space" {
    const Case = struct { free: env_mod.FreeSpace, min_free: u64, ok: bool };
    const cases = [_]Case{
        .{ .free = free(5), .min_free = 0, .ok = false },
        .{ .free = free(6), .min_free = 0, .ok = true },
        .{ .free = free(100), .min_free = 95, .ok = false },
        .{ .free = free(100), .min_free = 94, .ok = true },
        .{ .free = free(10), .min_free = 50, .ok = false },
        .{ .free = .{ .probe_fn = struct {
            fn f(_: ?*anyopaque, _: []const u8) anyerror!u64 {
                return error.NoStatfs;
            }
        }.f }, .min_free = 0, .ok = true },
    };
    for (cases) |c| {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        f.rig.env.cfg.free_space = c.free;
        f.rig.env.cfg.min_free = c.min_free;
        if (c.ok) {
            _ = try f.run();
        } else {
            try testing.expectError(error.DiskFull, f.run());
            try testing.expectEqual(@as(usize, 0), f.script.extracts);
            try f.requireContained();
        }
    }
}

test "the free-space probe is asked about the destination's parent" {
    const Probe = struct {
        var asked: [512]u8 = undefined;
        var len: usize = 0;
        fn f(_: ?*anyopaque, p: []const u8) anyerror!u64 {
            @memcpy(asked[0..p.len], p);
            len = p.len;
            return 1 << 40;
        }
    };
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.rig.env.cfg.free_space = .{ .probe_fn = Probe.f };
    _ = try f.run();
    try testing.expectEqualStrings(f.rig.path("base/jail"), Probe.asked[0..Probe.len]);
}

test "an existing destination is refused unless replace is set, and kept on failure" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    try f.rig.write("base/jail/out/keep", "old");
    try testing.expectError(error.Exists, f.run());
    try testing.expectEqualStrings("old", try f.rig.read("base/jail/out/keep"));
    // A failing replace leaves the existing result alone.
    f.rig.env.cfg.replace = true;
    f.script.extract_exit = 2;
    f.script.extract_stderr = "ERROR: Data Error";
    try testing.expectError(error.Corrupt, f.run());
    try testing.expectEqualStrings("old", try f.rig.read("base/jail/out/keep"));
    try testing.expect(!f.rig.exists("base/jail/out.tmp"));
    // And a good one swaps it.
    f.script.extract_exit = 0;
    const res = try f.run();
    try tu.expectStrings(&.{"a.txt"}, res.files);
    try testing.expect(!f.rig.exists("base/jail/out/keep"));
}

test "a leftover staging directory is cleared first, even without permissions or as a symlink" {
    {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        try f.rig.write("base/jail/out.tmp/old/deep/junk", "crash leftover");
        try f.rig.write("base/jail/out.tmp/a.txt", "stale");
        try f.rig.mkdir("base/jail/out.tmp/locked/inner");
        try tu.putMode(f.rig.path("base/jail/out.tmp"), "locked", 0);
        _ = try f.run();
        try tu.expectStrings(&.{"a.txt"}, try f.rig.tree("base/jail/out"));
        try tu.expectStrings(&.{"out"}, try f.rig.children("base/jail"));
    }
    {
        var f = try Fixture.init(one_file, extractHello);
        defer f.deinit();
        try f.rig.write("victim/precious", "keep");
        try f.rig.mkdir("base/jail");
        try f.rig.tmp.dir.symLink(tu.io, f.rig.path("victim"), "base/jail/out.tmp", .{});
        _ = try f.run();
        try tu.expectStrings(&.{"precious"}, try f.rig.children("victim"));
    }
}

test "the parent of the destination is created and a failure leaves bystanders alone" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    f.dst = f.rig.path("deep/er/out");
    _ = try f.run();
    try testing.expect(f.rig.exists("deep/er/out/a.txt"));
    try f.rig.write("keep/bystander", "x");
    f.dst = f.rig.path("keep/out");
    f.script.extract_exit = 2;
    f.script.extract_stderr = "ERROR: Wrong password";
    try testing.expectError(error.Password, f.run());
    try tu.expectStrings(&.{"bystander"}, try f.rig.children("keep"));
}

test "a relative destination and a relative volume work" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    // The temporary directory is below the working directory of the tests.
    f.dst = try std.fmt.allocPrint(f.rig.arena(), ".zig-cache/tmp/{s}/rel/out", .{f.rig.tmp.sub_path});
    const rel_vol = try std.fmt.allocPrint(f.rig.arena(), ".zig-cache/tmp/{s}/arch/x.7z", .{f.rig.tmp.sub_path});
    const vols = try f.rig.arena().alloc([]const u8, 1);
    vols[0] = rel_vol;
    f.set.volumes = vols;
    const res = try f.run();
    try tu.expectStrings(&.{"a.txt"}, res.files);
    try testing.expectEqualStrings("hello\n", try f.rig.read("rel/out/a.txt"));
    // 7zz is given absolute paths.
    try testing.expect(std.fs.path.isAbsolute(f.script.last_args[f.script.last_args.len - 1]));
}

test "an empty archive makes an empty destination, a directory-only one a tree" {
    var f = try Fixture.init("Path = d\nSize = 0\nAttributes = D drwxr-xr-x\n\n", struct {
        fn run(dir: []const u8) anyerror!void {
            try tu.putDir(dir, "d");
        }
    }.run);
    defer f.deinit();
    const res = try f.run();
    try testing.expectEqual(@as(usize, 0), res.files.len);
    try tu.expectStrings(&.{"d"}, try f.rig.tree("base/jail/out"));
}

test "the archives are never modified" {
    var f = try Fixture.init(one_file, extractHello);
    defer f.deinit();
    _ = try f.run();
    try testing.expectEqualStrings("not read", try f.rig.read("arch/x.7z"));
    try tu.expectStrings(&.{"x.7z"}, try f.rig.children("arch"));
}

test "tool failures are internal errors, not archive errors" {
    const Failing = struct {
        err: tool_types.Error,
        fn runner(s: *@This()) tool_types.Runner {
            return .{ .ctx = s, .run_fn = run };
        }
        fn run(ctx: *anyopaque, _: std.mem.Allocator, _: std.Io, _: tool_types.Spec) tool_types.Error!tool_types.Result {
            const s: *@This() = @ptrCast(@alignCast(ctx));
            return s.err;
        }
    };
    for ([_]tool_types.Error{ error.StartFailed, error.Timeout }) |e| {
        var failing: Failing = .{ .err = e };
        const rig = try tu.Rig.init(failing.runner(), .{ .seven_zip = "fake" });
        defer rig.deinit();
        try rig.write("arch/x.7z", "x");
        const vols = try rig.arena().alloc([]const u8, 1);
        vols[0] = rig.path("arch/x.7z");
        try testing.expectError(error.Internal, unpack.unpack(&rig.env, .{ .format = .seven_z, .name = "x", .volumes = vols }, rig.path("out")));
    }
    // A cancellation comes through as one.
    var failing: Failing = .{ .err = error.Canceled };
    const rig = try tu.Rig.init(failing.runner(), .{ .seven_zip = "fake" });
    defer rig.deinit();
    try rig.write("arch/x.7z", "x");
    const vols = try rig.arena().alloc([]const u8, 1);
    vols[0] = rig.path("arch/x.7z");
    try testing.expectError(error.Canceled, unpack.unpack(&rig.env, .{ .format = .seven_z, .name = "x", .volumes = vols }, rig.path("out")));
    try testing.expect(!rig.exists("out"));
}
