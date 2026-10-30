//! Writes decoded yEnc parts straight into pre-allocated files inside one
//! Download directory and keeps per-file range bookkeeping
//! (internal/usenet/filewriter).
//!
//! A `Writer` is created for one directory. Files are identified by a
//! caller-supplied key (the index of the file within the NZB), because yEnc
//! names are hostile and may differ between the parts of one file. The final
//! on-disk name is chosen from the first part's name through
//! `fsutil.safeName` and `fsutil.unique`, and every file is created and
//! opened relative to a handle of the directory, held open for the Writer's
//! lifetime, with symlinks refused: nothing is ever written outside it, not
//! even when the directory or a file in it is swapped for a symlink
//! meanwhile.
//!
//! Byte ranges are half-open and zero-based: `Range{ .start, .end }` covers
//! the offsets start <= i < end. Per file the Writer tracks which ranges were
//! written correctly (done), written from a part whose CRC32 did not match
//! (damaged), and permanently unavailable (lost, set with `markMissing`).
//! Missing is everything neither done nor damaged. The Writer never hashes a
//! whole file and never reads PAR2 data: verifying and repairing is
//! par2cmdline-turbo's job.
//!
//! Pre-allocation uses `setLength`, which creates a sparse file on every
//! supported file system. fallocate is deliberately not used: it is not
//! portable and a sparse file is enough for the byte-exact result.
//!
//! fsync policy: written bytes are fsynced when a file is evicted from the
//! pool of open files, by `finish`, `sync`, `snapshot` (so a snapshot never
//! claims bytes that are not durable) and `close`; the directory is fsynced
//! after files were created. A failure of an fsync during an eviction is
//! remembered and reported by the next `finish`, `sync`, `snapshot` or
//! `close` of that file.
//!
//! Concurrency: a Writer is safe to share between threads. Calls for
//! different keys run in parallel; calls for one key are serialized, and no
//! lock shared between keys is held during file I/O except while a new file
//! is being named and created. At most `max_open_files` files are open
//! (an LRU; a file that is busy is skipped, so the bound is best effort
//! under heavy contention); an evicted file is reopened on demand.
//!
//! Memory: the Writer owns everything it keeps (`gpa`), freed by `deinit`.
//! Anything it returns that is not a plain value (`ranges`, `snapshot`,
//! `restore`) is allocated in the arena the caller passes. Per-call scratch
//! is a short-lived arena on `gpa`.
//!
//! Permission errors: a refused write comes back as `error.AccessDenied` or
//! `error.PermissionDenied`; the user-facing text names the folder and the
//! user id (`fsutil.writeErrorDetail(a, w.dirPath(), fsutil.effectiveUid())`,
//! or `fsutil.deepestExistingParent` for a failed `init`).
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const yenc_types = @import("yenc_types.zig");
const fsutil = @import("fsutil.zig");
const ranges_mod = @import("filewriter_ranges.zig");
const snapshot_mod = @import("filewriter_snapshot.zig");

pub const Range = ranges_mod.Range;
pub const Part = yenc_types.Part;
pub const sum = ranges_mod.sum;
pub const max_file_size = snapshot_mod.max_file_size;
pub const max_key = snapshot_mod.max_key;

/// The default bound on simultaneously open files.
pub const default_max_open_files = 64;

/// Bounds the retries when a chosen name is taken by a racing creator.
const max_create_tries = 100;

pub const Error = error{
    /// A part announces a file size that differs from the one recorded for
    /// its key (Go: ErrSizeMismatch).
    SizeMismatch,
    /// A part whose offsets, size or data length are inconsistent, a key that
    /// does not fit 31 bits, or a range outside the file (ErrInvalidPart).
    InvalidPart,
    /// A part announcing an absurd file size, over 1 TiB (ErrTooLarge).
    TooLarge,
    /// A key that has no file yet (ErrUnknownFile).
    UnknownFile,
    /// The Writer is closed (ErrClosed).
    Closed,
    /// `restore` got a malformed snapshot (ErrInvalidSnapshot).
    InvalidSnapshot,
    /// `restore` on a Writer that already has files (ErrNotEmpty).
    NotEmpty,
    /// The name on disk is not the regular file this Writer created (a
    /// special file, or something swapped in): refused (Go: ErrSymlink).
    UnsafeFile,
    /// No free name was found after many tries (an attacker racing the
    /// Writer in its directory).
    NoFreeName,
    /// `init` with an empty directory.
    EmptyDirectory,
};

