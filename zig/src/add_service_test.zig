//! Tests of the add service (ports of internal/app/add_test.go,
//! add_request_test.go and add_cleannames_test.go, and the expectations of
//! internal/blackbox/usenet_add_test.go that belong to the service).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const testing = std.testing;
const dl = @import("download.zig");
const fetch = @import("add_fetch.zig");
const service = @import("add_service.zig");
const tu = @import("add_testutil.zig");
const Rig = tu.Rig;
const secret_mod = @import("secret.zig");
const source_repo = @import("add_source_repo.zig");
const downloads_service = @import("downloads_service.zig");

fn passwordOf(rig: *Rig, id: []const u8) ![]const u8 {
    const src = try rig.sources.get(rig.a(), id);
    return src.password;
}

fn secretOf(rig: *Rig, text: []const u8) !secret_mod.Secret {
    return secret_mod.Secret.init(rig.a(), text);
}

test "an upload becomes a Queued Download with its NZB stored for the runner" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const nzb = try tu.nzbWith(rig.a(), "", "", "");

    const d = try rig.addUpload("My.Release.nzb", nzb, .{});

    try testing.expectEqual(@as(usize, 26), d.id.len);
    try testing.expectEqualStrings("My.Release", d.name);
    try testing.expectEqual(dl.SourceKind.nzb, d.source_kind);
    try testing.expectEqualStrings("", d.category_id);
    try testing.expectEqual(dl.State.queued, d.state);
    try testing.expectEqual(dl.Priority.normal, d.priority);
    try testing.expectEqual(@as(i64, 0), d.queue_position);
    try testing.expect(!d.paused);
    try testing.expectEqual(@as(i64, 5000), d.total_bytes);
    try testing.expectEqual(@as(i64, 0), d.done_bytes);
    try testing.expectEqual(tu.epoch_ms, d.added_at);

    // The stored row is the answer, and the event carries the same Download.
    const stored = try rig.db.repo.get(rig.a(), d.id);
    try testing.expectEqualDeep(stored, d);
    try testing.expectEqual(@as(usize, 1), rig.recorder.added.items.len);
    try testing.expectEqualDeep(d, rig.recorder.added.items[0]);
    try testing.expectEqual(@as(usize, 1), rig.wake_count.load(.monotonic));

    // The NZB is kept, unchanged, with mode 0600; no staging file is left.
    const files = try rig.filesOf(d.id);
    try testing.expectEqual(@as(usize, 1), files.len);
    try testing.expectEqualStrings("My.Release.nzb", files[0]);
    const path = try std.fmt.allocPrint(rig.a(), "{s}/{s}/My.Release.nzb", .{ rig.incomplete, d.id });
    try testing.expectEqualStrings(nzb, try rig.readFile(path));
    const st = try Io.Dir.cwd().statFile(testing.io, path, .{});
    try testing.expectEqual(@as(u32, 0o600), @as(u32, @intCast(@backingInt(st.permissions) & 0o777)));
    const src = try rig.sources.get(rig.a(), d.id);
    try testing.expectEqualStrings(path, src.nzb_path);
}

test "added Downloads get the next Queue position, counting those still in the Queue" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var positions: [3]i64 = undefined;
    for (&positions) |*p| p.* = (try rig.addUpload("x.nzb", tu.tiny_nzb, .{})).queue_position;
    try testing.expectEqualSlices(i64, &.{ 0, 1, 2 }, &positions);
}

