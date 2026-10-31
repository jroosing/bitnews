//! Tests of the par2 wrapper through a scripted Runner (internal/postproc/par2
//! par2.go: the command line, the extras list, every output and exit-code
//! branch, the cleanup rules, and hostile sets of hostile_test.go). Sets are
//! made by the test packet builder, so no real par2 is needed.
const std = @import("std");
const testing = std.testing;
const Allocator = std.mem.Allocator;
const par2 = @import("par2.zig");
const ports = @import("postprocess_ports.zig");
const b = @import("par2_testbuilder.zig");
const u = @import("par2_testutil.zig");
const Env = u.Env;
const Fake = u.Fake;

const fake_path = "/fake/par2";

fn repairer(f: *Fake) par2.Repairer {
    return .{ .runner = f.runner(), .path = fake_path, .threads = 2, .memory_mb = 64 };
}

/// A set with the given file names. `index` is the .par2 file; the data files
/// are not written.
fn setOf(a: Allocator, ns: []const []const u8) !b.TestSet {
    const files = try a.alloc(b.FileSpec, ns.len);
    for (ns, files) |n, *f| f.* = .{ .name = n, .data = try std.fmt.allocPrint(a, "data of {s}", .{n}) };
    return b.TestSet.init(a, 1000, files);
}

fn expectArgs(want: []const []const u8, got: []const []const u8) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g);
}

// ---- no set ----

test "an empty directory has no set and starts nothing" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectEqual(ports.RepairOutcome{}, try run.outcome());
    try testing.expectEqual(0, f.calls);
}

test "a garbage or empty PAR2 file has no set" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.write("x.par2", "not a par2 file at all");
    try env.write("empty.par2", "");
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expect(!(try run.outcome()).has_par2);
    try testing.expectEqual(0, f.calls);
}

test "a missing directory is a plain error" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, try env.path("nope"));
    defer run.deinit();

    try testing.expectError(error.Failed, run.result);
    try testing.expect(std.mem.indexOf(u8, run.diag.message, "FileNotFound") != null);
}

// ---- the command line ----

test "the command line: base path, threads, memory, main file and the extras on stdin" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const a = env.a();
    const set = try setOf(a, &.{ "a.bin", "b.bin" });
    try env.write("set.par2", try set.index());
    try env.write("set.vol0+1.par2", try set.volume(&.{u.payload(env.a(), 9, 1024)}));
    try env.write("a.bin", "x"); // a target: checked as such, not an extra
    try env.write("zz-obfuscated", "x");
    try env.write("release.nfo", "x");
    try env.write("-weird name .txt", "x");
    try env.write(" leading space", "x"); // cannot survive a trimmed list
    try env.write("new\nline", "x"); // cannot survive a line list
    try env.write("sub/inner.bin", "x"); // not top level
    try env.tmp.dir.symLink(testing.io, "a.bin", "link.bin", .{}); // never given to par2
    var f = Fake.init();
    defer f.deinit();
    f.stdout = "All files are correct, repair is not required.\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    const out = try run.outcome();
    try testing.expectEqual(ports.RepairOutcome{ .has_par2 = true }, out);
    try testing.expectEqual(1, f.calls);
    try testing.expectEqualStrings(fake_path, f.path);
    try testing.expectEqualStrings(env.root, f.dir);
    try expectArgs(&.{
        "r",
        try std.fmt.allocPrint(a, "-B{s}", .{env.root}),
        "-t2",
        "-m64",
        "--",
        try env.path("set.par2"),
        "@",
    }, f.args);
    // The smaller PAR2 file is the main one; the other PAR2 files come first,
    // then the other regular files in name order.
    const want = try std.fmt.allocPrint(a, "{s}\n{s}\n{s}\n{s}\n", .{
        try env.path("set.vol0+1.par2"),
        try env.path("-weird name .txt"),
        try env.path("release.nfo"),
        try env.path("zz-obfuscated"),
    });
    try testing.expectEqualStrings(want, f.stdin);
}

