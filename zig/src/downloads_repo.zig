//! The downloads table (internal/store/repo_downloads.go, core.DownloadRepo).
//!
//! Stored form, as Go writes it: times are unix milliseconds; fetched_at and
//! ready_at are NULL until set (0 in a `Download`); category_id and
//! failure_reason are NULL when empty; the other strings are never NULL.
//!
//! Strings of a returned `Download` are allocated in the arena the caller
//! passes, so they live as long as the request or query result does.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const dl = @import("download.zig");

/// Paging limits: a query with no limit gets `default_list_limit` rows and no
/// query gets more than `max_list_limit`.
pub const default_list_limit = 50;
pub const max_list_limit = 200;
/// The page size of `listAll`, and of a history page without a limit.
const list_all_page = 200;
const default_history_limit = 25;
/// Longest cursor looked at; the ones this repo hands out are about 100 bytes.
const max_cursor_len = 1024;

pub const Error = error{
    /// No row matches the ID (Go: core.ErrNotFound).
    NotFound,
    /// The ID is taken, or the state is one the caller excluded (core.ErrConflict).
    Conflict,
    /// `Query.after` is not a cursor this repository handed out.
    InvalidCursor,
    NegativeBytes,
} || dl.ChangeError || sqlite.Error || Io.Cancelable;

/// Selects one page of Downloads, ordered by added_at, then ID. Unset filters
/// match everything.
pub const Query = struct {
    state: ?dl.State = null,
    category_id: []const u8 = "",
    source_kind: ?dl.SourceKind = null,
    /// The cursor of the previous page; empty starts at the first page.
    after: []const u8 = "",
    /// Caps the page size; the repository applies its own maximum.
    limit: i64 = 0,
};

pub const Page = struct {
    items: []dl.Download,
    /// The cursor of the next page, null on the last page.
    next: ?[]const u8 = null,
};

/// Selects one page of the finished Downloads (ready and failed), newest first
/// by when they finished.
pub const HistoryQuery = struct {
    /// ready or failed; null means both.
    state: ?dl.State = null,
    category_id: []const u8 = "",
    source_kind: ?dl.SourceKind = null,
    /// `before_at` (unix ms) and `before_id` continue after the last row of the
    /// previous page; an empty `before_id` starts at the newest.
    before_at: i64 = 0,
    before_id: []const u8 = "",
    /// Zero means 25.
    limit: i64 = 0,
};

pub const HistoryPage = struct {
    items: []dl.Download,
    /// Every Download the query matches, and the ones that come before the page.
    total: i64,
    newer: i64,
};

/// When a Download finished, as the history orders it (core.FinishedAt).
pub fn finishedAt(d: dl.Download) i64 {
    return if (d.state == .ready and d.ready_at != 0) d.ready_at else d.updated_at;
}

const columns =
    "id, name, source_kind, category_id, state, priority, queue_position, " ++
    "paused, failure_reason, failure_detail, total_bytes, done_bytes, " ++
    "added_at, fetched_at, ready_at, updated_at, output_path";

