//! Tests of the data remover: the cases of internal/daemon/remover_test.go,
//! the TestAPIDeleteDownloadWithData* scenarios of the black-box suite, and
//! the hostile cases the Go tests do not cover.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const fsutil = @import("fsutil.zig");
const remover = @import("remover.zig");
const Remover = remover.Remover;
const Category = remover.Category;
const Paths = remover.Paths;

const id_a = "aaaaaaaaaaaaaaaaaaaaaaaaaa";
const id_b = "bbbbbbbbbbbbbbbbbbbbbbbbbb";

/// Records what it was asked to hold and release.
const FakeHolder = struct {
    held: usize = 0,
    released: usize = 0,
    fail: bool = false,
    last: [64]u8 = undefined,
    last_len: usize = 0,

    fn holder(self: *FakeHolder) remover.FetchHolder {
        return .{ .ctx = self, .hold_fn = hold, .release_fn = release };
    }

    fn hold(ctx: ?*anyopaque, id: []const u8) anyerror!void {
        const self: *FakeHolder = @ptrCast(@alignCast(ctx.?));
        self.held += 1;
        self.last_len = id.len;
        @memcpy(self.last[0..id.len], id);
        if (self.fail) return error.StillRunning;
    }

    fn release(ctx: ?*anyopaque, id: []const u8) void {
        const self: *FakeHolder = @ptrCast(@alignCast(ctx.?));
        self.released += 1;
        std.debug.assert(std.mem.eql(u8, self.last[0..self.last_len], id));
    }
};

/// A temp tree with the four roots of two protocols and a place outside them.
/// Does not move after `init`: the remover holds pointers into it.
const Env = struct {
    tmp: testing.TmpDir,
    arena: std.heap.ArenaAllocator,
    base: []const u8,
    incomplete: []const u8,
    complete: []const u8,
    tincomplete: []const u8,
    tcomplete: []const u8,
    outside: []const u8,
    paths: Paths,
    cats: []const Category = &.{},
    usenet: FakeHolder = .{},
    torrent: FakeHolder = .{},
    work_dir: []const u8 = "",
    remover: Remover,

    fn init(env: *Env) !void {
        env.cats = &.{};
        env.usenet = .{};
        env.torrent = .{};
        env.work_dir = "";
        env.tmp = testing.tmpDir(.{});
        errdefer env.tmp.cleanup();
        env.arena = .init(testing.allocator);
        errdefer env.arena.deinit();
        const a = env.arena.allocator();
        var buf: [Io.Dir.max_path_bytes]u8 = undefined;
        const n = try env.tmp.dir.realPath(testing.io, &buf);
        env.base = try a.dupe(u8, buf[0..n]);
        env.incomplete = try fsutil.join(a, &.{ env.base, "incomplete" });
        env.complete = try fsutil.join(a, &.{ env.base, "complete" });
        env.tincomplete = try fsutil.join(a, &.{ env.base, "tincomplete" });
        env.tcomplete = try fsutil.join(a, &.{ env.base, "tcomplete" });
        env.outside = try fsutil.join(a, &.{ env.base, "outside" });
        for ([_][]const u8{ "incomplete", "complete", "tincomplete", "tcomplete", "outside" }) |d| {
            try env.tmp.dir.createDirPath(testing.io, d);
        }
        env.paths = .{
            .usenet = .{ .incomplete = env.incomplete, .complete = env.complete },
            .torrent = .{ .incomplete = env.tincomplete, .complete = env.tcomplete },
        };
        env.remover = Remover.init(testing.allocator, testing.io, .{
            .paths = .{ .ctx = env, .paths_fn = pathsFn },
            .categories = .{ .ctx = env, .list_fn = catsFn },
            .usenet_holder = env.usenet.holder(),
            .torrent_holder = env.torrent.holder(),
        });
    }

    fn deinit(env: *Env) void {
        env.remover.deinit();
        env.arena.deinit();
        env.tmp.cleanup();
    }

    fn pathsFn(ctx: ?*anyopaque, a: Allocator) anyerror!Paths {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        return .{
            .usenet = .{ .incomplete = try a.dupe(u8, env.paths.usenet.incomplete), .complete = try a.dupe(u8, env.paths.usenet.complete) },
            .torrent = .{ .incomplete = try a.dupe(u8, env.paths.torrent.incomplete), .complete = try a.dupe(u8, env.paths.torrent.complete) },
        };
    }

    fn catsFn(ctx: ?*anyopaque, _: Allocator) anyerror![]const Category {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        return env.cats;
    }

    /// The working directory of every Download of the protocol, from a seam.
    fn useWorkDir(env: *Env, kind: dl.SourceKind, dir: []const u8) void {
        env.work_dir = dir;
        const seam: remover.WorkDirs = .{ .ctx = env, .dir_fn = workDirFn };
        switch (kind) {
            .nzb => env.remover.deps.usenet_dirs = seam,
            .torrent => env.remover.deps.torrent_dirs = seam,
        }
    }

    fn workDirFn(ctx: ?*anyopaque, a: Allocator, _: dl.Download) anyerror![]const u8 {
        const env: *Env = @ptrCast(@alignCast(ctx.?));
        return a.dupe(u8, env.work_dir);
    }

    fn abs(env: *Env, rel: []const u8) []const u8 {
        return fsutil.join(env.arena.allocator(), &.{ env.base, rel }) catch @panic("oom");
    }

    fn writeFile(env: *Env, rel: []const u8) !void {
        if (std.fs.path.dirname(rel)) |parent| try env.tmp.dir.createDirPath(testing.io, parent);
        try env.tmp.dir.writeFile(testing.io, .{ .sub_path = rel, .data = "x" });
    }

    fn symlink(env: *Env, target: []const u8, rel: []const u8) !void {
        if (std.fs.path.dirname(rel)) |parent| try env.tmp.dir.createDirPath(testing.io, parent);
        try env.tmp.dir.symLink(testing.io, target, rel, .{});
    }

    /// Whether something (a symlink itself, never what it points at) is at `rel`.
    fn exists(env: *Env, rel: []const u8) bool {
        _ = env.tmp.dir.statFile(testing.io, rel, .{ .follow_symlinks = false }) catch return false;
        return true;
    }

    fn download(kind: dl.SourceKind, id: []const u8, output: []const u8) dl.Download {
        return .{
            .id = id,
            .name = "n",
            .source_kind = kind,
            .state = .ready,
            .added_at = 0,
            .updated_at = 0,
            .output_path = output,
        };
    }

    /// Holds `d` and removes its data as DownloadService.delete does: the
    /// hold is released when the removal fails.
    fn removeData(env: *Env, d: dl.Download) !void {
        const release = try env.remover.hold(d);
        env.remover.removeData(d) catch |err| {
            release.release();
            return err;
        };
    }
};

