//! The migration runner (internal/store/migrate.go). The SQL files are the
//! ones the Go version embeds (zig/sync.sh copies them), named NNNN_name.sql.
//! Each is applied in its own transaction together with its schema_migrations
//! row, forward only: a database that records a migration this binary does
//! not know was written by a newer version and is refused.

const std = @import("std");
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
pub const File = @import("migrations").File;

pub const embedded: []const File = &@import("migrations").files;

pub const Error = error{
    /// A file is not named NNNN_name.sql.
    BadFileName,
    DuplicateVersion,
    /// The versions do not run 1, 2, 3, ... without a gap.
    MissingVersion,
    /// The database has migrations this binary does not know.
    SchemaNewer,
    /// The SQL of a migration failed; it was rolled back.
    MigrationFailed,
} || sqlite.Error || Allocator.Error;

/// The Go error text of the last failure (for the log and the user), since a
/// Zig error carries no payload.
pub const Diagnostic = struct {
    buf: [512]u8 = undefined,
    len: usize = 0,

    pub fn message(self: *const Diagnostic) []const u8 {
        return self.buf[0..self.len];
    }

    fn set(self: *Diagnostic, comptime fmt: []const u8, args: anytype) void {
        const out = std.fmt.bufPrint(&self.buf, fmt, args) catch self.buf[0..self.buf.len];
        self.len = out.len;
    }
};

pub const Parsed = struct { version: u16, name: []const u8 };

/// Splits `^(\d{4})_([a-z0-9_]+)\.sql$` or returns null.
pub fn parseName(file_name: []const u8) ?Parsed {
    const suffix = ".sql";
    if (file_name.len < 4 + 1 + 1 + suffix.len) return null;
    if (!std.mem.endsWith(u8, file_name, suffix)) return null;
    var version: u16 = 0;
    for (file_name[0..4]) |ch| {
        if (ch < '0' or ch > '9') return null;
        version = version * 10 + (ch - '0');
    }
    if (file_name[4] != '_') return null;
    const name = file_name[5 .. file_name.len - suffix.len];
    if (name.len == 0) return null;
    for (name) |ch| switch (ch) {
        'a'...'z', '0'...'9', '_' => {},
        else => return null,
    };
    return .{ .version = version, .name = name };
}

pub const Migration = struct { version: u16, name: []const u8, sql: []const u8 };

/// Parses and orders the files by version and checks that the versions run
/// 1, 2, 3, ... The result is owned by the caller; names and SQL point into
/// `files`.
pub fn load(gpa: Allocator, files: []const File, diag: *Diagnostic) Error![]Migration {
    const ms = try gpa.alloc(Migration, files.len);
    errdefer gpa.free(ms);
    for (files, ms) |f, *m| {
        const p = parseName(f.name) orelse {
            diag.set("store: migration file \"{f}\" is not named NNNN_name.sql", .{std.zig.fmtString(f.name)});
            return error.BadFileName;
        };
        m.* = .{ .version = p.version, .name = p.name, .sql = f.sql };
    }
    // Stable, so that a duplicate reports its two files in directory order.
    std.mem.sortUnstable(Migration, ms, {}, struct {
        fn less(_: void, a: Migration, b: Migration) bool {
            return a.version < b.version;
        }
    }.less);
    for (ms, 0..) |m, i| {
        const want: u16 = @intCast(i + 1);
        if (m.version == want) continue;
        if (i > 0 and m.version == ms[i - 1].version) {
            diag.set("store: duplicate migration version {d:0>4} ({s} and {s})", .{ m.version, ms[i - 1].name, m.name });
            return error.DuplicateVersion;
        }
        diag.set("store: migration {d:0>4} is missing: found {d:0>4}_{s} next", .{ want, m.version, m.name });
        return error.MissingVersion;
    }
    return ms;
}

// The text is stored in sqlite_master and the golden schema compares it, so it
// is byte for byte the Go statement (tabs included).
const create_migrations_table = "CREATE TABLE IF NOT EXISTS schema_migrations (\n" ++
    "\tversion    INTEGER PRIMARY KEY,\n" ++
    "\tname       TEXT    NOT NULL,\n" ++
    "\tapplied_at INTEGER NOT NULL\n" ++
    ")";

