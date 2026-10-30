//! Managing existing Downloads (internal/app/download.go): list, read, pause,
//! resume, change priority, category and queue position, and delete. It never
//! moves a Download between lifecycle states: engines do that. Adding comes
//! with the Usenet and torrent milestones.
//!
//! Failures a client can act on are returned as `error.Rejected` with the
//! details in the `Failure` the caller passes; `error.NotFound` and
//! `error.Conflict` (from the repo) have generic messages. Anything else is a
//! 500. Results and their strings live in the caller's arena.
//!
//! Owner: whoever calls `init` (the daemon); it holds the only mutable state,
//! the set of Downloads being deleted.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const event = @import("event.zig");
const app_error = @import("app_error.zig");
const Failure = app_error.Failure;
const repo_mod = @import("downloads_repo.zig");
const DownloadRepo = repo_mod.DownloadRepo;
const order = @import("downloads_order.zig");

// ---- seams ----

/// Whether a Category exists (Go: CategoryRepo.Get). The Categories service
/// provides it.
pub const CategoryLookup = struct {
    ctx: *anyopaque,
    exists_fn: *const fn (ctx: *anyopaque, id: []const u8) anyerror!bool,

    pub fn exists(c: CategoryLookup, id: []const u8) anyerror!bool {
        return c.exists_fn(c.ctx, id);
    }
};

/// Undoes a hold (see `DataRemover.hold`). Call `release` at most once.
pub const Release = struct {
    ctx: ?*anyopaque = null,
    release_fn: *const fn (ctx: ?*anyopaque) void = noRelease,

    pub fn release(r: Release) void {
        r.release_fn(r.ctx);
    }

    fn noRelease(_: ?*anyopaque) void {}
};

/// Deletes the data a Download left on disk: the incomplete directory, the
/// output, any seeded files. The post-processing milestone (Z4) implements it.
/// Errors keep the Download, so the user can retry.
pub const DataRemover = struct {
    ctx: ?*anyopaque = null,
    hold_fn: *const fn (ctx: ?*anyopaque, d: dl.Download) anyerror!Release = noHold,
    remove_fn: *const fn (ctx: ?*anyopaque, d: dl.Download) anyerror!void = noRemove,

    /// Stops whatever still works on the Download's files (its fetch, or the
    /// torrent engine) and keeps it stopped. The returned `Release` undoes the
    /// hold; it is called when the removal does not go ahead. After a removal
    /// the hold ends when `removed` is published.
    pub fn hold(r: DataRemover, d: dl.Download) anyerror!Release {
        return r.hold_fn(r.ctx, d);
    }

    /// Deletes the files. Called with the hold in place and before the row is
    /// deleted, so it can still read the row's paths.
    pub fn removeData(r: DataRemover, d: dl.Download) anyerror!void {
        return r.remove_fn(r.ctx, d);
    }

    fn noHold(_: ?*anyopaque, _: dl.Download) anyerror!Release {
        return .{};
    }

    fn noRemove(_: ?*anyopaque, _: dl.Download) anyerror!void {}

    /// Holds and removes nothing.
    pub const noop: DataRemover = .{};
};

pub fn realNowMs(io: Io) i64 {
    return Io.Clock.now(.real, io).toMilliseconds();
}

pub const Deps = struct {
    categories: CategoryLookup,
    remover: DataRemover = .noop,
    publisher: event.Publisher = event.Publisher.discard,
    /// Unix milliseconds; tests pass a fake.
    now_fn: *const fn (io: Io) i64 = realNowMs,
};

// ---- requests ----

/// Selects one page of Downloads. Empty strings match everything; the values
/// are checked by the service, like the Go ListFilter.
pub const ListFilter = struct {
    state: []const u8 = "",
    category_id: []const u8 = "",
    kind: []const u8 = "",
    /// The `next` cursor of the previous page.
    after: []const u8 = "",
    /// Zero takes the repository default.
    limit: i64 = 0,
};

/// Whether a Patch changes the Category, and to what. The default leaves it
/// alone; `set` with an empty `id` removes the Category.
pub const CategoryChange = struct {
    set: bool = false,
    id: []const u8 = "",
};

/// The changes `update` applies; the default of a field means "leave alone".
/// The lifecycle state is deliberately absent: only engines move a Download
/// through its states.
pub const Patch = struct {
    /// force, high, normal or low.
    priority: ?[]const u8 = null,
    category: CategoryChange = .{},
    /// top, bottom, up or down; empty for no move.
    move: []const u8 = "",
};

pub const Page = repo_mod.Page;

/// The states in which Post-processing owns a Download's files: fetched (it is
/// about to start) and post_processing (par2 or 7zz may be running). A Download
/// in one of them is not deleted.
const busy_states = [_]dl.State{ .fetched, .post_processing };