// ---- the Go tests ----

test "removes the working directory and the output" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const out = env.abs("complete/tv/show");
    try env.writeFile("incomplete/" ++ id_a ++ "/part.bin");
    try env.writeFile("complete/tv/show/episode.mkv");

    try env.removeData(Env.download(.nzb, id_a, out));

    try testing.expect(!env.exists("incomplete/" ++ id_a));
    try testing.expect(!env.exists("complete/tv/show"));
    try testing.expect(env.exists("complete/tv"));
    try testing.expect(env.exists("incomplete"));
    try testing.expectEqual(@as(usize, 1), env.usenet.held);
    try testing.expectEqual(@as(usize, 0), env.usenet.released);
    try testing.expectEqual(@as(usize, 0), env.torrent.held);
}

test "removing twice is fine" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = Env.download(.nzb, id_a, env.abs("complete/gone"));
    try env.removeData(d);
    try env.removeData(d);
}

test "the roots themselves are refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/keep.txt");
    try env.writeFile("incomplete/keep.txt");
    try env.writeFile("outside/keep.txt");

    const trailing = try std.fmt.allocPrint(env.arena.allocator(), "{s}/", .{env.complete});
    const up = try std.fmt.allocPrint(env.arena.allocator(), "{s}/tv/..", .{env.complete});
    const targets = [_][]const u8{ env.complete, trailing, up, env.incomplete, env.base, "/", "", ".", "..", "//", "/." };
    for (targets) |out| try env.removeData(Env.download(.nzb, id_a, out));

    try testing.expect(env.exists("complete/keep.txt"));
    try testing.expect(env.exists("incomplete/keep.txt"));
    try testing.expect(env.exists("outside/keep.txt"));
}

