//! One fetch of one Article from one Server, classified for the scheduler
//! (internal/usenet/scheduler/fetch.go). The body streams through the yEnc
//! decoder as it arrives, so no encoded copy of the Article is kept.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const usenet_types = @import("usenet_types.zig");
const yenc = @import("yenc.zig");
const Part = @import("yenc_types.zig").Part;

/// What a fetch came to.
pub const Outcome = enum {
    /// Part decoded and its CRC matches (or none was announced).
    valid,
    /// Part decoded and an announced CRC does not match.
    damaged,
    /// The Server does not have it, or sent something that is not an Article.
    absent,
    /// Worth retrying on this Server.
    transient,
    /// The Server is unusable for the whole run.
    dead,
    /// The task was cancelled.
    cancelled,
};

pub const Fetched = struct {
    outcome: Outcome,
    /// Set for `valid` and `damaged`; lives in the arena given to `fetch`.
    part: Part = undefined,
    /// Why the outcome is not `valid`.
    err: ?anyerror = null,
    /// Encoded bytes received, failed fetches included.
    wire: u64 = 0,
};

/// The most encoded bytes one Article body may bring. A part decodes to at
/// most `yenc.max_part_size`, yEnc adds a few percent, each line two bytes of
/// terminator; the cap also stops a Server that sends endless text before the
/// =ybegin line, which the decoder ignores.
pub const max_wire_bytes: u64 = 2 * yenc.max_part_size + (1 << 20);

const Collect = struct {
    dec: yenc.Decoder,
    err: ?yenc.Error = null,
    wire: u64 = 0,

    fn line(ctx: *anyopaque, l: []const u8) error{Abort}!void {
        const c: *Collect = @ptrCast(@alignCast(ctx));
        c.wire += l.len + 2;
        if (c.wire > max_wire_bytes) {
            c.err = error.TooLarge;
            return error.Abort;
        }
        c.dec.feed(l) catch |e| {
            c.err = e;
            return error.Abort;
        };
    }
};

/// Asks `fetcher` once for the Article `message_id`. Everything the decoder
/// allocates comes from `arena`, which the caller resets after use.
pub fn fetch(io: Io, fetcher: usenet_types.Fetcher, arena: Allocator, message_id: []const u8) Fetched {
    var c: Collect = .{ .dec = .init(arena) };
    const sink: usenet_types.Sink = .{ .ctx = &c, .line_fn = Collect.line };
    const res = fetcher.body(io, message_id, sink);
    const wire = c.wire;
    if (res) |_| {} else |err| if (err == error.Canceled) return .{ .outcome = .cancelled, .err = err, .wire = wire };
    if (c.err) |e| {
        // Garbage counts as absent for this Server: asking again gets the same
        // garbage. Running out of memory is not the Server's fault.
        const oc: Outcome = if (e == error.OutOfMemory) .transient else .absent;
        return .{ .outcome = oc, .err = e, .wire = wire };
    }
    res catch |err| return .{ .outcome = classify(err), .err = err, .wire = wire };
    const part = c.dec.finish() catch |e| return .{ .outcome = .absent, .err = e, .wire = wire };
    // Only a CRC that is announced and wrong makes a part damaged; posts
    // without one are legitimate (same rule as the file writer).
    if (!part.has_expected_crc or part.crcMatch()) return .{ .outcome = .valid, .part = part, .wire = wire };
    return .{ .outcome = .damaged, .part = part, .err = error.CrcMismatch, .wire = wire };
}

fn classify(err: usenet_types.FetchError) Outcome {
    return switch (err) {
        error.ArticleNotFound, error.InvalidMessageId => .absent,
        error.ServerDisabled, error.Auth, error.PoolClosed => .dead,
        error.Canceled => .cancelled,
        // Connection, Protocol and anything unclassified: try again.
        error.Connection, error.Protocol, error.Aborted, error.OutOfMemory => .transient,
    };
}