test "the name comes from the request, the NZB, the file name or the first file" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.a();
    const named = try tu.nzbWith(a, "Name From The NZB", "", "");
    const plain = try tu.nzbWith(a, "", "", "");
    const cases = [_]struct { []const u8, []const u8, []const u8, []const u8, []const u8 }{
        // nzb, file name, name field, want, why
        .{ named, "file.nzb", "Chosen", "Chosen", "the name field wins" },
        .{ named, "file.nzb", "", "Name From The NZB", "the NZB's own name" },
        .{ plain, "Some.Show.S01E02.nzb", "", "Some.Show.S01E02", "the upload's file name" },
        .{ plain, "Loud.NZB", "", "Loud", "any case of .nzb is cut" },
        .{ plain, ".nzb", "", "first.bin", "a name that only has the extension falls through" },
        .{ plain, "", "", "first.bin", "the first file when the upload has no name" },
        .{ plain, "some/dir/Unix.Name.nzb", "", "Unix.Name", "a path in the file name is cut" },
        .{ named, "file.nzb", "   ", "Name From The NZB", "a blank name field falls through" },
    };
    for (cases) |c| {
        const d = rig.addUpload(c[1], c[0], .{ .name = c[2] }) catch |err| {
            std.debug.print("{s}\n", .{c[4]});
            return err;
        };
        testing.expectEqualStrings(c[3], d.name) catch |err| {
            std.debug.print("{s}\n", .{c[4]});
            return err;
        };
    }
}

test "the display name is cleaned" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const long: [300]u8 = @splat('x');
    var e200: [400]u8 = undefined;
    for (0..200) |i| @memcpy(e200[i * 2 ..][0..2], "é");
    const cases = [_]struct { []const u8, []const u8 }{
        .{ "a\x00b\x07c\n", "abc" },
        .{ "gnp.\u{202e}exe", "gnp.exe" },
        .{ "a\xffb", "ab" },
        .{ "  padded  ", "padded" },
        .{ &long, long[0..255] },
        .{ &e200, e200[0..254] },
    };
    for (cases) |c| {
        const d = try rig.addUpload("f.nzb", tu.tiny_nzb, .{ .name = c[0] });
        try testing.expectEqualStrings(c[1], d.name);
    }
}

test "the password comes from the field, the NZB or the name suffix, and is stored sealed" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.a();
    const plain = tu.tiny_nzb;
    const with_pw = try tu.nzbWith(a, "", "from-nzb", "");
    const cases = [_]struct { []const u8, []const u8, []const u8, []const u8, []const u8, []const u8 }{
        // nzb, file name, name field, password field, want name, want password
        .{ with_pw, "x.nzb", "", "from-field", "x", "from-field" },
        .{ with_pw, "x.nzb", "", "", "x", "from-nzb" },
        .{ plain, "Release{{from-suffix}}.nzb", "", "", "Release", "from-suffix" },
        .{ with_pw, "Release{{from-suffix}}.nzb", "", "", "Release", "from-nzb" },
        .{ plain, "x.nzb", "Named {{s3cr3t}}", "", "Named", "s3cr3t" },
        .{ plain, "x.nzb", "", "", "x", "" },
    };
    for (cases) |c| {
        const d = try rig.addUpload(c[1], c[0], .{ .name = c[2], .password = if (c[3].len > 0) try secretOf(&rig, c[3]) else null });
        try testing.expectEqualStrings(c[4], d.name);
        try testing.expectEqualStrings(c[5], try passwordOf(&rig, d.id));
    }
    // Nothing of a password in the log.
    try testing.expect(std.mem.indexOf(u8, rig.out.written(), "from-field") == null);
    try testing.expect(std.mem.indexOf(u8, rig.out.written(), "s3cr3t") == null);
}

test "the Category is the requested one, the NZB's by name, or none" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.a();
    try rig.addCategory("tvid", "tv");
    try rig.addCategory("movid", "movies");
    const cases = [_]struct { []const u8, []const u8, []const u8 }{
        // nzb category, category_id field, want
        .{ "", "", "" },
        .{ "", "movid", "movid" },
        .{ "tv", "", "tvid" },
        .{ "MoViEs", "", "movid" },
        .{ "tv", "movid", "movid" },
        .{ "TV > HD", "", "" },
    };
    for (cases) |c| {
        const nzb = try tu.nzbWith(a, "", "", c[0]);
        const d = try rig.addUpload("x.nzb", nzb, .{ .category_id = c[1] });
        try testing.expectEqualStrings(c[2], d.category_id);
    }
    // An unknown name in an NZB creates no Category.
    try testing.expectEqual(@as(usize, 2), (try rig.category_repo.list(a)).len);
}

