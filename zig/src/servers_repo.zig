//! The `servers` table (internal/store/repo_servers.go). The password is stored
//! sealed with the master key and the AAD "servers/password/<id>", in the
//! format Go uses ("v1:..."); no password is stored as "". A password that
//! cannot be opened (wrong key, damaged value) does not fail a read: the Server
//! comes back with `password_unavailable` set and stays listed.
//!
//! Everything a call returns is allocated by the allocator passed in (a request
//! arena in the daemon, the testing allocator in tests).

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const secretbox = @import("secretbox.zig");
const Key = @import("keyfile.zig").Key;
const Secret = @import("secret.zig").Secret;
const types = @import("servers_types.zig");
const Server = types.Server;

pub const Error = error{
    /// No Server has this ID (Go: core.ErrNotFound).
    NotFound,
    /// The name is taken (Go: core.ErrConflict).
    Conflict,
    /// The Server breaks a rule of `servers_types.check`; the service reports
    /// these before it gets here, so this is a defence.
    Invalid,
} || sqlite.Error || Allocator.Error || Io.Cancelable || Io.RandomSecureError;

const columns = "id, name, host, port, username, password, max_connections, retention_days, priority, enabled, created_at";

pub const ServerRepo = struct {
    store: *sqlite.Store,
    /// Owned by whoever loaded it; must outlive the repo.
    key: *const Key,

    /// Stores `s` with its password sealed.
    pub fn create(self: ServerRepo, alloc: Allocator, s: Server) Error!void {
        if (types.check(s).any()) return error.Invalid;
        const sealed = try self.sealPassword(alloc, s);
        defer alloc.free(sealed);
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            "INSERT INTO servers (" ++ columns ++ ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            .{ s.id, s.name, s.host, s.port, s.username, sealed, s.max_connections, s.retention_days, s.priority, s.enabled, s.created_at_ms },
        ) catch |err| return mapWriteError(w.db, err);
    }

    /// A password that cannot be opened is reported through
    /// `password_unavailable`, not as an error.
    pub fn get(self: ServerRepo, alloc: Allocator, id: []const u8) Error!Server {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT " ++ columns ++ " FROM servers WHERE id = ?");
        defer st.finalize();
        try st.bindAll(.{id});
        if (!try st.step()) return error.NotFound;
        return self.scan(alloc, &st);
    }

    /// Every Server ordered by priority, then name.
    pub fn list(self: ServerRepo, alloc: Allocator) Error![]Server {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT " ++ columns ++ " FROM servers ORDER BY priority, name");
        defer st.finalize();
        var out: std.ArrayList(Server) = .empty;
        errdefer {
            for (out.items) |*s| s.deinit(alloc);
            out.deinit(alloc);
        }
        while (try st.step()) {
            var s = try self.scan(alloc, &st);
            errdefer s.deinit(alloc);
            try out.append(alloc, s);
        }
        return out.toOwnedSlice(alloc);
    }

    /// Stores every field except the ID and the creation time. The password is
    /// sealed again, except when `s.password_unavailable` is set: then the
    /// stored one stays as it is.
    pub fn update(self: ServerRepo, alloc: Allocator, s: Server) Error!void {
        if (types.check(s).any()) return error.Invalid;
        const common = "UPDATE servers SET name = ?, host = ?, port = ?, username = ?, max_connections = ?, retention_days = ?, priority = ?, enabled = ?";
        if (s.password_unavailable) {
            const w = try self.store.acquireWriter();
            defer w.release();
            w.db.execArgs(common ++ " WHERE id = ?", .{ s.name, s.host, s.port, s.username, s.max_connections, s.retention_days, s.priority, s.enabled, s.id }) catch |err| return mapWriteError(w.db, err);
            if (w.db.changes() == 0) return error.NotFound;
            return;
        }
        const sealed = try self.sealPassword(alloc, s);
        defer alloc.free(sealed);
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(common ++ ", password = ? WHERE id = ?", .{ s.name, s.host, s.port, s.username, s.max_connections, s.retention_days, s.priority, s.enabled, sealed, s.id }) catch |err| return mapWriteError(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
    }

    pub fn delete(self: ServerRepo, id: []const u8) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        try w.db.execArgs("DELETE FROM servers WHERE id = ?", .{id});
        if (w.db.changes() == 0) return error.NotFound;
    }

    /// The stored form of the password; "" when there is none. Freed by the
    /// caller.
    fn sealPassword(self: ServerRepo, alloc: Allocator, s: Server) Error![]u8 {
        const pw = s.passwordBytes();
        if (pw.len == 0) return alloc.dupe(u8, "");
        const aad = try secretbox.aad(alloc, "servers", "password", s.id);
        defer alloc.free(aad);
        return secretbox.seal(alloc, self.store.io, self.key, pw, aad);
    }

    fn scan(self: ServerRepo, alloc: Allocator, st: *sqlite.Stmt) Error!Server {
        var s: Server = .{};
        s.id = try st.columnText(alloc, 0);
        errdefer alloc.free(s.id);
        s.name = try st.columnText(alloc, 1);
        errdefer alloc.free(s.name);
        s.host = try st.columnText(alloc, 2);
        errdefer alloc.free(s.host);
        s.port = st.columnInt(3);
        s.username = try st.columnText(alloc, 4);
        errdefer alloc.free(s.username);
        s.max_connections = st.columnInt(6);
        s.retention_days = st.columnInt(7);
        s.priority = st.columnInt(8);
        s.enabled = st.columnInt(9) != 0;
        s.created_at_ms = st.columnInt(10);

        const sealed = try st.columnText(alloc, 5);
        defer alloc.free(sealed);
        if (sealed.len > 0) {
            const aad = try secretbox.aad(alloc, "servers", "password", s.id);
            defer alloc.free(aad);
            if (secretbox.open(alloc, self.key, sealed, aad)) |plain| {
                // The plaintext becomes the Secret's buffer, without a copy.
                s.password = .{ .ptr = plain.ptr, .len = plain.len, .allocator = alloc };
            } else |err| switch (err) {
                // Wrong or missing key, or a damaged value: the Server stays
                // usable for listing, and the user enters the password again.
                error.Decrypt, error.UnsupportedFormat => s.password_unavailable = true,
                error.OutOfMemory => return error.OutOfMemory,
            }
        }
        return s;
    }
};