pub const Service = struct {
    gpa: Allocator,
    io: Io,
    repo: DownloadRepo,
    deps: Deps,
    /// Serializes the read-modify-write cycles of the mutations and guards
    /// `deleting`. Never held while files are deleted.
    mu: Io.Mutex = .init,
    /// The Downloads a delete is working on; the keys are owned (gpa).
    deleting: std.StringHashMapUnmanaged(void) = .empty,

    pub fn init(gpa: Allocator, io: Io, repo: DownloadRepo, deps: Deps) Service {
        return .{ .gpa = gpa, .io = io, .repo = repo, .deps = deps };
    }

    pub fn deinit(self: *Service) void {
        var it = self.deleting.keyIterator();
        while (it.next()) |k| self.gpa.free(k.*);
        self.deleting.deinit(self.gpa);
    }

    fn now(self: *Service) i64 {
        return self.deps.now_fn(self.io);
    }

    // ---- reads ----

    pub fn list(self: *Service, arena: Allocator, f: *Failure, filter: ListFilter) !Page {
        const q = try validate(arena, f, filter);
        return self.repo.list(arena, .{
            .state = q.state,
            .category_id = filter.category_id,
            .source_kind = q.kind,
            .after = filter.after,
            .limit = filter.limit,
        }) catch |err| switch (err) {
            error.InvalidCursor => return f.setLiteral(.invalid_input, "invalid_cursor", "the cursor is not valid"),
            else => return err,
        };
    }

    /// Every Download the filter matches, page after page; `after` and `limit`
    /// are ignored.
    pub fn all(self: *Service, arena: Allocator, f: *Failure, filter: ListFilter) ![]dl.Download {
        var plain = filter;
        plain.after = "";
        plain.limit = 0;
        const q = try validate(arena, f, plain);
        return self.repo.listAll(arena, .{ .state = q.state, .category_id = filter.category_id, .source_kind = q.kind });
    }

    /// error.NotFound when there is no such Download.
    pub fn get(self: *Service, arena: Allocator, id: []const u8) !dl.Download {
        return self.repo.get(arena, id);
    }

    /// One page of the finished Downloads, newest first.
    pub fn history(self: *Service, arena: Allocator, q: repo_mod.HistoryQuery) !repo_mod.HistoryPage {
        return self.repo.history(arena, q);
    }

    // ---- pause and resume ----

    /// Stops a Download from being active; pausing a paused one does nothing.
    /// A Download that has not been fetched can be paused, and so can a ready
    /// torrent, which stops its seeding (ADR-0012). For the others there is
    /// nothing to pause (conflict, code "not_pausable").
    pub fn pause(self: *Service, arena: Allocator, f: *Failure, id: []const u8) !dl.Download {
        return self.setPaused(arena, f, id, true);
    }

    /// Undoes `pause`; resuming a Download that is not paused does nothing.
    pub fn resume_(self: *Service, arena: Allocator, f: *Failure, id: []const u8) !dl.Download {
        return self.setPaused(arena, f, id, false);
    }

    fn setPaused(self: *Service, arena: Allocator, f: *Failure, id: []const u8, paused: bool) !dl.Download {
        try self.mu.lock(self.io);
        defer self.mu.unlock(self.io);
        var d = try self.repo.get(arena, id);
        if (d.paused == paused) return d;
        if (paused and !pausable(d)) {
            const q = try quote(arena, @tagName(d.state));
            return f.set(arena, .conflict, "not_pausable", "a download in state {s} cannot be paused", .{q});
        }
        d.paused = paused;
        d.updated_at = self.now();
        try self.repo.update(d);
        self.deps.publisher.publish(.{ .download_updated = d });
        return d;
    }

    // ---- update ----

    /// Validates the whole Patch, then applies it. A move renumbers the other
    /// Downloads of the same Queue, and each Download whose stored data changed
    /// gets a `download_updated` event. Returns the patched Download.
    pub fn update(self: *Service, arena: Allocator, f: *Failure, id: []const u8, p: Patch) !dl.Download {
        if (p.priority == null and !p.category.set and p.move.len == 0) {
            return f.setLiteral(.invalid_input, "empty_patch", "nothing to change: give priority, category_id or queue_position");
        }
        var priority: ?dl.Priority = null;
        if (p.priority) |s| {
            priority = std.meta.stringToEnum(dl.Priority, s) orelse {
                const q = try quote(arena, s);
                return f.set(arena, .invalid_input, "invalid_priority", "unknown priority {s}: use force, high, normal or low", .{q});
            };
        }
        var move: ?order.Move = null;
        if (p.move.len > 0) {
            move = order.Move.parse(p.move) orelse {
                const q = try quote(arena, p.move);
                return f.set(arena, .invalid_input, "invalid_move", "unknown move {s}: use top, bottom, up or down", .{q});
            };
        }

        try self.mu.lock(self.io);
        defer self.mu.unlock(self.io);
        var d = try self.repo.get(arena, id);
        const before = d;
        if (p.category.set and p.category.id.len > 0) {
            if (!try self.deps.categories.exists(p.category.id)) {
                const q = try quote(arena, p.category.id);
                return f.set(arena, .invalid_input, "invalid_category", "category {s} does not exist", .{q});
            }
        }

        // Queue neighbours whose position changes.
        var others: std.ArrayList(dl.Download) = .empty;
        if (move) |m| {
            if (!d.state.inQueue()) {
                const q = try quote(arena, @tagName(d.state));
                return f.set(arena, .conflict, "not_queued", "a download in state {s} is not in a queue", .{q});
            }
            const queue = try self.queueOf(arena, d.source_kind);
            const positions = order.reposition(arena, queue, d.id, m) catch |err| switch (err) {
                error.NotFound => return error.NotFound,
                error.OutOfMemory => return error.OutOfMemory,
            };
            d.queue_position = order.positionOf(positions, d.id).?;
            for (queue) |n| {
                const pos = order.positionOf(positions, n.id).?;
                if (!std.mem.eql(u8, n.id, d.id) and n.queue_position != pos) {
                    var moved = n;
                    moved.queue_position = pos;
                    try others.append(arena, moved);
                }
            }
        }

        if (priority) |pr| d.priority = pr;
        if (p.category.set) d.category_id = p.category.id;
        const t = self.now();
        if (d.priority != before.priority or d.queue_position != before.queue_position or
            !std.mem.eql(u8, d.category_id, before.category_id))
        {
            d.updated_at = t;
            try self.repo.update(d);
            self.deps.publisher.publish(.{ .download_updated = d });
        }
        for (others.items) |*o| {
            o.updated_at = t;
            try self.repo.update(o.*);
            self.deps.publisher.publish(.{ .download_updated = o.* });
        }
        return d;
    }

    /// The Downloads of the Queue of one source kind: those not yet fetched.
    fn queueOf(self: *Service, arena: Allocator, kind: dl.SourceKind) ![]dl.Download {
        var queue: std.ArrayList(dl.Download) = .empty;
        for (std.enums.values(dl.State)) |st| {
            if (!st.inQueue()) continue;
            try queue.appendSlice(arena, try self.repo.listAll(arena, .{ .state = st, .source_kind = kind, .limit = repo_mod.max_list_limit }));
        }
        return queue.items;
    }

    // ---- delete ----

    /// Removes the Download and publishes `removed`. A Download that is being
    /// post-processed is refused (conflict, code "download_busy"), with or
    /// without its data, because its files are in use; a second delete of a
    /// Download that is still being deleted is a conflict too ("download_deleting").
    ///
    /// With `delete_data` the DataRemover first stops and holds whatever still
    /// works on the files, then the state is checked again (the fetch may have
    /// finished just before the hold), and only then the files are deleted. If
    /// that fails the hold is released and the Download stays, so the user can
    /// retry. The row goes last, and only if Post-processing has not claimed it
    /// meanwhile. No lock is held while files are deleted.
    pub fn delete(self: *Service, arena: Allocator, f: *Failure, id: []const u8, delete_data: bool) !void {
        if (!try self.startDeleting(id)) {
            return f.setLiteral(.conflict, "download_deleting", "the download is already being removed");
        }
        defer self.doneDeleting(id);
        const d = try self.repo.get(arena, id);
        if (isBusy(d.state)) return busy(f);
        if (delete_data) try self.removeData(arena, f, d);
        self.repo.deleteUnlessIn(id, &busy_states) catch |err| switch (err) {
            error.Conflict => return busy(f),
            else => return err,
        };
        self.deps.publisher.publish(.{ .removed = id });
    }

    /// Holds `d`, checks that Post-processing has not taken it, and deletes its files.
    fn removeData(self: *Service, arena: Allocator, f: *Failure, d: dl.Download) !void {
        const release = try self.deps.remover.hold(d);
        errdefer release.release();
        const now_d = try self.repo.get(arena, d.id);
        if (isBusy(now_d.state)) return busy(f);
        try self.deps.remover.removeData(now_d);
    }

    fn startDeleting(self: *Service, id: []const u8) !bool {
        try self.mu.lock(self.io);
        defer self.mu.unlock(self.io);
        if (self.deleting.contains(id)) return false;
        const key = try self.gpa.dupe(u8, id);
        errdefer self.gpa.free(key);
        try self.deleting.put(self.gpa, key, {});
        return true;
    }

    fn doneDeleting(self: *Service, id: []const u8) void {
        self.mu.lockUncancelable(self.io);
        defer self.mu.unlock(self.io);
        if (self.deleting.fetchRemove(id)) |kv| self.gpa.free(kv.key);
    }
};