/// Applies every migration of files that is not yet recorded. `db` must be
/// the writer connection; `now_ms` is the applied_at of the rows written.
/// Safe to run again and from two processes: each migration re-checks its row
/// inside its transaction. On an error `diag` holds the message.
pub fn run(gpa: Allocator, db: *sqlite.Db, files: []const File, now_ms: i64, diag: *Diagnostic) Error!void {
    const ms = try load(gpa, files, diag);
    defer gpa.free(ms);
    db.exec(create_migrations_table) catch |err| {
        diag.set("store: create schema_migrations: {f}", .{db.failure()});
        return err;
    };
    try refuseNewerSchema(db, ms.len, diag);
    for (ms) |m| {
        applyMigration(db, m, now_ms) catch |err| {
            diag.set("store: migration {d:0>4}_{s}: {f}", .{ m.version, m.name, db.failure() });
            return if (err == error.OutOfMemory) err else error.MigrationFailed;
        };
    }
}

fn applyMigration(db: *sqlite.Db, m: Migration, now_ms: i64) sqlite.Error!void {
    var tx = try db.begin(.immediate);
    defer tx.deinit();
    var check = try sqlite.Stmt.prepare(db, "SELECT 1 FROM schema_migrations WHERE version = ?");
    defer check.finalize();
    try check.bindAll(.{m.version});
    if (try check.step()) return; // applied already, possibly by another process
    try db.exec(m.sql);
    try db.execArgs(
        "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
        .{ m.version, m.name, now_ms },
    );
    try tx.commit();
}

/// Fails when the database records migrations this binary does not know, which
/// means a newer version wrote to it. Migrations are forward-only, so running
/// an older binary on it is unsafe.
fn refuseNewerSchema(db: *sqlite.Db, known: usize, diag: *Diagnostic) Error!void {
    var st = try sqlite.Stmt.prepare(db, "SELECT max(version) FROM schema_migrations");
    defer st.finalize();
    if (!try st.step()) return;
    if (st.columnIsNull(0)) return;
    const newest: u64 = @intCast(@max(0, st.columnInt(0)));
    if (newest > known) {
        diag.set(
            "store: database schema is newer than this binary (database at migration {d:0>4}, binary knows up to {d:0>4}); upgrade bitnews or restore an older database",
            .{ newest, known },
        );
        return error.SchemaNewer;
    }
}

// ---- tests ----

const testing = std.testing;

fn runMem(db: *sqlite.Db, files: []const File) Error!void {
    var diag: Diagnostic = .{};
    return run(testing.allocator, db, files, 1_700_000_000_000, &diag);
}

fn tableNames(db: *sqlite.Db, gpa: Allocator) ![]u8 {
    var st = try sqlite.Stmt.prepare(db, "SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name)");
    defer st.finalize();
    _ = try st.step();
    return st.columnText(gpa, 0);
}

fn scalarInt(db: *sqlite.Db, sql: []const u8) !i64 {
    var st = try sqlite.Stmt.prepare(db, sql);
    defer st.finalize();
    try testing.expect(try st.step());
    return st.columnInt(0);
}

test "migrations apply in version order and are recorded" {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    try runMem(&db, &.{
        .{ .name = "0002_second.sql", .sql = "CREATE TABLE second (id INTEGER PRIMARY KEY, first_id INTEGER REFERENCES first(id));" },
        .{ .name = "0001_first.sql", .sql = "CREATE TABLE first (id INTEGER PRIMARY KEY);" },
    });

    var st = try sqlite.Stmt.prepare(&db, "SELECT version, name, applied_at FROM schema_migrations ORDER BY version");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 1), st.columnInt(0));
    const name = try st.columnText(testing.allocator, 1);
    defer testing.allocator.free(name);
    try testing.expectEqualStrings("first", name);
    try testing.expectEqual(@as(i64, 1_700_000_000_000), st.columnInt(2));
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 2), st.columnInt(0));
    try testing.expect(!try st.step());
}