fn mapWriteError(db: *const sqlite.Db, err: sqlite.Error) Error {
    if (err == error.Constraint and db.failure().isConflict()) return error.Conflict;
    return err;
}

// ---- tests ----

const testing = std.testing;
const migrate = @import("migrate.zig");
const keyfile = @import("keyfile.zig");

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    key: Key,

    fn path(tmp: *testing.TmpDir, name: []const u8) ![]u8 {
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try tmp.dir.realPath(testing.io, &buf);
        return std.fmt.allocPrint(testing.allocator, "{s}/{s}", .{ buf[0..n], name });
    }

    /// A fresh migrated database.
    fn open(rig: *Rig, key_byte: u8) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        const p = try path(&rig.tmp, "test.db");
        defer testing.allocator.free(p);
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, p, .{ .readers = 2 }, null);
        errdefer rig.store.close();
        const w = try rig.store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        rig.key = .{ .bytes = @splat(key_byte) };
    }

    /// A copy of the database Go wrote; the golden file itself is never opened.
    fn openGolden(rig: *Rig, key: Key) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        try Io.Dir.cwd().copyFile("testdata/compat/bitnews.db", rig.tmp.dir, "bitnews.db", testing.io, .{});
        const p = try path(&rig.tmp, "bitnews.db");
        defer testing.allocator.free(p);
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, p, .{ .readers = 2 }, null);
        rig.key = key;
    }

    fn close(rig: *Rig) void {
        rig.key.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }

    fn repo(rig: *Rig) ServerRepo {
        return .{ .store = rig.store, .key = &rig.key };
    }

    fn sql(rig: *Rig, comptime q: []const u8, args: anytype) !void {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.execArgs(q, args);
    }

    /// The raw password column of a row.
    fn storedPassword(rig: *Rig, id: []const u8) ![]u8 {
        const r = try rig.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT password FROM servers WHERE id = ?");
        defer st.finalize();
        try st.bindAll(.{id});
        try testing.expect(try st.step());
        return st.columnText(testing.allocator, 0);
    }
};

fn sample(name: []const u8, password: ?[]const u8) !Server {
    var s: Server = .{
        .id = try testing.allocator.dupe(u8, "id-1"),
        .name = try testing.allocator.dupe(u8, name),
        .host = try testing.allocator.dupe(u8, "news.example.com"),
        .username = try testing.allocator.dupe(u8, "joe"),
        .max_connections = 10,
        .created_at_ms = 1_700_000_000_123,
    };
    if (password) |p| s.password = try Secret.init(testing.allocator, p);
    return s;
}

