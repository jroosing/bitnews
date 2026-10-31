//! Running 7zz (internal/postproc/unpack sevenzip.go). All the arguments are
//! built here, from fixed words and the absolute paths Unpack computed; there
//! is no shell. "--" ends the switches so an archive named like a switch
//! ("-so.rar", "@list.rar") is a file name. The password travels on stdin,
//! where 7zz reads it at its prompt: never an argument, so it does not show
//! in the process list, and never part of an error.
//!
//! 7zz lists and extracts, nothing else happens here: the output of the
//! listing is parsed by `unpack_listing` and the exit status and text of a
//! failed run are mapped to the error classes by `classify`.
const std = @import("std");
const Allocator = std.mem.Allocator;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const listing = @import("unpack_listing.zig");
const name_mod = @import("unpack_name.zig");
const tool = @import("tool_types.zig");

// 7zz exit codes (7-Zip documentation).
const exit_ok = 0;
const exit_warning = 1;
const exit_fatal = 2;
const exit_command = 7;
const exit_memory = 8;
const exit_user_stop = 255;

/// Runs 7zz in `dir`. The output buffers live in `alloc`. The password, when
/// there is one, is fed on stdin from a buffer that is wiped afterwards.
fn run(env: *Env, alloc: Allocator, dir: []const u8, max_output: usize, args: []const []const u8) Error!tool.Result {
    const a = env.arena;
    if (env.cfg.seven_zip.len == 0) {
        return errors.fail(a, env.diag, error.Internal, "tool: program not found: 7zz (install it, or run `make tools` and add bin/tools to PATH; see docs/deploy.md)", .{});
    }
    var spec: tool.Spec = .{ .path = env.cfg.seven_zip, .args = args, .dir = dir, .max_output = max_output };
    const pw = env.cfg.password;
    var stdin: []u8 = &.{};
    defer {
        std.crypto.secureZero(u8, stdin);
        env.gpa.free(stdin);
    }
    if (pw.len > 0) {
        if (std.mem.indexOfAny(u8, pw, "\r\n\x00") != null or !std.unicode.utf8ValidateSlice(pw)) {
            return errors.fail(a, env.diag, error.Password, "the password has a line break, a NUL byte or invalid UTF-8", .{});
        }
        stdin = try env.gpa.alloc(u8, pw.len + 1);
        @memcpy(stdin[0..pw.len], pw);
        stdin[pw.len] = '\n';
        spec.stdin = stdin;
    }
    return env.runner.run(alloc, env.io, spec) catch |err| switch (err) {
        error.Canceled => error.Canceled,
        error.OutOfMemory => error.OutOfMemory,
        error.Timeout => errors.fail(a, env.diag, error.Internal, "tool: timed out: 7zz", .{}),
        error.StartFailed => errors.fail(a, env.diag, error.Internal, "tool: start 7zz: could not be started", .{}),
    };
}

/// The cap on the output of a listing, sized by the entry cap.
pub fn listLimit(max_entries: usize) usize {
    const per = @max(max_entries, 1) *| env_mod.list_bytes_per_entry +| env_mod.list_bytes_floor;
    return @min(per, env_mod.list_bytes_ceiling);
}

/// The entries of the archive whose first volume is `first`. Everything
/// (output and entries) lives in `scratch`, an arena of the caller's.
pub fn list(env: *Env, scratch: Allocator, dir: []const u8, first: []const u8) Error![]listing.Entry {
    const limit = listLimit(env.cfg.max_entries);
    const res = try run(env, scratch, dir, limit, &.{ "l", "-slt", "-ba", "-sccUTF-8", "--", first });
    if (res.truncated) {
        return errors.fail(env.arena, env.diag, error.TooManyEntries, "the archive listing is longer than {d} bytes", .{limit});
    }
    try classify(env, res);
    // The guard stops at the entry past the cap, so one more is enough.
    return listing.parse(env, scratch, res.stdout, env.cfg.max_entries +| 1);
}