test "the main file is the smallest, ties go by name" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const set = try setOf(env.a(), &.{"a.bin"});
    const idx = try set.index();
    try env.write("b.par2", idx);
    try env.write("a.par2", idx);
    try env.write("c.par2", try b.concat(env.a(), &.{ idx, idx }));
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try testing.expectEqualStrings(try env.path("a.par2"), f.args[5]);
    try testing.expectEqualStrings(try std.fmt.allocPrint(env.a(), "{s}\n{s}\n", .{ try env.path("b.par2"), try env.path("c.par2") }), f.stdin);
}

test "defaults: memory 512 MB and a thread count between 1 and 16" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.write("set.par2", try (try setOf(env.a(), &.{"a.bin"})).index());
    var f = Fake.init();
    defer f.deinit();
    const rep: par2.Repairer = .{ .runner = f.runner(), .path = fake_path };

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try testing.expectEqualStrings("-m512", f.args[3]);
    try testing.expect(std.mem.startsWith(u8, f.args[2], "-t"));
    const t = try std.fmt.parseInt(u32, f.args[2][2..], 10);
    try testing.expect(t >= 1 and t <= 16);
}

test "a relative directory is made absolute" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.write("set.par2", try (try setOf(env.a(), &.{"a.bin"})).index());
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);
    const cwd = try std.process.currentPathAlloc(testing.io, env.a());
    const rel = try std.fs.path.relativeAlloc(env.a(), cwd, null, cwd, env.root);

    var run = u.repairOf(&rep, rel);
    defer run.deinit();
    _ = try run.outcome();

    try testing.expect(std.fs.path.isAbsolute(f.dir));
    try testing.expectEqualStrings(env.root, f.dir);
}

// ---- obfuscated PAR2 files ----

test "a PAR2 file without a .par2 name is adopted by its content" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const set = try setOf(env.a(), &.{"a.bin"});
    try env.write("21cb82676987e551", try set.index());
    try env.write("b7578fd2e094aa01", try set.volume(&.{"0123456789abcdef"}));
    try env.write("13184cde52aaba45", "data, not a PAR2 file");
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try env.expectListing(&.{ "13184cde52aaba45", "21cb82676987e551.par2", "b7578fd2e094aa01.par2" });
    try testing.expectEqual(1, f.calls);
}

test "adopting never overwrites: a taken name gets a counter" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const set = try setOf(env.a(), &.{"a.bin"});
    try env.write("hex", try set.index());
    try env.write("hex.par2", try set.index());
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try env.expectListing(&.{ "hex.1.par2", "hex.par2" });
}

// ---- exit codes ----

fn setDir(env: *Env, ns: []const []const u8) !void {
    try env.write("set.par2", try (try setOf(env.a(), ns)).index());
}

test "exit 0 reports the slices rebuilt" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.stdout = "Repair is required.\r\nYou have 14 out of 17 data blocks available.\r\nYou have 8 recovery blocks available.\r\n3 recovery blocks will be used to repair.\r\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectEqual(ports.RepairOutcome{ .has_par2 = true, .repaired_slices = 3 }, try run.outcome());
}

test "exit 2 is a shortfall with the counts from the report" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.exit_code = 2;
    f.stdout = "Repair is required.\n1 file(s) are missing.\nYou have 7 out of 17 data blocks available.\nYou have 8 recovery blocks available.\nRepair is not possible.\nYou need 2 more recovery blocks to be able to repair.\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(10, run.diag.needed);
    try testing.expectEqual(8, run.diag.available);
    try testing.expectEqualStrings("not enough recovery data: need 10 blocks, have 8", run.diag.message);
}

test "exit 2 without the available-blocks line: recovery blocks plus the ones still needed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.exit_code = 2;
    f.stdout = "Loaded 4 new packets including 4 recovery blocks\nRepair is not possible.\nYou need 2 more recovery blocks to be able to repair.\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(6, run.diag.needed);
    try testing.expectEqual(4, run.diag.available);
}

