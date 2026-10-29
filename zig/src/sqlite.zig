//! A thin binding over the SQLite amalgamation (ADR-0014) and the database
//! handle of the daemon: one writer connection and a few readers, as in
//! internal/store/store.go. A database written by either side is valid for
//! the other because both use the same file format and the same pragmas
//! (WAL, foreign keys, busy timeout 5000 ms, synchronous NORMAL).
//!
//! Memory rules. SQLite owns the memory behind every column it returns, and
//! that memory is gone after the next step, reset or finalize. So the column
//! accessors copy into a caller-supplied allocator and never hand out a
//! pointer into SQLite; bound text and blobs are copied by SQLite too, so a
//! caller may free its buffer right after binding. A `Db` is one connection
//! and is not safe to share between threads: `Store` hands out connections
//! one user at a time.

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const c = @import("sqlite_c");

/// What went wrong, as a Zig error. The details (SQLite's own code and
/// message) are in `Db.failure()` after the call that returned it.
pub const Error = error{
    Busy,
    Locked,
    ReadOnly,
    Constraint,
    Corrupt,
    NotADatabase,
    CantOpen,
    DiskFull,
    IoError,
    TooBig,
    Misuse,
    OutOfMemory,
    PathTooLong,
    /// The SQL held no statement (empty, only a comment).
    EmptyStatement,
    /// Any other SQLite result code.
    Sqlite,
};

fn errorFromCode(rc: c_int) Error {
    return switch (rc & 0xff) {
        c.SQLITE_BUSY => error.Busy,
        c.SQLITE_LOCKED => error.Locked,
        c.SQLITE_READONLY => error.ReadOnly,
        c.SQLITE_CONSTRAINT => error.Constraint,
        c.SQLITE_CORRUPT => error.Corrupt,
        c.SQLITE_NOTADB => error.NotADatabase,
        c.SQLITE_CANTOPEN => error.CantOpen,
        c.SQLITE_FULL => error.DiskFull,
        c.SQLITE_IOERR => error.IoError,
        c.SQLITE_TOOBIG => error.TooBig,
        c.SQLITE_MISUSE => error.Misuse,
        c.SQLITE_NOMEM => error.OutOfMemory,
        else => error.Sqlite,
    };
}

/// SQLite's code and message of the last failed call on a connection, copied
/// out so it stays valid. `code` is the primary result code, `extended` the
/// extended one (for example SQLITE_CONSTRAINT_UNIQUE).
pub const Failure = struct {
    code: c_int = 0,
    extended: c_int = 0,
    msg_buf: [255]u8 = undefined,
    msg_len: u8 = 0,

    pub fn message(self: *const Failure) []const u8 {
        return self.msg_buf[0..self.msg_len];
    }

    /// The unique or primary key constraint failed, which the stores map to
    /// a conflict (store.mapErr in Go).
    pub fn isConflict(self: Failure) bool {
        return self.extended == c.SQLITE_CONSTRAINT_UNIQUE or self.extended == c.SQLITE_CONSTRAINT_PRIMARYKEY;
    }

    pub fn format(self: Failure, w: *std.Io.Writer) std.Io.Writer.Error!void {
        try w.print("{s} (SQLITE_{d})", .{ self.message(), self.extended });
    }
};

fn failureOf(handle: ?*c.sqlite3, rc: c_int) Failure {
    var f: Failure = .{ .code = rc & 0xff, .extended = rc };
    if (handle) |h| {
        f.extended = c.sqlite3_extended_errcode(h);
        f.code = f.extended & 0xff;
        const msg = std.mem.span(c.sqlite3_errmsg(h));
        const n = @min(msg.len, f.msg_buf.len);
        @memcpy(f.msg_buf[0..n], msg[0..n]);
        f.msg_len = @intCast(n);
    }
    return f;
}

pub const OpenOptions = struct {
    /// Create the file when it does not exist. Readers never create.
    create: bool = true,
    /// PRAGMA query_only: the connection refuses writes (the reader pool).
    query_only: bool = false,
    /// Switch to WAL, the journal mode of every bitnews database.
    wal: bool = true,
    busy_timeout_ms: c_int = 5000,
};