test "a second run is a no-op" {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    const files: []const File = &.{.{ .name = "0001_items.sql", .sql = "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);" }};
    try runMem(&db, files);
    var diag: Diagnostic = .{};
    try run(testing.allocator, &db, files, 42, &diag);
    try testing.expectEqual(@as(i64, 1_700_000_000_000), try scalarInt(&db, "SELECT applied_at FROM schema_migrations"));
    try testing.expectEqual(@as(i64, 1), try scalarInt(&db, "SELECT count(*) FROM schema_migrations"));
}

test "a failed migration rolls back completely" {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    var diag: Diagnostic = .{};
    const err = run(testing.allocator, &db, &.{
        .{ .name = "0001_good.sql", .sql = "CREATE TABLE good (id INTEGER PRIMARY KEY);" },
        // The first statement succeeds and the second fails: nothing may survive.
        .{ .name = "0002_bad.sql", .sql = "CREATE TABLE partial (id INTEGER PRIMARY KEY); INSERT INTO nonexistent VALUES (1);" },
    }, 1, &diag);
    try testing.expectError(error.MigrationFailed, err);
    try testing.expect(std.mem.indexOf(u8, diag.message(), "0002_bad") != null);
    const tables = try tableNames(&db, testing.allocator);
    defer testing.allocator.free(tables);
    try testing.expectEqualStrings("good,schema_migrations", tables);
    try testing.expectEqual(@as(i64, 1), try scalarInt(&db, "SELECT count(*) FROM schema_migrations"));
}

test "malformed migration sets are rejected before anything is applied" {
    const Case = struct { files: []const File, err: anyerror, want: []const u8 };
    const cases = [_]Case{
        .{ .files = &.{ .{ .name = "0001_a.sql", .sql = "SELECT 1;" }, .{ .name = "0003_c.sql", .sql = "SELECT 1;" } }, .err = error.MissingVersion, .want = "0002" },
        .{ .files = &.{.{ .name = "0002_b.sql", .sql = "SELECT 1;" }}, .err = error.MissingVersion, .want = "0001" },
        .{ .files = &.{ .{ .name = "0001_a.sql", .sql = "SELECT 1;" }, .{ .name = "0001_b.sql", .sql = "SELECT 1;" } }, .err = error.DuplicateVersion, .want = "duplicate" },
        .{ .files = &.{ .{ .name = "0001_a.sql", .sql = "SELECT 1;" }, .{ .name = "notes.sql", .sql = "SELECT 1;" } }, .err = error.BadFileName, .want = "notes.sql" },
        .{ .files = &.{.{ .name = "0001_.sql", .sql = "SELECT 1;" }}, .err = error.BadFileName, .want = "0001_.sql" },
    };
    for (cases) |tc| {
        var db = try sqlite.Db.open(":memory:", .{}, null);
        defer db.close();
        var diag: Diagnostic = .{};
        try testing.expectError(tc.err, run(testing.allocator, &db, tc.files, 1, &diag));
        try testing.expect(std.mem.indexOf(u8, diag.message(), tc.want) != null);
        const tables = try tableNames(&db, testing.allocator);
        defer testing.allocator.free(tables);
        try testing.expectEqualStrings("", tables);
    }
}

test "a database newer than the binary is refused" {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    try runMem(&db, &.{
        .{ .name = "0001_a.sql", .sql = "CREATE TABLE a (id INTEGER PRIMARY KEY);" },
        .{ .name = "0002_b.sql", .sql = "CREATE TABLE b (id INTEGER PRIMARY KEY);" },
    });
    var diag: Diagnostic = .{};
    try testing.expectError(error.SchemaNewer, run(testing.allocator, &db, &.{
        .{ .name = "0001_a.sql", .sql = "CREATE TABLE a (id INTEGER PRIMARY KEY);" },
    }, 1, &diag));
    try testing.expect(std.mem.indexOf(u8, diag.message(), "newer") != null);
    try testing.expect(std.mem.indexOf(u8, diag.message(), "0002") != null);
}