test "paths outside every root and relative paths are left alone" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/precious");
    try env.writeFile("precious");

    try env.removeData(Env.download(.nzb, id_a, env.outside));
    try env.removeData(Env.download(.nzb, id_a, "relative/path"));
    try env.removeData(Env.download(.nzb, id_a, "outside/precious"));
    // The next two have the shape of a path below a root, but are relative.
    try env.removeData(Env.download(.nzb, id_a, "complete/x"));
    try env.removeData(Env.download(.nzb, id_a, "../outside"));

    try testing.expect(env.exists("outside/precious"));
    try testing.expect(env.exists("precious"));
}

test "a symlinked output is unlinked without following it" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/precious");
    try env.symlink(env.outside, "complete/link");

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/link")));

    try testing.expect(!env.exists("complete/link"));
    try testing.expect(env.exists("outside/precious"));
}

test "symlinks inside the output are not entered" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/precious");
    try env.writeFile("complete/release/a.mkv");
    try env.symlink(env.outside, "complete/release/escape");
    try env.symlink("../../outside", "complete/release/sub/rel-escape");
    try env.symlink("nowhere", "complete/release/dangling");

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/release")));

    try testing.expect(!env.exists("complete/release"));
    try testing.expect(env.exists("outside/precious"));
}

test "a path that climbs out through a symlinked parent is refused and the hold released" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/release/precious");
    try env.symlink(env.outside, "complete/tv");

    const d = Env.download(.nzb, id_a, env.abs("complete/tv/release"));
    try testing.expectError(error.UnsafePath, env.removeData(d));

    try testing.expect(env.exists("outside/release/precious"));
    try testing.expectEqual(@as(usize, 1), env.usenet.released);
}

test "a symlinked parent that stays inside its root is followed and the target removed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/real/release/a.mkv");
    try env.symlink("real", "complete/inside");

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/inside/release")));

    try testing.expect(!env.exists("complete/real/release"));
    try testing.expect(env.exists("complete/real"));
    try testing.expect(env.exists("complete/inside"));
}

test "the folder override of a Category is a root, but never removed itself" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/override/show/a.mkv");
    env.cats = &.{.{ .id = "c", .name = "tv", .usenet_dir = env.abs("outside/override") }};

    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/override/show")));
    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/override")));

    try testing.expect(!env.exists("outside/override/show"));
    try testing.expect(env.exists("outside/override"));
}

test "the overrides of both protocols count" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/u/film/a.mkv");
    try env.writeFile("outside/t/show/a.mkv");
    env.cats = &.{.{ .id = "c", .name = "movies", .usenet_dir = env.abs("outside/u"), .torrent_dir = env.abs("outside/t") }};

    try env.removeData(Env.download(.torrent, id_a, env.abs("outside/u/film")));
    try env.removeData(Env.download(.nzb, id_b, env.abs("outside/t/show")));

    try testing.expect(!env.exists("outside/u/film"));
    try testing.expect(!env.exists("outside/t/show"));
}

test "the removal stops when the fetch cannot be held" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.usenet.fail = true;
    try env.writeFile("incomplete/" ++ id_a ++ "/part.bin");

    try testing.expectError(error.StillRunning, env.removeData(Env.download(.nzb, id_a, "")));

    try testing.expect(env.exists("incomplete/" ++ id_a ++ "/part.bin"));
    // A failed hold leaves nothing behind, so the Download can be held again.
    env.usenet.fail = false;
    try env.removeData(Env.download(.nzb, id_a, ""));
    try testing.expect(!env.exists("incomplete/" ++ id_a));
}

test "a torrent without a hold in a Usenet-only root is left alone" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/t/a");

    try env.removeData(Env.download(.torrent, id_a, env.abs("complete/t")));

    try testing.expect(env.exists("complete/t/a"));
    try testing.expectEqual(@as(usize, 0), env.usenet.held);
    try testing.expectEqual(@as(usize, 1), env.torrent.held);
}

