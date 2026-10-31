//! The restart, cancellation and safety tests of internal/postproc/finalize
//! (safety_test.go), and hostile trees the Go tests do not have.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const finalize = @import("finalize.zig");
const env_mod = @import("finalize_env.zig");
const Env = env_mod.Env;
const ports = @import("postprocess_ports.zig");
const fsutil = @import("fsutil.zig");

const io = env_mod.io;

fn contains(hay: []const u8, needle: []const u8) bool {
    return std.mem.indexOf(u8, hay, needle) != null;
}

// ---- moving, warnings, the marker ----

test "a cross-device move emits a warning" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.move_mode = .pretend_cross_device;
    const d = try env.usenet("Show");
    const out = try env.finishUsenet(d);
    try testing.expectEqual(@as(?ports.PortError, null), out.err);

    try testing.expectEqual(@as(usize, 1), env.warnings.items.len);
    const w = env.warnings.items[0];
    try testing.expectEqual(finalize.Warning.Kind.cross_device, w.kind);
    try testing.expectEqualStrings(d.id, w.download_id);
    try testing.expectEqualStrings(out.path, w.path);
    try testing.expect(contains(w.message, "copied instead of renamed"));
    try testing.expect(contains(w.message, "different filesystems"));
    try testing.expect(contains(w.message, "Sonarr/Radarr"));
    try testing.expect(contains(w.message, "complete folder"));
    try testing.expect(!contains(w.message, "hardlinks will not work")); // bitnews does not make hardlinks
}

test "a same-device move emits no warning" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    _ = try env.finishUsenet(try env.usenet("Show"));
    try testing.expectEqual(@as(usize, 0), env.warnings.items.len);
}

test "a move failure is returned" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.move_mode = .fail;
    const out = try env.finishUsenet(try env.usenet("Show"));
    try testing.expectEqual(@as(?ports.PortError, error.Failed), out.err);
    try testing.expect(contains(out.message, "DiskOnFire"));
}

test "the marker is written before the move with mode 0644" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = try env.usenet("Show");
    const out = try env.finishUsenet(d);

    const p = try env.join(&.{ out.path, ".bitnews-done" });
    const st = try Io.Dir.cwd().statFile(io, p, .{});
    try testing.expectEqual(@as(u32, 0o644), st.permissions.toMode() & 0o644);
    const text = try env.read(p);
    try testing.expectEqualStrings(try std.fmt.allocPrint(env.a(), "{s}\n2026-10-30T12:00:00Z\n", .{d.id}), text);
}

test "nothing changes inside the final dir after the move" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.move_mode = .record;
    const out = try env.finishUsenet(try env.usenet("Show"));
    try testing.expectEqualStrings(env.at_move, try env.listing(out.path)); // cleanup and marker happen before the move
}

// ---- restarts ----

test "a retry after a crash returns the already moved dir" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = try env.usenet("Show");
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{.{ "a.mkv", "a" }});

    const first = try env.ok(d, dir, "");
    try testing.expect(!env.has(dir));
    const before = try env.hashTree(first);

    const again = try env.ok(d, dir, "");
    try testing.expectEqualStrings(first, again);
    try testing.expectEqualSlices(u8, &before, &(try env.hashTree(first))); // retry changes nothing
}

test "a retry after a crash finds the Unique variant" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const taken = try env.join(&.{ env.usenet_complete, "Show" });
    try env.put(taken, &.{.{ "other.mkv", "other" }});
    const d = try env.usenet("Show");
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{.{ "a.mkv", "a" }});

    const first = try env.ok(d, dir, "");
    try testing.expectEqualStrings(try std.fmt.allocPrint(env.a(), "{s}.1", .{taken}), first);

    const again = try env.ok(d, dir, "");
    try testing.expectEqualStrings(first, again);
}

test "a retry does not adopt another Download's dir" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    _ = try env.finishUsenet(try env.usenet("Show"));

    // Same name, different Download, dir lost.
    const gone = try env.join(&.{ env.usenet_incomplete, "gone" });
    const msg = try env.failed(try env.usenet("Show"), gone, "");
    try testing.expect(contains(msg, "is gone and no finished copy"));
}

test "a retry with a partially cleaned dir resumes" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    // The crash came after the archives were deleted and half the output was merged.
    try env.put(dir, &.{ .{ "a.mkv", "merged" }, .{ "keep.nfo", "nfo" }, .{ "_unpack/b.mkv", "pending" } });

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try env.expectTree(out, &.{ .{ "a.mkv", "merged" }, .{ "b.mkv", "pending" }, .{ "keep.nfo", "nfo" } });
}