fn isBusy(state: dl.State) bool {
    return std.mem.indexOfScalar(dl.State, &busy_states, state) != null;
}

fn busy(f: *Failure) error{Rejected} {
    return f.setLiteral(.conflict, "download_busy", "This download is being post-processed (repaired or unpacked) and its files are in use. Remove it once it is ready or has failed.");
}

/// Whether pause acts on `d`: it is still waiting for or receiving data, or it
/// is a torrent that is ready and so only seeding.
fn pausable(d: dl.Download) bool {
    return switch (d.state) {
        .added, .fetching_metadata, .queued, .fetching => true,
        .ready => d.source_kind == .torrent,
        else => false,
    };
}

const Validated = struct { state: ?dl.State, kind: ?dl.SourceKind };

fn validate(arena: Allocator, f: *Failure, filter: ListFilter) !Validated {
    var out: Validated = .{ .state = null, .kind = null };
    if (filter.state.len > 0) {
        out.state = std.meta.stringToEnum(dl.State, filter.state) orelse {
            const q = try quote(arena, filter.state);
            return f.set(arena, .invalid_input, "invalid_state", "unknown state {s}", .{q});
        };
    }
    if (filter.kind.len > 0) {
        out.kind = std.meta.stringToEnum(dl.SourceKind, filter.kind) orelse {
            const q = try quote(arena, filter.kind);
            return f.set(arena, .invalid_input, "invalid_kind", "unknown kind {s}: use nzb or torrent", .{q});
        };
    }
    if (filter.limit < 0) return f.setLiteral(.invalid_input, "invalid_limit", "limit must not be negative");
    return out;
}