test "a torrent: the whole Download directory goes, unpack folder and .torrent included" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.useWorkDir(.torrent, env.abs("tcomplete/tv/Release"));
    try env.writeFile("tcomplete/tv/Release/Release/a.mkv");
    try env.writeFile("tcomplete/tv/Release/Release/_unpack/film.mkv");
    try env.writeFile("tcomplete/tv/Release/Release.torrent");
    try env.writeFile("tcomplete/tv/Other/keep.mkv");

    try env.removeData(Env.download(.torrent, id_a, env.abs("tcomplete/tv/Release/Release")));

    try testing.expect(!env.exists("tcomplete/tv/Release"));
    try testing.expect(env.exists("tcomplete/tv/Other/keep.mkv"));
    try testing.expectEqual(@as(usize, 1), env.torrent.held);
    try testing.expectEqual(@as(usize, 0), env.usenet.held);
}

test "a torrent is removed without an engine to hold" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.remover.deps.torrent_holder = null;
    env.useWorkDir(.torrent, env.abs("tcomplete/tv/Release"));
    try env.writeFile("tcomplete/tv/Release/a.mkv");

    try env.removeData(Env.download(.torrent, id_a, env.abs("tcomplete/tv/Release")));

    try testing.expect(!env.exists("tcomplete/tv/Release"));
}

test "a torrent is deleted only inside the torrent roots" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/usenet-stuff/precious");
    env.useWorkDir(.torrent, env.abs("complete/usenet-stuff"));

    try env.removeData(Env.download(.torrent, id_a, env.abs("complete/usenet-stuff")));

    try testing.expect(env.exists("complete/usenet-stuff/precious"));
}

test "a torrent whose hold fails keeps its data and nothing is released that was not held" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.torrent.fail = true;
    try env.writeFile("tincomplete/" ++ id_a ++ "/a");

    try testing.expectError(error.StillRunning, env.removeData(Env.download(.torrent, id_a, "")));

    try testing.expect(env.exists("tincomplete/" ++ id_a ++ "/a"));
    try testing.expectEqual(@as(usize, 0), env.torrent.released);
}

// ---- the black-box scenarios (internal/blackbox/api_downloads_test.go) ----

test "black-box: work dir and output go, siblings and roots stay" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("incomplete/" ++ id_a ++ "/sub/file.bin");
    try env.writeFile("complete/series/done/sub/file.bin");
    try env.writeFile("complete/series/sibling/sub/file.bin");

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/series/done")));

    try testing.expect(!env.exists("incomplete/" ++ id_a));
    try testing.expect(!env.exists("complete/series/done"));
    try testing.expect(env.exists("complete/series/sibling/sub/file.bin"));
    try testing.expect(env.exists("complete"));
    try testing.expect(env.exists("incomplete"));
}

test "black-box: outside, root, relative and climbing outputs are all left alone" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/precious/sub/file.bin");
    try env.writeFile("complete/keep.txt");
    const climbing = try std.fmt.allocPrint(env.arena.allocator(), "{s}/../outside/precious", .{env.complete});

    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/precious")));
    try env.removeData(Env.download(.nzb, id_a, env.complete));
    try env.removeData(Env.download(.nzb, id_a, "precious"));
    try env.removeData(Env.download(.nzb, id_a, climbing));

    try testing.expect(env.exists("outside/precious/sub/file.bin"));
    try testing.expect(env.exists("complete/keep.txt"));
}

test "black-box: only the Download's own work dir name is removed from the incomplete root" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("incomplete/" ++ id_b ++ "/sub/file.bin");
    try env.writeFile("incomplete/" ++ id_a ++ "/sub/file.bin");

    try env.removeData(Env.download(.nzb, id_a, ""));

    try testing.expect(!env.exists("incomplete/" ++ id_a));
    try testing.expect(env.exists("incomplete/" ++ id_b ++ "/sub/file.bin"));
}

// ---- beyond Go ----

test "a work dir made under an incomplete root that was changed since is still removed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("elsewhere/" ++ id_a ++ "/part.bin");
    try env.writeFile("elsewhere/other/part.bin");
    env.useWorkDir(.nzb, env.abs("elsewhere/" ++ id_a));

    try env.removeData(Env.download(.nzb, id_a, ""));

    try testing.expect(!env.exists("elsewhere/" ++ id_a));
    try testing.expect(env.exists("elsewhere/other/part.bin"));
}

test "a recorded work dir is trusted only when it is named after the Download" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("elsewhere/something/part.bin");
    env.useWorkDir(.nzb, env.abs("elsewhere/something"));

    try env.removeData(Env.download(.nzb, id_a, ""));
    try env.removeData(Env.download(.nzb, id_a, env.abs("elsewhere/something/part.bin")));

    try testing.expect(env.exists("elsewhere/something/part.bin"));
}