test "a retry when the unpacked dir was already merged and removed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "a.mkv", "merged" }, .{ "x.par2", "p" } });

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try testing.expect(!env.has(try env.join(&.{ out, "x.par2" })));
    try testing.expect(env.has(try env.join(&.{ out, "a.mkv" })));
}

test "a missing unpacked dir with archives left deletes nothing" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "movie.rar", "rar" }, .{ "movie.par2", "p" } });

    _ = try env.failed(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try testing.expect(env.has(try env.join(&.{ dir, "movie.rar" })));
    try testing.expect(env.has(try env.join(&.{ dir, "movie.par2" })));
}

// ---- cancellation ----

test "a Canceled from the lookup of the Category does nothing" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "a.mkv", "a" }, .{ "a.par2", "p" } });
    const before = try env.hashTree(dir);
    var d = try env.usenet("Show");
    d.category_id = try env.newId();
    env.category_error = error.Canceled;

    const out = try env.run(d, dir, "");

    try testing.expectEqual(@as(?ports.PortError, error.Canceled), out.err);
    try testing.expectEqualSlices(u8, &before, &(try env.hashTree(dir)));
    try testing.expect(!env.has(env.usenet_complete));
}

fn sleepyLookup(_: *Env) anyerror!void {
    try io.sleep(.fromSeconds(30), .awake); // returns error.Canceled when the task is cancelled
}

fn runTask(env: *Env, d: @import("download.zig").Download, dir: []const u8) anyerror!Env.Out {
    return env.run(d, dir, "");
}

test "cancelling the task before the move leaves the dir in place" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "a.mkv", "a" }, .{ "a.par2", "p" } });
    var d = try env.usenet("Show");
    d.category_id = try env.newId();
    env.category_hook = sleepyLookup;

    var fut = try io.concurrent(runTask, .{ &env, d, dir });
    try io.sleep(.fromMilliseconds(100), .awake); // let it block in the lookup
    const out = try fut.cancel(io);

    try testing.expectEqual(@as(?ports.PortError, error.Canceled), out.err);
    try testing.expect(env.has(try env.join(&.{ dir, "a.mkv" })));
    try testing.expect(env.has(try env.join(&.{ dir, "a.par2" })));
    try testing.expect(!env.has(try env.join(&.{ dir, finalize.marker_name })));
    try testing.expect(!env.has(env.usenet_complete));
}

// ---- cleanup never leaves the dir ----

test "cleanup never touches files outside the dir" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("Show");
    const parent = env.usenet_incomplete;
    const sibling = try env.join(&.{ parent, "Show2" }); // shares a name prefix with dir
    try env.put(parent, &.{ .{ "parent.par2", "p" }, .{ "parent.nzb", "n" }, .{ "parent.rar", "r" } });
    try env.put(sibling, &.{ .{ "x.par2", "p" }, .{ "x.rar", "r" }, .{ "x.nzb", "n" } });
    const outside = try env.join(&.{ env.tree.path, "outside" });
    try env.put(outside, &.{ .{ "victim.par2", "p" }, .{ "victim.rar", "r" }, .{ "victim.nzb", "n" } });
    try env.put(dir, &.{ .{ "a.mkv", "a" }, .{ "a.par2", "p" }, .{ "a.rar", "r" }, .{ "_unpack/b.mkv", "b" } });
    // Symlinks named like garbage, pointing at the outside files and dirs.
    try env.link(try env.join(&.{ outside, "victim.par2" }), try env.join(&.{ dir, "link.par2" }));
    try env.link(try env.join(&.{ outside, "victim.rar" }), try env.join(&.{ dir, "link.rar" }));
    try env.link(outside, try env.join(&.{ dir, "_unpack.tmp" }));
    try env.link(outside, try env.join(&.{ dir, "dirlink.par2" }));
    const before_parent = try env_mod.dropLines(env.a(), try env.listing(parent), &.{ "Show = ", "Show/" });
    const before_outside = try env.listing(outside);

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));

    const after_parent = try env_mod.dropLines(env.a(), try env.listing(parent), &.{ "Show = ", "Show/" });
    try testing.expectEqualStrings(before_parent, after_parent);
    try testing.expectEqualStrings(before_outside, try env.listing(outside));
    // The symlinks themselves were neither followed nor deleted.
    for ([_][]const u8{ "link.par2", "link.rar", "_unpack.tmp", "dirlink.par2" }) |l| {
        const st = try Io.Dir.cwd().statFile(io, try env.join(&.{ out, l }), .{ .follow_symlinks = false });
        try testing.expectEqual(Io.File.Kind.sym_link, st.kind);
    }
    try testing.expect(env.has(try env.join(&.{ out, "b.mkv" })));
    try testing.expect(!env.has(try env.join(&.{ out, "a.rar" })));
}