/// Go's strconv.Quote, which `%q` uses: a double-quoted string with the escapes
/// Go writes. Whether a non-ASCII character is printable is approximated with
/// the common non-printing ranges; the message is for people, not for parsers.
pub fn quote(arena: Allocator, s: []const u8) Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    try out.append(arena, '"');
    var i: usize = 0;
    while (i < s.len) {
        const c = s[i];
        if (c < 0x80) {
            i += 1;
            switch (c) {
                '"' => try out.appendSlice(arena, "\\\""),
                '\\' => try out.appendSlice(arena, "\\\\"),
                0x07 => try out.appendSlice(arena, "\\a"),
                0x08 => try out.appendSlice(arena, "\\b"),
                0x0c => try out.appendSlice(arena, "\\f"),
                '\n' => try out.appendSlice(arena, "\\n"),
                '\r' => try out.appendSlice(arena, "\\r"),
                '\t' => try out.appendSlice(arena, "\\t"),
                0x0b => try out.appendSlice(arena, "\\v"),
                0x00...0x06, 0x0e...0x1f, 0x7f => try out.print(arena, "\\x{x:0>2}", .{c}),
                else => try out.append(arena, c),
            }
            continue;
        }
        const len = std.unicode.utf8ByteSequenceLength(c) catch {
            try out.print(arena, "\\x{x:0>2}", .{c});
            i += 1;
            continue;
        };
        if (i + len > s.len) {
            try out.print(arena, "\\x{x:0>2}", .{c});
            i += 1;
            continue;
        }
        const r = std.unicode.utf8Decode(s[i .. i + len]) catch {
            try out.print(arena, "\\x{x:0>2}", .{c});
            i += 1;
            continue;
        };
        if (printable(r)) {
            try out.appendSlice(arena, s[i .. i + len]);
        } else if (r < 0x10000) {
            try out.print(arena, "\\u{x:0>4}", .{r});
        } else {
            try out.print(arena, "\\U{x:0>8}", .{r});
        }
        i += len;
    }
    try out.append(arena, '"');
    return out.items;
}

fn printable(r: u21) bool {
    return switch (r) {
        0x80...0xa0, 0xad, 0x200b...0x200f, 0x2028...0x202e, 0x2060...0x206f, 0xe000...0xf8ff, 0xfeff, 0xfff9...0xfffb, 0xf0000...0x10ffff => false,
        else => true,
    };
}

// ---- tests ----

const testing = std.testing;
const tu = @import("downloads_testutil.zig");

/// The clock of the tests: advances only when a test says so.
pub var fake_now: i64 = tu.epoch;

pub fn fakeNow(_: Io) i64 {
    return fake_now;
}

pub const Recorded = struct {
    kind: event.Kind,
    id: []const u8,
    paused: bool = false,
    priority: dl.Priority = .normal,
    queue_position: i64 = 0,
    category_id: []const u8 = "",
    updated_at: i64 = 0,
};

pub const Recorder = struct {
    events: std.ArrayList(Recorded) = .empty,

    pub fn publisher(r: *Recorder) event.Publisher {
        return .{ .ctx = r, .publish_fn = record };
    }

    fn record(ctx: *anyopaque, e: event.Event) void {
        const r: *Recorder = @ptrCast(@alignCast(ctx));
        const rec: Recorded = switch (e) {
            .download_updated => |d| .{
                .kind = .download_updated,
                .id = testing.allocator.dupe(u8, d.id) catch @panic("oom"),
                .paused = d.paused,
                .priority = d.priority,
                .queue_position = d.queue_position,
                .category_id = testing.allocator.dupe(u8, d.category_id) catch @panic("oom"),
                .updated_at = d.updated_at,
            },
            .removed => |id| .{ .kind = .removed, .id = testing.allocator.dupe(u8, id) catch @panic("oom") },
            else => @panic("unexpected event"),
        };
        r.events.append(testing.allocator, rec) catch @panic("oom");
    }

    /// Forgets what was published so far.
    pub fn clear(r: *Recorder) void {
        for (r.events.items) |e| {
            testing.allocator.free(e.id);
            testing.allocator.free(e.category_id);
        }
        r.events.clearRetainingCapacity();
    }

    pub fn deinit(r: *Recorder) void {
        r.clear();
        r.events.deinit(testing.allocator);
    }

    fn ids(r: *const Recorder, arena: Allocator) ![][]const u8 {
        const out = try arena.alloc([]const u8, r.events.items.len);
        for (r.events.items, out) |e, *o| o.* = e.id;
        return out;
    }
};

/// A fake Category store: exactly the IDs it is given exist.
pub const Cats = struct {
    known: []const []const u8 = &.{},

    fn lookup(c: *Cats) CategoryLookup {
        return .{ .ctx = c, .exists_fn = exists };
    }

    fn exists(ctx: *anyopaque, id: []const u8) anyerror!bool {
        const c: *Cats = @ptrCast(@alignCast(ctx));
        for (c.known) |k| if (std.mem.eql(u8, k, id)) return true;
        return false;
    }
};