test "a work dir directly under the file system root never makes it a root" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.useWorkDir(.nzb, "/" ++ id_a);
    // Would be inside "/" if it were a root.
    try env.writeFile("outside/precious");

    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/precious")));

    try testing.expect(env.exists("outside/precious"));
}

test "an ID that is not a plain name does not widen the roots" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/precious");
    // incomplete/.. is the temp base; its parent would become a root.
    env.useWorkDir(.nzb, env.abs("incomplete/.."));

    try env.removeData(Env.download(.nzb, "..", env.abs("outside/precious")));
    try env.removeData(Env.download(.nzb, "..", env.abs("incomplete/..")));

    try testing.expect(env.exists("outside/precious"));
}

test "the Settings are read on every call" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/data/a");

    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/data")));
    try testing.expect(env.exists("outside/data/a"));

    env.paths.usenet.complete = env.outside;
    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/data")));
    try testing.expect(!env.exists("outside/data"));
}

test "empty and relative roots never match" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/precious");
    env.paths = .{};
    env.cats = &.{.{ .id = "c", .name = "x", .usenet_dir = "relative", .torrent_dir = "../outside" }};

    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/precious")));
    try env.removeData(Env.download(.nzb, id_a, "relative/x"));
    try env.removeData(Env.download(.nzb, id_a, "../outside/precious"));

    try testing.expect(env.exists("outside/precious"));
}

test "a root of / lets everything below it go but still not / itself" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/data/a");
    env.paths.usenet.complete = "/";

    try env.removeData(Env.download(.nzb, id_a, "/"));
    try env.removeData(Env.download(.nzb, id_a, env.abs("outside/data")));

    try testing.expect(!env.exists("outside/data"));
    try testing.expect(env.exists("outside"));
}

test "a target with a NUL byte is refused" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/keep/a");
    const evil = try std.fmt.allocPrint(env.arena.allocator(), "{s}/keep\x00/../x", .{env.complete});

    try testing.expectError(error.UnsafePath, env.removeData(Env.download(.nzb, id_a, evil)));

    try testing.expect(env.exists("complete/keep/a"));
}

test "a dangling symlink, a symlink loop and a plain file are unlinked" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.symlink("nowhere", "complete/dangling");
    try env.symlink("loop", "complete/loop");
    try env.writeFile("complete/file.mkv");

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/dangling")));
    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/loop")));
    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/file.mkv")));

    try testing.expect(!env.exists("complete/dangling"));
    try testing.expect(!env.exists("complete/loop"));
    try testing.expect(!env.exists("complete/file.mkv"));
}

test "a parent that is a symlink loop is an error" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.symlink("loop", "complete/loop");

    const d = Env.download(.nzb, id_a, env.abs("complete/loop/x"));
    try testing.expectError(error.UnsafePath, env.removeData(d));
}

test "a parent that is a regular file is an error, a missing parent is not" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/file");

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/missing/deeper/x")));
    try testing.expectError(error.NotDir, env.removeData(Env.download(.nzb, id_a, env.abs("complete/file/x"))));
    try testing.expect(env.exists("complete/file"));
}

test "a deep tree with many entries is removed" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const a = env.arena.allocator();
    var path: std.ArrayList(u8) = .empty;
    try path.appendSlice(a, "complete/deep");
    for (0..40) |i| {
        try path.print(a, "/d{d}", .{i});
        try env.tmp.dir.createDirPath(testing.io, path.items);
        const f = try std.fmt.allocPrint(a, "{s}/f.bin", .{path.items});
        try env.tmp.dir.writeFile(testing.io, .{ .sub_path = f, .data = "x" });
    }
    for (0..50) |i| {
        const f = try std.fmt.allocPrint(a, "complete/deep/many{d}.bin", .{i});
        try env.tmp.dir.writeFile(testing.io, .{ .sub_path = f, .data = "x" });
    }

    try env.removeData(Env.download(.nzb, id_a, env.abs("complete/deep")));

    try testing.expect(!env.exists("complete/deep"));
}