/// One connection. Not safe to use from two threads at once.
pub const Db = struct {
    handle: *c.sqlite3,

    /// Opens path (or ":memory:") and applies the pragmas of internal/store:
    /// journal_mode WAL, foreign_keys 1, busy_timeout, synchronous NORMAL.
    /// On failure `diag`, when given, receives SQLite's code and message.
    pub fn open(path: []const u8, opts: OpenOptions, diag: ?*Failure) Error!Db {
        var buf: [4096:0]u8 = undefined;
        if (path.len >= buf.len or std.mem.indexOfScalar(u8, path, 0) != null) return error.PathTooLong;
        @memcpy(buf[0..path.len], path);
        buf[path.len] = 0;

        var handle: ?*c.sqlite3 = null;
        const flags: c_int = c.SQLITE_OPEN_READWRITE | (if (opts.create) c.SQLITE_OPEN_CREATE else 0);
        const rc = c.sqlite3_open_v2(&buf, &handle, flags, null);
        if (rc != c.SQLITE_OK) {
            if (diag) |d| d.* = failureOf(handle, rc);
            _ = c.sqlite3_close(handle);
            return errorFromCode(rc);
        }
        var db: Db = .{ .handle = handle.? };
        errdefer db.close();
        errdefer if (diag) |d| {
            d.* = db.failure();
        };
        _ = c.sqlite3_busy_timeout(db.handle, opts.busy_timeout_ms);
        if (opts.wal) try db.exec("PRAGMA journal_mode = WAL");
        try db.exec("PRAGMA foreign_keys = 1");
        try db.exec("PRAGMA synchronous = NORMAL");
        if (opts.query_only) try db.exec("PRAGMA query_only = 1");
        return db;
    }

    pub fn close(self: *Db) void {
        _ = c.sqlite3_close(self.handle);
        self.* = undefined;
    }

    /// Code and message of the last call that failed on this connection.
    pub fn failure(self: *const Db) Failure {
        return failureOf(self.handle, c.sqlite3_errcode(self.handle));
    }

    fn check(self: *const Db, rc: c_int) Error!void {
        _ = self;
        if (rc != c.SQLITE_OK and rc != c.SQLITE_ROW and rc != c.SQLITE_DONE) return errorFromCode(rc);
    }

    /// Runs every statement in sql, without parameters and discarding rows.
    /// sql need not be NUL-terminated; it may hold many statements.
    pub fn exec(self: *Db, sql: []const u8) Error!void {
        var rest = sql;
        while (rest.len > 0) {
            var stmt = Stmt.prepareTail(self, rest, &rest) catch |err| switch (err) {
                error.EmptyStatement => continue,
                else => |e| return e,
            };
            defer stmt.finalize();
            while (try stmt.step()) {}
        }
    }

    /// Runs one statement with parameters (see `Stmt.bindAll`), discarding rows.
    pub fn execArgs(self: *Db, sql: []const u8, args: anytype) Error!void {
        var stmt = try Stmt.prepare(self, sql);
        defer stmt.finalize();
        try stmt.bindAll(args);
        while (try stmt.step()) {}
    }

    /// Rows changed by the last INSERT, UPDATE or DELETE.
    pub fn changes(self: *const Db) usize {
        return @intCast(c.sqlite3_changes64(self.handle));
    }

    pub const TxKind = enum { deferred, immediate };

    /// Starts a transaction; `Tx.deinit` rolls back unless `commit` ran. The
    /// writer connection always uses `.immediate`, so the write lock is taken
    /// when the transaction begins (Go: _txlock=immediate).
    pub fn begin(self: *Db, kind: TxKind) Error!Tx {
        try self.exec(switch (kind) {
            .deferred => "BEGIN",
            .immediate => "BEGIN IMMEDIATE",
        });
        return .{ .db = self };
    }
};

