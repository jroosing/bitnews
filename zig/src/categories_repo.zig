//! Categories in SQLite (internal/store/repo_categories.go): the `categories`
//! table with id, a unique name and a folder override per protocol. Deleting
//! a Category leaves its Downloads without one (the foreign key sets
//! downloads.category_id to NULL).
//!
//! Strings of a result are allocated in the allocator the caller passes (an
//! arena per request); nothing is kept here.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const types = @import("categories_types.zig");
const Category = types.Category;

pub const Error = error{
    /// No such Category (Go: core.ErrNotFound).
    NotFound,
    /// The name or the ID is already taken (Go: core.ErrConflict).
    Conflict,
} || types.ValidationError || sqlite.Error || Allocator.Error || Io.Cancelable;

pub const CategoryRepo = struct {
    store: *sqlite.Store,

    /// Validates and stores c.
    pub fn create(self: CategoryRepo, c: Category) Error!void {
        try types.validate(c);
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            "INSERT INTO categories (id, name, usenet_dir, torrent_dir) VALUES (?, ?, ?, ?)",
            .{ c.id, c.name, c.usenet_dir, c.torrent_dir },
        ) catch |err| return mapWrite(w.db, err);
    }

    pub fn get(self: CategoryRepo, gpa: Allocator, id: []const u8) Error!Category {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT id, name, usenet_dir, torrent_dir FROM categories WHERE id = ?");
        defer st.finalize();
        try st.bindAll(.{id});
        if (!try st.step()) return error.NotFound;
        return readRow(&st, gpa);
    }

    /// Every Category ordered by name. The slice is empty, never undefined.
    pub fn list(self: CategoryRepo, gpa: Allocator) Error![]Category {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT id, name, usenet_dir, torrent_dir FROM categories ORDER BY name");
        defer st.finalize();
        var out: std.ArrayList(Category) = .empty;
        while (try st.step()) try out.append(gpa, try readRow(&st, gpa));
        return out.items;
    }

    /// Validates c and stores its name and folder overrides.
    pub fn update(self: CategoryRepo, c: Category) Error!void {
        try types.validate(c);
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            "UPDATE categories SET name = ?, usenet_dir = ?, torrent_dir = ? WHERE id = ?",
            .{ c.name, c.usenet_dir, c.torrent_dir, c.id },
        ) catch |err| return mapWrite(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
    }

    pub fn delete(self: CategoryRepo, id: []const u8) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        try w.db.execArgs("DELETE FROM categories WHERE id = ?", .{id});
        if (w.db.changes() == 0) return error.NotFound;
    }
};

fn readRow(st: *sqlite.Stmt, gpa: Allocator) Allocator.Error!Category {
    const id = try st.columnText(gpa, 0);
    const name = try st.columnText(gpa, 1);
    const usenet_dir = try st.columnText(gpa, 2);
    const torrent_dir = try st.columnText(gpa, 3);
    return .{ .id = id, .name = name, .usenet_dir = usenet_dir, .torrent_dir = torrent_dir };
}

/// A unique or primary key violation is a conflict (store.mapErr in Go).
fn mapWrite(db: *sqlite.Db, err: sqlite.Error) Error {
    if (err == error.Constraint and db.failure().isConflict()) return error.Conflict;
    return err;
}

// ---- tests ----

const testing = std.testing;
const migrate = @import("migrate.zig");

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
        errdefer rig.store.close();
        const w = try rig.store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
    }

    fn close(rig: *Rig) void {
        rig.store.close();
        rig.tmp.cleanup();
    }

    fn repo(rig: *Rig) CategoryRepo {
        return .{ .store = rig.store };
    }
};

fn expectEqualCategory(want: Category, got: Category) !void {
    try testing.expectEqualStrings(want.id, got.id);
    try testing.expectEqualStrings(want.name, got.name);
    try testing.expectEqualStrings(want.usenet_dir, got.usenet_dir);
    try testing.expectEqualStrings(want.torrent_dir, got.torrent_dir);
}

test "create then get round-trips" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const want: Category = .{ .id = "c1", .name = "tv", .usenet_dir = "/media/tv", .torrent_dir = "/media/torrents/tv" };

    try rig.repo().create(want);
    try expectEqualCategory(want, try rig.repo().get(arena.allocator(), "c1"));
}

test "get of an unknown ID is not found" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectError(error.NotFound, rig.repo().get(testing.allocator, "nope"));
}