pub const DownloadRepo = struct {
    store: *sqlite.Store,

    pub fn create(self: DownloadRepo, d: dl.Download) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs("INSERT INTO downloads (" ++ columns ++ ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", .{
            d.id,
            d.name,
            @tagName(d.source_kind),
            nullText(d.category_id),
            @tagName(d.state),
            @tagName(d.priority),
            d.queue_position,
            d.paused,
            if (d.failure_reason) |r| @tagName(r) else null,
            d.failure_detail,
            d.total_bytes,
            d.done_bytes,
            d.added_at,
            nullMs(d.fetched_at),
            nullMs(d.ready_at),
            d.updated_at,
            d.output_path,
        }) catch |err| return mapWrite(w.db, err);
    }

    pub fn get(self: DownloadRepo, arena: Allocator, id: []const u8) Error!dl.Download {
        const r = try self.store.acquireReader();
        defer r.release();
        return getOn(r.db, arena, id);
    }

    /// Like `get`, but null when no Download has the ID.
    pub fn find(self: DownloadRepo, arena: Allocator, id: []const u8) Error!?dl.Download {
        return self.get(arena, id) catch |err| switch (err) {
            error.NotFound => null,
            else => err,
        };
    }

    /// One page, ordered by (added_at, id), and the cursor of the next one. The
    /// cursor is keyset based, so Downloads inserted between two calls never
    /// duplicate or skip one that was already there.
    pub fn list(self: DownloadRepo, arena: Allocator, q: Query) Error!Page {
        var limit = q.limit;
        if (limit <= 0) limit = default_list_limit;
        limit = @min(limit, max_list_limit);

        var f: Filter = .{};
        if (q.state) |s| try f.add(arena, "state = ?", .{ .text = @tagName(s) });
        if (q.category_id.len > 0) try f.add(arena, "category_id = ?", .{ .text = q.category_id });
        if (q.source_kind) |k| try f.add(arena, "source_kind = ?", .{ .text = @tagName(k) });
        if (q.after.len > 0) {
            const c = try decodeCursor(arena, q.after);
            try f.add(arena, "(added_at, id) > (?, ?)", .{ .int = c.added_at });
            try f.binds.append(arena, .{ .text = c.id });
        }
        // One extra row tells whether another page follows.
        try f.binds.append(arena, .{ .int = limit + 1 });
        const sql = try std.fmt.allocPrint(arena, "SELECT " ++ columns ++ " FROM downloads{s} ORDER BY added_at, id LIMIT ?", .{f.where()});

        const r = try self.store.acquireReader();
        defer r.release();
        var items = try queryRows(r.db, arena, sql, f.binds.items);
        const want: usize = @intCast(limit);
        if (items.len <= want) return .{ .items = items };
        items = items[0..want];
        const last = items[want - 1];
        return .{ .items = items, .next = try encodeCursor(arena, last.added_at, last.id) };
    }

    /// Every Download `q` matches, page after page (`q.after` is ignored). For
    /// callers that need the whole set, such as a Queue.
    pub fn listAll(self: DownloadRepo, arena: Allocator, q: Query) Error![]dl.Download {
        var query = q;
        query.after = "";
        if (query.limit <= 0) query.limit = list_all_page;
        var out: std.ArrayList(dl.Download) = .empty;
        while (true) {
            const page = try self.list(arena, query);
            try out.appendSlice(arena, page.items);
            query.after = page.next orelse return out.items;
        }
    }

    /// The Downloads that use the Category (for the Categories service).
    pub fn listInCategory(self: DownloadRepo, arena: Allocator, category_id: []const u8) Error![]dl.Download {
        // An empty ID would match every Download.
        if (category_id.len == 0) return &.{};
        return self.listAll(arena, .{ .category_id = category_id });
    }

    /// The same under the name the Categories service uses.
    pub const list_in_category = listInCategory;

    /// Stores every field except id, added_at (both immutable, so paging on
    /// (added_at, id) stays stable), state and the fields only a state change
    /// sets (fetched_at, ready_at, output_path, failure_reason, failure_detail).
    pub fn update(self: DownloadRepo, d: dl.Download) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            \\UPDATE downloads SET name = ?, source_kind = ?, category_id = ?, priority = ?,
            \\    queue_position = ?, paused = ?, total_bytes = ?, done_bytes = ?, updated_at = ?
            \\WHERE id = ?
        , .{
            d.name,           @tagName(d.source_kind), nullText(d.category_id), @tagName(d.priority),
            d.queue_position, d.paused,                d.total_bytes,           d.done_bytes,
            d.updated_at,     d.id,
        }) catch |err| return mapWrite(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
    }

    /// Loads the Download, applies `c` with `download.changeState` and stores
    /// the result, in one write transaction. An illegal move leaves the row
    /// untouched and returns the error of changeState. The result is in `arena`.
    pub fn updateState(self: DownloadRepo, arena: Allocator, id: []const u8, c: dl.StateChange) Error!dl.Download {
        const w = try self.store.acquireWriter();
        defer w.release();
        var tx = try w.db.begin(.immediate);
        defer tx.deinit();
        var d = try getOn(w.db, arena, id);
        var change = c;
        // The change may point at the caller's memory; the result must not.
        change.output_path = try arena.dupe(u8, c.output_path);
        change.failure_detail = try arena.dupe(u8, c.failure_detail);
        try dl.changeState(&d, change);
        w.db.execArgs(
            \\UPDATE downloads SET state = ?, failure_reason = ?, failure_detail = ?,
            \\    fetched_at = ?, ready_at = ?, output_path = ?, updated_at = ?
            \\WHERE id = ?
        , .{
            @tagName(d.state),    if (d.failure_reason) |r| @tagName(r) else null, d.failure_detail,
            nullMs(d.fetched_at), nullMs(d.ready_at),                              d.output_path,
            d.updated_at,         d.id,
        }) catch |err| return mapWrite(w.db, err);
        try tx.commit();
        return d;
    }

    /// Stores done_bytes, total_bytes (when `total_bytes` is positive) and
    /// updated_at in one statement and nothing else, so an engine reporting
    /// progress cannot overwrite a concurrent edit of the priority, category
    /// or paused flag the way a get followed by `update` would.
    pub fn updateProgress(self: DownloadRepo, id: []const u8, done_bytes: i64, total_bytes: i64) Error!void {
        if (done_bytes < 0) return error.NegativeBytes;
        const now = Io.Clock.now(.real, self.store.io).toMilliseconds();
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            \\UPDATE downloads SET done_bytes = ?, updated_at = ?,
            \\    total_bytes = CASE WHEN ? > 0 THEN ? ELSE total_bytes END
            \\WHERE id = ?
        , .{ done_bytes, now, total_bytes, total_bytes, id }) catch |err| return mapWrite(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
    }

    /// Stores the name (when it is not empty) and total_bytes (when positive)
    /// and refreshes updated_at; a magnet Download learns both when its
    /// metadata arrives. Writes nothing else.
    pub fn updateDisplay(self: DownloadRepo, id: []const u8, name: []const u8, total_bytes: i64) Error!void {
        const now = Io.Clock.now(.real, self.store.io).toMilliseconds();
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            \\UPDATE downloads SET updated_at = ?,
            \\    name = CASE WHEN ? <> '' THEN ? ELSE name END,
            \\    total_bytes = CASE WHEN ? > 0 THEN ? ELSE total_bytes END
            \\WHERE id = ?
        , .{ now, name, name, total_bytes, total_bytes, id }) catch |err| return mapWrite(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
    }

    pub fn delete(self: DownloadRepo, id: []const u8) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs("DELETE FROM downloads WHERE id = ?", .{id}) catch |err| return mapWrite(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
    }

    /// Deletes the Download unless its state is one of `states` (Conflict),
    /// checked and deleted in one transaction so a concurrent state change
    /// cannot slip in between.
    pub fn deleteUnlessIn(self: DownloadRepo, id: []const u8, states: []const dl.State) Error!void {
        const w = try self.store.acquireWriter();
        defer w.release();
        var tx = try w.db.begin(.immediate);
        defer tx.deinit();
        {
            var st = try sqlite.Stmt.prepare(w.db, "SELECT state FROM downloads WHERE id = ?");
            defer st.finalize();
            try st.bindAll(.{id});
            if (!try st.step()) return error.NotFound;
            var buf: [32]u8 = undefined;
            const text = columnTextBuf(&st, 0, &buf);
            if (std.meta.stringToEnum(dl.State, text)) |s| {
                if (std.mem.indexOfScalar(dl.State, states, s) != null) return error.Conflict;
            }
        }
        w.db.execArgs("DELETE FROM downloads WHERE id = ?", .{id}) catch |err| return mapWrite(w.db, err);
        if (w.db.changes() == 0) return error.NotFound;
        try tx.commit();
    }

    /// One page of the finished Downloads, newest first (ready_at for a ready
    /// one, else updated_at), with the counts a summary needs.
    pub fn history(self: DownloadRepo, arena: Allocator, q: HistoryQuery) Error!HistoryPage {
        var limit = q.limit;
        if (limit <= 0) limit = default_history_limit;
        limit = @min(limit, max_list_limit);

        var f: Filter = .{};
        try f.add(arena, "state IN ('ready', 'failed')", null);
        if (q.state) |s| try f.add(arena, "state = ?", .{ .text = @tagName(s) });
        if (q.category_id.len > 0) try f.add(arena, "category_id = ?", .{ .text = q.category_id });
        if (q.source_kind) |k| try f.add(arena, "source_kind = ?", .{ .text = @tagName(k) });
        const filter_binds = f.binds.items.len;

        const finished_sql = "(CASE WHEN state = 'ready' AND ready_at IS NOT NULL THEN ready_at ELSE updated_at END)";
        const r = try self.store.acquireReader();
        defer r.release();

        var page: HistoryPage = .{ .items = &.{}, .total = 0, .newer = 0 };
        page.total = try countRows(r.db, try std.fmt.allocPrint(arena, "SELECT COUNT(*) FROM downloads{s}", .{f.where()}), f.binds.items);

        if (q.before_id.len > 0) {
            // The count of the rows before the page includes the cursor row.
            const newer_sql = try std.fmt.allocPrint(
                arena,
                "SELECT COUNT(*) FROM downloads{s} AND ({s} > ? OR ({s} = ? AND id >= ?))",
                .{ f.where(), finished_sql, finished_sql },
            );
            try f.binds.appendSlice(arena, &.{ .{ .int = q.before_at }, .{ .int = q.before_at }, .{ .text = q.before_id } });
            page.newer = try countRows(r.db, newer_sql, f.binds.items);
            f.binds.items.len = filter_binds;
            try f.add(arena, try std.fmt.allocPrint(arena, "({s} < ? OR ({s} = ? AND id < ?))", .{ finished_sql, finished_sql }), .{ .int = q.before_at });
            try f.binds.appendSlice(arena, &.{ .{ .int = q.before_at }, .{ .text = q.before_id } });
        }
        try f.binds.append(arena, .{ .int = limit });
        const sql = try std.fmt.allocPrint(
            arena,
            "SELECT " ++ columns ++ " FROM downloads{s} ORDER BY {s} DESC, id DESC LIMIT ?",
            .{ f.where(), finished_sql },
        );
        page.items = try queryRows(r.db, arena, sql, f.binds.items);
        return page;
    }
};