test "a Category by name is created once and reused, in any case" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const first = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .category_name = "TV" });
    const second = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .category_name = "tv" });
    const cats = try rig.category_repo.list(rig.a());
    try testing.expectEqual(@as(usize, 1), cats.len);
    try testing.expectEqualStrings("tv", cats[0].name);
    try testing.expectEqualStrings(cats[0].id, first.category_id);
    try testing.expectEqualStrings(cats[0].id, second.category_id);
    // The id wins over the name.
    try rig.addCategory("movid", "movies");
    const third = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .category_name = "other", .category_id = "movid" });
    try testing.expectEqualStrings("movid", third.category_id);
    try testing.expectEqual(@as(usize, 2), (try rig.category_repo.list(rig.a())).len);
}

test "priority and paused are kept" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    for ([_][]const u8{ "force", "high", "normal", "low" }) |p| {
        const d = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .priority = p });
        try testing.expectEqualStrings(p, @tagName(d.priority));
    }
    const d = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .paused = true });
    try testing.expect(d.paused);
    try testing.expectEqual(dl.State.queued, d.state);
    try testing.expect((try rig.db.repo.get(rig.a(), d.id)).paused);
}

test "refused fields leave nothing behind, the upload included" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const long_pw: [1025]u8 = @splat('p');
    try rig.expectRejected(rig.addUpload("x.nzb", tu.tiny_nzb, .{ .priority = "urgent" }), .invalid_input, "invalid_priority");
    try testing.expectEqualStrings("unknown priority \"urgent\": use force, high, normal or low", rig.f.message);
    try rig.expectRejected(rig.addUpload("x.nzb", tu.tiny_nzb, .{ .priority = "HIGH" }), .invalid_input, "invalid_priority");
    try rig.expectRejected(rig.addUpload("x.nzb", tu.tiny_nzb, .{ .category_id = "nosuchcategory" }), .invalid_input, "unknown_category");
    try testing.expectEqualStrings("category \"nosuchcategory\" does not exist", rig.f.message);
    try rig.expectRejected(rig.addUpload("x.nzb", tu.tiny_nzb, .{ .password = try secretOf(&rig, &long_pw) }), .invalid_input, "invalid_password");
    try testing.expectEqualStrings("the password is longer than 1024 bytes", rig.f.message);
    try rig.expectNothingAdded();

    // Exactly 1024 bytes is fine.
    const ok_pw: [1024]u8 = @splat('p');
    _ = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .password = try secretOf(&rig, &ok_pw) });
}

test "a source is needed, exactly one" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{}), .invalid_input, "missing_source");

    const up = try rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "", .nzb);
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .upload = up, .url = "magnet:?xt=urn:btih:abc" }), .invalid_input, "ambiguous_source");
    try testing.expectEqual(@as(usize, 0), (try rig.staged()).len); // the upload is removed
}

test "a received file that is not added can be discarded, twice" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const up = try rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "", .nzb);
    try testing.expectEqual(@as(usize, 1), (try rig.staged()).len);
    up.discard(testing.io);
    up.discard(testing.io);
    try testing.expectEqual(@as(usize, 0), (try rig.staged()).len);
}

test "the incomplete root and its parents are created" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectEqual(@as(usize, 0), (try rig.staged()).len);
    const up = try rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "", .nzb);
    defer up.discard(testing.io);
    var dir = try Io.Dir.cwd().openDir(testing.io, rig.incomplete, .{});
    dir.close(testing.io);
}

test "what is not a valid NZB is refused with Go's text, and nothing is staged" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const cases = [_]struct { []const u8, []const u8 }{
        .{ "<?xml version=\"1.0\"?><nzb xmlns=\"http://www.newzbin.com/DTD/2003/nzb\"></nzb>", "not a valid NZB: nzb: no usable files" },
        .{ "<nzb><file subject=\"x\"><segments></segments></file></nzb>", "not a valid NZB: nzb: no usable files" },
        .{ "this is not an nzb", "not a valid NZB: nzb: malformed document" },
        .{ "<nzb><file subject=\"x\"><segments><segment bytes=\"1\" number=\"1\">a@b</segment></segments></nzb>", "not a valid NZB: nzb: malformed document" },
        .{ "", "not a valid NZB: nzb: " },
        .{ "d8:announce3:foo4:infod6:lengthi1eee", "not a valid NZB: nzb: malformed document" },
    };
    for (cases) |c| {
        try rig.expectRejected(rig.addUpload("x.nzb", c[0], .{}), .invalid_input, "invalid_nzb");
        try testing.expect(std.mem.startsWith(u8, rig.f.message, c[1]));
        try rig.expectNothingAdded();
    }
}