/// An open transaction. Use `defer tx.deinit()` right after `begin`.
pub const Tx = struct {
    db: *Db,
    done: bool = false,

    pub fn commit(self: *Tx) Error!void {
        std.debug.assert(!self.done);
        try self.db.exec("COMMIT");
        self.done = true;
    }

    /// Rolls back unless committed. Safe to call after a failed commit.
    pub fn deinit(self: *Tx) void {
        if (self.done) return;
        self.done = true;
        // The transaction may already be gone (SQLite rolls back by itself on
        // some errors); the failure of this ROLLBACK is then of no interest.
        self.db.exec("ROLLBACK") catch {};
    }
};

// SQLITE_TRANSIENT makes SQLite copy the bound bytes. The header encodes it as
// the pointer value -1 cast to a function pointer, which @ptrFromInt refuses
// (a function pointer must be aligned), so the bits are read back through a
// pointer instead.
fn transient() c.sqlite3_destructor_type {
    const bits: usize = std.math.maxInt(usize);
    return @as(*const c.sqlite3_destructor_type, @ptrCast(&bits)).*;
}

/// A blob parameter for `Stmt.bindAll` (a plain byte slice binds as text).
pub const Blob = struct { bytes: []const u8 };

/// A prepared statement. Finalize it before the connection is closed.
pub const Stmt = struct {
    handle: *c.sqlite3_stmt,

    /// Prepares the first statement of sql. Any text after it is ignored, so
    /// use `Db.exec` for scripts.
    pub fn prepare(db: *Db, sql: []const u8) Error!Stmt {
        var rest: []const u8 = undefined;
        return prepareTail(db, sql, &rest);
    }

    fn prepareTail(db: *Db, sql: []const u8, rest: *[]const u8) Error!Stmt {
        if (sql.len > std.math.maxInt(c_int)) return error.TooBig;
        var handle: ?*c.sqlite3_stmt = null;
        var tail: [*c]const u8 = null;
        const rc = c.sqlite3_prepare_v3(db.handle, sql.ptr, @intCast(sql.len), 0, &handle, &tail);
        // The tail points into sql; the length of what is left follows from it.
        const used = @intFromPtr(tail) - @intFromPtr(sql.ptr);
        rest.* = if (rc == c.SQLITE_OK and used <= sql.len) sql[used..] else sql[sql.len..];
        try db.check(rc);
        return .{ .handle = handle orelse return error.EmptyStatement };
    }

    pub fn finalize(self: *Stmt) void {
        _ = c.sqlite3_finalize(self.handle);
        self.* = undefined;
    }

    /// Makes the statement runnable again; bindings are kept.
    pub fn reset(self: *Stmt) void {
        _ = c.sqlite3_reset(self.handle);
    }

    pub fn clearBindings(self: *Stmt) void {
        _ = c.sqlite3_clear_bindings(self.handle);
    }

    /// Parameter indexes are 1-based, as in SQLite.
    pub fn bindText(self: *Stmt, index: u32, text: []const u8) Error!void {
        // A NULL pointer would bind NULL, and an empty slice may have one.
        const ptr: [*]const u8 = if (text.len == 0) "" else text.ptr;
        try checkRc(c.sqlite3_bind_text64(self.handle, @intCast(index), ptr, text.len, transient(), c.SQLITE_UTF8));
    }

    pub fn bindBlob(self: *Stmt, index: u32, bytes: []const u8) Error!void {
        if (bytes.len == 0) return checkRc(c.sqlite3_bind_zeroblob(self.handle, @intCast(index), 0));
        try checkRc(c.sqlite3_bind_blob64(self.handle, @intCast(index), bytes.ptr, bytes.len, transient()));
    }

    pub fn bindInt(self: *Stmt, index: u32, value: i64) Error!void {
        try checkRc(c.sqlite3_bind_int64(self.handle, @intCast(index), value));
    }

    pub fn bindNull(self: *Stmt, index: u32) Error!void {
        try checkRc(c.sqlite3_bind_null(self.handle, @intCast(index)));
    }

    /// Binds a tuple to parameters 1..n: integers, bool, `[]const u8` (text),
    /// `Blob`, null, and optionals of those.
    pub fn bindAll(self: *Stmt, args: anytype) Error!void {
        inline for (args, 1..) |arg, i| try self.bindValue(i, arg);
    }

    fn bindValue(self: *Stmt, index: u32, value: anytype) Error!void {
        const T = @TypeOf(value);
        switch (@typeInfo(T)) {
            .null => try self.bindNull(index),
            .optional => if (value) |v| try self.bindValue(index, v) else try self.bindNull(index),
            .int, .comptime_int => try self.bindInt(index, @intCast(value)),
            .bool => try self.bindInt(index, @intFromBool(value)),
            .pointer => try self.bindText(index, value),
            .array => try self.bindText(index, &value),
            .@"struct" => if (T == Blob) try self.bindBlob(index, value.bytes) else @compileError("cannot bind " ++ @typeName(T)),
            else => @compileError("cannot bind " ++ @typeName(T)),
        }
    }

    /// Runs the statement to its next row. True: a row is ready to read.
    /// False: the statement is done.
    pub fn step(self: *Stmt) Error!bool {
        const rc = c.sqlite3_step(self.handle);
        switch (rc) {
            c.SQLITE_ROW => return true,
            c.SQLITE_DONE => return false,
            else => {
                // Prepared with v3, so the specific code is already in rc;
                // reset releases the locks of a failed statement.
                _ = c.sqlite3_reset(self.handle);
                return errorFromCode(rc);
            },
        }
    }

    pub const ColumnType = enum { integer, float, text, blob, null };

    /// Column indexes are 0-based.
    pub fn columnType(self: *Stmt, index: u32) ColumnType {
        return switch (c.sqlite3_column_type(self.handle, @intCast(index))) {
            c.SQLITE_INTEGER => .integer,
            c.SQLITE_FLOAT => .float,
            c.SQLITE_TEXT => .text,
            c.SQLITE_BLOB => .blob,
            else => .null,
        };
    }

    pub fn columnIsNull(self: *Stmt, index: u32) bool {
        return self.columnType(index) == .null;
    }

    pub fn columnInt(self: *Stmt, index: u32) i64 {
        return c.sqlite3_column_int64(self.handle, @intCast(index));
    }

    /// A copy of the text (or of the bytes of a blob) owned by the caller.
    /// NULL gives an empty slice; test `columnIsNull` first when it matters.
    pub fn columnText(self: *Stmt, gpa: Allocator, index: u32) Allocator.Error![]u8 {
        return self.copyColumn(gpa, index, true);
    }

    pub fn columnBlob(self: *Stmt, gpa: Allocator, index: u32) Allocator.Error![]u8 {
        return self.copyColumn(gpa, index, false);
    }

    fn copyColumn(self: *Stmt, gpa: Allocator, index: u32, text: bool) Allocator.Error![]u8 {
        const i: c_int = @intCast(index);
        // Fetch the pointer first, then the length: the order SQLite documents
        // for text and blobs.
        const ptr: ?[*]const u8 = if (text)
            @ptrCast(c.sqlite3_column_text(self.handle, i))
        else
            @ptrCast(c.sqlite3_column_blob(self.handle, i));
        const len: usize = @intCast(c.sqlite3_column_bytes(self.handle, i));
        if (ptr == null) {
            // NULL or an empty value; SQLite also returns NULL when it ran out
            // of memory, which it reports on the connection.
            if (c.sqlite3_errcode(c.sqlite3_db_handle(self.handle)) == c.SQLITE_NOMEM) return error.OutOfMemory;
            return gpa.alloc(u8, 0);
        }
        return gpa.dupe(u8, ptr.?[0..len]);
    }
};