// ---- SQL helpers ----

const Bind = union(enum) { text: []const u8, int: i64 };

/// A WHERE clause under construction: conditions joined by AND, with their
/// parameters in order.
const Filter = struct {
    conds: std.ArrayList(u8) = .empty,
    binds: std.ArrayList(Bind) = .empty,

    fn add(f: *Filter, arena: Allocator, cond: []const u8, bind: ?Bind) Allocator.Error!void {
        try f.conds.appendSlice(arena, if (f.conds.items.len == 0) " WHERE " else " AND ");
        try f.conds.appendSlice(arena, cond);
        if (bind) |b| try f.binds.append(arena, b);
    }

    fn where(f: *const Filter) []const u8 {
        return f.conds.items;
    }
};

fn bindAll(st: *sqlite.Stmt, binds: []const Bind) sqlite.Error!void {
    for (binds, 1..) |b, i| switch (b) {
        .text => |t| try st.bindText(@intCast(i), t),
        .int => |n| try st.bindInt(@intCast(i), n),
    };
}

fn queryRows(db: *sqlite.Db, arena: Allocator, sql: []const u8, binds: []const Bind) Error![]dl.Download {
    var st = try sqlite.Stmt.prepare(db, sql);
    defer st.finalize();
    try bindAll(&st, binds);
    var out: std.ArrayList(dl.Download) = .empty;
    while (try st.step()) try out.append(arena, try readRow(arena, &st));
    return out.items;
}

fn countRows(db: *sqlite.Db, sql: []const u8, binds: []const Bind) Error!i64 {
    var st = try sqlite.Stmt.prepare(db, sql);
    defer st.finalize();
    try bindAll(&st, binds);
    if (!try st.step()) return 0;
    return st.columnInt(0);
}

/// The Download with the ID, read on `db` (a reader, or the writer inside a
/// transaction).
pub fn getOn(db: *sqlite.Db, arena: Allocator, id: []const u8) Error!dl.Download {
    var st = try sqlite.Stmt.prepare(db, "SELECT " ++ columns ++ " FROM downloads WHERE id = ?");
    defer st.finalize();
    try st.bindAll(.{id});
    if (!try st.step()) return error.NotFound;
    return readRow(arena, &st);
}

fn readRow(arena: Allocator, st: *sqlite.Stmt) Error!dl.Download {
    // The CHECK constraints keep the enum columns valid; a value outside them
    // means the file was edited by hand.
    const source_kind = try enumColumn(dl.SourceKind, st, 2);
    const state = try enumColumn(dl.State, st, 4);
    const priority = try enumColumn(dl.Priority, st, 5);
    const failure_reason: ?dl.FailureReason = if (st.columnIsNull(8)) null else try enumColumn(dl.FailureReason, st, 8);
    return .{
        .id = try st.columnText(arena, 0),
        .name = try st.columnText(arena, 1),
        .source_kind = source_kind,
        .category_id = try st.columnText(arena, 3),
        .state = state,
        .priority = priority,
        .queue_position = st.columnInt(6),
        .paused = st.columnInt(7) != 0,
        .failure_reason = failure_reason,
        .failure_detail = try st.columnText(arena, 9),
        .total_bytes = st.columnInt(10),
        .done_bytes = st.columnInt(11),
        .added_at = st.columnInt(12),
        .fetched_at = st.columnInt(13),
        .ready_at = st.columnInt(14),
        .updated_at = st.columnInt(15),
        .output_path = try st.columnText(arena, 16),
    };
}

