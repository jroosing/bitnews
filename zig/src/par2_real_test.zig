//! End-to-end tests of the par2 wrapper against the real, pinned par2
//! (bin/tools/par2; a missing program fails the tests, run `make tools`).
//! The cases are those of internal/postproc/par2 repair_test.go and
//! hostile_test.go. Sets are made by `par2 c` in a temp directory by the test
//! (the Go tests do the same since ADR-0007); only the hostile ones are
//! crafted with the packet builder.
const std = @import("std");
const testing = std.testing;
const ports = @import("postprocess_ports.zig");
const b = @import("par2_testbuilder.zig");
const u = @import("par2_testutil.zig");
const Env = u.Env;

const two = [_]Env.Spec{ .{ .name = "a.bin", .size = 10000 }, .{ .name = "b.bin", .size = 4500 } };
const set_listing = [_][]const u8{ "a.bin", "b.bin", "set.par2", "set.vol0+3.par2", "set.vol3+3.par2" };

fn repair(env: *Env) !u.Run {
    const rep = try env.repairer();
    return u.repairOf(&rep, env.root);
}

fn mustRepair(env: *Env) !ports.RepairOutcome {
    var run = try repair(env);
    defer run.deinit();
    return run.result catch |e| {
        std.debug.print("repair: {s}: {s}\n", .{ @errorName(e), run.diag.message });
        return e;
    };
}

test "repair of an intact set changes nothing" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    const before = try env.snapshot();

    try testing.expectEqual(ports.RepairOutcome{ .has_par2 = true }, try mustRepair(&env));
    try testing.expectEqualStrings(before, try env.snapshot());
}

test "repair without PAR2 files has no set" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    for ([_][]const u8{ "set.par2", "set.vol0+3.par2", "set.vol3+3.par2" }) |p| try env.remove(p);

    try testing.expect(!(try mustRepair(&env)).has_par2);
}

test "repair of an empty directory has no set" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();

    try testing.expect(!(try mustRepair(&env)).has_par2);
}

test "repair restores damaged, missing and truncated files" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(8, &.{ .{ .name = "a.bin", .size = 10000 }, .{ .name = "b.bin", .size = 4500 }, .{ .name = "c.bin", .size = 3000 } });
    try env.corrupt("a.bin", &.{ 10, 7777 }); // blocks 0 and 7
    try env.truncate("b.bin", 2000); // blocks 2, 3, 4
    try env.remove("c.bin"); // 3 blocks

    try testing.expectEqual(ports.RepairOutcome{ .has_par2 = true, .repaired_slices = 8 }, try mustRepair(&env));
    try env.requireOriginals();
    try env.expectListing(&.{ "a.bin", "b.bin", "c.bin", "set.par2", "set.vol0+4.par2", "set.vol4+4.par2" }); // the name.1 backups are gone
}

test "repair is repeatable" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.corrupt("a.bin", &.{10});

    _ = try mustRepair(&env);
    const out = try mustRepair(&env); // nothing left to repair is success

    try testing.expectEqual(0, out.repaired_slices);
    try env.requireOriginals();
}

test "repair works without the main PAR2" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.remove("set.par2");
    try env.corrupt("a.bin", &.{0});

    try testing.expect((try mustRepair(&env)).has_par2);
    try env.requireOriginals();
}

test "repair works with a damaged main PAR2" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.write("set.par2", u.payload(env.a(), 7, 600));
    try env.corrupt("b.bin", &.{0});

    _ = try mustRepair(&env);
    try env.requireOriginals();
}

test "repair finds recovery volumes with other names" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.rename("set.vol0+3.par2", "zz9.vol0+3.par2");
    try env.rename("set.vol3+3.par2", "qq1.par2");
    try env.corrupt("a.bin", &.{ 0, 1500, 2500, 3500, 4500 }); // needs 5 of the 6 blocks

    _ = try mustRepair(&env);
    try env.requireOriginals();
}

test "repair renames obfuscated files" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.rename("a.bin", "x7f3a9");
    try env.rename("b.bin", "0c1d2e.dat");

    try testing.expect((try mustRepair(&env)).has_par2);
    try env.expectListing(&set_listing);
    try env.requireOriginals();

    _ = try mustRepair(&env); // a second run finds it already done
    try env.expectListing(&set_listing);
}

// Fully obfuscated posts name the PAR2 files like the data: random hex, no
// extension. They are found by their content and given a .par2 name, so the
// set is used and the data gets its real names back.
test "repair finds obfuscated PAR2 files by content" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.rename("a.bin", "13184cde52aaba45");
    try env.rename("b.bin", "8f491bbf0db79b93");
    try env.rename("set.par2", "21cb82676987e551");
    try env.rename("set.vol0+3.par2", "b7578fd2e094aa01");
    try env.rename("set.vol3+3.par2", "77e1c0de99aa1b2c");

    try testing.expect((try mustRepair(&env)).has_par2);
    try env.requireOriginals();
    var par2s: usize = 0;
    var rest: std.ArrayList([]const u8) = .empty;
    for (try env.listing()) |n| {
        if (std.mem.endsWith(u8, n, ".par2")) par2s += 1 else try rest.append(env.a(), n);
    }
    try testing.expectEqual(2, rest.items.len); // no obfuscated name is left
    try testing.expectEqualStrings("a.bin", rest.items[0]);
    try testing.expectEqualStrings("b.bin", rest.items[1]);
    try testing.expectEqual(3, par2s); // every PAR2 file now has a .par2 name
}

test "repair renames an obfuscated file over a wrong one and drops the backup" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.rename("a.bin", "x7f3a9");
    try env.write("a.bin", u.payload(env.a(), 99, 10000)); // a wrong file under the right name

    _ = try mustRepair(&env);

    try env.expectListing(&set_listing); // no a.bin.1 left
    try env.requireOriginals();
}

