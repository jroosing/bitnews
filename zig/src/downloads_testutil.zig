//! A temp database with the real migrations for the tests of the downloads
//! group. Only the tests import this file.
const std = @import("std");
const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const repo_mod = @import("downloads_repo.zig");

/// 2026-03-01T12:00:00Z in unix milliseconds, the base time of the tests.
pub const epoch: i64 = 1772366400000;

pub const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    repo: repo_mod.DownloadRepo,
    arena_state: std.heap.ArenaAllocator,

    /// A fresh database with every migration applied.
    pub fn open(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
        try rig.openAt(path, false);
    }

    /// The Go-written golden database, copied to the temp dir first.
    pub fn openGolden(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        errdefer rig.tmp.cleanup();
        try std.Io.Dir.cwd().copyFile("testdata/compat/bitnews.db", rig.tmp.dir, "bitnews.db", testing.io, .{});
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/bitnews.db", .{buf[0..n]});
        try rig.openAt(path, true);
    }

    fn openAt(rig: *Rig, path: []const u8, existing: bool) !void {
        _ = existing;
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        errdefer rig.store.close();
        {
            const w = try rig.store.acquireWriter();
            defer w.release();
            var diag: migrate.Diagnostic = .{};
            try migrate.run(testing.allocator, w.db, migrate.embedded, 99, &diag);
        }
        rig.repo = .{ .store = rig.store };
        rig.arena_state = .init(testing.allocator);
    }

    pub fn close(rig: *Rig) void {
        rig.arena_state.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }

    /// Memory that lives until the end of the test.
    pub fn arena(rig: *Rig) std.mem.Allocator {
        return rig.arena_state.allocator();
    }

    pub fn addCategory(rig: *Rig, id: []const u8, name: []const u8) !void {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.execArgs("INSERT INTO categories (id, name) VALUES (?, ?)", .{ id, name });
    }

    /// How many Downloads match a SQL condition.
    pub fn countWhere(rig: *Rig, cond: []const u8) !usize {
        const r = try rig.store.acquireReader();
        defer r.release();
        const sql = try std.fmt.allocPrint(rig.arena(), "SELECT COUNT(*) FROM downloads WHERE {s}", .{cond});
        var st = try sqlite.Stmt.prepare(r.db, sql);
        defer st.finalize();
        try testing.expect(try st.step());
        return @intCast(st.columnInt(0));
    }
};