pub const Options = struct {
    /// How many files the Writer keeps open at once. 0 means the default.
    max_open_files: usize = default_max_open_files,
};

/// A point-in-time copy of the bookkeeping of one file. All range lists are
/// sorted, disjoint and non-adjacent, half-open and zero-based. The slices
/// live in the arena passed to `Writer.ranges`.
pub const FileState = struct {
    key: u32,
    /// The final on-disk name inside the Writer's directory.
    name: []const u8,
    size: i64,
    /// Ranges written from a part whose CRC32 matched (or was not announced).
    done: []const Range,
    /// Ranges written from a part whose CRC32 did not match.
    damaged: []const Range,
    /// Every byte that is neither done nor damaged.
    missing: []const Range,
    /// The subset of `missing` that `markMissing` declared permanently
    /// unavailable.
    lost: []const Range,
    /// `missing` minus `lost`: bytes that may still arrive.
    pending: []const Range,

    /// Whether every byte is done: nothing is missing or damaged.
    pub fn complete(s: FileState) bool {
        return s.missing.len == 0 and s.damaged.len == 0;
    }

    pub fn doneBytes(s: FileState) i64 {
        return sum(s.done);
    }

    pub fn damagedBytes(s: FileState) i64 {
        return sum(s.damaged);
    }

    pub fn missingBytes(s: FileState) i64 {
        return sum(s.missing);
    }
};

/// The state of one file. `mu` guards every field except `lru` and `in_lru`,
/// which the pool's lock guards.
const Entry = struct {
    mu: Io.Mutex = .init,
    key: u32,
    /// The final on-disk name (gpa); empty until the file exists.
    name: []u8 = &.{},
    size: i64 = 0,
    t: ranges_mod.Tracker = .{},

    file: ?Io.File = null,
    /// Written since the last fsync.
    dirty: bool = false,
    /// An fsync error from an eviction, reported later.
    pending: ?Io.File.SyncError = null,

    lru: std.DoublyLinkedList.Node = .{},
    in_lru: bool = false,

    fn fromNode(n: *std.DoublyLinkedList.Node) *Entry {
        return @fieldParentPtr("lru", n);
    }
};

