//! The seams between the Usenet slices (NNTP client, scheduler, runner) and
//! the error classes they share (internal/usenet/nntp errors).
const std = @import("std");

/// Why fetching an Article from one Server failed. The scheduler decides by
/// class, so a client must use exactly these:
///   ArticleNotFound            the Server does not have it (430): tried.
///   Connection, Protocol       transient: retry the same Server a few times.
///   ServerDisabled, Auth,
///   PoolClosed                 the Server is dead for the rest of the run.
///   Aborted                    the sink refused a line (a decode error).
///   Canceled                   the caller cancelled (std.Io).
pub const FetchError = error{
    ArticleNotFound,
    Connection,
    Protocol,
    ServerDisabled,
    Auth,
    PoolClosed,
    InvalidMessageId,
    Aborted,
    Canceled,
    OutOfMemory,
};

/// Receives the lines of an Article body, dot-unstuffed and without the line
/// terminator. Returning an error aborts the fetch with `error.Aborted`.
pub const Sink = struct {
    ctx: *anyopaque,
    line_fn: *const fn (ctx: *anyopaque, line: []const u8) error{Abort}!void,

    pub fn line(s: Sink, bytes: []const u8) error{Abort}!void {
        return s.line_fn(s.ctx, bytes);
    }
};

/// Fetches Article bodies from one Server (a pool of connections to it).
/// The NNTP client implements it; the scheduler and its tests use it.
pub const Fetcher = struct {
    ctx: *anyopaque,
    body_fn: *const fn (ctx: *anyopaque, io: std.Io, message_id: []const u8, sink: Sink) FetchError!void,

    pub fn body(f: Fetcher, io: std.Io, message_id: []const u8, sink: Sink) FetchError!void {
        return f.body_fn(f.ctx, io, message_id, sink);
    }
};
