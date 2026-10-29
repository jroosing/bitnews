//! Compatibility with the Go store: the golden database and schema dump in
//! testdata/compat are written by internal/store/compat_db_test.go (run it
//! with -update after a new migration). `zig build test` runs with the
//! working directory zig/, so the paths below are relative to it.

const std = @import("std");
const Io = std.Io;
const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const appkey = @import("appkey_repo.zig");

const golden_key = "bitnews_golden_key_for_tests";

fn readGolden(name: []const u8) ![]u8 {
    var buf: [128]u8 = undefined;
    const path = try std.fmt.bufPrint(&buf, "testdata/compat/{s}", .{name});
    return Io.Dir.cwd().readFileAlloc(testing.io, path, testing.allocator, .limited(16 << 20));
}

/// Same query and format as schemaDump in compat_db_test.go.
fn schemaDump(gpa: std.mem.Allocator, db: *sqlite.Db) ![]u8 {
    var st = try sqlite.Stmt.prepare(db, "SELECT type, name, coalesce(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name");
    defer st.finalize();
    var out: std.ArrayList(u8) = .empty;
    errdefer out.deinit(gpa);
    while (try st.step()) {
        for (0..3) |col| {
            const text = try st.columnText(gpa, @intCast(col));
            defer gpa.free(text);
            try out.appendSlice(gpa, text);
            try out.append(gpa, if (col == 2) '\n' else '|');
        }
        try out.appendSlice(gpa, "--\n");
    }
    return out.toOwnedSlice(gpa);
}

fn tmpPath(tmp: *testing.TmpDir, name: []const u8) ![]u8 {
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    return std.fmt.allocPrint(testing.allocator, "{s}/{s}", .{ buf[0..n], name });
}

test "the migrator on a fresh database builds the schema Go built" {
    const want = try readGolden("schema.txt");
    defer testing.allocator.free(want);
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    var diag: migrate.Diagnostic = .{};
    try migrate.run(testing.allocator, &db, migrate.embedded, 1, &diag);
    const got = try schemaDump(testing.allocator, &db);
    defer testing.allocator.free(got);
    try testing.expectEqualStrings(want, got);
}

test "a database written by Go opens read/write and keeps its data" {
    const want = try readGolden("schema.txt");
    defer testing.allocator.free(want);
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    // A copy: the committed file must never change.
    try Io.Dir.cwd().copyFile("testdata/compat/bitnews.db", tmp.dir, "bitnews.db", testing.io, .{});
    const path = try tmpPath(&tmp, "bitnews.db");
    defer testing.allocator.free(path);

    const store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
    defer store.close();
    {
        // Migrating a current database changes nothing.
        const w = try store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 99, &diag);
        const got = try schemaDump(testing.allocator, w.db);
        defer testing.allocator.free(got);
        try testing.expectEqualStrings(want, got);
    }

    const repo: appkey.AppKeyRepo = .{ .store = store };
    const key = try repo.get(testing.allocator);
    defer testing.allocator.free(key);
    try testing.expectEqualStrings(golden_key, key);

    // Write through Zig, read back, and the rows Go wrote are still there.
    try repo.set("bitnews_written_by_zig");
    const key2 = try repo.get(testing.allocator);
    defer testing.allocator.free(key2);
    try testing.expectEqualStrings("bitnews_written_by_zig", key2);

    const r = try store.acquireReader();
    defer r.release();
    var st = try sqlite.Stmt.prepare(r.db, "SELECT (SELECT count(*) FROM servers), (SELECT count(*) FROM categories), (SELECT value FROM settings WHERE key = 'main'), (SELECT max(version) FROM schema_migrations)");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 1), st.columnInt(0));
    try testing.expectEqual(@as(i64, 1), st.columnInt(1));
    const settings = try st.columnText(testing.allocator, 2);
    defer testing.allocator.free(settings);
    try testing.expectEqualStrings("{\"queues\":{\"usenet\":1}}", settings);
    try testing.expectEqual(@as(i64, migrate.embedded.len), st.columnInt(3));
}

test "the Go database refuses a binary that knows fewer migrations" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try Io.Dir.cwd().copyFile("testdata/compat/bitnews.db", tmp.dir, "bitnews.db", testing.io, .{});
    const path = try tmpPath(&tmp, "bitnews.db");
    defer testing.allocator.free(path);
    var db = try sqlite.Db.open(path, .{}, null);
    defer db.close();
    var diag: migrate.Diagnostic = .{};
    try testing.expectError(error.SchemaNewer, migrate.run(testing.allocator, &db, migrate.embedded[0 .. migrate.embedded.len - 1], 1, &diag));
}

test "a bad migration file name is refused" {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    var diag: migrate.Diagnostic = .{};
    try testing.expectError(error.BadFileName, migrate.run(testing.allocator, &db, &.{
        .{ .name = "0001_initial.sql", .sql = "SELECT 1;" },
        .{ .name = "0002_Bad.sql", .sql = "SELECT 1;" },
    }, 1, &diag));
    try testing.expect(std.mem.indexOf(u8, diag.message(), "0002_Bad.sql") != null);
}
