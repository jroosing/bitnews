//! Cancelling an unpack (internal/postproc/unpack lifecycle_test.go,
//! fake7zz_unix_test.go): 7zz and everything it started are killed through
//! the real runner (tool.zig), the staging directory is removed and the
//! error is a cancellation, not an archive problem. The "7zz" is a shell
//! script that lists one file and then hangs while extracting.
const std = @import("std");
const testing = std.testing;
const Io = std.Io;
const tu = @import("unpack_testutil.zig");
const tool = @import("tool.zig");
const unpack = @import("unpack.zig");
const detect = @import("unpack_detect.zig");
const errors = @import("unpack_errors.zig");
const env_mod = @import("unpack_env.zig");

fn script(rig: *tu.Rig, marker_dir: []const u8) ![]const u8 {
    const body = try std.fmt.allocPrint(rig.arena(),
        \\#!/bin/sh
        \\case "$1" in
        \\l) printf 'Path = a.txt\nSize = 8\nAttributes = A -rw-r--r--\nEncrypted = -\n\n'; exit 0 ;;
        \\x) for a in "$@"; do case "$a" in -o*) out=${{a#-o}};; esac; done
        \\   mkdir -p "$out"; echo partial > "$out/a.txt"
        \\   (/bin/sleep 1; echo alive > "{s}/grandchild") &
        \\   echo $$ > "{s}/pid"
        \\   exec /bin/sleep 60 ;;
        \\esac
        \\exit 7
        \\
    , .{ marker_dir, marker_dir });
    try rig.write("bin/7zz", body);
    try rig.tmp.dir.setFilePermissions(tu.io, "bin/7zz", .fromMode(0o755), .{});
    return rig.path("bin/7zz");
}

const Job = struct {
    env: *env_mod.Env,
    set: detect.ArchiveSet,
    dst: []const u8,

    fn go(j: *Job) errors.Error!unpack.Result {
        return unpack.unpack(j.env, j.set, j.dst);
    }
};

fn sleepMs(ms: i64) !void {
    try tu.io.sleep(.fromMilliseconds(ms), .awake);
}

test "cancelling kills 7zz and what it started and leaves nothing behind" {
    const rig = try tu.Rig.init(tool.system(), .{});
    defer rig.deinit();
    try rig.mkdir("marker");
    rig.env.cfg.seven_zip = try script(rig, rig.path("marker"));
    try rig.write("arch/x.7z", "not read");
    const vols = try rig.arena().alloc([]const u8, 1);
    vols[0] = rig.path("arch/x.7z");
    var job: Job = .{ .env = &rig.env, .set = .{ .format = .seven_z, .name = "x", .volumes = vols }, .dst = rig.path("base/jail/out") };

    var fut = try tu.io.concurrent(Job.go, .{&job});
    // Wait until the extraction runs; an early cancel can be lost (Z3 findings).
    var waited: usize = 0;
    while (!rig.exists("marker/pid") and waited < 500) : (waited += 1) try sleepMs(10);
    try testing.expect(rig.exists("marker/pid"));
    const t0 = std.Io.Timestamp.now(tu.io, .awake);
    const res = fut.cancel(tu.io);
    try testing.expectError(error.Canceled, res);
    const took = t0.durationTo(std.Io.Timestamp.now(tu.io, .awake)).toMilliseconds();
    try testing.expect(took < 20_000);

    try testing.expect(!rig.exists("base/jail/out"));
    try testing.expect(!rig.exists("base/jail/out.tmp"));
    // The grandchild would write the file after a second if it lived.
    try sleepMs(1500);
    try testing.expect(!rig.exists("marker/grandchild"));
    // The process itself is gone.
    const pid_text = std.mem.trim(u8, try rig.read("marker/pid"), " \n");
    const pid = try std.fmt.parseInt(std.posix.pid_t, pid_text, 10);
    var gone = false;
    for (0..250) |_| {
        std.posix.kill(pid, @fromBackingInt(@intCast(0))) catch |err| {
            if (err == error.ProcessNotFound) {
                gone = true;
                break;
            }
        };
        try sleepMs(20);
    }
    try testing.expect(gone);
}

test "a run with the real runner reports a 7zz that cannot start as an internal error" {
    const rig = try tu.Rig.init(tool.system(), .{ .seven_zip = "/nonexistent/7zz" });
    defer rig.deinit();
    try rig.write("arch/x.7z", "x");
    const vols = try rig.arena().alloc([]const u8, 1);
    vols[0] = rig.path("arch/x.7z");
    try testing.expectError(error.Internal, unpack.unpack(&rig.env, .{ .format = .seven_z, .name = "x", .volumes = vols }, rig.path("out")));
    try testing.expect(!rig.exists("out"));
}