fn checkRc(rc: c_int) Error!void {
    if (rc != c.SQLITE_OK) return errorFromCode(rc);
}

/// Options of `Store.open`.
pub const StoreOptions = struct {
    /// Reader connections; Go uses max(4, NumCPU).
    readers: ?usize = null,
};

/// The database of the daemon: one writer connection behind a mutex, and a
/// pool of read-only connections. In WAL mode readers are never blocked by the
/// writer. Owner: whoever called `open`; `close` once, after all work ended.
///
/// A `Store` lives on the heap so the pointer stays valid while connections
/// are lent out.
pub const Store = struct {
    gpa: Allocator,
    io: Io,
    writer_db: Db,
    writer_lock: Io.Mutex = .init,
    readers: []Db,
    /// Which readers are in use; guarded by pool_lock. The semaphore counts
    /// the free ones, so waiting for a reader never spins.
    busy: []bool,
    pool_lock: Io.Mutex = .init,
    free_readers: Io.Semaphore,
    /// Set by `open` when an existing file was readable by group or others
    /// (the mode, for the caller to log as a warning; Go: ensureFile).
    broad_permissions: ?u32 = null,

    /// Opens the database at path, creating the file with mode 0600 when it
    /// does not exist, and opens the connections. Applying migrations is the
    /// caller's job (`migrate.run` on `writer`), so that the schema code stays
    /// out of this file.
    pub fn open(gpa: Allocator, io: Io, path: []const u8, opts: StoreOptions, diag: ?*Failure) !*Store {
        const broad = try ensureFile(io, path);

        const self = try gpa.create(Store);
        errdefer gpa.destroy(self);
        const n_readers = opts.readers orelse @max(4, std.Thread.getCpuCount() catch 4);
        const readers = try gpa.alloc(Db, n_readers);
        errdefer gpa.free(readers);
        const busy = try gpa.alloc(bool, n_readers);
        errdefer gpa.free(busy);
        @memset(busy, false);

        var w = try Db.open(path, .{}, diag);
        errdefer w.close();
        self.* = .{
            .gpa = gpa,
            .io = io,
            .writer_db = w,
            .readers = readers,
            .busy = busy,
            .free_readers = .{ .permits = n_readers },
            .broad_permissions = broad,
        };
        // Readers are opened after the writer so a new file is already WAL.
        var opened: usize = 0;
        errdefer for (readers[0..opened]) |*r| r.close();
        while (opened < n_readers) : (opened += 1) {
            readers[opened] = try Db.open(path, .{ .create = false, .query_only = true }, diag);
        }
        return self;
    }

    pub fn close(self: *Store) void {
        for (self.readers) |*r| r.close();
        self.writer_db.close();
        const gpa = self.gpa;
        gpa.free(self.readers);
        gpa.free(self.busy);
        gpa.destroy(self);
    }

    /// The writer connection, held until `release`. All writes go through it.
    pub const WriteGuard = struct {
        store: *Store,
        db: *Db,

        pub fn release(self: WriteGuard) void {
            self.store.writer_lock.unlock(self.store.io);
        }
    };

    pub fn acquireWriter(self: *Store) Io.Cancelable!WriteGuard {
        try self.writer_lock.lock(self.io);
        return .{ .store = self, .db = &self.writer_db };
    }

    /// A reader connection, held until `release`.
    pub const ReadGuard = struct {
        store: *Store,
        db: *Db,
        index: usize,

        pub fn release(self: ReadGuard) void {
            const s = self.store;
            s.pool_lock.lockUncancelable(s.io);
            s.busy[self.index] = false;
            s.pool_lock.unlock(s.io);
            s.free_readers.post(s.io);
        }
    };

    pub fn acquireReader(self: *Store) Io.Cancelable!ReadGuard {
        try self.free_readers.wait(self.io);
        self.pool_lock.lockUncancelable(self.io);
        defer self.pool_lock.unlock(self.io);
        // The semaphore guarantees one is free.
        for (self.busy, 0..) |*b, i| {
            if (!b.*) {
                b.* = true;
                return .{ .store = self, .db = &self.readers[i], .index = i };
            }
        }
        unreachable;
    }
};