fn enumColumn(comptime E: type, st: *sqlite.Stmt, index: u32) Error!E {
    var buf: [32]u8 = undefined;
    return std.meta.stringToEnum(E, columnTextBuf(st, index, &buf)) orelse error.Corrupt;
}

/// The column's text if it fits `buf`, else an empty string (no enum name is
/// longer than the buffer, so an empty string never matches).
fn columnTextBuf(st: *sqlite.Stmt, index: u32, buf: []u8) []const u8 {
    // columnText allocates; a fixed-size allocator keeps this free of the heap.
    var fba: std.heap.FixedBufferAllocator = .init(buf);
    return st.columnText(fba.allocator(), index) catch "";
}

fn nullText(s: []const u8) ?[]const u8 {
    return if (s.len == 0) null else s;
}

fn nullMs(ms: i64) ?i64 {
    return if (ms == 0) null else ms;
}

/// A unique or primary key violation is a conflict (store.mapErr in Go).
fn mapWrite(db: *sqlite.Db, err: sqlite.Error) Error {
    if (err == error.Constraint and db.failure().isConflict()) return error.Conflict;
    return err;
}

// ---- cursors ----

const b64 = std.base64.url_safe_no_pad;

fn encodeCursor(arena: Allocator, added_at: i64, id: []const u8) Allocator.Error![]const u8 {
    const raw = try std.fmt.allocPrint(arena, "{d}:{s}", .{ added_at, id });
    const out = try arena.alloc(u8, b64.Encoder.calcSize(raw.len));
    return b64.Encoder.encode(out, raw);
}

const Cursor = struct { added_at: i64, id: []const u8 };

fn decodeCursor(arena: Allocator, cursor: []const u8) (Allocator.Error || error{InvalidCursor})!Cursor {
    if (cursor.len > max_cursor_len) return error.InvalidCursor;
    const size = b64.Decoder.calcSizeForSlice(cursor) catch return error.InvalidCursor;
    const raw = try arena.alloc(u8, size);
    b64.Decoder.decode(raw, cursor) catch return error.InvalidCursor;
    const colon = std.mem.indexOfScalar(u8, raw, ':') orelse return error.InvalidCursor;
    const id = raw[colon + 1 ..];
    if (id.len == 0) return error.InvalidCursor;
    const added_at = parseDecimal(raw[0..colon]) orelse return error.InvalidCursor;
    return .{ .added_at = added_at, .id = id };
}

/// A base 10 integer with an optional sign and nothing else, like Go's
/// strconv.ParseInt(s, 10, 64) (std.fmt.parseInt would also accept '_').
pub fn parseDecimal(s: []const u8) ?i64 {
    var digits = s;
    var negative = false;
    if (digits.len > 0 and (digits[0] == '+' or digits[0] == '-')) {
        negative = digits[0] == '-';
        digits = digits[1..];
    }
    if (digits.len == 0) return null;
    var n: i64 = 0;
    for (digits) |c| {
        if (c < '0' or c > '9') return null;
        n = std.math.mul(i64, n, 10) catch return null;
        const d: i64 = c - '0';
        n = (if (negative) std.math.sub(i64, n, d) else std.math.add(i64, n, d)) catch return null;
    }
    return n;
}

// ---- tests ----

const testing = std.testing;
const tu = @import("downloads_testutil.zig");

fn sample(id: []const u8, offset_ms: i64) dl.Download {
    return .{ .id = id, .name = "n", .source_kind = .nzb, .state = .added, .added_at = tu.epoch + offset_ms, .updated_at = tu.epoch + offset_ms };
}

fn expectIds(want: []const []const u8, got: []const dl.Download) !void {
    try testing.expectEqual(want.len, got.len);
    for (want, got) |w, g| try testing.expectEqualStrings(w, g.id);
}

fn expectEqualDownload(want: dl.Download, got: dl.Download) !void {
    try testing.expectEqualDeep(want, got);
}

test "a minimal Download round-trips and is stored with NULLs, milliseconds and 0/1" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    const d = sample("d1", 1);
    try rig.repo.create(d);
    try expectEqualDownload(d, try rig.repo.get(a, "d1"));

    var st = try sqlite.Stmt.prepare(&rig.store.writer_db, "SELECT typeof(category_id), typeof(failure_reason), typeof(fetched_at), typeof(ready_at), typeof(added_at), added_at, typeof(paused), typeof(failure_detail), typeof(output_path) FROM downloads WHERE id = 'd1'");
    defer st.finalize();
    try testing.expect(try st.step());
    const want = [_][]const u8{ "null", "null", "null", "null", "integer" };
    for (want, 0..) |w, i| {
        const got = try st.columnText(a, @intCast(i));
        try testing.expectEqualStrings(w, got);
    }
    try testing.expectEqual(d.added_at, st.columnInt(5));
    try testing.expectEqualStrings("integer", try st.columnText(a, 6));
    try testing.expectEqualStrings("text", try st.columnText(a, 7));
    try testing.expectEqualStrings("text", try st.columnText(a, 8));
}

test "every field round-trips" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.addCategory("cat1", "tv");
    const d: dl.Download = .{
        .id = "d1",
        .name = "Some.Show.S01E01",
        .source_kind = .torrent,
        .category_id = "cat1",
        .state = .failed,
        .priority = .force,
        .queue_position = 7,
        .paused = true,
        .failure_reason = .disk_full,
        .failure_detail = "no space left",
        .total_bytes = 1 << 40,
        .done_bytes = 12345,
        .added_at = tu.epoch,
        .fetched_at = tu.epoch + 60_000,
        .ready_at = tu.epoch + 120_000,
        .updated_at = tu.epoch + 180_000,
        .output_path = "/data/complete/tv/Some.Show",
    };
    try rig.repo.create(d);
    try expectEqualDownload(d, try rig.repo.get(a, "d1"));
}

