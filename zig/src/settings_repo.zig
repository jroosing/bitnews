//! The Settings row (internal/store/repo_settings.go): the `settings` table
//! holds the JSON of the typed Settings under the key "settings", with
//! updated_at in milliseconds. A row written by the Go daemon loads here and
//! the other way round: a stored key that is missing keeps its default, and
//! keys this version does not know are ignored.
//!
//! Go stores an empty list as `null` when it was never set and `[]` otherwise;
//! this repo always writes `[]`. Both load the same.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const types = @import("settings_types.zig");
const json = @import("settings_json.zig");
const validate = @import("settings_validate.zig").validate;

pub const key = "settings";

/// A stored row is a few KiB; this only bounds a row someone edited by hand.
pub const max_stored_len = 1 << 20;

pub const Error = error{
    /// The stored JSON is not Settings (Go: a decode error at start-up).
    CorruptSettings,
    /// `save` was given Settings that do not validate.
    InvalidSettings,
} || sqlite.Error || Allocator.Error || Io.Cancelable;

pub const SettingsRepo = struct {
    store: *sqlite.Store,

    /// The stored Settings, or the defaults when none were saved. Strings
    /// live in `arena`.
    pub fn load(self: SettingsRepo, arena: Allocator) Error!types.Settings {
        const r = try self.store.acquireReader();
        defer r.release();
        return loadOn(r.db, arena);
    }

    /// Validates `s` and stores it, replacing any previous Settings. Invalid
    /// Settings are refused before anything is written.
    pub fn save(self: SettingsRepo, scratch: Allocator, s: types.Settings, now_ms: i64) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        return saveOn(w.db, scratch, s, now_ms);
    }
};

pub fn loadOn(db: *sqlite.Db, arena: Allocator) Error!types.Settings {
    var st = try sqlite.Stmt.prepare(db, "SELECT value FROM settings WHERE key = ?");
    defer st.finalize();
    try st.bindAll(.{key});
    var s = types.Settings.defaults();
    if (!try st.step()) return s;
    const raw = try st.columnText(arena, 0);
    if (raw.len > max_stored_len) return error.CorruptSettings;
    switch (try json.decode(arena, raw, &s, false)) {
        .ok => return s,
        .invalid => return error.CorruptSettings,
    }
}

pub fn saveOn(db: *sqlite.Db, scratch: Allocator, s: types.Settings, now_ms: i64) Error!void {
    if (try validate(scratch, s) != null) return error.InvalidSettings;
    var aw: Io.Writer.Allocating = .init(scratch);
    json.write(&aw.writer, s) catch return error.OutOfMemory;
    try db.execArgs(
        \\INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
        \\ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
    , .{ key, aw.written(), now_ms });
}

// ---- tests ----

const testing = std.testing;
const migrate = @import("migrate.zig");
const golden = @import("settings_golden.zig");

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,

    fn open(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        const w = try rig.store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
    }

    fn close(rig: *Rig) void {
        rig.store.close();
        rig.tmp.cleanup();
    }

    fn rawRow(rig: *Rig, value: []const u8) !void {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.execArgs("INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES ('settings', ?, 1)", .{value});
    }
};

fn expectEqualSettings(a: Allocator, want: types.Settings, got: types.Settings) !void {
    var w1: Io.Writer.Allocating = .init(a);
    var w2: Io.Writer.Allocating = .init(a);
    try json.write(&w1.writer, want);
    try json.write(&w2.writer, got);
    try testing.expectEqualStrings(w1.written(), w2.written());
}

test "a database that never saved Settings loads the defaults" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const repo: SettingsRepo = .{ .store = rig.store };
    try expectEqualSettings(arena.allocator(), types.Settings.defaults(), try repo.load(arena.allocator()));
}