/// Creates an empty file with mode 0600 before SQLite sees it, so the
/// database is never world-readable. Returns the mode of an existing file
/// that is broader than 0600, else null (Go: ensureFile).
fn ensureFile(io: Io, path: []const u8) !?u32 {
    if (std.mem.eql(u8, path, ":memory:")) return null;
    const cwd = Io.Dir.cwd();
    if (cwd.statFile(io, path, .{})) |st| {
        if (@import("builtin").os.tag == .windows) return null;
        const mode: u32 = @intCast(st.permissions.toMode() & 0o777);
        return if (mode & 0o077 != 0) mode else null;
    } else |err| switch (err) {
        error.FileNotFound => {},
        else => |e| return e,
    }
    const f = cwd.createFile(io, path, .{ .truncate = false, .permissions = .fromMode(0o600) }) catch |err| switch (err) {
        // Created by someone else between the stat and now: fine.
        error.PathAlreadyExists => return null,
        else => |e| return e,
    };
    f.close(io);
    return null;
}

// ---- tests ----

const testing = std.testing;

test "exec, bind and read back through the public API" {
    var db = try Db.open(":memory:", .{}, null);
    defer db.close();
    try db.exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, data BLOB, n INTEGER) STRICT");
    try db.execArgs("INSERT INTO t VALUES (?, ?, ?, ?)", .{ 1, "alpha", Blob{ .bytes = &.{ 0, 1, 2 } }, null });
    try db.execArgs("INSERT INTO t VALUES (?, ?, ?, ?)", .{ 2, "", Blob{ .bytes = "" }, @as(?i64, 7) });
    try testing.expectEqual(@as(usize, 1), db.changes());

    var st = try Stmt.prepare(&db, "SELECT id, name, data, n FROM t ORDER BY id");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 1), st.columnInt(0));
    const name = try st.columnText(testing.allocator, 1);
    defer testing.allocator.free(name);
    try testing.expectEqualStrings("alpha", name);
    const blob = try st.columnBlob(testing.allocator, 2);
    defer testing.allocator.free(blob);
    try testing.expectEqualSlices(u8, &.{ 0, 1, 2 }, blob);
    try testing.expect(st.columnIsNull(3));
    try testing.expect(try st.step());
    const empty = try st.columnText(testing.allocator, 1);
    defer testing.allocator.free(empty);
    try testing.expectEqual(@as(usize, 0), empty.len);
    try testing.expect(!st.columnIsNull(1)); // an empty string is not NULL
    try testing.expect(!st.columnIsNull(2));
    try testing.expectEqual(Stmt.ColumnType.integer, st.columnType(3));
    try testing.expect(!try st.step());
}

