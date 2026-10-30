//! What a Queue is given (internal/queue Config and Protocol): the protocol's
//! runner, the Downloads store, the policy read from the Settings, the free
//! space probe, the clock and the wake-up. Each seam is a context pointer plus
//! functions, like `event.Publisher`, so a test plugs in a fake.
//!
//! Lifetime rule for all of them: the object behind `ctx` outlives the Queue.
//! Strings passed to a seam function are borrowed for the call only; strings
//! it returns are allocated in the arena the Queue passes.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const fsutil_space = @import("fsutil_space.zig");

/// What one protocol's runner gives its Queue. Methods are called from the
/// thread that runs `reconcile` (and `hold` calls `running` and `abandon` from
/// the caller's thread), so an implementation guards its own state.
///
/// Build one with `Protocol.init(&runner)`; `runner` has these methods:
///
///     fn prepare(self: *R) bool
///     fn exclude(self: *R, id: []const u8) bool
///     fn running(self: *R, id: []const u8) bool
///     fn start(self: *R, d: dl.Download) anyerror!void
///     fn stop(self: *R, id: []const u8) void
///     fn abandon(self: *R, id: []const u8) anyerror!void
pub const Protocol = struct {
    ctx: *anyopaque,
    vtable: *const VTable,

    pub const VTable = struct {
        prepare: *const fn (ctx: *anyopaque) bool,
        exclude: *const fn (ctx: *anyopaque, id: []const u8) bool,
        running: *const fn (ctx: *anyopaque, id: []const u8) bool,
        start: *const fn (ctx: *anyopaque, d: dl.Download) anyerror!void,
        stop: *const fn (ctx: *anyopaque, id: []const u8) void,
        abandon: *const fn (ctx: *anyopaque, id: []const u8) anyerror!void,
    };

    /// Runs at the start of each pass, before the Queue lists its Downloads.
    /// False keeps the pass from starting fetches (it still stops the ones that
    /// must stop), for example while no Server is configured.
    pub fn prepare(p: Protocol) bool {
        return p.vtable.prepare(p.ctx);
    }

    /// Whether the Download is out of the Queue's hands for this pass, for
    /// example a torrent that is already Seeding.
    pub fn exclude(p: Protocol, id: []const u8) bool {
        return p.vtable.exclude(p.ctx, id);
    }

    /// Whether a fetch of the Download is running.
    pub fn running(p: Protocol, id: []const u8) bool {
        return p.vtable.running(p.ctx, id);
    }

    /// Starts the fetch of `d`, which the Queue has just moved to Fetching (or
    /// found Fetching without a running fetch). `d` is valid for the call
    /// only: copy what the fetch keeps. `error.NotNow` skips the Download for
    /// this pass (it cools down after a failure, say): the Queue leaves it as
    /// it is and tries again on a later pass. Any other error is logged; the
    /// protocol records a failure itself.
    pub fn start(p: Protocol, d: dl.Download) anyerror!void {
        return p.vtable.start(p.ctx, d);
    }

    /// Stops the fetch of the Download. The Queue moves it to Queued.
    pub fn stop(p: Protocol, id: []const u8) void {
        p.vtable.stop(p.ctx, id);
    }

    /// Stops the fetch, forgets its resume state and returns once it has
    /// stopped (cancel the calling task to bound the wait). `hold` calls it.
    pub fn abandon(p: Protocol, id: []const u8) anyerror!void {
        return p.vtable.abandon(p.ctx, id);
    }

    pub fn init(ptr: anytype) Protocol {
        const Ptr = @TypeOf(ptr);
        const gen = struct {
            fn self(ctx: *anyopaque) Ptr {
                return @ptrCast(@alignCast(ctx));
            }
            fn prepareFn(ctx: *anyopaque) bool {
                return self(ctx).prepare();
            }
            fn excludeFn(ctx: *anyopaque, id: []const u8) bool {
                return self(ctx).exclude(id);
            }
            fn runningFn(ctx: *anyopaque, id: []const u8) bool {
                return self(ctx).running(id);
            }
            fn startFn(ctx: *anyopaque, d: dl.Download) anyerror!void {
                return self(ctx).start(d);
            }
            fn stopFn(ctx: *anyopaque, id: []const u8) void {
                self(ctx).stop(id);
            }
            fn abandonFn(ctx: *anyopaque, id: []const u8) anyerror!void {
                return self(ctx).abandon(id);
            }
            const vtable: VTable = .{
                .prepare = prepareFn,
                .exclude = excludeFn,
                .running = runningFn,
                .start = startFn,
                .stop = stopFn,
                .abandon = abandonFn,
            };
        };
        return .{ .ctx = ptr, .vtable = &gen.vtable };
    }
};