test "get of an unknown ID is not found, create of a used ID is a conflict" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try testing.expectError(error.NotFound, rig.repo.get(rig.arena(), "nope"));
    try rig.repo.create(sample("d1", 0));
    try testing.expectError(error.Conflict, rig.repo.create(sample("d1", 0)));
}

test "create with a Category that does not exist is a constraint failure, not a conflict" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    var d = sample("d1", 0);
    d.category_id = "ghost";
    try testing.expectError(error.Constraint, rig.repo.create(d));
}

test "update stores the editable fields and clears the Category" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.addCategory("cat1", "movies");
    var d = sample("d1", 0);
    try rig.repo.create(d);
    d.name = "new";
    d.category_id = "cat1";
    d.priority = .high;
    d.queue_position = 3;
    d.paused = true;
    d.total_bytes = 1000;
    d.done_bytes = 400;
    d.updated_at += 3_600_000;
    try rig.repo.update(d);
    try expectEqualDownload(d, try rig.repo.get(a, "d1"));

    d.category_id = "";
    try rig.repo.update(d);
    try testing.expectEqualStrings("", (try rig.repo.get(a, "d1")).category_id);
    try testing.expectEqual(@as(usize, 1), try rig.countWhere("category_id IS NULL"));
}

test "update ignores the state and the fields only a state change sets" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const d = sample("d1", 0);
    try rig.repo.create(d);
    var tampered = d;
    tampered.state = .ready;
    tampered.fetched_at = tu.epoch + 60_000;
    tampered.ready_at = tu.epoch + 60_000;
    tampered.output_path = "/elsewhere";
    tampered.failure_reason = .internal;
    tampered.failure_detail = "boom";
    tampered.added_at = 5;
    try rig.repo.update(tampered);
    try expectEqualDownload(d, try rig.repo.get(rig.arena(), "d1"));
    try testing.expectError(error.NotFound, rig.repo.update(sample("ghost", 0)));
}

test "updateState applies the lifecycle and persists it" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    var d = sample("d1", 0);
    d.state = .post_processing;
    try rig.repo.create(d);
    const at = tu.epoch + 3_600_000;

    var out = try testing.allocator.dupe(u8, "/data/complete/x");
    const got = try rig.repo.updateState(a, "d1", .{ .to = .ready, .at = at, .output_path = out });
    // The result does not point at the caller's memory.
    testing.allocator.free(out);
    out = &.{};
    var want = d;
    try dl.changeState(&want, .{ .to = .ready, .at = at, .output_path = "/data/complete/x" });
    try expectEqualDownload(want, got);
    try expectEqualDownload(want, try rig.repo.get(a, "d1"));
}

test "updateState persists a failure" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    var d = sample("d1", 0);
    d.state = .fetching;
    try rig.repo.create(d);
    _ = try rig.repo.updateState(a, "d1", .{ .to = .failed, .at = tu.epoch + 60_000, .failure_reason = .missing_articles, .failure_detail = "12 missing" });
    const stored = try rig.repo.get(a, "d1");
    try testing.expectEqual(dl.State.failed, stored.state);
    try testing.expectEqual(dl.FailureReason.missing_articles, stored.failure_reason.?);
    try testing.expectEqualStrings("12 missing", stored.failure_detail);
}

test "updateState refuses bad moves and leaves the row untouched" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    const d = sample("d1", 0);
    try rig.repo.create(d);
    try testing.expectError(error.IllegalTransition, rig.repo.updateState(a, "d1", .{ .to = .ready, .at = 1, .output_path = "/out" }));
    try expectEqualDownload(d, try rig.repo.get(a, "d1"));

    var p = sample("d2", 0);
    p.state = .post_processing;
    try rig.repo.create(p);
    try testing.expectError(error.InvalidStateChange, rig.repo.updateState(a, "d2", .{ .to = .ready, .at = 1 }));
    try expectEqualDownload(p, try rig.repo.get(a, "d2"));

    try testing.expectError(error.NotFound, rig.repo.updateState(a, "ghost", .{ .to = .queued, .at = 1 }));
}

test "delete removes the row, an unknown ID is not found" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.repo.create(sample("d1", 0));
    try rig.repo.delete("d1");
    try testing.expectError(error.NotFound, rig.repo.get(rig.arena(), "d1"));
    try testing.expectError(error.NotFound, rig.repo.delete("d1"));
}

test "deleteUnlessIn keeps a Download in one of the states" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    const busy = [_]dl.State{ .fetched, .post_processing };
    try rig.repo.create(sample("d1", 0));
    try rig.repo.deleteUnlessIn("d1", &busy);
    try testing.expectError(error.NotFound, rig.repo.get(a, "d1"));

    var p = sample("d2", 0);
    p.state = .post_processing;
    try rig.repo.create(p);
    try testing.expectError(error.Conflict, rig.repo.deleteUnlessIn("d2", &busy));
    _ = try rig.repo.get(a, "d2");
    try testing.expectError(error.NotFound, rig.repo.deleteUnlessIn("ghost", &busy));
}

test "deleting a Category clears it from its Downloads" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try rig.addCategory("cat1", "tv");
    var d = sample("d1", 0);
    d.category_id = "cat1";
    try rig.repo.create(d);
    {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.exec("DELETE FROM categories WHERE id = 'cat1'");
    }
    try testing.expectEqualStrings("", (try rig.repo.get(rig.arena(), "d1")).category_id);
}