test "create then get round-trips every field and seals the password" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", "hunter2");
    defer s.deinit(testing.allocator);
    s.port = 119;
    s.retention_days = 3000;
    s.priority = 2;
    s.enabled = false;

    try rig.repo().create(testing.allocator, s);

    var got = try rig.repo().get(testing.allocator, "id-1");
    defer got.deinit(testing.allocator);
    try testing.expectEqualStrings("main", got.name);
    try testing.expectEqualStrings("news.example.com", got.host);
    try testing.expectEqualStrings("joe", got.username);
    try testing.expectEqualStrings("hunter2", got.passwordBytes());
    try testing.expectEqual(@as(i64, 119), got.port);
    try testing.expectEqual(@as(i64, 10), got.max_connections);
    try testing.expectEqual(@as(i64, 3000), got.retention_days);
    try testing.expectEqual(@as(i64, 2), got.priority);
    try testing.expect(!got.enabled);
    try testing.expectEqual(@as(i64, 1_700_000_000_123), got.created_at_ms);
    try testing.expect(!got.password_unavailable);

    const stored = try rig.storedPassword("id-1");
    defer testing.allocator.free(stored);
    try testing.expect(std.mem.startsWith(u8, stored, "v1:"));
    try testing.expect(std.mem.indexOf(u8, stored, "hunter2") == null);
}

test "no password is stored as the empty string" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", null);
    defer s.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, s);

    const stored = try rig.storedPassword("id-1");
    defer testing.allocator.free(stored);
    try testing.expectEqualStrings("", stored);
    var got = try rig.repo().get(testing.allocator, "id-1");
    defer got.deinit(testing.allocator);
    try testing.expect(!got.hasPassword());
}

test "a taken name is a conflict and an unknown id is not found" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var a = try sample("main", null);
    defer a.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, a);
    var b = try sample("main", null);
    defer b.deinit(testing.allocator);
    testing.allocator.free(b.id);
    b.id = try testing.allocator.dupe(u8, "id-2");

    try testing.expectError(error.Conflict, rig.repo().create(testing.allocator, b));
    try testing.expectError(error.NotFound, rig.repo().get(testing.allocator, "nope"));
    try testing.expectError(error.NotFound, rig.repo().delete("nope"));
    a.priority = 1;
    testing.allocator.free(a.id);
    a.id = try testing.allocator.dupe(u8, "nope");
    try testing.expectError(error.NotFound, rig.repo().update(testing.allocator, a));
}

test "a Server that breaks a rule is not stored" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", null);
    defer s.deinit(testing.allocator);
    s.port = 0;
    try testing.expectError(error.Invalid, rig.repo().create(testing.allocator, s));
}

test "list is ordered by priority, then name" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    const rows = [_]struct { id: []const u8, name: []const u8, prio: i64 }{
        .{ .id = "1", .name = "zeta", .prio = 0 },
        .{ .id = "2", .name = "fill", .prio = 1 },
        .{ .id = "3", .name = "alpha", .prio = 0 },
    };
    for (rows) |row| {
        var s = try sample(row.name, null);
        defer s.deinit(testing.allocator);
        testing.allocator.free(s.id);
        s.id = try testing.allocator.dupe(u8, row.id);
        s.priority = row.prio;
        try rig.repo().create(testing.allocator, s);
    }
    const got = try rig.repo().list(testing.allocator);
    defer types.deinitList(testing.allocator, got);
    try testing.expectEqual(@as(usize, 3), got.len);
    try testing.expectEqualStrings("alpha", got[0].name);
    try testing.expectEqualStrings("zeta", got[1].name);
    try testing.expectEqualStrings("fill", got[2].name);
}

test "update seals again, or keeps the stored password when it is unavailable" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", "old");
    defer s.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, s);
    const before = try rig.storedPassword("id-1");
    defer testing.allocator.free(before);

    // password_unavailable: the other fields change, the column does not.
    var kept = s;
    kept.password = null;
    kept.password_unavailable = true;
    kept.host = "other.example.org";
    kept.port = 443;
    try rig.repo().update(testing.allocator, kept);
    const after = try rig.storedPassword("id-1");
    defer testing.allocator.free(after);
    try testing.expectEqualStrings(before, after);
    var got = try rig.repo().get(testing.allocator, "id-1");
    defer got.deinit(testing.allocator);
    try testing.expectEqualStrings("other.example.org", got.host);
    try testing.expectEqualStrings("old", got.passwordBytes());

    // A new password replaces it; an empty one removes it.
    var fresh = try Secret.init(testing.allocator, "new");
    defer fresh.deinit();
    var changed = got;
    changed.password = fresh;
    try rig.repo().update(testing.allocator, changed);
    var again = try rig.repo().get(testing.allocator, "id-1");
    defer again.deinit(testing.allocator);
    try testing.expectEqualStrings("new", again.passwordBytes());

    changed.password = null;
    try rig.repo().update(testing.allocator, changed);
    const none = try rig.storedPassword("id-1");
    defer testing.allocator.free(none);
    try testing.expectEqualStrings("", none);
}

test "delete removes the row" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", null);
    defer s.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, s);
    try rig.repo().delete("id-1");
    try testing.expectError(error.NotFound, rig.repo().get(testing.allocator, "id-1"));
}