test "exit 2 with no recovery volumes: all damage needed, none available" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.exit_code = 2;
    f.stdout = "You have 16 out of 17 data blocks available.\nRepair is not possible.\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(1, run.diag.needed);
    try testing.expectEqual(0, run.diag.available);
}

test "exit 4 means no usable set" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.exit_code = 4;
    f.stdout = "Main packet not found.\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectEqual(ports.RepairOutcome{}, try run.outcome());
}

test "exit 5 is a failed repair with the output tail" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.exit_code = 5;
    f.stdout = "Repair is possible.\r\nRepair Failed.\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectError(error.RepairFailed, run.result);
    try testing.expectEqualStrings("repair failed: par2: repair: exit status 5 (repair failed): Repair is possible. |  | Repair Failed. |", run.diag.message);
}

test "the other exits are plain errors named after the exit, with the tail of stdout and stderr" {
    const cases = [_]struct { code: i32, name: []const u8 }{
        .{ .code = 1, .name = "repair required" },
        .{ .code = 3, .name = "invalid arguments" },
        .{ .code = 6, .name = "file I/O error" },
        .{ .code = 7, .name = "logic error" },
        .{ .code = 8, .name = "out of memory" },
        .{ .code = -1, .name = "killed" },
        .{ .code = 42, .name = "unknown" },
    };
    for (cases) |c| {
        var env: Env = undefined;
        try env.init();
        defer env.deinit();
        try setDir(&env, &.{"a.bin"});
        var f = Fake.init();
        defer f.deinit();
        f.exit_code = c.code;
        f.stdout = "out\x1b[0m\x00";
        f.stderr = "boom\n";
        const rep = repairer(&f);

        var run = u.repairOf(&rep, env.root);
        defer run.deinit();

        try testing.expectError(error.Failed, run.result);
        const want = try std.fmt.allocPrint(env.a(), "par2: repair: exit status {d} ({s}): out[0mboom |", .{ c.code, c.name });
        try testing.expectEqualStrings(want, run.diag.message);
    }
}

test "the error carries at most the last 400 bytes of output" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    var f = Fake.init();
    defer f.deinit();
    f.exit_code = 6;
    const text = try env.a().alloc(u8, 1000);
    @memset(text[0..600], 'a');
    @memset(text[600..], 'z');
    f.stdout = text;
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();

    try testing.expectError(error.Failed, run.result);
    const prefix = "par2: repair: exit status 6 (file I/O error): ";
    try testing.expectEqual(prefix.len + 400, run.diag.message.len);
    try testing.expect(std.mem.endsWith(u8, run.diag.message, "zzzz"));
    try testing.expect(std.mem.indexOfScalar(u8, run.diag.message[prefix.len..], 'a') == null);
}

test "runner errors keep their class and tell which program" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    const cases = [_]struct { e: @import("tool_types.zig").Error, want: ports.PortError, msg: []const u8 }{
        .{ .e = error.StartFailed, .want = error.Failed, .msg = "tool: start par2" },
        .{ .e = error.Timeout, .want = error.Failed, .msg = "tool: timed out: par2" },
        .{ .e = error.Canceled, .want = error.Canceled, .msg = "" },
        .{ .e = error.OutOfMemory, .want = error.OutOfMemory, .msg = "" },
    };
    for (cases) |c| {
        var f = Fake.init();
        defer f.deinit();
        f.fail = c.e;
        const rep = repairer(&f);

        var run = u.repairOf(&rep, env.root);
        defer run.deinit();

        try testing.expectError(c.want, run.result);
        try testing.expectEqualStrings(c.msg, run.diag.message);
    }
}

// ---- cleanup ----