test "repair repairs an obfuscated and damaged file and deletes the damaged copy" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.corrupt("a.bin", &.{10});
    try env.rename("a.bin", "x7f3a9");

    _ = try mustRepair(&env);

    try env.requireOriginals();
    try env.expectListing(&set_listing); // the damaged copy is gone
}

test "insufficient recovery is a shortfall and changes nothing" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.corrupt("a.bin", &.{ 0, 1500, 2500, 3500, 4500, 5500, 6500, 7500 }); // 8 blocks, 6 recovery
    const before = try env.snapshot();

    var run = try repair(&env);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(8, run.diag.needed);
    try testing.expectEqual(6, run.diag.available);
    try testing.expectEqualStrings("not enough recovery data: need 8 blocks, have 6", run.diag.message);
    try testing.expectEqualStrings(before, try env.snapshot());
}

test "repair of a missing file needs all its blocks" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.remove("a.bin");

    var run = try repair(&env);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(10, run.diag.needed);
    try testing.expectEqual(6, run.diag.available);
}

test "repair without recovery volumes is a shortfall" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.remove("set.vol0+3.par2");
    try env.remove("set.vol3+3.par2");
    try env.corrupt("a.bin", &.{0});

    var run = try repair(&env);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(1, run.diag.needed);
    try testing.expectEqual(0, run.diag.available);
}

test "repair counts a partial recovery volume" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.corrupt("a.bin", &.{ 0, 1500, 2500 });
    try env.truncate("set.vol3+3.par2", @intCast(try env.fileSize("set.vol3+3.par2") / 2)); // loses its last blocks
    try env.remove("set.vol0+3.par2");

    var run = try repair(&env);
    defer run.deinit();

    try testing.expectError(error.InsufficientRecovery, run.result);
    try testing.expectEqual(3, run.diag.needed);
    try testing.expect(run.diag.available > 0 and run.diag.available < 3);
}

test "repair picks up the state of an interrupted repair" {
    // A repair killed after par2 moved the damaged a.bin to a.bin.1 and before
    // it wrote the new a.bin leaves exactly this.
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.corrupt("a.bin", &.{ 10, 4500 });
    try env.rename("a.bin", "a.bin.1");

    _ = try mustRepair(&env);

    try env.requireOriginals();
    try env.expectListing(&set_listing);
}

test "repair drops the backups of an interrupted repair when clean" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.write("a.bin.1", u.payload(env.a(), 3, 777));
    try env.write("a.bin.12", u.payload(env.a(), 4, 777));
    try env.write("unrelated.1", "keep");

    _ = try mustRepair(&env);

    try env.expectListing(&(set_listing ++ [_][]const u8{"unrelated.1"}));
}

test "repair restores files in subdirectories" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &.{ .{ .name = "a.bin", .size = 5000 }, .{ .name = "sub/c.bin", .size = 3000 } });
    try env.remove("sub/c.bin");
    try env.tmp.dir.deleteDir(testing.io, "sub");

    _ = try mustRepair(&env);

    try env.requireOriginals();
}

test "repair fixes trailing garbage" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.write("b.bin", try std.mem.concat(env.a(), u8, &.{ try env.read("b.bin"), "trailing junk" }));

    _ = try mustRepair(&env);

    try env.requireOriginals();
}

test "repair keeps unrelated files and subdirectories" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.makeSet(6, &two);
    try env.write("release.nfo", "hello");
    try env.write("sample.mkv", u.payload(env.a(), 8, 2000));
    try env.write("dup.dat", env.files.get("b.bin").?);
    try env.write("other/inner.bin", u.payload(env.a(), 5, 3000));
    try env.write("-weird name .txt", "x");
    try env.corrupt("a.bin", &.{10});

    _ = try mustRepair(&env);

    try testing.expectEqualStrings("hello", try env.read("release.nfo"));
    try testing.expectEqualSlices(u8, u.payload(env.a(), 5, 3000), try env.read("other/inner.bin"));
    try env.expectListing(&(set_listing ++ [_][]const u8{ "dup.dat", "sample.mkv", "release.nfo", "other/inner.bin", "-weird name .txt" }));
}

// ---- hostile sets, hand-crafted packets with the real par2 ----

/// A Download folder `dl` inside the temp root, with a bystander file next to
/// it and an obfuscated copy of the data that a crafted PAR2 file names.
fn hostileDir(env: *Env, name: []const u8) !void {
    try env.write("bystander.txt", "mine");
    const data = u.payload(env.a(), 3, 2500);
    try env.write("dl/obfuscated", data);
    const set = try b.TestSet.init(env.a(), u.block_size, &.{.{ .name = name, .data = data }});
    try env.write("dl/set.par2", try set.index());
}

test "a hostile set is refused with the real par2 and nothing lands outside" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try hostileDir(&env, "../escaped.bin");
    const before = try env.snapshot();
    const rep = try env.repairer();

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expectError(error.PathViolation, run.result);
    try testing.expectEqualStrings(before, try env.snapshot());
}

test "a safe hand-written set works with par2: the obfuscated file is renamed" {
    // Control for the test above: the same builder, a harmless name.
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try hostileDir(&env, "renamed.bin");
    const rep = try env.repairer();

    var run = u.repairOf(&rep, try env.path("dl"));
    defer run.deinit();

    try testing.expect((try run.outcome()).has_par2);
    try testing.expect(env.exists("dl/renamed.bin"));
    try testing.expect(!env.exists("dl/obfuscated"));
    try testing.expect(env.exists("bystander.txt"));
}