test "list orders by added_at then ID" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try testing.expectEqual(@as(usize, 0), (try rig.repo.list(a, .{})).items.len);
    try rig.repo.create(sample("late", 3_600_000));
    try rig.repo.create(sample("bbb", 0));
    try rig.repo.create(sample("aaa", 0));
    const page = try rig.repo.list(a, .{});
    try expectIds(&.{ "aaa", "bbb", "late" }, page.items);
    try testing.expect(page.next == null);
}

test "list filters by state, category and source kind" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.addCategory("cat1", "tv");
    try rig.repo.create(sample("plain", 0));
    var queued = sample("queued", 1000);
    queued.state = .queued;
    try rig.repo.create(queued);
    var in_cat = sample("in-cat", 2000);
    in_cat.category_id = "cat1";
    try rig.repo.create(in_cat);
    var torrent = sample("torrent", 3000);
    torrent.source_kind = .torrent;
    try rig.repo.create(torrent);

    try expectIds(&.{"queued"}, (try rig.repo.list(a, .{ .state = .queued })).items);
    try expectIds(&.{"in-cat"}, (try rig.repo.list(a, .{ .category_id = "cat1" })).items);
    try expectIds(&.{"torrent"}, (try rig.repo.list(a, .{ .source_kind = .torrent })).items);
    try expectIds(&.{ "plain", "in-cat" }, (try rig.repo.list(a, .{ .state = .added, .source_kind = .nzb })).items);
    try expectIds(&.{}, (try rig.repo.list(a, .{ .state = .ready })).items);
}

test "list pages with a cursor and ends without an empty page" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    for (0..5) |i| try rig.repo.create(sample(try std.fmt.allocPrint(a, "d{d}", .{i}), @as(i64, @intCast(i)) * 1000));

    var got: std.ArrayList([]const u8) = .empty;
    var after: []const u8 = "";
    var pages: usize = 0;
    while (true) {
        const page = try rig.repo.list(a, .{ .after = after, .limit = 2 });
        for (page.items) |d| try got.append(a, d.id);
        pages += 1;
        after = page.next orelse break;
    }
    try testing.expectEqual(@as(usize, 3), pages);
    try testing.expectEqual(@as(usize, 5), got.items.len);
    for (got.items, 0..) |id, i| try testing.expectEqualStrings(try std.fmt.allocPrint(a, "d{d}", .{i}), id);

    // An exact multiple of the limit: the last full page has no cursor.
    _ = try rig.repo.list(a, .{ .limit = 2 });
    try rig.repo.delete("d4");
    const first = try rig.repo.list(a, .{ .limit = 2 });
    const second = try rig.repo.list(a, .{ .limit = 2, .after = first.next.? });
    try testing.expectEqual(@as(usize, 2), second.items.len);
    try testing.expect(second.next == null);
}

test "paging keeps the filters" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    for (0..6) |i| {
        var d = sample(try std.fmt.allocPrint(a, "d{d}", .{i}), @as(i64, @intCast(i)) * 1000);
        if (i % 2 == 0) d.source_kind = .torrent;
        try rig.repo.create(d);
    }
    const first = try rig.repo.list(a, .{ .source_kind = .torrent, .limit = 2 });
    const second = try rig.repo.list(a, .{ .source_kind = .torrent, .limit = 2, .after = first.next.? });
    try expectIds(&.{ "d0", "d2" }, first.items);
    try expectIds(&.{"d4"}, second.items);
    try testing.expect(second.next == null);
}

test "the cursor is stable under inserts between pages" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    for (0..6) |i| try rig.repo.create(sample(try std.fmt.allocPrint(a, "e{d}", .{i}), (@as(i64, @intCast(i)) + 1) * 1000));
    const first = try rig.repo.list(a, .{ .limit = 3 });
    var next = first.next;

    try rig.repo.create(sample("before", 0));
    try rig.repo.create(sample("inside", 4001));
    try rig.repo.create(sample("after", 3_600_000));
    var got: std.ArrayList([]const u8) = .empty;
    for (first.items) |d| try got.append(a, d.id);
    while (next) |n| {
        const page = try rig.repo.list(a, .{ .limit = 3, .after = n });
        for (page.items) |d| try got.append(a, d.id);
        next = page.next;
    }
    const want = [_][]const u8{ "e0", "e1", "e2", "e3", "inside", "e4", "e5", "after" };
    try testing.expectEqual(want.len, got.items.len);
    for (want, got.items) |w, g| try testing.expectEqualStrings(w, g);
}

test "the limit defaults to 50 and is capped at 200" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.exec(
            \\WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 204)
            \\INSERT INTO downloads (id, name, source_kind, state, added_at, updated_at)
            \\SELECT printf('d%04d', i), 'd', 'nzb', 'added', i * 1000, i * 1000 FROM n
        );
    }
    const by_default = try rig.repo.list(a, .{});
    try testing.expectEqual(@as(usize, default_list_limit), by_default.items.len);
    try testing.expect(by_default.next != null);
    const capped = try rig.repo.list(a, .{ .limit = 10 * max_list_limit });
    try testing.expectEqual(@as(usize, max_list_limit), capped.items.len);
    try testing.expect(capped.next != null);
    // listAll reads past the cap.
    try testing.expectEqual(@as(usize, 205), (try rig.repo.listAll(a, .{})).len);
}

test "a malformed cursor is invalid" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    // "not base64!", "nocolon", "x:y", ":id" and "5:" encoded or not
    for ([_][]const u8{ "not base64!", "bm9jb2xvbg", "eDp5", "Omlk", "NTo", "!!!" }) |c| {
        try testing.expectError(error.InvalidCursor, rig.repo.list(rig.arena(), .{ .after = c }));
    }
    const long = try rig.arena().alloc(u8, max_cursor_len + 1);
    @memset(long, 'A');
    try testing.expectError(error.InvalidCursor, rig.repo.list(rig.arena(), .{ .after = long }));
}

