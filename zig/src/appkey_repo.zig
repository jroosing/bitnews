//! The app key (internal/store/repo_appkey.go): the one secret that
//! authenticates every client (ADR-0006). The app_key table holds a single
//! row (id 1) with the key in plain text and updated_at in milliseconds.
//!
//! The functions return the key as plain bytes owned by the caller; wrap it in
//! `Secret` straight away and wipe it when done, so it never reaches a log or
//! JSON.

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");

/// The generated keys are about 60 bytes; this only bounds hostile input.
pub const max_key_len = 1024;

pub const Error = error{
    /// No key is stored yet (Go: core.ErrNotFound).
    NotFound,
    /// The table refuses an empty key (CHECK key <> '').
    EmptyKey,
    KeyTooLong,
} || sqlite.Error || Allocator.Error || Io.Cancelable;

pub const AppKeyRepo = struct {
    store: *sqlite.Store,

    /// The stored key, owned by the caller.
    pub fn get(self: AppKeyRepo, gpa: Allocator) Error![]u8 {
        const r = try self.store.acquireReader();
        defer r.release();
        return getOn(r.db, gpa);
    }

    /// Stores key as the one app key, replacing any previous one.
    pub fn set(self: AppKeyRepo, key: []const u8) Error!void {
        const now_ms = Io.Clock.now(.real, self.store.io).toMilliseconds();
        const w = try self.store.acquireWriter();
        defer w.release();
        return setOn(w.db, key, now_ms);
    }
};

pub fn getOn(db: *sqlite.Db, gpa: Allocator) Error![]u8 {
    var st = try sqlite.Stmt.prepare(db, "SELECT key FROM app_key WHERE id = 1");
    defer st.finalize();
    if (!try st.step()) return error.NotFound;
    return st.columnText(gpa, 0);
}

pub fn setOn(db: *sqlite.Db, key: []const u8, now_ms: i64) Error!void {
    if (key.len == 0) return error.EmptyKey;
    if (key.len > max_key_len) return error.KeyTooLong;
    try db.execArgs(
        \\INSERT INTO app_key (id, key, updated_at) VALUES (1, ?, ?)
        \\ON CONFLICT (id) DO UPDATE SET key = excluded.key, updated_at = excluded.updated_at
    , .{ key, now_ms });
}

// ---- tests ----

const testing = std.testing;
const migrate = @import("migrate.zig");

fn memDb() !sqlite.Db {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    errdefer db.close();
    var diag: migrate.Diagnostic = .{};
    try migrate.run(testing.allocator, &db, migrate.embedded, 1, &diag);
    return db;
}

const plain_key = "bitnews_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqr";

test "a fresh database has no key" {
    var db = try memDb();
    defer db.close();
    try testing.expectError(error.NotFound, getOn(&db, testing.allocator));
}

test "set then get round-trips, replaces the key and keeps one row" {
    var db = try memDb();
    defer db.close();
    try setOn(&db, plain_key, 1_700_000_000_000);
    const got = try getOn(&db, testing.allocator);
    defer testing.allocator.free(got);
    try testing.expectEqualStrings(plain_key, got);

    try setOn(&db, "bitnews_other", 1_700_000_001_000);
    const got2 = try getOn(&db, testing.allocator);
    defer testing.allocator.free(got2);
    try testing.expectEqualStrings("bitnews_other", got2);

    var st = try sqlite.Stmt.prepare(&db, "SELECT count(*), max(updated_at) FROM app_key");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 1), st.columnInt(0));
    try testing.expectEqual(@as(i64, 1_700_000_001_000), st.columnInt(1));
}

test "the key is stored as plain text and empty or oversized keys are refused" {
    var db = try memDb();
    defer db.close();
    try setOn(&db, plain_key, 5);
    var st = try sqlite.Stmt.prepare(&db, "SELECT typeof(key), key FROM app_key WHERE id = 1");
    defer st.finalize();
    try testing.expect(try st.step());
    const kind = try st.columnText(testing.allocator, 0);
    defer testing.allocator.free(kind);
    try testing.expectEqualStrings("text", kind);

    try testing.expectError(error.EmptyKey, setOn(&db, "", 5));
    const big = try testing.allocator.alloc(u8, max_key_len + 1);
    defer testing.allocator.free(big);
    @memset(big, 'a');
    try testing.expectError(error.KeyTooLong, setOn(&db, big, 5));
    // The table itself allows only the row with id 1.
    try testing.expectError(error.Constraint, db.exec("INSERT INTO app_key (id, key, updated_at) VALUES (2, 'x', 0)"));
}

test "the repo works through the store with the reader pool" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &buf);
    const path = try std.fmt.allocPrint(testing.allocator, "{s}/bitnews.db", .{buf[0..dir_len]});
    defer testing.allocator.free(path);
    const store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
    defer store.close();
    {
        const w = try store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 1, &diag);
    }
    const repo: AppKeyRepo = .{ .store = store };
    try testing.expectError(error.NotFound, repo.get(testing.allocator));
    try repo.set(plain_key);
    const got = try repo.get(testing.allocator);
    defer testing.allocator.free(got);
    try testing.expectEqualStrings(plain_key, got);
}

fn fuzzRoundTrip(_: void, smith: *testing.Smith) !void {
    var buf: [max_key_len + 64]u8 = undefined;
    const n = smith.slice(&buf);
    const key = buf[0..n];
    // Only the table, not every migration: a fuzz run must be fast.
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    try db.exec("CREATE TABLE app_key (id INTEGER PRIMARY KEY CHECK (id = 1), key TEXT NOT NULL CHECK (key <> ''), updated_at INTEGER NOT NULL) STRICT");
    if (setOn(&db, key, 1)) {
        const got = try getOn(&db, testing.allocator);
        defer testing.allocator.free(got);
        // Whatever the bytes (NUL, invalid UTF-8), they come back unchanged.
        try testing.expectEqualSlices(u8, key, got);
    } else |err| switch (err) {
        error.EmptyKey => try testing.expectEqual(@as(usize, 0), key.len),
        error.KeyTooLong => try testing.expect(key.len > max_key_len),
        else => return err,
    }
}

test "fuzz: any key bytes round-trip or are refused cleanly" {
    try testing.fuzz({}, fuzzRoundTrip, .{ .corpus = &.{
        "\x1d\x00\x00\x00bitnews_golden_key_for_tests",
        "\x00\x00\x00\x00",
        "\x05\x00\x00\x00a\x00b\xff\xfe",
    } });
}