test "a directory the daemon may not write to cannot be emptied: an error, and the Download stays" {
    if (std.c.geteuid() == 0) return error.SkipZigTest;
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/locked/a");
    try env.writeFile("complete/open/b");
    const perm = Io.File.Permissions.fromMode(0o500);
    try env.tmp.dir.setFilePermissions(testing.io, "complete/locked", perm, .{});
    defer env.tmp.dir.setFilePermissions(testing.io, "complete/locked", .fromMode(0o700), .{}) catch {};

    try testing.expectError(error.AccessDenied, env.removeData(Env.download(.nzb, id_a, env.abs("complete/locked"))));

    try testing.expect(env.exists("complete/locked/a"));
    try testing.expectEqual(@as(usize, 1), env.usenet.released);
    // The other Downloads are not affected.
    try env.removeData(Env.download(.nzb, id_b, env.abs("complete/open")));
    try testing.expect(!env.exists("complete/open"));
}

test "both targets are tried when the first fails; the error is reported" {
    if (std.c.geteuid() == 0) return error.SkipZigTest;
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("incomplete/" ++ id_a ++ "/a");
    try env.writeFile("complete/out/b");
    try env.tmp.dir.setFilePermissions(testing.io, "incomplete/" ++ id_a, .fromMode(0o500), .{});
    defer env.tmp.dir.setFilePermissions(testing.io, "incomplete/" ++ id_a, .fromMode(0o700), .{}) catch {};

    const d = Env.download(.nzb, id_a, env.abs("complete/out"));
    try testing.expectError(error.AccessDenied, env.removeData(d));

    try testing.expect(env.exists("incomplete/" ++ id_a ++ "/a"));
    try testing.expect(!env.exists("complete/out"));
}

test "holds: no leak across hold, release, removal and a second hold of the same Download" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    const d = Env.download(.nzb, id_a, "");

    const r1 = try env.remover.hold(d);
    try testing.expectError(error.AlreadyHeld, env.remover.hold(d));
    try testing.expectEqual(@as(usize, 1), env.usenet.held);
    r1.release();
    try testing.expectEqual(@as(usize, 1), env.usenet.released);

    // Held again after the release, then removed: the node ends with the removal.
    _ = try env.remover.hold(d);
    try env.remover.removeData(d);
    _ = try env.remover.hold(d);
    try env.remover.removeData(d);
    try testing.expectEqual(@as(usize, 0), env.remover.holds.count());
}

test "holds: a Download that was never held can still be removed, and deinit forgets holds that stay" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.remover.removeData(Env.download(.nzb, id_a, ""));
    // Left in the table on purpose: deinit frees it (testing.allocator checks).
    _ = try env.remover.hold(Env.download(.nzb, id_b, ""));
}

test "holds: a Download with no holder for its protocol gets a Release that does nothing" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    env.remover.deps.usenet_holder = null;
    const r = try env.remover.hold(Env.download(.nzb, id_a, ""));
    r.release();
    try testing.expectEqual(@as(usize, 0), env.remover.holds.count());
}

test "the DataRemover seam holds, removes and releases through the service types" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("complete/out/a");
    const seam = env.remover.asDataRemover();
    const d = Env.download(.nzb, id_a, env.abs("complete/out"));

    const release = try seam.hold(d);
    try seam.removeData(d);
    try testing.expect(!env.exists("complete/out"));
    try testing.expectEqual(@as(usize, 0), env.usenet.released);
    _ = release;

    const bad = Env.download(.nzb, id_b, "");
    const r2 = try seam.hold(bad);
    r2.release();
    try testing.expectEqual(@as(usize, 1), env.usenet.released);
}

// ---- the adapters ----

const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const categories_repo = @import("categories_repo.zig");
const settings_service = @import("settings_service.zig");
const settings_repo = @import("settings_repo.zig");
const event = @import("event.zig");