test "a failure carries SQLite's code and message" {
    var db = try Db.open(":memory:", .{}, null);
    defer db.close();
    try db.exec("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE) STRICT");
    try db.execArgs("INSERT INTO t VALUES (1, 'a')", .{});
    try testing.expectError(error.Constraint, db.execArgs("INSERT INTO t VALUES (2, 'a')", .{}));
    const f = db.failure();
    try testing.expect(f.isConflict());
    try testing.expect(std.mem.indexOf(u8, f.message(), "UNIQUE") != null);
    try testing.expectError(error.Sqlite, db.exec("SELEKT 1"));
    try testing.expect(std.mem.indexOf(u8, db.failure().message(), "syntax error") != null);
    try testing.expectError(error.EmptyStatement, Stmt.prepare(&db, "-- nothing"));
}

test "a transaction commits, and rolls back unless committed" {
    var db = try Db.open(":memory:", .{}, null);
    defer db.close();
    try db.exec("CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT");
    {
        var tx = try db.begin(.immediate);
        defer tx.deinit();
        try db.exec("INSERT INTO t VALUES (1)");
        try tx.commit();
    }
    {
        var tx = try db.begin(.immediate);
        defer tx.deinit();
        try db.exec("INSERT INTO t VALUES (2)");
    }
    var st = try Stmt.prepare(&db, "SELECT count(*) FROM t");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 1), st.columnInt(0));
}