test "save then load round-trips, replaces the row and keeps one" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const repo: SettingsRepo = .{ .store = rig.store };

    var s = types.Settings.defaults();
    s.queues.usenet = 3;
    s.auth.allowed_hosts = &.{ "h<&>", "b.lan" };
    s.network.torrent.interface = "wg0";
    s.seeding.ratio_limit = 1.5;
    try repo.save(a, s, 1_700_000_000_000);
    try expectEqualSettings(a, s, try repo.load(a));

    s.queues.usenet = 4;
    try repo.save(a, s, 1_700_000_001_000);
    try expectEqualSettings(a, s, try repo.load(a));

    const r = try rig.store.acquireReader();
    defer r.release();
    var st = try sqlite.Stmt.prepare(r.db, "SELECT count(*), max(updated_at), min(key) FROM settings");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(1, st.columnInt(0));
    try testing.expectEqual(1_700_000_001_000, st.columnInt(1));
    const k = try st.columnText(a, 2);
    try testing.expectEqualStrings("settings", k);
}

test "invalid Settings are refused before anything is written" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const repo: SettingsRepo = .{ .store = rig.store };
    var s = types.Settings.defaults();
    s.queues.usenet = -1;
    try testing.expectError(error.InvalidSettings, repo.save(arena.allocator(), s, 1));
    try expectEqualSettings(arena.allocator(), types.Settings.defaults(), try repo.load(arena.allocator()));
}

test "rows written by the Go daemon load" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const repo: SettingsRepo = .{ .store = rig.store };

    try rig.rawRow(golden.stored_default);
    try expectEqualSettings(a, types.Settings.defaults(), try repo.load(a));

    try rig.rawRow(golden.stored_changed);
    const s = try repo.load(a);
    try testing.expectEqual(3, s.queues.usenet);
    try testing.expectEqualStrings("h<&>", s.auth.allowed_hosts[0]);

    // An older row without the later keys keeps their defaults; an unknown key is ignored.
    try rig.rawRow("{\"queues\":{\"usenet\":2},\"future\":{\"x\":1}}");
    const old = try repo.load(a);
    try testing.expectEqual(2, old.queues.usenet);
    try testing.expectEqual(5, old.queues.torrent);
    try testing.expectEqualStrings("pause", old.seeding.action);
}

test "a row that is not Settings is reported, not replaced by defaults" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const repo: SettingsRepo = .{ .store = rig.store };
    try rig.rawRow("{\"queues\":{\"usenet\":\"many\"}}");
    try testing.expectError(error.CorruptSettings, repo.load(arena.allocator()));
    try rig.rawRow("[1]");
    try testing.expectError(error.CorruptSettings, repo.load(arena.allocator()));
}

test "the database of the Go daemon (a copy of the compat file) loads" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    // Never open the golden file itself: SQLite would leave -wal and -shm next to it.
    try Io.Dir.cwd().copyFile("testdata/compat/bitnews.db", tmp.dir, "bitnews.db", testing.io, .{});
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
    const path = try std.fmt.bufPrint(&path_buf, "{s}/bitnews.db", .{buf[0..n]});
    const store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 1 }, null);
    defer store.close();

    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const repo: SettingsRepo = .{ .store = store };
    // The file has its settings under the key "main", which Go never reads
    // (it uses "settings"), so Go and this repo both see the defaults.
    try expectEqualSettings(a, types.Settings.defaults(), try repo.load(a));

    var s = types.Settings.defaults();
    s.queues.torrent = 8;
    try repo.save(a, s, 5);
    try expectEqualSettings(a, s, try repo.load(a));
}

fn fuzzLoad(_: void, smith: *testing.Smith) !void {
    var buf: [400]u8 = undefined;
    const n = smith.slice(&buf);
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    try db.exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL) STRICT");
    try db.execArgs("INSERT INTO settings VALUES ('settings', ?, 0)", .{buf[0..n]});
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    _ = loadOn(&db, arena.allocator()) catch |err| switch (err) {
        error.CorruptSettings => return,
        else => return err,
    };
}

test "fuzz: a stored row of any bytes loads or is reported corrupt" {
    try testing.fuzz({}, fuzzLoad, .{ .corpus = &.{
        "\x1a\x00\x00\x00{\"queues\":{\"usenet\":1},\"x\":1}",
        "\x00\x00\x00\x00",
        "\x05\x00\x00\x00{\"a\":",
    } });
}