test "after exit 0 the name.N backups of targets are deleted, and nothing else" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.write("set.par2", try (try setOf(env.a(), &.{ "a.bin", "sub/c.bin", "b", "b.1" })).index());
    try env.write("a.bin", "x");
    try env.write("a.bin.1", "x");
    try env.write("a.bin.12", "x");
    try env.write("a.bin.1234", "x");
    try env.write("a.bin.12345", "x"); // too many digits
    try env.write("a.bin.x1", "x");
    try env.write("unrelated.1", "x");
    try env.write("b.1", "x"); // a target itself
    try env.write("b.2", "x"); // a backup of b
    try env.write("sub/c.bin.1", "x");
    try env.write("sub/other.1", "x");
    try env.write("c.bin.1", "x"); // wrong directory
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try env.expectListing(&.{ "set.par2", "a.bin", "a.bin.12345", "a.bin.x1", "unrelated.1", "b.1", "sub/other.1", "c.bin.1" });
}

test "a directory called like a backup is kept" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    try env.write("a.bin.1/inner", "x");
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try env.expectListing(&.{ "a.bin.1/inner", "set.par2" });
}

test "nothing is deleted when par2 failed" {
    for ([_]i32{ 2, 4, 5, 6 }) |code| {
        var env: Env = undefined;
        try env.init();
        defer env.deinit();
        try setDir(&env, &.{"a.bin"});
        try env.write("a.bin.1", "x");
        try env.write("obf", "x");
        var f = Fake.init();
        defer f.deinit();
        f.exit_code = code;
        f.stdout = "File: \"obf\" - found 9 of 10 data blocks from \"a.bin\".\n";
        const rep = repairer(&f);

        var run = u.repairOf(&rep, env.root);
        defer run.deinit();
        _ = run.result catch {};

        try env.expectListing(&.{ "a.bin.1", "obf", "set.par2" });
    }
}

test "the damaged copy of an obfuscated target is deleted, only when it is exactly an extra file" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.write("set.par2", try (try setOf(env.a(), &.{"a.bin"})).index());
    try env.write("set.vol0+1.par2", try (try setOf(env.a(), &.{"a.bin"})).volume(&.{"0123456789abcdef"}));
    try env.write("x7f3a9", "x"); // reported, an extra: deleted
    try env.write("a.bin", "x"); // reported but a target, not an extra: kept
    try env.write("kept.dat", "x"); // not reported: kept
    try env.write("dir/inner", "x"); // reported through ..: kept
    try env.write("sub", ""); // an unreported file called like the path part below
    try env.tmp.dir.createDirPath(testing.io, "adir"); // a directory: kept
    var f = Fake.init();
    defer f.deinit();
    f.stdout =
        "File: \"x7f3a9\" - found 9 of 10 data blocks from \"a.bin\".\n" ++
        "File: \"a.bin\" - found 9 of 10 data blocks from \"a.bin\".\n" ++
        "File: \"set.vol0+1.par2\" - found 1 of 10 data blocks from \"a.bin\".\n" ++
        "File: \"sub/../x7f3a9\" - found 1 of 10 data blocks from \"a.bin\".\n" ++
        "File: \"adir\" - found 1 of 10 data blocks from \"a.bin\".\n" ++
        "File: \"../x7f3a9\" - found 1 of 10 data blocks from \"a.bin\".\n" ++
        "File: \"missing\" - found 1 of 10 data blocks from \"a.bin\".\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try env.expectListing(&.{ "set.par2", "set.vol0+1.par2", "a.bin", "kept.dat", "dir/inner", "sub" });
}

test "a partial copy that is a symlink is never followed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    try env.write("precious", "keep me");
    try env.tmp.dir.symLink(testing.io, "precious", "obf", .{});
    var f = Fake.init();
    defer f.deinit();
    f.stdout = "File: \"obf\" - found 9 of 10 data blocks from \"a.bin\".\n";
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try testing.expect(env.exists("obf"));
    try testing.expectEqualStrings("keep me", try env.read("precious"));
    // Symlinks are not extras, so the name was not listed either.
    try testing.expect(std.mem.indexOf(u8, f.stdin, "obf") == null);
}

// ---- hostile sets (hostile_test.go) ----

const hostile_names = [_][]const u8{
    "../escaped.bin",         "sub/../../escaped.bin", "/tmp/escaped.bin", "..\\escaped.bin", "C:\\escaped.bin",        "C:escaped.bin",
    "a\x00../../escaped.bin", "",                      "..",               "dir//x",          "x\nRepair is possible.",
};