test "foreign keys are enforced and a reader refuses writes" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &buf);
    const path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..dir_len]});
    defer testing.allocator.free(path);

    const store = try Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
    defer store.close();
    try testing.expect(store.broad_permissions == null);

    const w = try store.acquireWriter();
    try w.db.exec("CREATE TABLE p (id INTEGER PRIMARY KEY) STRICT; CREATE TABLE ch (id INTEGER PRIMARY KEY, p INTEGER NOT NULL REFERENCES p(id)) STRICT");
    try testing.expectError(error.Constraint, w.db.exec("INSERT INTO ch VALUES (1, 99)"));
    w.release();

    const r = try store.acquireReader();
    defer r.release();
    try testing.expectError(error.ReadOnly, r.db.exec("INSERT INTO p VALUES (1)"));
}

fn pragmaText(db: *Db, sql: []const u8) ![]u8 {
    var st = try Stmt.prepare(db, sql);
    defer st.finalize();
    try testing.expect(try st.step());
    return st.columnText(testing.allocator, 0);
}

test "every connection has the pragmas of the Go store" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &buf);
    const path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..dir_len]});
    defer testing.allocator.free(path);
    const store = try Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
    defer store.close();

    const want = [_]struct { sql: []const u8, value: []const u8 }{
        .{ .sql = "PRAGMA journal_mode", .value = "wal" },
        .{ .sql = "PRAGMA foreign_keys", .value = "1" },
        .{ .sql = "PRAGMA busy_timeout", .value = "5000" },
        .{ .sql = "PRAGMA synchronous", .value = "1" },
    };
    const w = try store.acquireWriter();
    defer w.release();
    const r1 = try store.acquireReader();
    defer r1.release();
    const r2 = try store.acquireReader();
    defer r2.release();
    for ([_]*Db{ w.db, r1.db, r2.db }) |db| {
        for (want) |p| {
            const got = try pragmaText(db, p.sql);
            defer testing.allocator.free(got);
            try testing.expectEqualStrings(p.value, got);
        }
    }
}

test "a new database file has mode 0600 and a broader existing one is reported" {
    if (@import("builtin").os.tag == .windows) return error.SkipZigTest;
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &buf);
    const path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..dir_len]});
    defer testing.allocator.free(path);

    {
        const store = try Store.open(testing.allocator, testing.io, path, .{ .readers = 1 }, null);
        defer store.close();
        try testing.expect(store.broad_permissions == null);
    }
    const st = try tmp.dir.statFile(testing.io, "test.db", .{});
    try testing.expectEqual(@as(u32, 0o600), @as(u32, @intCast(st.permissions.toMode() & 0o777)));

    var f = try tmp.dir.openFile(testing.io, "test.db", .{ .mode = .read_write });
    try f.setPermissions(testing.io, .fromMode(0o644));
    f.close(testing.io);
    const store = try Store.open(testing.allocator, testing.io, path, .{ .readers = 1 }, null);
    defer store.close();
    try testing.expectEqual(@as(?u32, 0o644), store.broad_permissions);
}

test "readers are not blocked while the writer holds a transaction" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &buf);
    const path = try std.fmt.allocPrint(testing.allocator, "{s}/test.db", .{buf[0..dir_len]});
    defer testing.allocator.free(path);
    const store = try Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
    defer store.close();

    const w = try store.acquireWriter();
    defer w.release();
    try w.db.exec("CREATE TABLE t (id INTEGER PRIMARY KEY) STRICT; INSERT INTO t VALUES (1)");
    var tx = try w.db.begin(.immediate);
    defer tx.deinit();
    try w.db.exec("INSERT INTO t VALUES (2)");

    const r = try store.acquireReader();
    defer r.release();
    var st = try Stmt.prepare(r.db, "SELECT count(*) FROM t");
    defer st.finalize();
    try testing.expect(try st.step());
    try testing.expectEqual(@as(i64, 1), st.columnInt(0)); // the uncommitted row is invisible
}
