//! The listening socket of the HTTP server, with SO_REUSEADDR and without
//! SO_REUSEPORT, as Go's net.Listen makes it.
//!
//! std's `reuse_address` sets both. SO_REUSEPORT lets a second process of the
//! same user bind the same port and share it: a second daemon would start
//! without an error and take half of the connections. Clearing it after the
//! bind does not help on Linux (the kernel keeps the port group), so the
//! socket is made here. SO_REUSEADDR alone keeps what a restart needs: binding
//! while connections of the old process are still in TIME_WAIT.
const std = @import("std");
const builtin = @import("builtin");
const Io = std.Io;
const net = Io.net;
const posix = std.posix;

pub const ListenError = net.IpAddress.ListenError;

/// Binds and listens on `address` (port 0 picks a free port; the returned
/// server's address has the port that was bound).
pub fn listen(io: Io, address: net.IpAddress, backlog: u31) ListenError!net.Server {
    if (builtin.os.tag == .windows or builtin.os.tag == .wasi) {
        return address.listen(io, .{ .reuse_address = true, .kernel_backlog = backlog });
    }
    const family: posix.sa_family_t = switch (address) {
        .ip4 => posix.AF.INET,
        .ip6 => posix.AF.INET6,
    };

    const rc = posix.system.socket(family, posix.SOCK.STREAM, 0);
    const fd: posix.fd_t = switch (posix.errno(rc)) {
        .SUCCESS => @intCast(rc),
        .AFNOSUPPORT => return error.AddressFamilyUnsupported,
        .MFILE => return error.ProcessFdQuotaExceeded,
        .NFILE => return error.SystemFdQuotaExceeded,
        .NOBUFS, .NOMEM => return error.SystemResources,
        else => return error.Unexpected,
    };
    errdefer _ = posix.system.close(fd);
    // Not inherited by the post-processing tools (macOS has no SOCK_CLOEXEC).
    _ = posix.system.fcntl(fd, posix.F.SETFD, @as(usize, posix.FD_CLOEXEC));

    const on = std.mem.toBytes(@as(c_int, 1));
    posix.setsockopt(fd, posix.SOL.SOCKET, posix.SO.REUSEADDR, &on) catch return error.Unexpected;

    var storage: posix.sockaddr.storage = undefined;
    var len: posix.socklen_t = switch (address) {
        .ip4 => |a| blk: {
            const sa: posix.sockaddr.in = .{ .port = std.mem.nativeToBig(u16, a.port), .addr = @bitCast(a.bytes) };
            @memcpy(std.mem.asBytes(&storage)[0..@sizeOf(@TypeOf(sa))], std.mem.asBytes(&sa));
            break :blk @sizeOf(@TypeOf(sa));
        },
        .ip6 => |a| blk: {
            const sa: posix.sockaddr.in6 = .{
                .port = std.mem.nativeToBig(u16, a.port),
                .flowinfo = a.flow,
                .addr = a.bytes,
                .scope_id = 0,
            };
            @memcpy(std.mem.asBytes(&storage)[0..@sizeOf(@TypeOf(sa))], std.mem.asBytes(&sa));
            break :blk @sizeOf(@TypeOf(sa));
        },
    };
    switch (posix.errno(posix.system.bind(fd, @ptrCast(&storage), len))) {
        .SUCCESS => {},
        .ACCES, .PERM => return error.AccessDenied,
        .ADDRINUSE => return error.AddressInUse,
        .ADDRNOTAVAIL => return error.AddressUnavailable,
        else => return error.Unexpected,
    }
    switch (posix.errno(posix.system.listen(fd, backlog))) {
        .SUCCESS => {},
        .ADDRINUSE => return error.AddressInUse,
        else => return error.Unexpected,
    }

    len = @sizeOf(posix.sockaddr.storage);
    if (posix.errno(posix.system.getsockname(fd, @ptrCast(&storage), &len)) != .SUCCESS) return error.Unexpected;
    var bound = address;
    switch (bound) {
        .ip4 => |*a| a.port = std.mem.bigToNative(u16, @as(*const posix.sockaddr.in, @ptrCast(&storage)).port),
        .ip6 => |*a| a.port = std.mem.bigToNative(u16, @as(*const posix.sockaddr.in6, @ptrCast(&storage)).port),
    }
    return .{ .socket = .{ .handle = fd, .address = bound }, .options = {} };
}

const testing = std.testing;

test "a taken port cannot be bound again, even by the same program" {
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var first = try listen(testing.io, any, 8);
    defer first.deinit(testing.io);
    const taken: net.IpAddress = try .parse("127.0.0.1", first.socket.address.getPort());

    try testing.expectError(error.AddressInUse, listen(testing.io, taken, 8));
}

test "the port can be bound again right after a restart, connections in TIME_WAIT or not" {
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var first = try listen(testing.io, any, 8);
    const port = first.socket.address.getPort();
    const target: net.IpAddress = try .parse("127.0.0.1", port);

    // The server side closes first, which leaves its end in TIME_WAIT.
    const client = try target.connect(testing.io, .{ .mode = .stream });
    const peer = try first.accept(testing.io);
    peer.close(testing.io);
    client.close(testing.io);
    first.deinit(testing.io);

    var second = try listen(testing.io, target, 8);
    defer second.deinit(testing.io);
    try testing.expectEqual(port, second.socket.address.getPort());
}

test "port 0 reports the port that was bound" {
    const any: net.IpAddress = try .parse("127.0.0.1", 0);
    var l = try listen(testing.io, any, 8);
    defer l.deinit(testing.io);
    try testing.expect(l.socket.address.getPort() != 0);
}

test "IPv6 loopback binds too" {
    const any: net.IpAddress = try .parse("::1", 0);
    var l = listen(testing.io, any, 8) catch |err| switch (err) {
        error.AddressUnavailable, error.AddressFamilyUnsupported => return error.SkipZigTest, // no IPv6 here
        else => return err,
    };
    defer l.deinit(testing.io);
    try testing.expect(l.socket.address.getPort() != 0);
}