test "update keeps added_at so paging stays stable" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    for (0..4) |i| try rig.repo.create(sample(try std.fmt.allocPrint(a, "d{d}", .{i}), @as(i64, @intCast(i)) * 1000));
    const first = try rig.repo.list(a, .{ .limit = 2 });
    var moved = sample("d2", -3_600_000);
    moved.name = "renamed";
    try rig.repo.update(moved);
    const stored = try rig.repo.get(a, "d2");
    try testing.expectEqual(tu.epoch + 2000, stored.added_at);
    const second = try rig.repo.list(a, .{ .limit = 2, .after = first.next.? });
    try expectIds(&.{ "d2", "d3" }, second.items);
}

test "progress writes the bytes and updated_at and nothing else" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    var d = sample("d1", 0);
    d.total_bytes = 100;
    try rig.repo.create(d);

    // The row is edited after the runner read it.
    var edited = d;
    edited.priority = .force;
    edited.paused = true;
    edited.queue_position = 7;
    try rig.repo.update(edited);
    _ = try rig.repo.updateState(a, "d1", .{ .to = .queued, .at = tu.epoch + 60_000 });

    try rig.repo.updateProgress("d1", 55, 99);
    var got = try rig.repo.get(a, "d1");
    try testing.expectEqual(dl.Priority.force, got.priority);
    try testing.expect(got.paused);
    try testing.expectEqual(@as(i64, 7), got.queue_position);
    try testing.expectEqual(dl.State.queued, got.state);
    try testing.expectEqual(@as(i64, 55), got.done_bytes);
    try testing.expectEqual(@as(i64, 99), got.total_bytes);
    const now = Io.Clock.now(.real, testing.io).toMilliseconds();
    try testing.expect(got.updated_at > now - 5000 and got.updated_at <= now);

    // No total (zero or negative) keeps the stored one.
    try rig.repo.updateProgress("d1", 10, 0);
    try rig.repo.updateProgress("d1", 20, -5);
    got = try rig.repo.get(a, "d1");
    try testing.expectEqual(@as(i64, 20), got.done_bytes);
    try testing.expectEqual(@as(i64, 99), got.total_bytes);

    try testing.expectError(error.NotFound, rig.repo.updateProgress("ghost", 1, 1));
    try testing.expectError(error.NegativeBytes, rig.repo.updateProgress("d1", -1, 0));
}

test "display stores the name and total without touching the rest" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    var d = sample("d1", 0);
    d.name = "magnet";
    d.priority = .high;
    d.paused = true;
    d.queue_position = 3;
    d.total_bytes = 100;
    try rig.repo.create(d);

    try rig.repo.updateDisplay("d1", "", 0);
    var got = try rig.repo.get(a, "d1");
    try testing.expectEqualStrings("magnet", got.name);
    try testing.expectEqual(@as(i64, 100), got.total_bytes);

    try rig.repo.updateDisplay("d1", "Ubuntu 26.04", 4096);
    got = try rig.repo.get(a, "d1");
    try testing.expectEqualStrings("Ubuntu 26.04", got.name);
    try testing.expectEqual(@as(i64, 4096), got.total_bytes);
    try testing.expectEqual(dl.Priority.high, got.priority);
    try testing.expect(got.paused);
    try testing.expectEqual(@as(i64, 3), got.queue_position);
    try testing.expectEqual(dl.State.added, got.state);
    try testing.expectError(error.NotFound, rig.repo.updateDisplay("ghost", "x", 1));
}

test "listInCategory lists every Download of the Category, beyond one page" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.addCategory("cat1", "tv");
    try rig.addCategory("cat2", "movies");
    {
        const w = try rig.store.acquireWriter();
        defer w.release();
        try w.db.exec(
            \\WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 249)
            \\INSERT INTO downloads (id, name, source_kind, state, added_at, updated_at, category_id)
            \\SELECT printf('d%04d', i), 'd', 'nzb', 'added', i * 1000, i * 1000, CASE WHEN i % 5 = 0 THEN 'cat2' ELSE 'cat1' END FROM n
        );
    }
    try testing.expectEqual(@as(usize, 200), (try rig.repo.listInCategory(a, "cat1")).len);
    try testing.expectEqual(@as(usize, 50), (try rig.repo.listInCategory(a, "cat2")).len);
    try testing.expectEqual(@as(usize, 0), (try rig.repo.listInCategory(a, "")).len);
    try testing.expectEqual(@as(usize, 0), (try rig.repo.listInCategory(a, "ghost")).len);
}

fn finishedDl(id: []const u8, state: dl.State, ready_at: i64, updated_at: i64) dl.Download {
    var d = sample(id, 0);
    d.state = state;
    d.ready_at = ready_at;
    d.updated_at = updated_at;
    if (state == .failed) d.failure_reason = .internal;
    if (state == .ready) d.output_path = "/out";
    return d;
}

test "history lists the finished Downloads, newest first, with counts" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.repo.create(sample("active", 0));
    try rig.repo.create(finishedDl("r1", .ready, 1000, 9000));
    try rig.repo.create(finishedDl("f1", .failed, 0, 5000));
    try rig.repo.create(finishedDl("r2", .ready, 7000, 7000));
    try rig.repo.create(finishedDl("r3", .ready, 5000, 5000));

    const all = try rig.repo.history(a, .{});
    // r2 (7000), f1 and r3 (5000, by ID descending), r1 (ready_at 1000).
    try expectIds(&.{ "r2", "r3", "f1", "r1" }, all.items);
    try testing.expectEqual(@as(i64, 4), all.total);
    try testing.expectEqual(@as(i64, 0), all.newer);

    const only_failed = try rig.repo.history(a, .{ .state = .failed });
    try expectIds(&.{"f1"}, only_failed.items);
    try testing.expectEqual(@as(i64, 1), only_failed.total);

    // Continue after r3: the rows from r2 to r3 are before the page.
    const next = try rig.repo.history(a, .{ .before_at = 5000, .before_id = "r3", .limit = 10 });
    try expectIds(&.{ "f1", "r1" }, next.items);
    try testing.expectEqual(@as(i64, 4), next.total);
    try testing.expectEqual(@as(i64, 2), next.newer);

    const limited = try rig.repo.history(a, .{ .limit = 1 });
    try expectIds(&.{"r2"}, limited.items);
}

