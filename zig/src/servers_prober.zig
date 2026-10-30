//! The connection test of a Server as the Server service sees it
//! (internal/app/prober.go: the Prober interface and NNTPProber). The
//! classification of failures lives in the NNTP client (`nntp_probe.zig`); this
//! file is the seam the service calls and the one implementation that dials
//! through a `nntp.Dialer`.
//!
//! The daemon owns one `nntp.TlsDialer` (strict TLS, no way around it) and
//! hands its `dialer()` to `NntpProber.init`; tests hand it the plain loopback
//! dialer of `nntptest.zig`. A result never contains credentials.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const nntp = @import("nntp.zig");

pub const Target = nntp.ProbeTarget;
pub const Result = nntp.ProbeResult;
pub const Options = nntp.probe_mod.Options;

/// Tests a connection to a Server. A problem with the Server is the returned
/// result, never an error; only cancellation and out-of-memory fail.
pub const Prober = struct {
    ctx: *anyopaque,
    probe_fn: *const fn (ctx: *anyopaque, io: Io, target: Target) error{ Canceled, OutOfMemory }!Result,

    pub fn probe(p: Prober, io: Io, target: Target) error{ Canceled, OutOfMemory }!Result {
        return p.probe_fn(p.ctx, io, target);
    }
};

/// The Prober that dials with the NNTP client: connect, authenticate, DATE,
/// within the 20 second deadline of `options`.
pub const NntpProber = struct {
    /// Allocates the connection of a test; must be safe to use from any
    /// thread (the daemon's allocator).
    gpa: Allocator,
    dialer: nntp.Dialer,
    options: Options = .{},

    pub fn init(gpa: Allocator, dialer: nntp.Dialer) NntpProber {
        return .{ .gpa = gpa, .dialer = dialer };
    }

    /// The returned Prober points at `self`: keep `self` where it is.
    pub fn prober(self: *NntpProber) Prober {
        return .{ .ctx = self, .probe_fn = probeFn };
    }

    fn probeFn(ctx: *anyopaque, io: Io, target: Target) error{ Canceled, OutOfMemory }!Result {
        const self: *NntpProber = @ptrCast(@alignCast(ctx));
        return nntp.probe(self.gpa, io, self.dialer, target, self.options);
    }
};