test "an NZB over the cap is too large" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    rig.rebuild(.{ .max_nzb_bytes = 2 << 20 });
    const big = try rig.a().alloc(u8, (2 << 20) + 1);
    @memset(big, ' ');
    @memcpy(big[0..tu.tiny_nzb.len], tu.tiny_nzb);
    try rig.expectRejected(rig.addUpload("x.nzb", big, .{}), .too_large, "nzb_too_large");
    try testing.expectEqualStrings("the NZB is larger than 2 MiB", rig.f.message);
    try rig.expectNothingAdded();
    // Exactly at the cap is fine.
    _ = try rig.addUpload("x.nzb", big[0 .. 2 << 20], .{});
}

// ---- failures while storing ----

const FailingSources = struct {
    mode: enum { fail, move_to_queued } = .fail,
    calls: usize = 0,
    saw_state: ?dl.State = null,
    saw_listed_queued: bool = false,
    rig: *Rig,

    fn seam(self: *FailingSources) service.Sources {
        return .{ .ctx = self, .create_fn = create };
    }

    fn create(ctx: ?*anyopaque, arena: Allocator, id: []const u8, _: []const u8, _: []const u8) anyerror!void {
        const self: *FailingSources = @ptrCast(@alignCast(ctx.?));
        self.calls += 1;
        const got = try self.rig.db.repo.get(arena, id);
        self.saw_state = got.state;
        const queued = try self.rig.db.repo.listAll(arena, .{ .state = .queued });
        for (queued) |q| if (std.mem.eql(u8, q.id, id)) {
            self.saw_listed_queued = true;
        };
        switch (self.mode) {
            .fail => return error.DiskOnFire,
            // The Download is Queued behind the service's back: its own move to
            // Queued is then illegal.
            .move_to_queued => _ = try self.rig.db.repo.updateState(arena, id, .{ .to = .queued, .at = tu.epoch_ms }),
        }
    }
};

test "a source that cannot be stored removes the Download and the files" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var failing: FailingSources = .{ .rig = &rig };
    rig.sources_seam = failing.seam();
    rig.rebuild(.{});

    try testing.expectError(error.DiskOnFire, rig.addUpload("x.nzb", tu.tiny_nzb, .{}));

    try testing.expectEqual(dl.State.added, failing.saw_state.?);
    try testing.expect(!failing.saw_listed_queued);
    try rig.expectNothingAdded();
}

test "the move to Queued failing removes everything" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var failing: FailingSources = .{ .rig = &rig, .mode = .move_to_queued };
    rig.sources_seam = failing.seam();
    rig.rebuild(.{});

    try testing.expectError(error.IllegalTransition, rig.addUpload("x.nzb", tu.tiny_nzb, .{ .password = try secretOf(&rig, "pw") }));

    try rig.expectNothingAdded();
}

