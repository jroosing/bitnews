//! Z7 local check of the libtorrent C face: a session bound to 127.0.0.1
//! with DHT off, then magnet add, pause, resume, add-peer, and remove.
//! No swarm traffic. `bnlt` is the translate-c module of lt/bitnews_lt.h,
//! linked the same way as spike/main.zig.
const std = @import("std");
const bnlt = @import("bnlt");

const Io = std.Io;

const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=bitnews-z7";
const rel_dir = "bitnews-lt-check";
const fallback_dir = "/tmp/bitnews-lt-check";

pub fn main(init: std.process.Init) !void {
    const io = init.io;

    const ses = bnlt.bnlt_new("127.0.0.1:0", 0) orelse return error.SessionFailed;
    defer bnlt.bnlt_free(ses);

    const port = try waitListenPort(io, ses);
    std.debug.print("listen {d}\n", .{port});

    var path_buf: [std.fs.max_path_bytes]u8 = undefined;
    const save_path = try saveDir(io, &path_buf);
    if (bnlt.bnlt_add_magnet(ses, magnet, save_path) != 0) return error.AddFailed;

    try io.sleep(.fromMilliseconds(200), .awake);

    var st: bnlt.bnlt_status = undefined;
    if (bnlt.bnlt_status_first(ses, &st) != 0) return error.StatusFailed;
    std.debug.print("status state={d} name={s}\n", .{ st.state, std.mem.sliceTo(&st.name, 0) });

    const hash = try infoHashFromMagnet(magnet);
    if (bnlt.bnlt_pause(ses, &hash) != 0) return error.PauseFailed;
    if (bnlt.bnlt_resume(ses, &hash) != 0) return error.ResumeFailed;

    // 127.0.0.1:1 is not a peer. A refusal (1) is a successful call.
    const peer = bnlt.bnlt_add_peer(ses, &hash, "127.0.0.1:1");
    std.debug.print("add_peer {d}\n", .{peer});

    if (bnlt.bnlt_remove(ses, &hash, 0) != 0) return error.RemoveFailed;
    std.debug.print("lt-check ok\n", .{});
}

fn waitListenPort(io: Io, ses: *bnlt.bnlt_session) !c_int {
    var waited: u64 = 0;
    while (true) {
        const port = bnlt.bnlt_listen_port(ses);
        if (port > 0) return port;
        if (waited >= 2000) return error.ListenPort;
        try io.sleep(.fromMilliseconds(50), .awake);
        waited += 50;
    }
}

/// `<cwd>/bitnews-lt-check`, else `/tmp/bitnews-lt-check`. Zig 0.17 has no
/// `std.fs.cwd`; `Io.Dir.cwd` is that directory.
fn saveDir(io: Io, buf: *[std.fs.max_path_bytes]u8) ![*:0]const u8 {
    const cwd = Io.Dir.cwd();
    cwd.createDirPath(io, rel_dir) catch return fallbackDir(io, buf);
    const n = cwd.realPathFile(io, rel_dir, buf) catch return fallbackDir(io, buf);
    return nulTerminate(buf, n);
}

fn fallbackDir(io: Io, buf: *[std.fs.max_path_bytes]u8) ![*:0]const u8 {
    try Io.Dir.cwd().createDirPath(io, fallback_dir);
    const n = try Io.Dir.realPathFileAbsolute(io, fallback_dir, buf);
    return nulTerminate(buf, n);
}

fn nulTerminate(buf: *[std.fs.max_path_bytes]u8, n: usize) error{PathTooLong}![*:0]const u8 {
    if (n >= buf.len) return error.PathTooLong;
    buf[n] = 0;
    return buf[0..n :0];
}

fn infoHashFromMagnet(uri: []const u8) error{BadMagnet}![20]u8 {
    const marker = "xt=urn:btih:";
    const at = std.mem.indexOf(u8, uri, marker) orelse return error.BadMagnet;
    const rest = uri[at + marker.len ..];
    const end = std.mem.indexOfAny(u8, rest, "&") orelse rest.len;
    const hex = rest[0..end];
    if (hex.len != 40) return error.BadMagnet;
    var out: [20]u8 = undefined;
    _ = std.fmt.hexToBytes(&out, hex) catch return error.BadMagnet;
    return out;
}