/// A Download folder `dl` in the temp root, which also holds a bystander
/// file, plus an obfuscated copy of the data that a hostile PAR2 file names
/// with an attacker-chosen path.
fn hostileDir(env: *Env, name: []const u8) !void {
    try env.write("bystander.txt", "mine");
    const data = u.payload(env.a(), 3, 2500);
    try env.write("dl/obfuscated", data);
    const set = try b.TestSet.init(env.a(), 1000, &.{.{ .name = name, .data = data }});
    try env.write("dl/set.par2", try set.index());
}

test "a hostile set is refused before par2 starts and nothing moves" {
    for (hostile_names) |name| {
        var env: Env = undefined;
        try env.init();
        defer env.deinit();
        try hostileDir(&env, name);
        const before = try env.snapshot();
        var f = Fake.init();
        defer f.deinit();
        const rep = repairer(&f);

        var run = u.repairOf(&rep, try env.path("dl"));
        defer run.deinit();

        testing.expectError(error.PathViolation, run.result) catch |e| {
            std.debug.print("name {any}\n", .{name});
            return e;
        };
        try testing.expect(std.mem.startsWith(u8, run.diag.message, "par2: unsafe PAR2 set: path violation: "));
        try testing.expectEqual(0, f.calls);
        try testing.expectEqualStrings(before, try env.snapshot());
    }
}

test "a hostile name hidden in a recovery volume is found" {
    // The index is clean; the evil description sits inside the payload of a
    // recovery packet. par2 finds packets by magic wherever it resynchronises,
    // so the check has to as well.
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const a = env.a();
    const data = u.payload(a, 3, 2500);
    const good = try b.TestSet.init(a, 1000, &.{.{ .name = "ok.bin", .data = data }});
    const evil = try b.TestSet.init(a, 1000, &.{.{ .name = "../escaped.bin", .data = data }});
    try env.write("dl/ok.bin", data);
    try env.write("dl/set.par2", try good.index());
    try env.write("dl/set.vol0+1.par2", try good.volume(&.{try evil.fileDescPacket(0)}));
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expectError(error.PathViolation, run.result);
    try testing.expectEqual(0, f.calls);
}

test "a symlinked target is refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try hostileDir(&env, "target.bin");
    try env.tmp.dir.symLink(testing.io, try env.path("bystander.txt"), "dl/target.bin", .{});
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expectError(error.PathViolation, run.result);
    try testing.expectEqual(0, f.calls);
}

test "a target that is a directory is refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try hostileDir(&env, "target.bin");
    try env.tmp.dir.createDirPath(testing.io, "dl/target.bin");
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expectError(error.PathViolation, run.result);
}

test "a target below a symlinked directory that leaves the folder is refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try hostileDir(&env, "link/x.bin");
    try env.tmp.dir.createDirPath(testing.io, "outside");
    try env.tmp.dir.symLink(testing.io, try env.path("outside"), "dl/link", .{});
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expectError(error.PathViolation, run.result);
    try testing.expectEqual(0, f.calls);
}

test "a set with a safe name goes through to par2" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try hostileDir(&env, "renamed.bin");
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expect((try run.outcome()).has_par2);
    try testing.expectEqual(1, f.calls);
}

test "sets with many files allocate in the arena and nowhere else" {
    // A directory with many top-level files: the extras list grows with it,
    // everything is freed with the arena (the allocator checks for leaks).
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try setDir(&env, &.{"a.bin"});
    for (0..300) |i| try env.write(try std.fmt.allocPrint(env.a(), "f{d:0>4}.dat", .{i}), "x");
    var f = Fake.init();
    defer f.deinit();
    const rep = repairer(&f);

    var run = u.repairOf(&rep, env.root);
    defer run.deinit();
    _ = try run.outcome();

    try testing.expectEqual(300, std.mem.count(u8, f.stdin, "\n"));
}