test "a Download never shows as Queued before its source exists" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    // The real source store, watched.
    const Watch = struct {
        rig: *Rig,
        saw_state: ?dl.State = null,
        saw_queued: bool = false,
        fn create(ctx: ?*anyopaque, arena: Allocator, id: []const u8, path: []const u8, pw: []const u8) anyerror!void {
            const self: *@This() = @ptrCast(@alignCast(ctx.?));
            self.saw_state = (try self.rig.db.repo.get(arena, id)).state;
            for (try self.rig.db.repo.listAll(arena, .{ .state = .queued })) |q| {
                if (std.mem.eql(u8, q.id, id)) self.saw_queued = true;
            }
            try self.rig.sources.create(arena, id, path, pw);
        }
    };
    var watch: Watch = .{ .rig = &rig };
    rig.sources_seam = .{ .ctx = &watch, .create_fn = Watch.create };
    rig.rebuild(.{});

    const d = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .paused = true, .priority = "high" });

    try testing.expectEqual(dl.State.added, watch.saw_state.?);
    try testing.expect(!watch.saw_queued);
    const stored = try rig.db.repo.get(rig.a(), d.id);
    try testing.expectEqual(dl.State.queued, stored.state);
    try testing.expect(stored.paused);
    try testing.expectEqual(dl.Priority.high, stored.priority);
    try testing.expectEqualDeep(stored, d);
    try testing.expectEqual(@as(usize, 1), rig.recorder.added.items.len);
}

test "an incomplete root that cannot be written names the folder and the user" {
    if (@import("fsutil.zig").effectiveUid() == 0) return error.SkipZigTest;
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try Io.Dir.cwd().createDirPath(testing.io, rig.incomplete);
    var dir = try Io.Dir.cwd().openDir(testing.io, rig.incomplete, .{});
    defer dir.close(testing.io);
    try dir.setPermissions(testing.io, .fromMode(0o555));
    defer dir.setPermissions(testing.io, .fromMode(0o755)) catch {};

    try rig.expectRejected(rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "", .nzb), .unavailable, "folder_not_writable");

    try testing.expect(std.mem.indexOf(u8, rig.f.message, rig.incomplete) != null);
    const uid = try std.fmt.allocPrint(rig.a(), "uid {d}", .{@import("fsutil.zig").effectiveUid()});
    try testing.expect(std.mem.indexOf(u8, rig.f.message, uid) != null);
}

// ---- cleaning names ----

fn stripObfuscated(_: ?*anyopaque, arena: Allocator, name: []const u8) anyerror![]const u8 {
    const suffix = "-Obfuscated";
    if (std.mem.endsWith(u8, name, suffix)) return arena.dupe(u8, name[0 .. name.len - suffix.len]);
    return name;
}

test "CleanNames cleans the final name, the stored file follows it, and it is read at every add" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    rig.rebuild(.{ .name_cleaner = .{ .clean_fn = stripObfuscated } });
    const dirty = "Some.Show.S01E01.1080p-GRP-Obfuscated";

    const off = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .name = dirty });
    try testing.expectEqualStrings(dirty, off.name);

    rig.clean_names = true;
    const on = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .name = dirty });
    try testing.expectEqualStrings("Some.Show.S01E01.1080p-GRP", on.name);
    const files = try rig.filesOf(on.id);
    try testing.expectEqualStrings("Some.Show.S01E01.1080p-GRP.nzb", files[0]);

    const from_file = try rig.addUpload(dirty ++ ".nzb", tu.tiny_nzb, .{});
    try testing.expectEqualStrings("Some.Show.S01E01.1080p-GRP", from_file.name);

    rig.clean_names = false;
    const off_again = try rig.addUpload("x.nzb", tu.tiny_nzb, .{ .name = dirty });
    try testing.expectEqualStrings(dirty, off_again.name);
}

// ---- receiving by content ----

test "receive tells an NZB from a torrent by its content and refuses a lying extension" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var torrents: Torrents = .{ .rig = &rig };
    rig.rebuild(.{ .torrents = torrents.seam() });

    const torrent = try rig.add.receive(rig.a(), &rig.f, "d8:announce3:foo4:infod6:lengthi1eee", "whatever.bin", null);
    try testing.expectEqual(dl.SourceKind.torrent, torrent.kind);
    const nzb = try rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "", null);
    defer nzb.discard(testing.io);
    try testing.expectEqual(dl.SourceKind.nzb, nzb.kind);

    try rig.expectRejected(rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "a.torrent", null), .invalid_input, "not_a_torrent");
    try testing.expectEqualStrings("That file ends in .torrent but is not a torrent file.", rig.f.message);
    try rig.expectRejected(rig.add.receive(rig.a(), &rig.f, "d4:infoe", "a.nzb", null), .invalid_input, "not_an_nzb");
    try rig.expectRejected(rig.add.receive(rig.a(), &rig.f, "", "a.nzb", null), .invalid_input, "empty_file");
    try testing.expectEqualStrings("That file is empty.", rig.f.message);
    try testing.expectEqual(@as(usize, 1), (try rig.staged()).len); // only `nzb`
}