/// Records what it was asked to hold and remove, and whether the row still
/// existed at that moment.
pub const FakeRemover = struct {
    repo: DownloadRepo,
    held: usize = 0,
    released: usize = 0,
    removed: usize = 0,
    row_seen: bool = false,
    fail_remove: bool = false,
    fail_hold: bool = false,
    /// Runs inside hold.
    on_hold: ?*const fn (*FakeRemover) void = null,
    /// Runs inside removeData; the concurrency tests block here.
    on_remove: ?*const fn (*FakeRemover) void = null,
    blocker: ?*Blocker = null,

    fn remover(r: *FakeRemover) DataRemover {
        return .{ .ctx = r, .hold_fn = hold, .remove_fn = remove };
    }

    fn hold(ctx: ?*anyopaque, _: dl.Download) anyerror!Release {
        const r: *FakeRemover = @ptrCast(@alignCast(ctx.?));
        r.held += 1;
        if (r.on_hold) |f| f(r);
        if (r.fail_hold) return error.HoldFailed;
        return .{ .ctx = r, .release_fn = releaseFn };
    }

    fn releaseFn(ctx: ?*anyopaque) void {
        const r: *FakeRemover = @ptrCast(@alignCast(ctx.?));
        r.released += 1;
    }

    fn remove(ctx: ?*anyopaque, d: dl.Download) anyerror!void {
        const r: *FakeRemover = @ptrCast(@alignCast(ctx.?));
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        r.row_seen = if (r.repo.get(arena.allocator(), d.id)) |_| true else |_| false;
        r.removed += 1;
        if (r.on_remove) |f| f(r);
        if (r.fail_remove) return error.RemoveFailed;
    }
};

/// Lets a test stop a thread inside the remover and let it go again.
pub const Blocker = struct {
    entered: Io.Event = .unset,
    go: Io.Event = .unset,
};

fn blockInRemove(r: *FakeRemover) void {
    const b = r.blocker.?;
    b.entered.set(testing.io);
    b.go.waitUncancelable(testing.io);
}

pub const Fixture = struct {
    rig: tu.Rig,
    recorder: Recorder = .{},
    cats: Cats = .{},
    remover: FakeRemover,
    svc: Service,
    failure: Failure = .{},

    pub fn open(fx: *Fixture) !void {
        try fx.rig.open();
        fake_now = tu.epoch;
        fx.recorder = .{};
        fx.cats = .{};
        fx.remover = .{ .repo = fx.rig.repo };
        fx.failure = .{};
        fx.svc = Service.init(testing.allocator, testing.io, fx.rig.repo, .{
            .categories = fx.cats.lookup(),
            .remover = fx.remover.remover(),
            .publisher = fx.recorder.publisher(),
            .now_fn = fakeNow,
        });
    }

    pub fn close(fx: *Fixture) void {
        fx.svc.deinit();
        fx.recorder.deinit();
        fx.rig.close();
    }

    pub fn a(fx: *Fixture) Allocator {
        return fx.rig.arena();
    }

    /// A queued NZB Download added `offset_ms` after the epoch.
    pub fn add(fx: *Fixture, id: []const u8, offset_ms: i64) !dl.Download {
        const d: dl.Download = .{
            .id = id,
            .name = id,
            .source_kind = .nzb,
            .state = .queued,
            .added_at = tu.epoch + offset_ms,
            .updated_at = tu.epoch + offset_ms,
        };
        try fx.rig.repo.create(d);
        return d;
    }

    pub fn moveTo(fx: *Fixture, id: []const u8, states: []const dl.State) !void {
        for (states) |s| {
            _ = try fx.rig.repo.updateState(fx.a(), id, .{ .to = s, .at = tu.epoch, .output_path = if (s == .ready) "/data/complete/release" else "" });
        }
    }

    fn expectRejected(fx: *Fixture, result: anytype, kind: app_error.Kind, code: []const u8) !void {
        try testing.expectError(error.Rejected, result);
        try testing.expectEqual(kind, fx.failure.kind);
        try testing.expectEqualStrings(code, fx.failure.code);
        fx.failure = .{};
    }
};

test "pause sets the flag, stores it and publishes download_updated" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    fake_now += 60_000;

    const got = try fx.svc.pause(fx.a(), &fx.failure, "d1");
    try testing.expect(got.paused);
    try testing.expectEqual(fake_now, got.updated_at);
    try testing.expectEqualDeep(got, try fx.rig.repo.get(fx.a(), "d1"));
    try testing.expectEqual(@as(usize, 1), fx.recorder.events.items.len);
    try testing.expectEqual(event.Kind.download_updated, fx.recorder.events.items[0].kind);
    try testing.expect(fx.recorder.events.items[0].paused);

    fx.recorder.clear();
    const back = try fx.svc.resume_(fx.a(), &fx.failure, "d1");
    try testing.expect(!back.paused);
    try testing.expectEqual(@as(usize, 1), fx.recorder.events.items.len);
}

test "pause and resume are idempotent and publish nothing when nothing changes" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    _ = try fx.svc.resume_(fx.a(), &fx.failure, "d1");
    _ = try fx.svc.pause(fx.a(), &fx.failure, "d1");
    fx.recorder.clear();
    _ = try fx.svc.pause(fx.a(), &fx.failure, "d1");
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
}

test "a fetched Download cannot be paused, nor can a ready NZB" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    try fx.moveTo("d1", &.{ .fetching, .fetched });
    try fx.expectRejected(fx.svc.pause(fx.a(), &fx.failure, "d1"), .conflict, "not_pausable");
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);

    _ = try fx.add("d2", 1000);
    try fx.moveTo("d2", &.{ .fetching, .fetched, .ready });
    try fx.expectRejected(fx.svc.pause(fx.a(), &fx.failure, "d2"), .conflict, "not_pausable");
}

