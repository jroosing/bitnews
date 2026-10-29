//! The Download and its lifecycle: internal/core/download.go, state.go and
//! failure.go. Strings in a Download belong to whoever produced it (an arena
//! per request or per query result); copy them to keep them.
const std = @import("std");

pub const SourceKind = enum { nzb, torrent };

/// The rank of a Download within its Queue; force ignores the concurrency limit.
pub const Priority = enum { force, high, normal, low };

/// A Download's position in its lifecycle. Pausing is not a State (it is the
/// `paused` flag), nor is Seeding (a torrent attribute that runs in parallel
/// from fetched onwards). Removal deletes the Download.
pub const State = enum {
    added,
    fetching_metadata,
    queued,
    fetching,
    fetched,
    post_processing,
    ready,
    failed,

    /// Whether a Download in this state is still in a Queue: added but not
    /// fetched yet.
    pub fn inQueue(s: State) bool {
        return switch (s) {
            .added, .fetching_metadata, .queued, .fetching => true,
            else => false,
        };
    }

    /// Ready and failed are terminal.
    pub fn terminal(s: State) bool {
        return s == .ready or s == .failed;
    }
};

/// Why a Download is failed; shared across protocols.
pub const FailureReason = enum {
    missing_articles,
    repair_insufficient,
    archive_unsupported,
    archive_password,
    archive_corrupt,
    disk_full,
    path_violation,
    source_invalid,
    internal,
};

/// The single table of legal lifecycle moves (the "Download lifecycle" of
/// docs/architecture.md plus the M3 and M5 moves). No state moves to itself.
pub fn canTransition(from: State, to: State) bool {
    const allowed: []const State = switch (from) {
        .added => &.{ .queued, .fetching_metadata },
        .fetching_metadata => &.{ .queued, .failed },
        .queued => &.{ .fetching, .failed },
        .fetching => &.{ .queued, .fetched, .failed },
        .fetched => &.{ .post_processing, .ready, .failed },
        .post_processing => &.{ .fetching, .ready, .failed },
        .ready, .failed => &.{},
    };
    return std.mem.indexOfScalar(State, allowed, to) != null;
}

/// One thing a user or Media manager asked for, from being added until it is
/// removed. Times are unix milliseconds (UTC), 0 for "not yet".
pub const Download = struct {
    id: []const u8,
    name: []const u8,
    source_kind: SourceKind,
    /// Empty when the Download has no Category.
    category_id: []const u8 = "",
    state: State,
    priority: Priority = .normal,
    queue_position: i64 = 0,
    /// Orthogonal to state: a paused Download stays queued or fetching but is
    /// never active.
    paused: bool = false,
    /// Set only when state is failed.
    failure_reason: ?FailureReason = null,
    /// Free text, already redacted.
    failure_detail: []const u8 = "",
    total_bytes: i64 = 0,
    done_bytes: i64 = 0,
    added_at: i64,
    fetched_at: i64 = 0,
    ready_at: i64 = 0,
    updated_at: i64,
    /// Where the importable files are; set once ready.
    output_path: []const u8 = "",
};

/// One lifecycle move.
pub const StateChange = struct {
    to: State,
    at: i64,
    /// Required when `to` is ready.
    output_path: []const u8 = "",
    /// Required when `to` is failed, and only then.
    failure_reason: ?FailureReason = null,
    /// Optional, already redacted.
    failure_detail: []const u8 = "",
};

pub const ChangeError = error{
    /// The lifecycle does not allow this move (a bug in the caller).
    IllegalTransition,
    /// A legal move that lacks data its target needs or carries data it must not.
    InvalidStateChange,
};

/// Moves the Download to `c.to`. An illegal move returns an error and leaves
/// the Download unchanged.
pub fn changeState(d: *Download, c: StateChange) ChangeError!void {
    if (!canTransition(d.state, c.to)) return error.IllegalTransition;
    if (c.to == .ready and c.output_path.len == 0) return error.InvalidStateChange;
    if (c.to == .failed and c.failure_reason == null) return error.InvalidStateChange;
    if (c.to != .failed and c.failure_reason != null) return error.InvalidStateChange;
    d.state = c.to;
    d.updated_at = c.at;
    switch (c.to) {
        .fetched => d.fetched_at = c.at,
        .ready => {
            d.ready_at = c.at;
            d.output_path = c.output_path;
        },
        .failed => {
            d.failure_reason = c.failure_reason;
            d.failure_detail = c.failure_detail;
        },
        else => {},
    }
}

const testing = std.testing;

fn blank(state: State) Download {
    return .{ .id = "d1", .name = "n", .source_kind = .nzb, .state = state, .added_at = 1, .updated_at = 1 };
}

test "the lifecycle table is the Go one: legal moves, and nothing out of a terminal state or to itself" {
    try testing.expect(canTransition(.added, .queued));
    try testing.expect(canTransition(.added, .fetching_metadata));
    try testing.expect(canTransition(.fetching, .queued));
    try testing.expect(canTransition(.post_processing, .fetching));
    try testing.expect(canTransition(.fetched, .ready));
    try testing.expect(!canTransition(.queued, .fetched));
    try testing.expect(!canTransition(.added, .fetching));
    for (std.enums.values(State)) |s| try testing.expect(!canTransition(s, s));
    for (std.enums.values(State)) |s| {
        try testing.expect(!canTransition(.ready, s));
        try testing.expect(!canTransition(.failed, s));
    }
}

test "inQueue is true until a Download is fetched" {
    try testing.expect(State.fetching.inQueue());
    try testing.expect(State.added.inQueue());
    try testing.expect(!State.fetched.inQueue());
    try testing.expect(!State.failed.inQueue());
}

test "changeState sets the fields of the target state and refuses bad data" {
    var d = blank(.fetched);
    try testing.expectError(error.InvalidStateChange, changeState(&d, .{ .to = .ready, .at = 5 }));
    try testing.expectError(error.InvalidStateChange, changeState(&d, .{ .to = .failed, .at = 5 }));
    try testing.expectError(error.InvalidStateChange, changeState(&d, .{ .to = .post_processing, .at = 5, .failure_reason = .internal }));
    try testing.expectError(error.IllegalTransition, changeState(&d, .{ .to = .queued, .at = 5 }));
    try testing.expectEqual(State.fetched, d.state);
    try testing.expectEqual(@as(i64, 1), d.updated_at);

    try changeState(&d, .{ .to = .ready, .at = 9, .output_path = "/data/x" });
    try testing.expectEqual(State.ready, d.state);
    try testing.expectEqual(@as(i64, 9), d.ready_at);
    try testing.expectEqualStrings("/data/x", d.output_path);

    var f = blank(.queued);
    try changeState(&f, .{ .to = .failed, .at = 7, .failure_reason = .disk_full, .failure_detail = "no space" });
    try testing.expectEqual(FailureReason.disk_full, f.failure_reason.?);
    try testing.expectEqualStrings("no space", f.failure_detail);
}