// ---- torrents: the seam ----

const Torrents = struct {
    rig: *Rig,
    last: ?service.TorrentRequest = null,
    last_file_len: usize = 0,
    last_magnet: []const u8 = "",

    fn seam(self: *Torrents) service.Torrents {
        return .{ .ctx = self, .add_fn = add };
    }

    fn add(ctx: ?*anyopaque, arena: Allocator, _: *app_error_Failure, req: service.TorrentRequest) anyerror!dl.Download {
        const self: *Torrents = @ptrCast(@alignCast(ctx.?));
        self.last = req;
        switch (req.source) {
            .file => |f| self.last_file_len = f.len,
            .magnet => |m| self.last_magnet = try arena.dupe(u8, m),
        }
        self.rig.torrent_calls += 1;
        return .{ .id = "torrentid", .name = req.name, .source_kind = .torrent, .state = .fetching_metadata, .added_at = 1, .updated_at = 1 };
    }
};
const app_error_Failure = @import("app_error.zig").Failure;

test "torrents are unavailable until the engine is wired in" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();

    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567" }), .unavailable, "add_unavailable");
    try testing.expectEqualStrings("adding downloads is not available in this build yet", rig.f.message);
    try rig.expectRejected(rig.add.receive(rig.a(), &rig.f, "d4:infoe", "a.torrent", .torrent), .unavailable, "add_unavailable");

    // A link that turns out to be a .torrent.
    rig.fetcher.answer = .{ .body = "d8:announce3:foo4:infod6:lengthi1eee" };
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x" }), .unavailable, "add_unavailable");
    try rig.expectNothingAdded();
}

test "the torrent seam gets magnet links, torrent files and fetched torrents with the checked fields" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var torrents: Torrents = .{ .rig = &rig };
    rig.rebuild(.{ .torrents = torrents.seam() });
    try rig.addCategory("movid", "movies");

    const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x";
    const d1 = try rig.add.add(rig.a(), &rig.f, .{ .url = magnet, .name = "N", .priority = "low", .paused = true, .category_id = "movid" });
    try testing.expectEqual(dl.SourceKind.torrent, d1.source_kind);
    try testing.expectEqualStrings(magnet, torrents.last_magnet);
    try testing.expectEqualStrings("N", torrents.last.?.name);
    try testing.expectEqual(dl.Priority.low, torrents.last.?.priority);
    try testing.expect(torrents.last.?.paused);
    try testing.expectEqualStrings("movid", torrents.last.?.category_id);

    const up = try rig.add.receive(rig.a(), &rig.f, "d4:infoe", "My.Torrent.torrent", .torrent);
    _ = try rig.add.add(rig.a(), &rig.f, .{ .upload = up });
    try testing.expectEqual(@as(usize, 8), torrents.last_file_len);
    try testing.expectEqualStrings("My.Torrent.torrent", torrents.last.?.file_name);

    rig.fetcher.answer = .{ .body = "d4:infoe" };
    _ = try rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/t" });
    try testing.expectEqual(@as(usize, 8), torrents.last_file_len);

    rig.fetcher.answer = .{ .magnet = magnet };
    _ = try rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/m" });
    try testing.expectEqualStrings(magnet, torrents.last_magnet);
    try testing.expectEqual(@as(usize, 4), rig.torrent_calls);
    // The NZB side did not run for any of it.
    try testing.expectEqual(@as(usize, 0), rig.recorder.added.items.len);
}