test "the not_pausable message names the state like Go's %q" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    try fx.moveTo("d1", &.{ .fetching, .fetched });
    try testing.expectError(error.Rejected, fx.svc.pause(fx.a(), &fx.failure, "d1"));
    try testing.expectEqualStrings("a download in state \"fetched\" cannot be paused", fx.failure.message);
}

test "a ready torrent can be paused and resumed, which is seeding paused" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    const d: dl.Download = .{ .id = "t1", .name = "t", .source_kind = .torrent, .state = .queued, .added_at = tu.epoch, .updated_at = tu.epoch };
    try fx.rig.repo.create(d);
    try fx.moveTo("t1", &.{ .fetching, .fetched, .ready });
    fx.recorder.clear();

    const paused = try fx.svc.pause(fx.a(), &fx.failure, "t1");
    try testing.expect(paused.paused);
    try testing.expectEqual(dl.State.ready, paused.state);
    try testing.expectEqual(@as(usize, 1), fx.recorder.events.items.len);
    try testing.expect(!(try fx.svc.resume_(fx.a(), &fx.failure, "t1")).paused);
}

test "pause of an unknown Download is not found" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    try testing.expectError(error.NotFound, fx.svc.pause(fx.a(), &fx.failure, "ghost"));
    try testing.expectError(error.NotFound, fx.svc.get(fx.a(), "ghost"));
}

test "list filters, pages and reports an empty page as no items" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    try fx.rig.addCategory("cat1", "tv");
    _ = try fx.add("a", 0);
    var b = try fx.add("b", 1000);
    b.category_id = "cat1";
    try fx.rig.repo.update(b);
    _ = try fx.add("c", 2000);
    try fx.moveTo("c", &.{.fetching});

    const by_cat = try fx.svc.list(fx.a(), &fx.failure, .{ .category_id = "cat1" });
    try testing.expectEqualStrings("b", by_cat.items[0].id);
    const by_state = try fx.svc.list(fx.a(), &fx.failure, .{ .state = "fetching" });
    try testing.expectEqualStrings("c", by_state.items[0].id);
    const none = try fx.svc.list(fx.a(), &fx.failure, .{ .kind = "torrent" });
    try testing.expectEqual(@as(usize, 0), none.items.len);
    const first = try fx.svc.list(fx.a(), &fx.failure, .{ .limit = 2 });
    try testing.expectEqual(@as(usize, 2), first.items.len);
    const second = try fx.svc.list(fx.a(), &fx.failure, .{ .limit = 2, .after = first.next.? });
    try testing.expectEqualStrings("c", second.items[0].id);
    try testing.expect(second.next == null);
    try testing.expectEqual(@as(usize, 3), (try fx.svc.all(fx.a(), &fx.failure, .{ .limit = 1, .after = "!!!" })).len);
}

test "list refuses unknown filter values and a bad cursor" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    try fx.expectRejected(fx.svc.list(fx.a(), &fx.failure, .{ .state = "bogus" }), .invalid_input, "invalid_state");
    try fx.expectRejected(fx.svc.list(fx.a(), &fx.failure, .{ .kind = "bogus" }), .invalid_input, "invalid_kind");
    try fx.expectRejected(fx.svc.list(fx.a(), &fx.failure, .{ .limit = -1 }), .invalid_input, "invalid_limit");
    try fx.expectRejected(fx.svc.list(fx.a(), &fx.failure, .{ .after = "!!!" }), .invalid_input, "invalid_cursor");
    try testing.expectError(error.Rejected, fx.svc.list(fx.a(), &fx.failure, .{ .state = "a\"b" }));
    try testing.expectEqualStrings("unknown state \"a\\\"b\"", fx.failure.message);
}

test "update stores the priority and publishes download_updated" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    fake_now += 1000;
    const got = try fx.svc.update(fx.a(), &fx.failure, "d1", .{ .priority = "high" });
    try testing.expectEqual(dl.Priority.high, got.priority);
    try testing.expectEqual(fake_now, got.updated_at);
    try testing.expectEqualDeep(got, try fx.rig.repo.get(fx.a(), "d1"));
    try testing.expectEqual(@as(usize, 1), fx.recorder.events.items.len);

    // The same priority again changes nothing and publishes nothing.
    fx.recorder.clear();
    fake_now += 1000;
    const same = try fx.svc.update(fx.a(), &fx.failure, "d1", .{ .priority = "high" });
    try testing.expectEqual(fake_now - 1000, same.updated_at);
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
}

test "update refuses an unknown priority, an empty patch and an unknown move without changing anything" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    const d = try fx.add("d1", 0);
    try fx.expectRejected(fx.svc.update(fx.a(), &fx.failure, "d1", .{ .priority = "urgent" }), .invalid_input, "invalid_priority");
    try fx.expectRejected(fx.svc.update(fx.a(), &fx.failure, "d1", .{}), .invalid_input, "empty_patch");
    try fx.expectRejected(fx.svc.update(fx.a(), &fx.failure, "d1", .{ .move = "sideways" }), .invalid_input, "invalid_move");
    try testing.expectEqualDeep(d, try fx.rig.repo.get(fx.a(), "d1"));
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
}