test "create refuses an invalid category without writing" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectError(error.InvalidName, rig.repo().create(.{ .id = "c1", .name = "Not Valid" }));
    try testing.expectError(error.InvalidDirOverride, rig.repo().create(.{ .id = "c1", .name = "tv", .torrent_dir = "rel/dir" }));
    try testing.expectError(error.NotFound, rig.repo().get(testing.allocator, "c1"));
}

test "a taken name or ID is a conflict" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    try testing.expectError(error.Conflict, rig.repo().create(.{ .id = "c2", .name = "tv" }));
    try testing.expectError(error.Conflict, rig.repo().create(.{ .id = "c1", .name = "movies" }));
}

test "list returns every category ordered by name, or nothing" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqual(0, (try rig.repo().list(arena.allocator())).len);

    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    try rig.repo().create(.{ .id = "c2", .name = "movies", .usenet_dir = "/m" });
    const got = try rig.repo().list(arena.allocator());
    try testing.expectEqual(2, got.len);
    try expectEqualCategory(.{ .id = "c2", .name = "movies", .usenet_dir = "/m" }, got[0]);
    try expectEqualCategory(.{ .id = "c1", .name = "tv" }, got[1]);
}

test "update changes the name and the folder overrides" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    const want: Category = .{ .id = "c1", .name = "series", .usenet_dir = "/media/series", .torrent_dir = "/media/torrents/series" };

    try rig.repo().update(want);
    try expectEqualCategory(want, try rig.repo().get(arena.allocator(), "c1"));
}

test "update of an unknown ID is not found, to a taken name a conflict" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectError(error.NotFound, rig.repo().update(.{ .id = "nope", .name = "tv" }));
    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    try rig.repo().create(.{ .id = "c2", .name = "movies" });
    try testing.expectError(error.Conflict, rig.repo().update(.{ .id = "c2", .name = "tv" }));
}

test "update refuses an invalid category and keeps the row" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    try testing.expectError(error.InvalidDirOverride, rig.repo().update(.{ .id = "c1", .name = "tv", .torrent_dir = "relative/dir" }));
    try expectEqualCategory(.{ .id = "c1", .name = "tv" }, try rig.repo().get(arena.allocator(), "c1"));
}

test "delete removes the category; an unknown ID is not found" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    try rig.repo().delete("c1");
    try testing.expectError(error.NotFound, rig.repo().get(testing.allocator, "c1"));
    try testing.expectError(error.NotFound, rig.repo().delete("c1"));
}

test "delete leaves the downloads of the category without one" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.repo().create(.{ .id = "c1", .name = "tv" });
    {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.exec(
            \\INSERT INTO downloads (id, name, source_kind, category_id, state, added_at, updated_at)
            \\VALUES ('d1', 'show', 'nzb', 'c1', 'queued', 1, 1)
        );
    }
    try rig.repo().delete("c1");

    const r = try rig.store.acquireReader();
    defer r.release();
    var st = try sqlite.Stmt.prepare(r.db, "SELECT category_id FROM downloads WHERE id = 'd1'");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expect(st.columnIsNull(0));
}

test "rows migrated from the single dir_override keep it for both protocols" {
    // Migration 9 (shared with Go) splits dir_override: run up to 8, add a
    // row the old way, then apply the rest.
    var db = try sqlite.Db.open(":memory:", .{}, null);
    defer db.close();
    var diag: migrate.Diagnostic = .{};
    var upto8: std.ArrayList(migrate.File) = .empty;
    defer upto8.deinit(testing.allocator);
    for (migrate.embedded) |f| {
        const p = migrate.parseName(f.name) orelse continue;
        if (p.version < 9) try upto8.append(testing.allocator, f);
    }
    try migrate.run(testing.allocator, &db, upto8.items, 0, &diag);
    try db.exec("INSERT INTO categories (id, name, dir_override) VALUES ('c1', 'movies', '/data/usenet/complete/movies'), ('c2', 'tv', '')");
    try migrate.run(testing.allocator, &db, migrate.embedded, 0, &diag);

    var st = try sqlite.Stmt.prepare(&db, "SELECT id, usenet_dir, torrent_dir FROM categories ORDER BY id");
    defer st.finalize();
    try testing.expect(try st.step());
    const a = try st.columnText(testing.allocator, 1);
    defer testing.allocator.free(a);
    const b = try st.columnText(testing.allocator, 2);
    defer testing.allocator.free(b);
    try testing.expectEqualStrings("/data/usenet/complete/movies", a);
    try testing.expectEqualStrings("/data/usenet/complete/movies", b);
    try testing.expect(try st.step());
    const c = try st.columnText(testing.allocator, 2);
    defer testing.allocator.free(c);
    try testing.expectEqualStrings("", c);
}