test "an unpacked dir that is a symlink is refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    const outside = try env.join(&.{ env.tree.path, "outside" });
    try env.put(outside, &.{.{ "secret", "s" }});
    try env.put(dir, &.{.{ "a.rar", "r" }});
    try env.link(outside, try env.join(&.{ dir, "_unpack" }));

    const msg = try env.failed(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try testing.expect(contains(msg, "symlink refused"));
    try testing.expect(env.has(try env.join(&.{ dir, "a.rar" })));
    try testing.expect(env.has(try env.join(&.{ outside, "secret" })));
}

test "an unpacked dir overlapping the download dir is refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{.{ "a.rar", "r" }});
    for ([_][]const u8{ dir, env.usenet_incomplete, try env.join(&.{ dir, "_unpack.tmp", "x" }) }) |un| {
        _ = try env.failed(try env.usenet("Show"), dir, un);
        try testing.expect(env.has(try env.join(&.{ dir, "a.rar" })));
    }
}

test "an unpacked entry never overwrites a kept file" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "movie.nfo", "kept" }, .{ "a.rar", "r" }, .{ "_unpack/movie.nfo", "from archive" } });

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try testing.expectEqualStrings("kept", try env.read(try env.join(&.{ out, "movie.nfo" })));
    try testing.expectEqualStrings("from archive", try env.read(try env.join(&.{ out, "movie.1.nfo" })));
}

test "an unpacked dir outside the download dir is merged and removed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    const un = try env.join(&.{ env.usenet_incomplete, "w.unpacked" });
    try env.put(dir, &.{ .{ "a.rar", "r" }, .{ "a.nfo", "nfo" } });
    try env.put(un, &.{.{ "film.mkv", "film" }});

    const out = try env.ok(try env.usenet("Show"), dir, un);
    try env.expectTree(out, &.{ .{ "a.nfo", "nfo" }, .{ "film.mkv", "film" } });
    try testing.expect(!env.has(un));
}

test "an existing foreign marker is kept with a warning" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.torrentDir("Rel");
    try env.put(dir, &.{ .{ finalize.marker_name, "i am an original" }, .{ "a", "a" } });

    const out = try env.ok(try env.torrent("Rel"), dir, "");
    try testing.expectEqualStrings("i am an original", try env.read(try env.join(&.{ out, finalize.marker_name })));
    try testing.expectEqual(@as(usize, 1), env.warnings.items.len);
    try testing.expectEqual(finalize.Warning.Kind.marker_conflict, env.warnings.items[0].kind);
}

test "the dir must exist and be a directory" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    _ = try env.failed(try env.usenet("S"), "", "");
    _ = try env.failed(try env.usenet("S"), try env.join(&.{ env.tree.path, "nope" }), "");
    try env.tree.write("file", "");
    _ = try env.failed(try env.usenet("S"), try env.join(&.{ env.tree.path, "file" }), "");
    try env.link(env.tree.path, try env.join(&.{ env.tree.path, "link" }));
    const msg = try env.failed(try env.usenet("S"), try env.join(&.{ env.tree.path, "link" }), "");
    try testing.expect(contains(msg, "symlink refused"));
}

test "an unwritable complete folder fails naming the folder and the user" {
    if (fsutil.effectiveUid() == 0) return error.SkipZigTest; // root can write anywhere
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.tree.mkdir("usenet/complete");
    try env.tree.tmp.dir.setFilePermissions(io, "usenet/complete", .fromMode(0o555), .{});
    defer env.tree.tmp.dir.setFilePermissions(io, "usenet/complete", .fromMode(0o755), .{}) catch {};

    const out = try env.finishUsenet(try env.usenet("Show"));

    try testing.expectEqual(@as(?ports.PortError, error.Failed), out.err);
    try testing.expect(out.vouched); // the orchestrator shows it as it is
    try testing.expect(contains(out.message, env.usenet_complete));
    try testing.expect(contains(out.message, try std.fmt.allocPrint(env.a(), "uid {d}", .{fsutil.effectiveUid()})));
}

// ---- hostile trees the Go tests do not have ----

