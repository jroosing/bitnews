//! Events: something that happened that other parts of the daemon and SSE
//! clients want to hear about (internal/core/event.go). Events are published
//! on a bus and never persisted. Strings in an event are borrowed for the
//! duration of the `publish` call: the bus copies what it keeps.
const std = @import("std");
const dl = @import("download.zig");

/// The names are stable: the SSE stream uses them as event names.
pub const Kind = enum {
    download_added,
    download_updated,
    download_progress,
    fetched,
    ready,
    failed,
    removed,
    settings_changed,
    servers_changed,
    queue_paused,
    queue_resumed,
    postprocess_stage,

    /// Progress events are high-frequency and superseded by the next one, so
    /// a bus may drop them for a slow subscriber. Every other kind is a
    /// lifecycle event and must not be lost silently.
    pub fn isProgress(k: Kind) bool {
        return k == .download_progress;
    }
};

pub const Progress = struct {
    id: []const u8,
    done_bytes: i64,
    total_bytes: i64,
    /// The current transfer rate.
    bytes_per_second: i64 = 0,
};

pub const Ready = struct {
    id: []const u8,
    /// Where the importable files are.
    output_path: []const u8,
};

pub const Failed = struct {
    id: []const u8,
    reason: dl.FailureReason,
    /// Already redacted.
    detail: []const u8 = "",
};

/// A step of the post-processing pipeline, in the order they run (verify and
/// unpack are conditional). `repairing` is never entered as a stage of its own
/// (the repair is part of the verifying pass); it names the failures of the
/// repair. The names are the wire names of the SSE event.
pub const Stage = enum { verifying, repairing, unpacking, finishing };

/// A Download entered a post-processing stage. It is published after the
/// `download_updated` that moved the Download to post_processing, because a
/// Download has no stage field.
pub const PostProcessStage = struct {
    id: []const u8,
    stage: Stage,
};

pub const QueuePaused = struct {
    /// The kind of the Queue.
    source: dl.SourceKind,
    /// The Pause reason that was added: "user", "disk_full", "bind_unavailable".
    reason: []const u8,
    /// Free text, for example the free and needed bytes.
    detail: []const u8 = "",
};

pub const Event = union(Kind) {
    /// A Download was created.
    download_added: dl.Download,
    /// A Download changed (state, priority, category or paused flag); the new snapshot.
    download_updated: dl.Download,
    download_progress: Progress,
    /// A Download's data was received and checked; the ID.
    fetched: []const u8,
    ready: Ready,
    failed: Failed,
    /// A Download was deleted; the ID.
    removed: []const u8,
    /// The Settings were written. No payload: receivers read the current
    /// Settings, which may include secrets.
    settings_changed: void,
    /// A Server was created, changed or deleted. No payload either: receivers
    /// (the Usenet runner rebuilding its pools) read the Servers themselves,
    /// which include passwords.
    servers_changed: void,
    queue_paused: QueuePaused,
    /// A Queue runs again; its source.
    queue_resumed: dl.SourceKind,
    postprocess_stage: PostProcessStage,

    pub fn kind(e: Event) Kind {
        return e;
    }
};

/// Where services publish events. The bus implements it; tests use a
/// recorder. `publish` must not block and may be called from many threads.
pub const Publisher = struct {
    ctx: *anyopaque,
    publish_fn: *const fn (ctx: *anyopaque, e: Event) void,

    pub fn publish(p: Publisher, e: Event) void {
        p.publish_fn(p.ctx, e);
    }

    var discard_ctx: u8 = 0;
    fn discardFn(_: *anyopaque, _: Event) void {}

    /// Drops everything. For tests and for services without a bus.
    pub const discard: Publisher = .{ .ctx = &discard_ctx, .publish_fn = discardFn };
};

const testing = std.testing;

test "only download_progress is a progress event" {
    for (std.enums.values(Kind)) |k| try testing.expectEqual(k == .download_progress, k.isProgress());
}

test "event names are the stable SSE names" {
    try testing.expectEqualStrings("postprocess_stage", @tagName(Kind.postprocess_stage));
    try testing.expectEqualStrings("download_added", @tagName(Kind.download_added));
    try testing.expectEqualStrings("settings_changed", @tagName(Kind.settings_changed));
    try testing.expectEqualStrings("queue_resumed", @tagName(Kind.queue_resumed));
}

test "the discard publisher accepts anything" {
    Publisher.discard.publish(.{ .removed = "d1" });
    Publisher.discard.publish(.settings_changed);
    Publisher.discard.publish(.servers_changed);
}