test "the embedded migrations apply to a fresh database" {
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    try runMem(&db, embedded);
    try testing.expectEqual(@as(i64, embedded.len), try scalarInt(&db, "SELECT count(*) FROM schema_migrations"));
    // Migration 0009 is the newest at the time of writing; later ones add to it.
    try testing.expect(try scalarInt(&db, "SELECT count(*) FROM sqlite_master WHERE name = 'app_key'") == 1);
}

test "parseName follows ^(\\d{4})_([a-z0-9_]+)\\.sql$" {
    const ok = parseName("0007_torrent_state.sql").?;
    try testing.expectEqual(@as(u16, 7), ok.version);
    try testing.expectEqualStrings("torrent_state", ok.name);
    for ([_][]const u8{ "", "0001_.sql", "001_a.sql", "00001_a.sql", "0001-a.sql", "0001_A.sql", "0001_a.SQL", "0001_a.sql\n", "0001_a.sqlx", "a001_a.sql", "0001_a b.sql", "0001__.sqll" }) |bad| {
        try testing.expect(parseName(bad) == null);
    }
    try testing.expect(parseName("0000_a.sql") != null);
    try testing.expect(parseName("0001__.sql") != null);
}

/// Reference implementation of the regular expression, written differently
/// from parseName (scan, not index), to check it against.
fn matchesPattern(s: []const u8) bool {
    var i: usize = 0;
    while (i < 4) : (i += 1) if (i >= s.len or !std.ascii.isDigit(s[i])) return false;
    if (i >= s.len or s[i] != '_') return false;
    i += 1;
    const start = i;
    while (i < s.len and (std.ascii.isLower(s[i]) or std.ascii.isDigit(s[i]) or s[i] == '_')) i += 1;
    // The class includes '_' but not '.', so the name ends at the first '.'.
    return i > start and std.mem.eql(u8, s[i..], ".sql");
}

fn fuzzParse(_: void, smith: *testing.Smith) !void {
    var buf: [64]u8 = undefined;
    const n = smith.slice(&buf);
    const input = buf[0..n];
    const got = parseName(input);
    try testing.expectEqual(matchesPattern(input), got != null);
    if (got) |p| {
        // The parts always rebuild the input.
        var out: [96]u8 = undefined;
        const rebuilt = try std.fmt.bufPrint(&out, "{d:0>4}_{s}.sql", .{ p.version, p.name });
        try testing.expectEqualStrings(input, rebuilt);
    }
}

test "fuzz: migration file names" {
    try testing.fuzz({}, fuzzParse, .{ .corpus = &.{
        "\x15\x00\x00\x000001_initial.sql",
        "\x0c\x00\x00\x000009_app_key.sql",
        "\x09\x00\x00\x000001_.sql",
        "\x09\x00\x00\x00notes.sql",
    } });
}

fn fuzzLoad(_: void, smith: *testing.Smith) !void {
    // Up to eight names split out of one buffer; load must never crash and
    // must accept exactly the sets that run 1, 2, 3, ...
    var buf: [256]u8 = undefined;
    const n = smith.slice(&buf);
    var files: [8]File = undefined;
    var count: usize = 0;
    var it = std.mem.splitScalar(u8, buf[0..n], '\n');
    while (it.next()) |part| {
        if (count == files.len) break;
        files[count] = .{ .name = part, .sql = "" };
        count += 1;
    }
    var diag: Diagnostic = .{};
    if (load(testing.allocator, files[0..count], &diag)) |ms| {
        defer testing.allocator.free(ms);
        for (ms, 0..) |m, i| try testing.expectEqual(@as(u16, @intCast(i + 1)), m.version);
    } else |_| {
        try testing.expect(diag.message().len > 0);
    }
}

test "fuzz: migration sets" {
    try testing.fuzz({}, fuzzLoad, .{ .corpus = &.{
        "\x23\x00\x00\x000001_a.sql\n0002_b.sql\n0003_c.sql",
        "\x17\x00\x00\x000002_a.sql\n0001_b.sql\n0001_c",
    } });
}