test "a symlink named like the marker is not followed or replaced" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.torrentDir("Rel");
    const victim = try env.join(&.{ env.tree.path, "victim" });
    try env.put(env.tree.path, &.{.{ "victim", "precious" }});
    try env.put(dir, &.{.{ "a", "a" }});
    try env.link(victim, try env.join(&.{ dir, finalize.marker_name }));

    const out = try env.ok(try env.torrent("Rel"), dir, "");

    try testing.expectEqualStrings("precious", try env.read(victim));
    try testing.expectEqual(@as(usize, 1), env.warnings.items.len);
    try testing.expectEqual(finalize.Warning.Kind.marker_conflict, env.warnings.items[0].kind);
    const st = try Io.Dir.cwd().statFile(io, try env.join(&.{ out, finalize.marker_name }), .{ .follow_symlinks = false });
    try testing.expectEqual(Io.File.Kind.sym_link, st.kind);
}

test "a symlink named like the marker never counts as a finished copy" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = try env.usenet("Show");
    // A directory with the wanted name whose marker is a link to a file that holds our ID.
    const fake = try env.join(&.{ env.usenet_complete, "Show" });
    try env.put(env.tree.path, &.{.{ "idfile", d.id }});
    try env.put(fake, &.{.{ "x", "x" }});
    try env.link(try env.join(&.{ env.tree.path, "idfile" }), try env.join(&.{ fake, finalize.marker_name }));

    const msg = try env.failed(d, try env.join(&.{ env.usenet_incomplete, "gone" }), "");
    try testing.expect(contains(msg, "is gone"));
}

test "garbage names that are directories or links to directories are kept" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "dir.par2/inner.txt", "inner" }, .{ "dir.rar/inner.txt", "inner" }, .{ "a.mkv", "a" } });

    const out = try env.ok(try env.usenet("Show"), dir, "");
    try env.expectTree(out, &.{
        .{ "dir.par2", "<dir>" },
        .{ "dir.par2/inner.txt", "inner" },
        .{ "dir.rar", "<dir>" },
        .{ "dir.rar/inner.txt", "inner" },
        .{ "a.mkv", "a" },
    });
}

test "a regular file named like the staging dir is kept" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "_unpack.tmp", "a file" }, .{ "a.rar", "r" }, .{ "_unpack/x", "x" } });

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try env.expectTree(out, &.{ .{ "_unpack.tmp", "a file" }, .{ "x", "x" } });
}

test "the staging dir is removed even when it holds read-only dirs and links" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    const outside = try env.join(&.{ env.tree.path, "outside" });
    try env.put(outside, &.{.{ "keep", "k" }});
    try env.put(dir, &.{ .{ "a.rar", "r" }, .{ "_unpack.tmp/ro/file", "f" }, .{ "_unpack/x", "x" } });
    try env.link(outside, try env.join(&.{ dir, "_unpack.tmp", "ro", "link" }));
    try env.tree.tmp.dir.setFilePermissions(io, try env.join(&.{ env.rel(dir), "_unpack.tmp", "ro" }), .fromMode(0o500), .{});

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try env.expectTree(out, &.{.{ "x", "x" }});
    try testing.expectEqualStrings("k", try env.read(try env.join(&.{ outside, "keep" })));
}

test "a link inside the unpacked output is merged as a link, never followed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    const outside = try env.join(&.{ env.tree.path, "outside" });
    try env.put(outside, &.{.{ "secret.par2", "s" }});
    try env.put(dir, &.{ .{ "a.rar", "r" }, .{ "_unpack/ok.mkv", "ok" } });
    try env.link(outside, try env.join(&.{ dir, "_unpack", "evil" }));

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    const st = try Io.Dir.cwd().statFile(io, try env.join(&.{ out, "evil" }), .{ .follow_symlinks = false });
    try testing.expectEqual(Io.File.Kind.sym_link, st.kind);
    try testing.expectEqualStrings("s", try env.read(try env.join(&.{ outside, "secret.par2" })));
}

test "a download dir given with dots and a trailing slash is cleaned" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "a.mkv", "a" }, .{ "a.par2", "p" } });
    const messy = try std.fmt.allocPrint(env.a(), "{s}/../w/./", .{dir});

    const out = try env.ok(try env.usenet("Show"), messy, "");
    try env.expectTree(out, &.{.{ "a.mkv", "a" }});
}

test "names with newlines and spaces survive the merge" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const dir = try env.usenetDir("w");
    try env.put(dir, &.{ .{ "a.rar", "r" }, .{ "_unpack/we ird\nname.mkv", "w" }, .{ "_unpack/ .hidden", "h" } });

    const out = try env.ok(try env.usenet("Show"), dir, try env.join(&.{ dir, "_unpack" }));
    try env.expectTree(out, &.{ .{ "we ird\nname.mkv", "w" }, .{ " .hidden", "h" } });
}