test "finishedAt is ready_at for a ready Download, else updated_at" {
    try testing.expectEqual(@as(i64, 10), finishedAt(finishedDl("a", .ready, 10, 20)));
    try testing.expectEqual(@as(i64, 20), finishedAt(finishedDl("a", .ready, 0, 20)));
    try testing.expectEqual(@as(i64, 20), finishedAt(finishedDl("a", .failed, 10, 20)));
}

test "parseDecimal is Go's ParseInt for base 10" {
    try testing.expectEqual(@as(?i64, 5), parseDecimal("5"));
    try testing.expectEqual(@as(?i64, 5), parseDecimal("+5"));
    try testing.expectEqual(@as(?i64, -5), parseDecimal("-5"));
    try testing.expectEqual(@as(?i64, std.math.maxInt(i64)), parseDecimal("9223372036854775807"));
    try testing.expectEqual(@as(?i64, std.math.minInt(i64)), parseDecimal("-9223372036854775808"));
    for ([_][]const u8{ "", "+", "-", "1_0", "0x10", " 1", "1 ", "9223372036854775808", "-9223372036854775809", "1e3", "٣" }) |bad| {
        try testing.expectEqual(@as(?i64, null), parseDecimal(bad));
    }
}

test "cursors round-trip" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const c = try encodeCursor(arena.allocator(), 1793365901873, "abc:def");
    const d = try decodeCursor(arena.allocator(), c);
    try testing.expectEqual(@as(i64, 1793365901873), d.added_at);
    try testing.expectEqualStrings("abc:def", d.id);
}

fn fuzzCursor(_: void, smith: *testing.Smith) !void {
    var buf: [200]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const c = decodeCursor(arena.allocator(), buf[0..n]) catch |err| switch (err) {
        error.InvalidCursor => return,
        else => return err,
    };
    try testing.expect(c.id.len > 0);
    // What decodes re-encodes to a cursor that decodes to the same values.
    const again = try decodeCursor(arena.allocator(), try encodeCursor(arena.allocator(), c.added_at, c.id));
    try testing.expectEqual(c.added_at, again.added_at);
    try testing.expectEqualStrings(c.id, again.id);
}

fn fuzzSeed(comptime s: []const u8) []const u8 {
    return &(std.mem.toBytes(@as(u32, s.len)) ++ s.*);
}

test "fuzz: any cursor text is refused or round-trips" {
    try testing.fuzz({}, fuzzCursor, .{ .corpus = &.{
        fuzzSeed("MTc5MzM2NTkwMTg3MzphYmM"),
        fuzzSeed("bm9jb2xvbg"),
        fuzzSeed("eDp5"),
        fuzzSeed("not base64!"),
        fuzzSeed(""),
    } });
}

test "fuzz: parseDecimal agrees with a reference on any text" {
    try testing.fuzz({}, struct {
        fn one(_: void, smith: *testing.Smith) anyerror!void {
            var buf: [40]u8 = undefined;
            const n = smith.slice(&buf);
            const s = buf[0..n];
            const got = parseDecimal(s);
            // The reference: parseInt, once '_' (which Go refuses) is out.
            const reference = if (std.mem.indexOfScalar(u8, s, '_') != null) null else std.fmt.parseInt(i64, s, 10) catch null;
            try testing.expectEqual(reference, got);
        }
    }.one, .{ .corpus = &.{ fuzzSeed("123"), fuzzSeed("-9223372036854775808"), fuzzSeed("+"), fuzzSeed("1_000") } });
}

test "the Go-written database takes Downloads from Zig in the stored form Go reads" {
    var rig: tu.Rig = undefined;
    try rig.openGolden();
    defer rig.close();
    const a = rig.arena();
    // The golden file holds one Category and no Download.
    try testing.expectEqual(@as(usize, 0), (try rig.repo.list(a, .{})).items.len);
    var cat_id: []const u8 = "";
    {
        const r = try rig.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT id FROM categories LIMIT 1");
        defer st.finalize();
        try testing.expect(try st.step());
        cat_id = try st.columnText(a, 0);
    }
    var d = sample("golden1", 123);
    d.category_id = cat_id;
    d.state = .fetched;
    d.fetched_at = tu.epoch + 5;
    try rig.repo.create(d);
    try expectEqualDownload(d, try rig.repo.get(a, "golden1"));

    // Go stores added_at in milliseconds, ready_at and failure_reason as NULL.
    try testing.expectEqual(@as(usize, 1), try rig.countWhere("id = 'golden1' AND added_at = " ++ std.fmt.comptimePrint("{d}", .{tu.epoch + 123}) ++
        " AND ready_at IS NULL AND failure_reason IS NULL AND fetched_at = " ++ std.fmt.comptimePrint("{d}", .{tu.epoch + 5}) ++ " AND paused = 0 AND typeof(category_id) = 'text'"));
}

test "find is null for an unknown Download and list_in_category is listInCategory" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const a = rig.arena();
    try rig.addCategory("cat1", "tv");
    var d = sample("d1", 0);
    d.category_id = "cat1";
    try rig.repo.create(d);
    try testing.expect((try rig.repo.find(a, "ghost")) == null);
    try testing.expectEqualStrings("d1", (try rig.repo.find(a, "d1")).?.id);
    try expectIds(&.{"d1"}, try rig.repo.list_in_category(a, "cat1"));
}