test "the adapters read the Settings service and the Categories repository" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [Io.Dir.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    const db_path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..n]});
    defer testing.allocator.free(db_path);
    const store = try sqlite.Store.open(testing.allocator, testing.io, db_path, .{ .readers = 2 }, null);
    defer store.close();
    {
        const w = try store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 99, &diag);
    }
    const repo: categories_repo.CategoryRepo = .{ .store = store };
    try repo.create(.{ .id = "cat1", .name = "tv", .usenet_dir = "/srv/u", .torrent_dir = "/srv/t" });

    var svc = try settings_service.Service.init(testing.allocator, testing.io, .{ .store = store }, event.Publisher.discard, null);
    defer svc.deinit();
    var fail: @import("app_error.zig").Failure = .{};
    var scratch: std.heap.ArenaAllocator = .init(testing.allocator);
    defer scratch.deinit();
    const snap = svc.current();
    var s = try snap.settings.clone(scratch.allocator());
    snap.release();
    s.paths.usenet = .{ .incomplete = "/data/u/inc", .complete = "/data/u/done" };
    s.paths.torrent = .{ .incomplete = "/data/t/inc", .complete = "/data/t/done" };
    try svc.update(scratch.allocator(), &fail, s);

    var rm = Remover.init(testing.allocator, testing.io, .{
        .paths = remover.fromSettings(&svc),
        .categories = remover.fromRepo(&repo),
    });
    defer rm.deinit();

    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const p = try rm.deps.paths.paths_fn(rm.deps.paths.ctx, arena.allocator());
    try testing.expectEqualStrings("/data/u/inc", p.usenet.incomplete);
    try testing.expectEqualStrings("/data/t/done", p.torrent.complete);
    const cats = try rm.deps.categories.list_fn(rm.deps.categories.ctx, arena.allocator());
    try testing.expectEqual(@as(usize, 1), cats.len);
    try testing.expectEqualStrings("/srv/t", cats[0].torrent_dir);
}

// ---- fuzz ----

const FuzzCtx = struct { env: *Env };

/// A corpus entry for `Smith.slice`: the length, then the bytes.
fn seed(comptime bytes: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, bytes.len)) ++ bytes.*);
}

fn restore(env: *Env) !void {
    try env.writeFile("incomplete/" ++ id_a ++ "/part");
    try env.tmp.dir.createDirPath(testing.io, "complete/dir");
    if (!env.exists("complete/link")) try env.tmp.dir.symLink(testing.io, env.outside, "complete/link", .{});
    if (!env.exists("complete/escape")) try env.tmp.dir.symLink(testing.io, env.outside, "complete/escape", .{});
}

// Whatever the output path of a Download is, the canary outside every root
// survives, and so do the roots, whether or not the call fails.
fn fuzzTarget(ctx: FuzzCtx, smith: *testing.Smith) !void {
    var buf: [160]u8 = undefined;
    const n = smith.slice(&buf);
    const env = ctx.env;
    const a = env.arena.allocator();
    // The hostile text after each real prefix, so the interesting paths
    // (inside a root, with dots, links and separators) are reached.
    const prefixes = [_][]const u8{ "", env.complete, env.incomplete, env.base };
    for (prefixes) |prefix| {
        const target = try std.fmt.allocPrint(a, "{s}{s}", .{ prefix, buf[0..n] });
        defer a.free(target);
        env.removeData(Env.download(.nzb, id_b, target)) catch {};
        try testing.expect(env.exists("outside/canary/a"));
        try testing.expect(env.exists("outside"));
        try testing.expect(env.exists("complete"));
        try testing.expect(env.exists("incomplete"));
        // Put back what a legal removal may have taken, for the next input.
        try restore(env);
    }
}

const corpus = [_][]const u8{
    seed("/../../outside/canary"),
    seed("/escape/canary"),
    seed("/link/canary/a"),
    seed("/dir/../../outside/canary"),
    seed("/outside/canary"),
    seed("/link"),
    seed("/"),
    seed("/" ++ id_a),
    seed("/../outside/canary/a"),
    seed("/dir/../escape/canary"),
};

/// Runs every corpus entry once, as `testing.fuzz` does when the test binary
/// is not built for fuzzing (on some backends it does not even do that).
fn runCorpus(ctx: anytype, comptime one: fn (@TypeOf(ctx), *testing.Smith) anyerror!void, entries: []const []const u8) !void {
    for (entries) |input| {
        var smith: testing.Smith = .{ .in = input };
        try one(ctx, &smith);
    }
}

test "fuzz: no output path reaches the canary outside the roots" {
    var env: Env = undefined;
    try env.init();
    defer env.deinit();
    try env.writeFile("outside/canary/a");
    try restore(&env);
    try runCorpus(FuzzCtx{ .env = &env }, fuzzTarget, &corpus);
    try testing.fuzz(FuzzCtx{ .env = &env }, fuzzTarget, .{ .corpus = &corpus });
}