/// Unpacks the archive into `dir`, which is also the working directory.
pub fn extract(env: *Env, dir: []const u8, first: []const u8) Error!void {
    var step: std.heap.ArenaAllocator = .init(env.gpa);
    defer step.deinit();
    const out_arg = try std.fmt.allocPrint(step.allocator(), "-o{s}", .{dir});
    const res = try run(env, step.allocator(), dir, tool.default_max_output, &.{ "x", "-y", "-bd", "-aos", "-sccUTF-8", out_arg, "--", first });
    try classify(env, res);
}

/// Turns the outcome of a 7zz run into one of the errors. The exit status
/// says how bad it was; the text says what, because 7zz uses status 2 for
/// everything from a wrong password to a truncated archive.
///
/// The text contains entry names, which an attacker chooses, so a name can
/// pick between classes (a file called "Wrong password" makes a corrupt
/// archive look like a password problem). It cannot do more than that: every
/// class is a failure of the Download, and nothing is parsed out of the text
/// but the class.
pub fn classify(env: *Env, res: tool.Result) Error!void {
    if (res.exit_code == exit_ok) return;
    const a = env.arena;
    const joined = try std.mem.concat(env.gpa, u8, &.{ res.stdout, "\n", res.stderr });
    defer env.gpa.free(joined);
    const text = try name_mod.lowerAlloc(env.gpa, joined);
    defer env.gpa.free(text);
    const detail = try excerpt(a, res);
    const code = res.exit_code;

    if (has(text, &.{ "wrong password", "cannot open encrypted archive" }) or (code == exit_user_stop and has(text, &.{"enter password"}))) {
        return errors.fail(a, env.diag, error.Password, "{s}", .{detail});
    }
    if (has(text, &.{ "no space left", "not enough space", "disk full" })) {
        return errors.fail(a, env.diag, error.DiskFull, "{s}", .{detail});
    }
    if (code == exit_memory or has(text, &.{ "can't allocate required memory", "not enough memory" })) {
        return errors.fail(a, env.diag, error.TooLarge, "7zz ran out of memory: {s}", .{detail});
    }
    if (has(text, &.{ "unsupported method", "unsupported feature", "unsupported archive" })) {
        return errors.fail(a, env.diag, error.Unsupported, "{s}", .{detail});
    }
    return switch (code) {
        exit_command => errors.fail(a, env.diag, error.Internal, "unpack: 7zz rejected its command line (a bug): {s}", .{detail}),
        exit_user_stop => errors.fail(a, env.diag, error.Internal, "unpack: 7zz was stopped: {s}", .{detail}),
        exit_warning => errors.fail(a, env.diag, error.Corrupt, "7zz reported a warning (exit status 1): {s}", .{detail}),
        exit_fatal => errors.fail(a, env.diag, error.Corrupt, "{s}", .{detail}),
        else => errors.fail(a, env.diag, error.Corrupt, "7zz exit status {d}: {s}", .{ code, detail }),
    };
}

fn has(text: []const u8, subs: []const []const u8) bool {
    for (subs) |s| if (std.mem.indexOf(u8, text, s) != null) return true;
    return false;
}

/// Picks one line of the output for the error message: the first line that
/// starts with ERROR (stderr first), else the first non-empty line of stderr.
/// It is bounded and has no control characters.
pub fn excerpt(a: Allocator, res: tool.Result) Allocator.Error![]const u8 {
    var first: []const u8 = "";
    for ([_][]const u8{ res.stderr, res.stdout }, 0..) |out, i| {
        var it = std.mem.splitScalar(u8, out, '\n');
        while (it.next()) |line| {
            const s = name_mod.trimSpace(line);
            if (s.len == 0) continue;
            if (std.mem.startsWith(u8, s, "ERROR")) return name_mod.truncateForError(a, s);
            if (i == 0 and first.len == 0) first = s;
        }
    }
    if (first.len == 0) return "no further detail";
    return name_mod.truncateForError(a, first);
}