test "update sets and clears the Category and refuses an unknown one" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    try fx.rig.addCategory("cat1", "tv");
    fx.cats.known = &.{"cat1"};
    _ = try fx.add("d1", 0);

    const set = try fx.svc.update(fx.a(), &fx.failure, "d1", .{ .category = .{ .set = true, .id = "cat1" } });
    try testing.expectEqualStrings("cat1", set.category_id);
    try fx.expectRejected(fx.svc.update(fx.a(), &fx.failure, "d1", .{ .category = .{ .set = true, .id = "nope" } }), .invalid_input, "invalid_category");
    const cleared = try fx.svc.update(fx.a(), &fx.failure, "d1", .{ .category = .{ .set = true } });
    try testing.expectEqualStrings("", cleared.category_id);
    try testing.expectEqual(@as(usize, 2), fx.recorder.events.items.len);
}

test "update of an unknown Download is not found, even with a bad category" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    try testing.expectError(error.NotFound, fx.svc.update(fx.a(), &fx.failure, "ghost", .{ .priority = "high" }));
    try testing.expectError(error.NotFound, fx.svc.update(fx.a(), &fx.failure, "ghost", .{ .category = .{ .set = true, .id = "nope" } }));
}

fn positionIn(fx: *Fixture, id: []const u8) !i64 {
    return (try fx.rig.repo.get(fx.a(), id)).queue_position;
}

test "a move renumbers the Queue and publishes every changed Download" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("a", 0);
    _ = try fx.add("b", 1000);
    _ = try fx.add("c", 2000);

    const got = try fx.svc.update(fx.a(), &fx.failure, "c", .{ .move = "top" });
    try testing.expectEqual(@as(i64, 0), got.queue_position);
    try testing.expectEqual(@as(i64, 1), try positionIn(&fx, "a"));
    try testing.expectEqual(@as(i64, 2), try positionIn(&fx, "b"));
    // Every position started at 0 (ties go by added_at), so a and b moved.
    var seen_a = false;
    var seen_b = false;
    for (fx.recorder.events.items) |e| {
        if (std.mem.eql(u8, e.id, "a")) seen_a = true;
        if (std.mem.eql(u8, e.id, "b")) seen_b = true;
    }
    try testing.expect(seen_a and seen_b);
}

test "a move only ranks Downloads of the same Queue" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    const nzb = try fx.add("nzb", 0);
    try fx.rig.repo.create(.{ .id = "tor", .name = "t", .source_kind = .torrent, .state = .queued, .added_at = tu.epoch + 1000, .updated_at = tu.epoch + 1000 });

    const got = try fx.svc.update(fx.a(), &fx.failure, "tor", .{ .move = "bottom" });
    try testing.expectEqual(@as(i64, 0), got.queue_position);
    try testing.expectEqualDeep(nzb, try fx.rig.repo.get(fx.a(), "nzb"));
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
}

test "a move of a Download outside the Queue is a conflict" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    try fx.moveTo("d1", &.{ .fetching, .fetched });
    try fx.expectRejected(fx.svc.update(fx.a(), &fx.failure, "d1", .{ .move = "up" }), .conflict, "not_queued");
}

test "delete removes the row, then publishes removed, without touching data" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    try fx.svc.delete(fx.a(), &fx.failure, "d1", false);
    try testing.expectError(error.NotFound, fx.rig.repo.get(fx.a(), "d1"));
    try testing.expectEqual(@as(usize, 1), fx.recorder.events.items.len);
    try testing.expectEqual(event.Kind.removed, fx.recorder.events.items[0].kind);
    try testing.expectEqualStrings("d1", fx.recorder.events.items[0].id);
    try testing.expectEqual(@as(usize, 0), fx.remover.removed);
}

test "delete with data holds, removes the files while the row exists, and keeps the hold" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    try fx.svc.delete(fx.a(), &fx.failure, "d1", true);
    try testing.expectEqual(@as(usize, 1), fx.remover.held);
    try testing.expectEqual(@as(usize, 1), fx.remover.removed);
    try testing.expect(fx.remover.row_seen);
    try testing.expectEqual(@as(usize, 0), fx.remover.released);
    try testing.expectError(error.NotFound, fx.rig.repo.get(fx.a(), "d1"));
    try testing.expectEqual(@as(usize, 1), fx.recorder.events.items.len);
}

test "delete keeps the row and publishes nothing when the data removal fails" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    fx.remover.fail_remove = true;
    try testing.expectError(error.RemoveFailed, fx.svc.delete(fx.a(), &fx.failure, "d1", true));
    _ = try fx.rig.repo.get(fx.a(), "d1");
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
    try testing.expectEqual(@as(usize, 1), fx.remover.released);
}

test "delete with data fails when the hold fails" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    fx.remover.fail_hold = true;
    try testing.expectError(error.HoldFailed, fx.svc.delete(fx.a(), &fx.failure, "d1", true));
    try testing.expectEqual(@as(usize, 0), fx.remover.removed);
    _ = try fx.rig.repo.get(fx.a(), "d1");
}