pub const Writer = struct {
    gpa: Allocator,
    io: Io,
    /// The directory held open: every file is opened through it.
    dir: Io.Dir,
    /// The path the Writer was created with (gpa), for error messages.
    dir_path: []u8,
    max_open: usize,

    closed: std.atomic.Value(bool) = .init(false),
    dir_dirty: std.atomic.Value(bool) = .init(false),

    /// Guards `entries`.
    mu: Io.Mutex = .init,
    entries: std.AutoHashMapUnmanaged(u32, *Entry) = .empty,

    /// Serializes file creation; guards `names`: every name this Writer ever
    /// created (keys are gpa-owned), so a name that was deleted again is not
    /// handed out twice.
    name_mu: Io.Mutex = .init,
    names: std.StringHashMapUnmanaged(void) = .empty,

    /// Guards the LRU of open files: `lru` (most recently used first) and
    /// `lru_len`.
    pool_mu: Io.Mutex = .init,
    lru: std.DoublyLinkedList = .{},
    lru_len: usize = 0,

    /// Creates the Writer for `dir_path`, creating the directory (and its
    /// parents) if needed, and opens a handle to it. Free with `deinit`.
    pub fn init(gpa: Allocator, io: Io, dir_path: []const u8, options: Options) !*Writer {
        if (dir_path.len == 0) return error.EmptyDirectory;
        const cwd = Io.Dir.cwd();
        try cwd.createDirPath(io, dir_path);
        var dir = try cwd.openDir(io, dir_path, .{ .access_sub_paths = true });
        errdefer dir.close(io);
        const path_copy = try gpa.dupe(u8, dir_path);
        errdefer gpa.free(path_copy);
        const w = try gpa.create(Writer);
        w.* = .{
            .gpa = gpa,
            .io = io,
            .dir = dir,
            .dir_path = path_copy,
            .max_open = if (options.max_open_files == 0) default_max_open_files else options.max_open_files,
        };
        return w;
    }

    /// Closes the Writer (errors ignored: call `close` first to see them) and
    /// frees everything. No other call may be in flight.
    pub fn deinit(w: *Writer) void {
        w.close() catch {};
        var it = w.entries.valueIterator();
        while (it.next()) |e| {
            w.freeEntry(e.*);
        }
        w.entries.deinit(w.gpa);
        var nit = w.names.keyIterator();
        while (nit.next()) |k| w.gpa.free(k.*);
        w.names.deinit(w.gpa);
        w.gpa.free(w.dir_path);
        w.gpa.destroy(w);
    }

    /// The path the Writer was created with.
    pub fn dirPath(w: *const Writer) []const u8 {
        return w.dir_path;
    }

    fn validatePart(p: Part) Error!void {
        if (p.size < 1) return error.InvalidPart;
        if (p.size > max_file_size) return error.TooLarge;
        if (p.begin < 1 or p.end < p.begin or p.end > p.size) return error.InvalidPart;
        if (@as(i64, @intCast(p.data.len)) != p.end - p.begin + 1) return error.InvalidPart;
    }

    /// Writes `part` into the file identified by `key`, creating and
    /// pre-allocating the file on the first part.
    ///
    /// A part whose announced CRC32 does not match is written, but only over
    /// bytes that do not already hold a good copy, and is recorded as
    /// damaged. A good part overwrites its range and clears damaged state
    /// there. Duplicate parts are therefore idempotent. On error the
    /// bookkeeping is unchanged. `part.data` is read, not kept.
    pub fn writePart(w: *Writer, key: u32, part: Part) !void {
        try validatePart(part);
        if (key > max_key) return error.InvalidPart;
        const e = try w.entry(key);
        e.mu.lockUncancelable(w.io);
        defer e.mu.unlock(w.io);
        if (w.closed.load(.acquire)) return error.Closed;

        if (e.name.len == 0) {
            try w.create(e, part);
        } else if (part.size != e.size) {
            return error.SizeMismatch;
        }

        const f = try w.handle(e);
        const r: Range = .{ .start = part.begin - 1, .end = part.end };
        if (part.has_expected_crc and !part.crcMatch()) {
            var scratch: std.heap.ArenaAllocator = .init(w.gpa);
            defer scratch.deinit();
            var pieces: std.ArrayList(Range) = .empty;
            try e.t.damageable(scratch.allocator(), r, &pieces);
            try e.t.reserve(w.gpa, pieces.items.len);
            for (pieces.items) |p| {
                try f.writePositionalAll(w.io, part.data[@intCast(p.start - r.start)..@intCast(p.end - r.start)], @intCast(p.start));
                e.dirty = true;
            }
            e.t.markDamaged(pieces.items);
            return;
        }
        try e.t.reserve(w.gpa, 0);
        try f.writePositionalAll(w.io, part.data, @intCast(r.start));
        e.dirty = true;
        e.t.markGood(r);
    }

    /// The entry for `key`, created empty if there is none.
    fn entry(w: *Writer, key: u32) Allocator.Error!*Entry {
        w.mu.lockUncancelable(w.io);
        defer w.mu.unlock(w.io);
        const slot = try w.entries.getOrPut(w.gpa, key);
        if (slot.found_existing) return slot.value_ptr.*;
        const e = w.gpa.create(Entry) catch |err| {
            _ = w.entries.remove(key);
            return err;
        };
        e.* = .{ .key = key };
        slot.value_ptr.* = e;
        return e;
    }

    fn lookup(w: *Writer, key: u32) ?*Entry {
        w.mu.lockUncancelable(w.io);
        defer w.mu.unlock(w.io);
        return w.entries.get(key);
    }

    /// The entries in key order, allocated in `a`.
    fn sortedEntries(w: *Writer, a: Allocator) Allocator.Error![]*Entry {
        w.mu.lockUncancelable(w.io);
        defer w.mu.unlock(w.io);
        const out = try a.alloc(*Entry, w.entries.count());
        var i: usize = 0;
        var it = w.entries.valueIterator();
        while (it.next()) |e| : (i += 1) out[i] = e.*;
        std.mem.sort(*Entry, out, {}, struct {
            fn less(_: void, x: *Entry, y: *Entry) bool {
                return x.key < y.key;
            }
        }.less);
        return out;
    }

    // ---- creating and opening files ----

    /// Picks a safe, unique name for `e`, creates and pre-allocates the file
    /// and leaves it open. The caller holds `e.mu`.
    fn create(w: *Writer, e: *Entry, part: Part) !void {
        w.name_mu.lockUncancelable(w.io);
        const made = w.createFile(e.key, part.name);
        w.name_mu.unlock(w.io);
        const created = try made;
        errdefer w.gpa.free(created.name);

        // Sparse pre-allocation; see the module documentation.
        created.file.setLength(w.io, @intCast(part.size)) catch |err| {
            created.file.close(w.io);
            w.discard(created.name);
            return err;
        };
        w.dir_dirty.store(true, .release);

        e.t.deinit(w.gpa);
        e.t = .{};
        e.name = created.name;
        e.size = part.size;
        e.file = created.file;
        e.dirty = false;
        w.use(e);
        w.evict(e);
    }

    /// Removes a file this Writer just created, and frees its name.
    fn discard(w: *Writer, name: []const u8) void {
        w.dir.deleteFile(w.io, name) catch {};
        w.name_mu.lockUncancelable(w.io);
        defer w.name_mu.unlock(w.io);
        if (w.names.fetchRemove(name)) |kv| w.gpa.free(kv.key);
    }

    const Created = struct { name: []u8, file: Io.File };

    /// Chooses the final name and creates the file exclusively. The caller
    /// holds `name_mu`. `name` is gpa-owned.
    fn createFile(w: *Writer, key: u32, raw: []const u8) !Created {
        var scratch: std.heap.ArenaAllocator = .init(w.gpa);
        defer scratch.deinit();
        const a = scratch.allocator();

        var base: []const u8 = fsutil.safeName(a, raw) catch |err| switch (err) {
            error.InvalidName => try std.fmt.allocPrint(a, "file-{d}.bin", .{key}),
            error.OutOfMemory => return error.OutOfMemory,
        };
        var attempt: usize = 0;
        while (attempt < max_create_tries) : (attempt += 1) {
            // `base` comes from safeName, so it has no separators. `unique`
            // looks at the entry itself (no symlink following), so a planted
            // symlink counts as taken.
            const name = try fsutil.unique(a, w.io, w.dir, base);
            if (w.names.contains(name)) {
                // Taken by a file that is not on disk right now.
                base = try std.fmt.allocPrint(a, "file-{d}-{d}.bin", .{ key, attempt });
                continue;
            }
            const f = w.dir.createFile(w.io, name, .{ .read = true, .truncate = false, .exclusive = true }) catch |err| switch (err) {
                error.PathAlreadyExists => continue,
                else => |e| return e,
            };
            errdefer f.close(w.io);
            const key_copy = try w.gpa.dupe(u8, name);
            errdefer w.gpa.free(key_copy);
            const name_copy = try w.gpa.dupe(u8, name);
            errdefer w.gpa.free(name_copy);
            try w.names.put(w.gpa, key_copy, {});
            return .{ .name = name_copy, .file = f };
        }
        return error.NoFreeName;
    }

    /// The open file of `e`, reopening it by its recorded name if it was
    /// evicted or finished. It never follows a symlink. The caller holds
    /// `e.mu`.
    fn handle(w: *Writer, e: *Entry) !Io.File {
        if (e.file == null) e.file = try w.openExisting(e.name);
        w.use(e);
        w.evict(e);
        return e.file.?;
    }

    /// Opens an existing regular file inside the directory for reading and
    /// writing, refusing symlinks and special files. The final component is
    /// opened without following symlinks; the checks before and after
    /// opening refuse anything that is not a plain file (a FIFO would block
    /// the open).
    fn openExisting(w: *Writer, name: []const u8) !Io.File {
        if (!isPlainName(name)) return error.UnsafeFile;
        const st = try w.dir.statFile(w.io, name, .{ .follow_symlinks = false });
        switch (st.kind) {
            .file => {},
            .sym_link => return error.SymLinkLoop,
            .directory => return error.IsDir,
            else => return error.UnsafeFile,
        }
        const f = try w.dir.openFile(w.io, name, .{ .mode = .read_write, .follow_symlinks = false, .allow_directory = false });
        errdefer f.close(w.io);
        const after = try f.stat(w.io);
        if (after.kind != .file) return error.UnsafeFile;
        return f;
    }

    // ---- the pool of open files ----

    /// Marks `e` as open and most recently used. The caller holds `e.mu`.
    fn use(w: *Writer, e: *Entry) void {
        w.pool_mu.lockUncancelable(w.io);
        defer w.pool_mu.unlock(w.io);
        if (e.in_lru) {
            w.lru.remove(&e.lru);
        } else {
            e.in_lru = true;
            w.lru_len += 1;
        }
        w.lru.prepend(&e.lru);
    }

    /// Removes `e` from the LRU. The caller holds `e.mu`.
    fn drop(w: *Writer, e: *Entry) void {
        w.pool_mu.lockUncancelable(w.io);
        defer w.pool_mu.unlock(w.io);
        if (e.in_lru) {
            w.lru.remove(&e.lru);
            e.in_lru = false;
            w.lru_len -= 1;
        }
    }

    /// The least recently used entry other than `keep`, after skipping
    /// `skip` of them, if the pool is over its bound.
    fn victim(w: *Writer, keep: *Entry, skip: usize) ?*Entry {
        w.pool_mu.lockUncancelable(w.io);
        defer w.pool_mu.unlock(w.io);
        if (w.lru_len <= w.max_open) return null;
        var seen: usize = 0;
        var node = w.lru.last;
        while (node) |n| : (node = n.prev) {
            const c = Entry.fromNode(n);
            if (c == keep) continue;
            if (seen < skip) {
                seen += 1;
                continue;
            }
            return c;
        }
        return null;
    }

    /// Closes least recently used files, skipping any that are busy, until
    /// the pool is back within its bound. An entry is evicted only when its
    /// lock can be taken without waiting, so a handle is never closed under a
    /// writer that is using it, and eviction cannot deadlock with a thread
    /// that holds another entry's lock. The caller holds `keep.mu`.
    fn evict(w: *Writer, keep: *Entry) void {
        var skip: usize = 0;
        while (w.victim(keep, skip)) |c| {
            if (!c.mu.tryLock()) {
                skip += 1;
                continue;
            }
            defer c.mu.unlock(w.io);
            if (c.file != null) {
                w.closeEntry(c) catch |err| {
                    if (c.pending == null) c.pending = err;
                };
            }
        }
    }

    /// fsyncs (when dirty) and closes the handle of `e`. The caller holds
    /// `e.mu`. Returns the first fsync error, including one remembered from
    /// an eviction.
    fn closeEntry(w: *Writer, e: *Entry) Io.File.SyncError!void {
        var err: ?Io.File.SyncError = null;
        if (e.file) |f| {
            if (e.dirty) {
                f.sync(w.io) catch |se| {
                    err = se;
                };
            }
            f.close(w.io);
            if (err == null) e.dirty = false;
            e.file = null;
        }
        w.drop(e);
        const remembered = e.pending;
        e.pending = null;
        if (remembered) |p| return p;
        if (err) |x| return x;
    }

    /// fsyncs `e` if it has unsynced writes. The caller holds `e.mu`.
    fn syncEntry(w: *Writer, e: *Entry) Io.File.SyncError!void {
        const pending = e.pending;
        e.pending = null;
        if (e.file) |f| {
            if (e.dirty) {
                f.sync(w.io) catch |se| return pending orelse se;
                e.dirty = false;
            }
        }
        if (pending) |p| return p;
    }

    const DirSyncError = Io.File.SyncError || Io.File.OpenError;

    fn syncDir(w: *Writer) DirSyncError!void {
        if (!w.dir_dirty.load(.acquire)) return;
        const d = try w.dir.openFile(w.io, ".", .{ .mode = .read_only });
        defer d.close(w.io);
        try d.sync(w.io);
        w.dir_dirty.store(false, .release);
    }

    // ---- bookkeeping ----

    /// A copy of the bookkeeping of the file for `key`, in `a`; null if no
    /// part has been written for `key` yet.
    pub fn ranges(w: *Writer, a: Allocator, key: u32) Allocator.Error!?FileState {
        const e = w.lookup(key) orelse return null;
        e.mu.lockUncancelable(w.io);
        defer e.mu.unlock(w.io);
        if (e.name.len == 0) return null;
        return try stateOf(a, e);
    }

    fn stateOf(a: Allocator, e: *Entry) Allocator.Error!FileState {
        var missing: std.ArrayList(Range) = .empty;
        try e.t.missing(a, e.size, &missing);
        var pending: std.ArrayList(Range) = .empty;
        for (missing.items) |m| try e.t.lost.gaps(a, m, &pending);
        return .{
            .key = e.key,
            .name = try a.dupe(u8, e.name),
            .size = e.size,
            .done = try a.dupe(Range, e.t.done.slice()),
            .damaged = try a.dupe(Range, e.t.damaged.slice()),
            .missing = missing.items,
            .lost = try a.dupe(Range, e.t.lost.slice()),
            .pending = pending.items,
        };
    }

    /// Declares `r` (half-open, zero-based) permanently unavailable, for
    /// example because the Article carrying it is a Missing Article. Bytes
    /// that are already done or damaged are unaffected, and a later part
    /// covering `r` clears the mark. The file must already exist: its size
    /// is unknown before the first part.
    pub fn markMissing(w: *Writer, key: u32, r: Range) !void {
        const e = w.lookup(key) orelse return error.UnknownFile;
        e.mu.lockUncancelable(w.io);
        defer e.mu.unlock(w.io);
        if (e.name.len == 0) return error.UnknownFile;
        if (r.start < 0 or r.end < r.start or r.end > e.size) return error.InvalidPart;
        var scratch: std.heap.ArenaAllocator = .init(w.gpa);
        defer scratch.deinit();
        var pieces: std.ArrayList(Range) = .empty;
        try e.t.lostPieces(scratch.allocator(), r, &pieces);
        try e.t.reserve(w.gpa, pieces.items.len);
        e.t.markLost(pieces.items);
    }

    // ---- finishing ----

    /// fsyncs the file for `key` and closes its handle. The bookkeeping is
    /// kept, and a later `writePart` reopens the file.
    pub fn finish(w: *Writer, key: u32) !void {
        const e = w.lookup(key) orelse return error.UnknownFile;
        e.mu.lockUncancelable(w.io);
        defer e.mu.unlock(w.io);
        if (e.name.len == 0) return error.UnknownFile;
        const closed_err = w.closeEntry(e);
        try w.syncDirIfOpen();
        try closed_err;
    }

    /// fsyncs every file with unsynced writes (and the directory, if files
    /// were created). Call it before persisting state. Every file is tried;
    /// the first error is returned.
    pub fn sync(w: *Writer) !void {
        var scratch: std.heap.ArenaAllocator = .init(w.gpa);
        defer scratch.deinit();
        var first: ?anyerror = null;
        for (try w.sortedEntries(scratch.allocator())) |e| {
            e.mu.lockUncancelable(w.io);
            w.syncEntry(e) catch |err| {
                if (first == null) first = err;
            };
            e.mu.unlock(w.io);
        }
        w.syncDirIfOpen() catch |err| {
            if (first == null) first = err;
        };
        if (first) |err| return err;
    }

    /// Like `syncDir`, but a no-op after `close` (the handle is gone).
    fn syncDirIfOpen(w: *Writer) DirSyncError!void {
        if (w.closed.load(.acquire)) return;
        return w.syncDir();
    }

    /// fsyncs and closes every file and the directory. Further writes fail
    /// with `error.Closed`. Idempotent. Every file is closed even if one
    /// fails; the first error is returned.
    pub fn close(w: *Writer) !void {
        if (w.closed.swap(true, .acq_rel)) return;
        var first: ?anyerror = null;
        {
            // The table stays locked, so that closing needs no memory (it
            // must work when the memory is gone). Lock order: `mu`, then an
            // entry's lock; nothing takes `mu` while holding an entry's lock.
            w.mu.lockUncancelable(w.io);
            defer w.mu.unlock(w.io);
            var it = w.entries.valueIterator();
            while (it.next()) |slot| {
                const e = slot.*;
                e.mu.lockUncancelable(w.io);
                w.closeEntry(e) catch |err| {
                    if (first == null) first = err;
                };
                e.mu.unlock(w.io);
            }
        }
        w.syncDir() catch |err| {
            if (first == null) first = err;
        };
        w.dir.close(w.io);
        if (first) |err| return err;
    }

    // ---- snapshot and restore ----

    /// The per-file bookkeeping (name, size, done, damaged and lost ranges)
    /// in the compact, versioned binary form of `filewriter_snapshot.zig`
    /// (byte-compatible with Go), allocated in `a`. Each file with unsynced
    /// writes is fsynced first, under that file's lock, so a snapshot never
    /// claims bytes that are not durable.
    pub fn snapshot(w: *Writer, a: Allocator) ![]u8 {
        var scratch: std.heap.ArenaAllocator = .init(w.gpa);
        defer scratch.deinit();
        var enc: snapshot_mod.Encoder = .{};
        for (try w.sortedEntries(scratch.allocator())) |e| {
            e.mu.lockUncancelable(w.io);
            defer e.mu.unlock(w.io);
            if (e.name.len == 0) continue;
            try w.syncEntry(e);
            try enc.add(scratch.allocator(), .{
                .key = e.key,
                .name = e.name,
                .size = e.size,
                .done = e.t.done.slice(),
                .damaged = e.t.damaged.slice(),
                .lost = e.t.lost.slice(),
            });
        }
        try w.syncDirIfOpen();
        return enc.finish(a);
    }

    const OnDisk = enum {
        /// The file matches the record.
        ok,
        /// The file was re-allocated, its state cleared.
        reset,
        /// The file is missing or unusable, forgotten.
        gone,
    };

    /// Loads bookkeeping produced by `snapshot` (by this or the Go version)
    /// into an empty Writer whose directory still holds the files, and
    /// validates it against the disk. A file that is missing or not a regular
    /// file is forgotten; one whose size differs from the recorded size is
    /// re-allocated and its state reset. The keys that were reset or
    /// forgotten are returned in ascending order (allocated in `a`), so the
    /// caller knows their Articles must be fetched again.
    pub fn restore(w: *Writer, a: Allocator, bytes: []const u8) ![]u32 {
        if (w.closed.load(.acquire)) return error.Closed;
        var scratch: std.heap.ArenaAllocator = .init(w.gpa);
        defer scratch.deinit();
        const recs = snapshot_mod.decode(scratch.allocator(), bytes) catch |err| switch (err) {
            error.InvalidSnapshot => return error.InvalidSnapshot,
            error.OutOfMemory => return error.OutOfMemory,
        };
        if (!w.isEmpty()) return error.NotEmpty;

        var reset: std.ArrayList(u32) = .empty;
        defer reset.deinit(w.gpa);
        var keep: std.ArrayList(*Entry) = .empty;
        defer {
            for (keep.items) |e| w.freeEntry(e);
            keep.deinit(w.gpa);
        }
        for (recs) |rec| {
            const status = try w.checkOnDisk(rec);
            switch (status) {
                .ok, .reset => {
                    if (status == .reset) try reset.append(w.gpa, rec.key);
                    const e = try w.newEntry(rec, status == .reset);
                    keep.append(w.gpa, e) catch |err| {
                        w.freeEntry(e);
                        return err;
                    };
                },
                .gone => try reset.append(w.gpa, rec.key),
            }
        }
        std.mem.sort(u32, reset.items, {}, std.sort.asc(u32));
        const out = try a.dupe(u32, reset.items);

        // Install: everything that can fail is done first.
        var name_keys: std.ArrayList([]u8) = .empty;
        defer {
            for (name_keys.items) |k| w.gpa.free(k);
            name_keys.deinit(w.gpa);
        }
        for (keep.items) |e| {
            const k = try w.gpa.dupe(u8, e.name);
            name_keys.append(w.gpa, k) catch |err| {
                w.gpa.free(k);
                return err;
            };
        }

        w.mu.lockUncancelable(w.io);
        defer w.mu.unlock(w.io);
        if (w.entries.count() != 0) return error.NotEmpty;
        w.name_mu.lockUncancelable(w.io);
        defer w.name_mu.unlock(w.io);
        try w.entries.ensureUnusedCapacity(w.gpa, @intCast(keep.items.len));
        try w.names.ensureUnusedCapacity(w.gpa, @intCast(keep.items.len));
        for (keep.items, 0..) |e, i| {
            w.entries.putAssumeCapacity(e.key, e);
            const slot = w.names.getOrPutAssumeCapacity(name_keys.items[i]);
            if (slot.found_existing) {
                // Cannot happen with an empty Writer; keep the table sane.
                continue;
            }
            // The table owns this key now.
            name_keys.items[i] = &.{};
        }
        keep.clearRetainingCapacity();
        return out;
    }

    fn isEmpty(w: *Writer) bool {
        w.mu.lockUncancelable(w.io);
        defer w.mu.unlock(w.io);
        return w.entries.count() == 0;
    }

    fn freeEntry(w: *Writer, e: *Entry) void {
        e.t.deinit(w.gpa);
        w.gpa.free(e.name);
        w.gpa.destroy(e);
    }

    fn newEntry(w: *Writer, rec: snapshot_mod.Record, cleared: bool) Allocator.Error!*Entry {
        const e = try w.gpa.create(Entry);
        errdefer w.gpa.destroy(e);
        const name = try w.gpa.dupe(u8, rec.name);
        errdefer w.gpa.free(name);
        e.* = .{ .key = rec.key, .name = name, .size = rec.size };
        errdefer e.t.deinit(w.gpa);
        if (!cleared) {
            try e.t.done.items.appendSlice(w.gpa, rec.done);
            try e.t.damaged.items.appendSlice(w.gpa, rec.damaged);
            try e.t.lost.items.appendSlice(w.gpa, rec.lost);
        }
        return e;
    }

    fn checkOnDisk(w: *Writer, rec: snapshot_mod.Record) Io.Cancelable!OnDisk {
        const st = w.dir.statFile(w.io, rec.name, .{ .follow_symlinks = false }) catch |err| switch (err) {
            error.Canceled => return error.Canceled,
            else => return .gone,
        };
        if (st.kind != .file) return .gone;
        if (st.size == rec.size) return .ok;
        const f = w.openExisting(rec.name) catch |err| switch (err) {
            error.Canceled => return error.Canceled,
            else => return .gone,
        };
        defer f.close(w.io);
        f.setLength(w.io, @intCast(rec.size)) catch |err| switch (err) {
            error.Canceled => return error.Canceled,
            else => return .gone,
        };
        return .reset;
    }
};

/// Whether `name` is one plain path component.
fn isPlainName(name: []const u8) bool {
    return name.len > 0 and !std.mem.eql(u8, name, ".") and !std.mem.eql(u8, name, "..") and
        std.mem.indexOfAny(u8, name, "/\x00") == null;
}