test "only restricts the kind" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var torrents: Torrents = .{ .rig = &rig };
    rig.rebuild(.{ .torrents = torrents.seam() });

    const up = try rig.add.receive(rig.a(), &rig.f, tu.tiny_nzb, "", .nzb);
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .upload = up, .only = .torrent }), .invalid_input, "not_a_torrent");
    try testing.expectEqual(@as(usize, 0), (try rig.staged()).len);
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "magnet:?xt=urn:btih:abc", .only = .nzb }), .invalid_input, "not_an_nzb");
    try testing.expectEqualStrings("a magnet link is a torrent, and only NZBs can be added here", rig.f.message);
    try testing.expectEqual(@as(usize, 0), torrents.rig.torrent_calls);
}

// ---- links ----

test "a link is fetched, named from its content, and the cheap fields are checked first" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const nzb = try tu.nzbWith(rig.a(), "", "", "");
    rig.fetcher.answer = .{ .body = nzb };

    const d = try rig.add.add(rig.a(), &rig.f, .{ .url = "  http://host.example/get/some.release.nzb?apikey=SECRETKEY  ", .priority = "high", .paused = true });

    try testing.expectEqualStrings("first.bin", d.name);
    try testing.expectEqual(dl.Priority.high, d.priority);
    try testing.expect(d.paused);
    try testing.expectEqual(@as(i64, 5000), d.total_bytes);
    // Fetched as given (trimmed), with Go's Accept and the NZB cap.
    try testing.expectEqualStrings("http://host.example/get/some.release.nzb?apikey=SECRETKEY", rig.fetcher.last_url);
    try testing.expectEqualStrings("application/x-bittorrent, application/x-nzb, text/xml;q=0.9, */*;q=0.1", rig.fetcher.last_accept);
    try testing.expectEqual(@as(u64, 64 << 20), rig.fetcher.last_max_bytes);
    try testing.expectEqual(@as(u32, 30_000), rig.fetcher.last_timeout_ms);
    try testing.expect(std.mem.indexOf(u8, rig.out.written(), "SECRETKEY") == null);
    const files = try rig.filesOf(d.id);
    try testing.expectEqualStrings("first.bin.nzb", files[0]);

    const calls = rig.fetcher.calls;
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x", .priority = "urgent" }), .invalid_input, "invalid_priority");
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x", .category_id = "nope" }), .invalid_input, "unknown_category");
    const long_pw: [1025]u8 = @splat('p');
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x", .password = try secretOf(&rig, &long_pw) }), .invalid_input, "invalid_password");
    try testing.expectEqual(calls, rig.fetcher.calls); // nothing was fetched
}

test "a link that is only an NZB asks for NZBs and names them so" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    rig.fetcher.answer = .{ .status = 404 };
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x", .only = .nzb }), .upstream, "fetch_failed");
    try testing.expectEqualStrings("could not fetch the NZB: the server answered 404 Not Found", rig.f.message);
    try testing.expectEqualStrings("application/x-nzb, text/xml;q=0.9, */*;q=0.1", rig.fetcher.last_accept);

    // A torrent body is not looked for: it is not a valid NZB.
    rig.fetcher.answer = .{ .body = "d4:infoe" };
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x", .only = .nzb }), .invalid_input, "invalid_nzb");
    rig.fetcher.answer = .{ .magnet = "magnet:?xt=urn:btih:abc" };
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x", .only = .nzb }), .upstream, "fetch_failed");
    try testing.expectEqualStrings("could not fetch the NZB: refused a redirect to a \"magnet\" URL: only http and https are allowed", rig.f.message);
}