test "delete refuses a Download that is being post-processed, with or without data" {
    const walks = [_][]const dl.State{ &.{ .fetching, .fetched }, &.{ .fetching, .fetched, .post_processing } };
    for (walks) |walk| {
        for ([_]bool{ false, true }) |with_data| {
            var fx: Fixture = undefined;
            try fx.open();
            defer fx.close();
            _ = try fx.add("d1", 0);
            try fx.moveTo("d1", walk);
            try fx.expectRejected(fx.svc.delete(fx.a(), &fx.failure, "d1", with_data), .conflict, "download_busy");
            _ = try fx.rig.repo.get(fx.a(), "d1");
            try testing.expectEqual(@as(usize, 0), fx.remover.removed);
            try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
        }
    }
}

fn finishFetchOnHold(r: *FakeRemover) void {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    _ = r.repo.updateState(arena.allocator(), "d1", .{ .to = .fetched, .at = tu.epoch }) catch @panic("move failed");
}

test "delete with data refuses when the fetch finished before the hold took effect" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("d1", 0);
    try fx.moveTo("d1", &.{.fetching});
    fx.remover.on_hold = finishFetchOnHold;

    try fx.expectRejected(fx.svc.delete(fx.a(), &fx.failure, "d1", true), .conflict, "download_busy");
    try testing.expectEqual(@as(usize, 0), fx.remover.removed);
    try testing.expectEqual(@as(usize, 1), fx.remover.released);
    _ = try fx.rig.repo.get(fx.a(), "d1");
}

test "delete of an unknown Download is not found and publishes nothing" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    try testing.expectError(error.NotFound, fx.svc.delete(fx.a(), &fx.failure, "ghost", false));
    try testing.expectEqual(@as(usize, 0), fx.recorder.events.items.len);
    // The no-op remover (the default) lets delete with data succeed.
    _ = try fx.add("d1", 0);
    fx.svc.deps.remover = .noop;
    try fx.svc.delete(fx.a(), &fx.failure, "d1", true);
}

const DeleteJob = struct {
    svc: *Service,
    id: []const u8,
    result: ?anyerror = null,

    fn run(job: *DeleteJob) void {
        var arena: std.heap.ArenaAllocator = .init(testing.allocator);
        defer arena.deinit();
        var f: Failure = .{};
        job.svc.delete(arena.allocator(), &f, job.id, true) catch |err| {
            job.result = err;
        };
    }
};

test "while files are removed other Downloads can be paused, and a second delete is a conflict" {
    var fx: Fixture = undefined;
    try fx.open();
    defer fx.close();
    _ = try fx.add("big", 0);
    _ = try fx.add("other", 1000);
    var blocker: Blocker = .{};
    fx.remover.blocker = &blocker;
    fx.remover.on_remove = blockInRemove;

    var job: DeleteJob = .{ .svc = &fx.svc, .id = "big" };
    const thread = try std.Thread.spawn(.{}, DeleteJob.run, .{&job});
    blocker.entered.waitUncancelable(testing.io);

    // Pausing another Download does not wait for the file removal.
    const paused = try fx.svc.pause(fx.a(), &fx.failure, "other");
    try testing.expect(paused.paused);
    // A second delete of the same Download is a conflict.
    try fx.expectRejected(fx.svc.delete(fx.a(), &fx.failure, "big", true), .conflict, "download_deleting");

    blocker.go.set(testing.io);
    thread.join();
    try testing.expect(job.result == null);
    try testing.expectError(error.NotFound, fx.rig.repo.get(fx.a(), "big"));
    // After the first finished, the ID is free again (and the Download gone).
    try testing.expectError(error.NotFound, fx.svc.delete(fx.a(), &fx.failure, "big", false));
}

test "quote is Go's strconv.Quote" {
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    // Expected values are what Go's fmt.Sprintf("%q", s) prints.
    const cases = [_]struct { in: []const u8, want: []const u8 }{
        .{ .in = "plain", .want = "\"plain\"" },
        .{ .in = "a\"b\\c", .want = "\"a\\\"b\\\\c\"" },
        .{ .in = "tab\tnl\n", .want = "\"tab\\tnl\\n\"" },
        .{ .in = "\x00\x01\x7f", .want = "\"\\x00\\x01\\x7f\"" },
        .{ .in = "caf\xc3\xa9", .want = "\"caf\xc3\xa9\"" },
        .{ .in = "bad\xffbyte", .want = "\"bad\\xffbyte\"" },
        .{ .in = "\xe2\x80\xae", .want = "\"\\u202e\"" },
        .{ .in = "", .want = "\"\"" },
    };
    for (cases) |c| try testing.expectEqualStrings(c.want, try quote(arena.allocator(), c.in));
}

fn fuzzQuote(_: void, smith: *testing.Smith) !void {
    var buf: [64]u8 = undefined;
    const n = smith.slice(&buf);
    var arena: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena.deinit();
    const q = try quote(arena.allocator(), buf[0..n]);
    try testing.expect(q.len >= 2 and q[0] == '"' and q[q.len - 1] == '"');
    // Nothing that could break a log line or a terminal survives unescaped.
    for (q) |c| try testing.expect(c >= 0x20 and c != 0x7f);
}

test "fuzz: quote escapes any bytes" {
    try testing.fuzz({}, fuzzQuote, .{ .corpus = &.{
        "\x07\x00\x00\x00a\"b\\c\n",
        "\x04\x00\x00\x00\xff\xfe\x00\x7f",
        "\x03\x00\x00\x00\xe2\x80\xae",
    } });
}