/// The part of the Downloads repository a Queue uses (Go: core.DownloadRepo).
/// `queue_adapters.downloadsFromRepo` makes one from the real repo. Errors the
/// Queue tells apart are matched by name: `error.NotFound` and
/// `error.IllegalTransition`; `error.Canceled` ends a pass quietly.
pub const Downloads = struct {
    ctx: *anyopaque,
    /// Every Download of `kind` in `state`.
    list_fn: *const fn (ctx: *anyopaque, arena: Allocator, state: dl.State, kind: dl.SourceKind) anyerror![]dl.Download,
    get_fn: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!dl.Download,
    /// Applies the change and stores the result in one step.
    update_state_fn: *const fn (ctx: *anyopaque, arena: Allocator, id: []const u8, c: dl.StateChange) anyerror!dl.Download,
    /// Stores done_bytes, and total_bytes when positive.
    update_progress_fn: *const fn (ctx: *anyopaque, id: []const u8, done_bytes: i64, total_bytes: i64) anyerror!void,

    pub fn list(d: Downloads, arena: Allocator, state: dl.State, kind: dl.SourceKind) anyerror![]dl.Download {
        return d.list_fn(d.ctx, arena, state, kind);
    }

    pub fn get(d: Downloads, arena: Allocator, id: []const u8) anyerror!dl.Download {
        return d.get_fn(d.ctx, arena, id);
    }

    pub fn updateState(d: Downloads, arena: Allocator, id: []const u8, c: dl.StateChange) anyerror!dl.Download {
        return d.update_state_fn(d.ctx, arena, id, c);
    }

    pub fn updateProgress(d: Downloads, id: []const u8, done_bytes: i64, total_bytes: i64) anyerror!void {
        return d.update_progress_fn(d.ctx, id, done_bytes, total_bytes);
    }
};

/// What the Queue reads from the Settings on each pass.
pub const Policy = struct {
    /// How many non-force Downloads run at once.
    limit: i64,
    /// The incomplete root the disk guard watches.
    incomplete: []const u8,
    /// Free bytes to keep on top of what a Download needs.
    min_free: u64,
};

/// Reads the Policy of one Queue (`queue_adapters.policyFromSettings` for the
/// Settings service). Strings of the result are allocated in `arena`.
pub const PolicySource = struct {
    ctx: *anyopaque,
    read_fn: *const fn (ctx: *anyopaque, arena: Allocator) anyerror!Policy,

    pub fn read(s: PolicySource, arena: Allocator) anyerror!Policy {
        return s.read_fn(s.ctx, arena);
    }
};

/// The free bytes of the filesystem holding a path. `error.Unsupported` means
/// "unknown" and passes the guard silently; any other error passes it with a
/// warning. A Queue without a FreeSpace has no disk guard.
pub const FreeSpace = struct {
    ctx: ?*anyopaque = null,
    free_fn: *const fn (ctx: ?*anyopaque, path: []const u8) anyerror!u64,

    pub fn free(f: FreeSpace, path: []const u8) anyerror!u64 {
        return f.free_fn(f.ctx, path);
    }

    /// The real filesystem (`fsutil_space.freeBytes`).
    pub const system: FreeSpace = .{ .free_fn = systemFree };

    fn systemFree(_: ?*anyopaque, path: []const u8) anyerror!u64 {
        return fsutil_space.freeBytes(path);
    }
};

/// Asks the runner for another pass. Must not block and may be called from any
/// thread, including while the Queue holds no lock.
pub const Wake = struct {
    ctx: ?*anyopaque = null,
    wake_fn: *const fn (ctx: ?*anyopaque) void = none,

    pub fn wake(w: Wake) void {
        w.wake_fn(w.ctx);
    }

    fn none(_: ?*anyopaque) void {}
};

/// Unix milliseconds. Tests pass a fake.
pub const Clock = struct {
    ctx: ?*anyopaque = null,
    now_fn: *const fn (ctx: ?*anyopaque, io: Io) i64 = real,

    pub fn nowMs(c: Clock, io: Io) i64 {
        return c.now_fn(c.ctx, io);
    }

    fn real(_: ?*anyopaque, io: Io) i64 {
        return Io.Clock.now(.real, io).toMilliseconds();
    }
};