test "a link that is refused or fails says why, without the URL" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const url = "http://host.example/x?apikey=SECRETKEY";
    const cases = [_]struct { tu.Answer, std.http.Status, app_error_Kind, []const u8, []const u8 }{
        .{ .{ .status = 404 }, .bad_gateway, .upstream, "fetch_failed", "could not fetch the NZB or torrent: the server answered 404 Not Found" },
        .{ .{ .status = 500 }, .bad_gateway, .upstream, "fetch_failed", "could not fetch the NZB or torrent: the server answered 500 Internal Server Error" },
        .{ .{ .status = 599 }, .bad_gateway, .upstream, "fetch_failed", "could not fetch the NZB or torrent: the server answered 599 " },
        .{ .{ .failed = "dial tcp 127.0.0.1:9: connect: connection refused" }, .bad_gateway, .upstream, "fetch_failed", "could not fetch the NZB or torrent: dial tcp 127.0.0.1:9: connect: connection refused" },
        .{ .timeout, .bad_gateway, .upstream, "fetch_failed", "could not fetch the NZB or torrent: timed out after 30s" },
        .{ .too_large, .payload_too_large, .too_large, "nzb_too_large", "the NZB is larger than 64 MiB" },
        .{ .{ .body = "<html><body>login</body></html>" }, .bad_request, .invalid_input, "invalid_nzb", "not a valid NZB: nzb: malformed document" },
        .{ .{ .body = "" }, .bad_request, .invalid_input, "invalid_nzb", "not a valid NZB: nzb: " },
    };
    for (cases) |c| {
        rig.fetcher.answer = c[0];
        try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = url }), c[2], c[3]);
        try testing.expect(std.mem.startsWith(u8, rig.f.message, c[4]));
        try testing.expect(std.mem.indexOf(u8, rig.f.message, "SECRETKEY") == null);
        try rig.expectNothingAdded();
    }
    try testing.expect(std.mem.indexOf(u8, rig.out.written(), "SECRETKEY") == null);
}

const app_error_Kind = @import("app_error.zig").Kind;

test "links that are not absolute http or https URLs are refused before any fetch" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    for ([_][]const u8{ "ftp://host.example/a.nzb", "file:///etc/passwd", "not a url", "/relative/a.nzb", "http://", "//host.example/a.nzb", "javascript:alert(1)", "http:///nohost" }) |link| {
        try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = link }), .invalid_input, "invalid_url");
        try testing.expectEqualStrings("url must be an absolute http or https URL", rig.f.message);
    }
    // Blank is not "no link": the check for a source counts bytes, the URL
    // check then refuses it.
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "   " }), .invalid_input, "invalid_url");
    try testing.expectEqual(@as(usize, 0), rig.fetcher.calls);
}

test "the fetch timeout is the service's" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    rig.rebuild(.{ .fetch_timeout_ms = 1500 });
    rig.fetcher.answer = .timeout;
    try rig.expectRejected(rig.add.add(rig.a(), &rig.f, .{ .url = "http://host.example/x" }), .upstream, "fetch_failed");
    try testing.expectEqualStrings("could not fetch the NZB or torrent: timed out after 1.5s", rig.f.message);
    try testing.expectEqual(@as(u32, 1500), rig.fetcher.last_timeout_ms);
}

test "the Settings service gives the incomplete root and CleanNames" {
    const settings_service = @import("settings_service.zig");
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var svc = try settings_service.Service.init(testing.allocator, testing.io, .{ .store = rig.db.store }, @import("event.zig").Publisher.discard, null);
    defer svc.deinit();
    const view = try service.settingsFromService(&svc).read(rig.a());
    try testing.expectEqualStrings("/data/usenet/incomplete", view.usenet_incomplete);
    try testing.expect(!view.clean_names);
}

fn addOne(rig: *Rig, out: *i64) void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    var f: @import("app_error.zig").Failure = .{};
    const up = rig.add.receive(arena.allocator(), &f, tu.tiny_nzb, "x.nzb", .nzb) catch {
        out.* = -1;
        return;
    };
    const d = rig.add.add(arena.allocator(), &f, .{ .upload = up }) catch {
        out.* = -1;
        return;
    };
    out.* = d.queue_position;
}

test "adds at the same time get distinct Queue positions" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var results: [4]i64 = undefined;
    var futs: [4]Io.Future(void) = undefined;
    for (&futs, &results) |*fut, *out| fut.* = try testing.io.concurrent(addOne, .{ &rig, out });
    for (&futs) |*fut| fut.await(testing.io);
    std.mem.sort(i64, &results, {}, std.sort.asc(i64));
    try testing.expectEqualSlices(i64, &.{ 0, 1, 2, 3 }, &results);
    try testing.expectEqual(@as(usize, 4), rig.recorder.added.items.len);
    try testing.expectEqual(@as(usize, 4), rig.wake_count.load(.monotonic));
}
