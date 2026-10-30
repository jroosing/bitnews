//! Where a fetch keeps its state: the resume blob, the fetch report and the
//! un-deferred volumes, in the tables of internal/store/repo_usenet_state.go
//! (usenet_progress, usenet_reports, usenet_undeferred), and the in-memory
//! versions the runner falls back on when none is given (a pause then continues
//! where it left off, a restart does not).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const seams = @import("runner_seams.zig");
const blob_mod = @import("runner_blob.zig");

const FetchDetail = seams.FetchDetail;
const VolumeRow = seams.VolumeRow;

pub const Error = error{
    /// A blob above `runner_blob.max_bytes` is refused and the previous one stays.
    BlobTooLarge,
} || sqlite.Error || Io.Cancelable;

/// The tables of the Go daemon on the daemon's database. `store` must outlive
/// the value.
pub const Sqlite = struct {
    store: *sqlite.Store,

    pub fn progress(s: *Sqlite) seams.ProgressStore {
        return .{ .ctx = s, .load_fn = loadBlob, .save_fn = saveBlob, .delete_fn = deleteBlob };
    }

    pub fn reports(s: *Sqlite) seams.ReportStore {
        return .{ .ctx = s, .save_fn = saveReport, .load_fn = loadReport };
    }

    pub fn volumes(s: *Sqlite) seams.VolumeStore {
        return .{ .ctx = s, .list_fn = listVolumes, .want_fn = wantVolumes, .mark_fetched_fn = markVolumes };
    }

    fn of(ctx: *anyopaque) *Sqlite {
        return @ptrCast(@alignCast(ctx));
    }

    fn nowMs(s: *Sqlite) i64 {
        return Io.Clock.now(.real, s.store.io).toMilliseconds();
    }

    fn loadBlob(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?[]const u8 {
        const s = of(ctx);
        const r = try s.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT blob FROM usenet_progress WHERE download_id = ?");
        defer st.finalize();
        try st.bindAll(.{id});
        if (!try st.step()) return null;
        return try st.columnBlob(arena, 0);
    }

    fn saveBlob(ctx: *anyopaque, id: []const u8, blob: []const u8) anyerror!void {
        const s = of(ctx);
        if (blob.len > blob_mod.max_bytes) return error.BlobTooLarge;
        const w = try s.store.acquireWriter();
        defer w.release();
        try w.db.execArgs(
            "INSERT INTO usenet_progress (download_id, blob, updated_at) VALUES (?, ?, ?) " ++
                "ON CONFLICT (download_id) DO UPDATE SET blob = excluded.blob, updated_at = excluded.updated_at",
            .{ id, sqlite.Blob{ .bytes = blob }, s.nowMs() },
        );
    }

    fn deleteBlob(ctx: *anyopaque, id: []const u8) anyerror!void {
        const w = try of(ctx).store.acquireWriter();
        defer w.release();
        try w.db.execArgs("DELETE FROM usenet_progress WHERE download_id = ?", .{id});
    }

    fn saveReport(ctx: *anyopaque, id: []const u8, d: FetchDetail) anyerror!void {
        const s = of(ctx);
        const w = try s.store.acquireWriter();
        defer w.release();
        try w.db.execArgs(
            "INSERT INTO usenet_reports (download_id, missing_articles, missing_bytes, damaged_articles, " ++
                "damaged_bytes, missing_files, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) " ++
                "ON CONFLICT (download_id) DO UPDATE SET " ++
                "missing_articles = excluded.missing_articles, missing_bytes = excluded.missing_bytes, " ++
                "damaged_articles = excluded.damaged_articles, damaged_bytes = excluded.damaged_bytes, " ++
                "missing_files = excluded.missing_files, updated_at = excluded.updated_at",
            .{ id, d.missing_articles, d.missing_bytes, d.damaged_articles, d.damaged_bytes, d.missing_files, s.nowMs() },
        );
    }

    fn loadReport(ctx: *anyopaque, id: []const u8) anyerror!?FetchDetail {
        const r = try of(ctx).store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT missing_articles, missing_bytes, damaged_articles, damaged_bytes, missing_files " ++
            "FROM usenet_reports WHERE download_id = ?");
        defer st.finalize();
        try st.bindAll(.{id});
        if (!try st.step()) return null;
        return .{
            .missing_articles = st.columnInt(0),
            .missing_bytes = st.columnInt(1),
            .damaged_articles = st.columnInt(2),
            .damaged_bytes = st.columnInt(3),
            .missing_files = st.columnInt(4),
        };
    }

    fn listVolumes(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror![]const VolumeRow {
        const r = try of(ctx).store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT file_index, fetched FROM usenet_undeferred WHERE download_id = ? ORDER BY file_index");
        defer st.finalize();
        try st.bindAll(.{id});
        var out: std.ArrayList(VolumeRow) = .empty;
        while (try st.step()) {
            const idx = std.math.cast(u32, st.columnInt(0)) orelse continue;
            try out.append(arena, .{ .file_index = idx, .fetched = st.columnInt(1) != 0 });
        }
        return out.items;
    }

    fn wantVolumes(ctx: *anyopaque, id: []const u8, idx: []const u32) anyerror!void {
        if (idx.len == 0) return;
        const w = try of(ctx).store.acquireWriter();
        defer w.release();
        var tx = try w.db.begin(.immediate);
        defer tx.deinit();
        for (idx) |i| {
            try w.db.execArgs(
                "INSERT INTO usenet_undeferred (download_id, file_index, fetched) VALUES (?, ?, 0) " ++
                    "ON CONFLICT (download_id, file_index) DO NOTHING",
                .{ id, i },
            );
        }
        try tx.commit();
    }

    fn markVolumes(ctx: *anyopaque, id: []const u8, idx: []const u32) anyerror!void {
        if (idx.len == 0) return;
        const w = try of(ctx).store.acquireWriter();
        defer w.release();
        var tx = try w.db.begin(.immediate);
        defer tx.deinit();
        for (idx) |i| {
            try w.db.execArgs("UPDATE usenet_undeferred SET fetched = 1 WHERE download_id = ? AND file_index = ?", .{ id, i });
        }
        try tx.commit();
    }
};

/// The in-memory state. One value serves the three seams; it outlives the
/// runner that uses it, which is how the tests play a restart.
pub const Mem = struct {
    gpa: Allocator,
    io: Io,
    mu: Io.Mutex = .init,
    blobs: std.StringHashMapUnmanaged([]u8) = .empty,
    details: std.StringHashMapUnmanaged(FetchDetail) = .empty,
    /// Download ID -> rows, ascending by file index.
    rows: std.StringHashMapUnmanaged(std.ArrayList(VolumeRow)) = .empty,
    saves: usize = 0,
    /// While set, saves and deletes of blobs vanish: it plays a crash between
    /// two periodic saves.
    frozen: bool = false,

    pub fn init(gpa: Allocator, io: Io) Mem {
        return .{ .gpa = gpa, .io = io };
    }

    pub fn deinit(m: *Mem) void {
        var b = m.blobs.iterator();
        while (b.next()) |e| {
            m.gpa.free(e.key_ptr.*);
            m.gpa.free(e.value_ptr.*);
        }
        m.blobs.deinit(m.gpa);
        var d = m.details.keyIterator();
        while (d.next()) |k| m.gpa.free(k.*);
        m.details.deinit(m.gpa);
        var r = m.rows.iterator();
        while (r.next()) |e| {
            m.gpa.free(e.key_ptr.*);
            e.value_ptr.deinit(m.gpa);
        }
        m.rows.deinit(m.gpa);
    }

    pub fn progress(m: *Mem) seams.ProgressStore {
        return .{ .ctx = m, .load_fn = loadBlob, .save_fn = saveBlob, .delete_fn = deleteBlob };
    }

    pub fn reports(m: *Mem) seams.ReportStore {
        return .{ .ctx = m, .save_fn = saveReport, .load_fn = loadReport };
    }

    pub fn volumes(m: *Mem) seams.VolumeStore {
        return .{ .ctx = m, .list_fn = listVolumes, .want_fn = wantVolumes, .mark_fetched_fn = markVolumes };
    }

    fn of(ctx: *anyopaque) *Mem {
        return @ptrCast(@alignCast(ctx));
    }

    /// How many blobs were saved so far.
    pub fn saveCount(m: *Mem) usize {
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        return m.saves;
    }

    pub fn hasBlob(m: *Mem, id: []const u8) bool {
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        return m.blobs.contains(id);
    }

    pub fn setFrozen(m: *Mem, frozen: bool) void {
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        m.frozen = frozen;
    }

    fn loadBlob(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!?[]const u8 {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        const b = m.blobs.get(id) orelse return null;
        return try arena.dupe(u8, b);
    }

    fn saveBlob(ctx: *anyopaque, id: []const u8, blob: []const u8) anyerror!void {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        if (m.frozen) return;
        const copy = try m.gpa.dupe(u8, blob);
        errdefer m.gpa.free(copy);
        m.saves += 1;
        if (m.blobs.getPtr(id)) |old| {
            m.gpa.free(old.*);
            old.* = copy;
            return;
        }
        const key = try m.gpa.dupe(u8, id);
        errdefer m.gpa.free(key);
        try m.blobs.putNoClobber(m.gpa, key, copy);
    }

    fn deleteBlob(ctx: *anyopaque, id: []const u8) anyerror!void {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        if (m.frozen) return;
        if (m.blobs.fetchRemove(id)) |kv| {
            m.gpa.free(kv.key);
            m.gpa.free(kv.value);
        }
    }

    fn saveReport(ctx: *anyopaque, id: []const u8, d: FetchDetail) anyerror!void {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        if (m.details.getPtr(id)) |p| {
            p.* = d;
            return;
        }
        const key = try m.gpa.dupe(u8, id);
        errdefer m.gpa.free(key);
        try m.details.putNoClobber(m.gpa, key, d);
    }

    fn loadReport(ctx: *anyopaque, id: []const u8) anyerror!?FetchDetail {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        return m.details.get(id);
    }

    fn listVolumes(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror![]const VolumeRow {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        const list = m.rows.get(id) orelse return &.{};
        return arena.dupe(VolumeRow, list.items);
    }

    fn wantVolumes(ctx: *anyopaque, id: []const u8, idx: []const u32) anyerror!void {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        const list = try m.rowsOf(id);
        for (idx) |i| {
            const at = std.sort.lowerBound(VolumeRow, list.items, i, struct {
                fn order(k: u32, r: VolumeRow) std.math.Order {
                    return std.math.order(k, r.file_index);
                }
            }.order);
            if (at < list.items.len and list.items[at].file_index == i) continue;
            try list.insert(m.gpa, at, .{ .file_index = i, .fetched = false });
        }
    }

    fn markVolumes(ctx: *anyopaque, id: []const u8, idx: []const u32) anyerror!void {
        const m = of(ctx);
        m.mu.lockUncancelable(m.io);
        defer m.mu.unlock(m.io);
        const list = m.rows.getPtr(id) orelse return;
        for (list.items) |*r| {
            if (std.mem.indexOfScalar(u32, idx, r.file_index) != null) r.fetched = true;
        }
    }

    fn rowsOf(m: *Mem, id: []const u8) !*std.ArrayList(VolumeRow) {
        if (m.rows.getPtr(id)) |p| return p;
        const key = try m.gpa.dupe(u8, id);
        errdefer m.gpa.free(key);
        try m.rows.putNoClobber(m.gpa, key, .empty);
        return m.rows.getPtr(id).?;
    }
};

const testing = std.testing;
const tu = @import("downloads_testutil.zig");

test "the in-memory stores keep blobs, reports and volumes" {
    var m: Mem = .init(testing.allocator, testing.io);
    defer m.deinit();
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();

    const p = m.progress();
    try testing.expectEqual(null, try p.load(a, "d1"));
    try p.save("d1", "one");
    try p.save("d1", "two");
    try testing.expectEqualStrings("two", (try p.load(a, "d1")).?);
    m.setFrozen(true);
    try p.delete("d1");
    m.setFrozen(false);
    try testing.expect(m.hasBlob("d1"));
    try p.delete("d1");
    try p.delete("d1");
    try testing.expect(!m.hasBlob("d1"));

    const r = m.reports();
    try testing.expectEqual(null, try r.load("d1"));
    try r.save("d1", .{ .missing_articles = 3, .missing_bytes = 9 });
    try testing.expectEqual(@as(i64, 3), (try r.load("d1")).?.missing_articles);

    const v = m.volumes();
    try testing.expectEqual(0, (try v.list(a, "d1")).len);
    try v.want("d1", &.{ 3, 1 });
    try v.want("d1", &.{ 2, 1 });
    try v.markFetched("d1", &.{ 1, 7 });
    const rows = try v.list(a, "d1");
    try testing.expectEqual(3, rows.len);
    try testing.expectEqual(@as(u32, 1), rows[0].file_index);
    try testing.expect(rows[0].fetched);
    try testing.expect(!rows[1].fetched and !rows[2].fetched);
}

test "the SQLite stores read and write the Go tables" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.repo.create(.{ .id = "d1", .name = "n", .source_kind = .nzb, .state = .queued, .added_at = tu.epoch, .updated_at = tu.epoch });
    var s: Sqlite = .{ .store = rig.store };

    const p = s.progress();
    try testing.expectEqual(null, try p.load(a, "d1"));
    try p.save("d1", "blob-1");
    try p.save("d1", "blob-2");
    try testing.expectEqualStrings("blob-2", (try p.load(a, "d1")).?);
    try p.save("d1", "");
    try testing.expectEqual(0, (try p.load(a, "d1")).?.len);
    const big = try testing.allocator.alloc(u8, blob_mod.max_bytes + 1);
    defer testing.allocator.free(big);
    try testing.expectError(error.BlobTooLarge, p.save("d1", big));
    try p.delete("d1");
    try p.delete("d1");
    try testing.expectEqual(null, try p.load(a, "d1"));

    const r = s.reports();
    try testing.expectEqual(null, try r.load("d1"));
    try r.save("d1", .{ .missing_articles = 1, .missing_bytes = 2, .damaged_articles = 3, .damaged_bytes = 4, .missing_files = 5 });
    try r.save("d1", .{ .missing_articles = 6, .missing_bytes = 2, .damaged_articles = 3, .damaged_bytes = 4, .missing_files = 5 });
    const got = (try r.load("d1")).?;
    try testing.expectEqual(@as(i64, 6), got.missing_articles);
    try testing.expectEqual(@as(i64, 5), got.missing_files);

    const v = s.volumes();
    try testing.expectEqual(0, (try v.list(a, "d1")).len);
    try v.want("d1", &.{ 3, 1 });
    try v.markFetched("d1", &.{1});
    try v.want("d1", &.{ 1, 2 }); // a recorded volume keeps its flag
    const rows = try v.list(a, "d1");
    try testing.expectEqual(3, rows.len);
    try testing.expect(rows[0].file_index == 1 and rows[0].fetched);
    try testing.expect(rows[1].file_index == 2 and !rows[1].fetched);
    try testing.expect(rows[2].file_index == 3 and !rows[2].fetched);

    // Everything goes away with its Download.
    try rig.repo.delete("d1");
    try testing.expectEqual(0, (try v.list(a, "d1")).len);
    try testing.expectEqual(null, try r.load("d1"));
}