test "another key makes the password unavailable but the Server stays listed" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", "hunter2");
    defer s.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, s);

    var other: Key = .{ .bytes = @splat(9) };
    defer other.deinit();
    const wrong: ServerRepo = .{ .store = rig.store, .key = &other };
    const listed = try wrong.list(testing.allocator);
    defer types.deinitList(testing.allocator, listed);
    try testing.expectEqual(@as(usize, 1), listed.len);
    try testing.expect(listed[0].password_unavailable);
    try testing.expect(listed[0].hasPassword());
    try testing.expectEqualStrings("", listed[0].passwordBytes());
}

test "a tampered, moved or foreign value is unavailable, not an error" {
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", "hunter2");
    defer s.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, s);
    const good = try rig.storedPassword("id-1");
    defer testing.allocator.free(good);

    // Flip a byte of the ciphertext, then an unknown format, then the value
    // of another row (the AAD binds the row).
    const damaged = try testing.allocator.dupe(u8, good);
    defer testing.allocator.free(damaged);
    damaged[20] = if (damaged[20] == 'A') 'B' else 'A';
    for ([_][]const u8{ damaged, "plaintext", "v2:abcd", "v1:***" }) |value| {
        try rig.sql("UPDATE servers SET password = ? WHERE id = 'id-1'", .{value});
        var got = try rig.repo().get(testing.allocator, "id-1");
        defer got.deinit(testing.allocator);
        try testing.expect(got.password_unavailable);
    }
    try rig.sql("UPDATE servers SET id = 'id-9', password = ? WHERE id = 'id-1'", .{good});
    var moved = try rig.repo().get(testing.allocator, "id-9");
    defer moved.deinit(testing.allocator);
    try testing.expect(moved.password_unavailable);
}

// ---- compatibility with Go ----

const Sealed = struct { table: []const u8, column: []const u8, row_id: []const u8, plaintext: []const u8, stored: []const u8 };
const Golden = struct { master_key: []const u8, sealed: []const Sealed };

fn readGolden(arena: Allocator) !Golden {
    const text = try Io.Dir.cwd().readFileAlloc(testing.io, "testdata/compat/crypto.json", arena, .limited(1 << 20));
    return std.json.parseFromSliceLeaky(Golden, arena, text, .{ .ignore_unknown_fields = true });
}

test "the server row Go wrote opens, and a password Go sealed opens here" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const golden = try readGolden(arena.allocator());
    var rig: Rig = undefined;
    try rig.openGolden(try keyfile.parse(golden.master_key));
    defer rig.close();

    {
        var go_row = try rig.repo().get(testing.allocator, "srv1");
        defer go_row.deinit(testing.allocator);
        try testing.expectEqualStrings("news", go_row.name);
        try testing.expectEqualStrings("news.example.com", go_row.host);
        try testing.expectEqual(@as(i64, 563), go_row.port);
        try testing.expectEqualStrings("user", go_row.username);
        try testing.expectEqual(@as(i64, 8), go_row.max_connections);
        try testing.expect(go_row.enabled);
        try testing.expectEqual(@as(i64, 1_700_000_000_000), go_row.created_at_ms);
        try testing.expect(!go_row.hasPassword());
    }

    // The Go crypto golden file has passwords sealed for servers/password/<row>.
    var seen: usize = 0;
    for (golden.sealed) |entry| {
        if (!std.mem.eql(u8, entry.table, "servers") or !std.mem.eql(u8, entry.column, "password")) continue;
        seen += 1;
        try rig.sql("UPDATE servers SET id = ?, password = ? WHERE rowid = (SELECT min(rowid) FROM servers)", .{ entry.row_id, entry.stored });
        var got = try rig.repo().get(testing.allocator, entry.row_id);
        defer got.deinit(testing.allocator);
        try testing.expect(!got.password_unavailable);
        try testing.expectEqualStrings(entry.plaintext, got.passwordBytes());
    }
    try testing.expect(seen > 0);
}

test "a password Zig sealed opens with the Go algorithm's inputs (AAD and key)" {
    // The Go side reads zig_written.json for the format; here the row written
    // through the repo is opened with secretbox directly, with the AAD Go uses.
    var rig: Rig = undefined;
    try rig.open(7);
    defer rig.close();
    var s = try sample("main", "p\xc3\xa4ss-\xe6\x97\xa5\xe6\x9c\xac");
    defer s.deinit(testing.allocator);
    try rig.repo().create(testing.allocator, s);

    const stored = try rig.storedPassword("id-1");
    defer testing.allocator.free(stored);
    const plain = try secretbox.open(testing.allocator, &rig.key, stored, "servers/password/id-1");
    defer secretbox.wipeAndFree(testing.allocator, plain);
    try testing.expectEqualStrings("p\xc3\xa4ss-\xe6\x97\xa5\xe6\x9c\xac", plain);
}
