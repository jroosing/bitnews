//! Admin created from BITNEWS_ADMIN_PASSWORD_FILE.
//! Go: internal/auth/bootstrap.go BootstrapAdminFromFile.
//!
//! Count users first. When one already exists, return false and do not open
//! the file. Otherwise stat (so a fifo cannot block the open), read at most
//! 4 KiB plus one byte, trim a single trailing newline, hash with the
//! production argon2id parameters, and insert username `admin`. The password
//! bytes are wiped after the hash. A mode readable by group or others is only
//! a warning.
//!
//! The password never appears in a log line or an error string. `fromFile`
//! returns an error code; `writeErrorMessage` prints the Go text. Callers
//! (the daemon) own that printing, as with the key file.

const std = @import("std");
const builtin = @import("builtin");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const log = @import("log.zig");
const id = @import("id.zig");
const password = @import("password.zig");
const sqlite = @import("sqlite.zig");

/// Username of the bootstrapped admin (Go: BootstrapAdminUsername).
pub const admin_username = "admin";

/// Cap on the password file, not counting the one extra byte used to detect
/// a file that is over the cap (Go: MaxAdminPasswordFileBytes).
pub const max_file_bytes: usize = 4 << 10;

const created_msg = "admin account created from the admin password file";
const broad_msg = "admin password file is readable by group or others; restrict it to the owner (chmod 600)";
const not_regular_msg = "auth: admin password file is not a regular file";
const too_large_msg = "auth: admin password file is larger than 4 KiB";

pub const Error = password.PasswordError || error{
    /// `SELECT count(*)` failed. Message: `auth: count users: `.
    CountUsers,
    /// Stat or open failed because the path is missing.
    /// Message starts with `auth: admin password file: ` and does not put the
    /// path in the `file {path}:` slot (that slot is for rule failures).
    FileNotFound,
    /// Stat or open failed for another reason. Same missing-file prefix.
    PasswordFileUnavailable,
    /// The path is not a regular file. Message includes the path.
    PasswordFileNotRegular,
    /// The read itself failed. Message: `auth: read admin password file: `.
    PasswordFileRead,
    /// The file is larger than 4 KiB. Message includes the path.
    PasswordFileTooLarge,
    /// argon2 or the salt failed. Message: `auth: salt: `.
    HashPassword,
    /// The insert failed and it was not a lost race. Message:
    /// `auth: create admin from file: `.
    CreateAdmin,
} || Allocator.Error || Io.Cancelable;

/// Creates the admin from `path` when the database has no user.
/// `true` when this call inserted the row.
///
/// The writer lock is taken only for the insert. Counting uses a reader and
/// the hash runs with no database lock, so a 64 MiB argon2 does not stall
/// other work. A unique constraint from a concurrent creator is `false`:
/// setup is closed either way.
pub fn fromFile(gpa: Allocator, io: Io, store: *sqlite.Store, path: []const u8, logger: log.Logger) Error!bool {
    if (try countUsers(store) > 0) return false;

    var secret: [max_file_bytes + 1]u8 = undefined;
    defer std.crypto.secureZero(u8, &secret);
    const pw = try readPassword(io, path, &secret, logger);
    password.validatePassword(pw) catch |err| return err;

    const hash = password.hashPassword(gpa, io, pw, password.default_params) catch |err| return mapHash(err);
    defer gpa.free(hash);
    // The PHC string is what gets stored. Drop the password before the insert.
    std.crypto.secureZero(u8, &secret);

    if (!try insertAdmin(io, store, hash)) return false;
    logger.info(created_msg, &.{
        log.Attr.string("username", admin_username),
        log.Attr.string("path", path),
    });
    return true;
}

/// The Go error text for `err`. It never contains the password.
/// `path` is the password file; it is omitted from the missing-file prefix.
pub fn writeErrorMessage(w: *Io.Writer, err: Error, path: []const u8) Io.Writer.Error!void {
    switch (err) {
        error.PasswordTooShort => try w.print("auth: admin password file {s}: {s}", .{ path, password.passwordErrorMessage(error.PasswordTooShort) }),
        error.PasswordTooLong => try w.print("auth: admin password file {s}: {s}", .{ path, password.passwordErrorMessage(error.PasswordTooLong) }),
        error.PasswordInvalid => try w.print("auth: admin password file {s}: {s}", .{ path, password.passwordErrorMessage(error.PasswordInvalid) }),
        error.PasswordFileNotRegular => try w.print("auth: admin password file {s}: {s}", .{ path, not_regular_msg }),
        error.PasswordFileTooLarge => try w.print("auth: admin password file {s}: {s}", .{ path, too_large_msg }),
        error.FileNotFound => try w.print("auth: admin password file: stat {s}: file not found", .{path}),
        error.PasswordFileUnavailable => try w.print("auth: admin password file: open {s}: failed", .{path}),
        error.PasswordFileRead => try w.print("auth: read admin password file: {s}", .{path}),
        error.CountUsers => try w.writeAll("auth: count users: query failed"),
        error.CreateAdmin => try w.writeAll("auth: create admin from file: insert failed"),
        error.HashPassword => try w.writeAll("auth: salt: hash failed"),
        error.OutOfMemory => try w.writeAll("OutOfMemory"),
        error.Canceled => try w.writeAll("Canceled"),
    }
}

fn countUsers(store: *sqlite.Store) Error!i64 {
    const r = try store.acquireReader();
    defer r.release();
    var st = sqlite.Stmt.prepare(r.db, "SELECT count(*) FROM users") catch |err| return mapCount(err);
    defer st.finalize();
    const has_row = st.step() catch |err| return mapCount(err);
    if (!has_row) return error.CountUsers;
    return st.columnInt(0);
}

fn mapCount(err: sqlite.Error) Error {
    return switch (err) {
        error.OutOfMemory => error.OutOfMemory,
        else => error.CountUsers,
    };
}

fn readPassword(io: Io, path: []const u8, buf: *[max_file_bytes + 1]u8, logger: log.Logger) Error![]u8 {
    const cwd = Io.Dir.cwd();
    // Stat before open: a fifo or device must not block the read.
    const st = cwd.statFile(io, path, .{}) catch |err| return mapStat(err);
    if (st.kind != .file) return error.PasswordFileNotRegular;

    var file = cwd.openFile(io, path, .{}) catch |err| return mapOpen(err);
    defer file.close(io);
    if (file.stat(io)) |opened| {
        warnIfBroad(opened.permissions, path, logger);
    } else |_| {}

    const n = file.readPositionalAll(io, buf, 0) catch |err| return mapRead(err);
    if (n > max_file_bytes) return error.PasswordFileTooLarge;
    return trimOneNewline(buf[0..n]);
}

fn mapStat(err: Io.Dir.StatFileError) Error {
    return switch (err) {
        error.FileNotFound => error.FileNotFound,
        error.IsDir => error.PasswordFileNotRegular,
        error.Canceled => error.Canceled,
        else => error.PasswordFileUnavailable,
    };
}

fn mapOpen(err: Io.File.OpenError) Error {
    return switch (err) {
        error.FileNotFound => error.FileNotFound,
        error.IsDir => error.PasswordFileNotRegular,
        error.Canceled => error.Canceled,
        else => error.PasswordFileUnavailable,
    };
}

fn mapRead(err: Io.File.ReadPositionalError) Error {
    return switch (err) {
        error.Canceled => error.Canceled,
        else => error.PasswordFileRead,
    };
}

fn mapHash(err: password.HashError) Error {
    return switch (err) {
        error.OutOfMemory => error.OutOfMemory,
        error.Canceled => error.Canceled,
        else => error.HashPassword,
    };
}

/// One trailing `\n`, then one `\r` if that newline was a CRLF.
/// `foo\n\n` keeps the inner newline. A bare `\r` stays.
fn trimOneNewline(data: []u8) []u8 {
    if (data.len == 0 or data[data.len - 1] != '\n') return data;
    var end = data.len - 1;
    if (end > 0 and data[end - 1] == '\r') end -= 1;
    return data[0..end];
}

fn warnIfBroad(permissions: Io.File.Permissions, path: []const u8, logger: log.Logger) void {
    if (builtin.os.tag == .windows) return;
    const perm: u32 = @intCast(permissions.toMode() & 0o777);
    if (perm & 0o077 == 0) return;
    var mode_buf: [8]u8 = undefined;
    const text = std.fmt.bufPrint(&mode_buf, "{o:0>4}", .{perm}) catch return;
    logger.warn(broad_msg, &.{
        log.Attr.string("path", path),
        log.Attr.string("mode", text),
    });
}

fn insertAdmin(io: Io, store: *sqlite.Store, hash: []const u8) Error!bool {
    const w = try store.acquireWriter();
    defer w.release();
    const user_id = id.new(io);
    const now_ms = Io.Clock.now(.real, io).toMilliseconds();
    w.db.execArgs(
        "INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)",
        .{ user_id, admin_username, sqlite.Blob{ .bytes = hash }, now_ms },
    ) catch |err| switch (err) {
        error.Constraint => {
            if (w.db.failure().isConflict()) return false;
            return error.CreateAdmin;
        },
        error.OutOfMemory => return error.OutOfMemory,
        else => return error.CreateAdmin,
    };
    return true;
}

// ---------------------------------------------------------------- tests

const testing = std.testing;
const migrate = @import("migrate.zig");

fn openStore(tmp: *testing.TmpDir) !*sqlite.Store {
    var buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
    const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
    const store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
    errdefer store.close();
    const w = try store.acquireWriter();
    defer w.release();
    var diag: migrate.Diagnostic = .{};
    try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
    return store;
}

const LogRig = struct {
    out: std.Io.Writer.Allocating,
    core: log.Core,

    fn init(rig: *LogRig) void {
        rig.out = .init(testing.allocator);
        rig.core = .init(testing.io, &rig.out.writer, .{ .level = .debug, .format = .json, .clock = log.systemClock });
    }
    fn deinit(rig: *LogRig) void {
        rig.out.deinit();
    }
    fn logger(rig: *LogRig) log.Logger {
        return rig.core.logger();
    }
    fn written(rig: *LogRig) []const u8 {
        return rig.out.written();
    }
};

fn absJoin(tmp: *testing.TmpDir, rel: []const u8, buf: []u8) ![]u8 {
    var dir_buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &dir_buf);
    return std.fmt.bufPrint(buf, "{s}/{s}", .{ dir_buf[0..n], rel });
}

fn expectMessage(err: Error, path: []const u8, want: []const u8) !void {
    var aw: std.Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    try writeErrorMessage(&aw.writer, err, path);
    try testing.expectEqualStrings(want, aw.written());
}

test "one trailing newline is trimmed, and only one" {
    var lf = "twelvechars!\n".*;
    try testing.expectEqualStrings("twelvechars!", trimOneNewline(&lf));
    var crlf = "windows passphrase\r\n".*;
    try testing.expectEqualStrings("windows passphrase", trimOneNewline(&crlf));
    var two = "passphrase with spaces \n\n".*;
    try testing.expectEqualStrings("passphrase with spaces \n", trimOneNewline(&two));
    var bare = "bare cr\r".*;
    try testing.expectEqualStrings("bare cr\r", trimOneNewline(&bare));
    var only = "\n".*;
    try testing.expectEqualStrings("", trimOneNewline(&only));
}

test "a user already in the store skips the password file" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();
    {
        const w = try store.acquireWriter();
        defer w.release();
        try w.db.exec("INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'someone', x'24617267', 1700000000000)");
    }

    var path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const missing = try absJoin(&tmp, "does-not-exist", &path_buf);
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expect(!try fromFile(testing.allocator, testing.io, store, missing, rig.logger()));
    try testing.expectEqual(@as(usize, 0), rig.written().len);
}

test "a missing password file uses the prefix without the path slot" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    var path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const missing = try absJoin(&tmp, "missing", &path_buf);
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expectError(error.FileNotFound, fromFile(testing.allocator, testing.io, store, missing, rig.logger()));
    var want_buf: [std.fs.max_path_bytes + 80]u8 = undefined;
    const want = try std.fmt.bufPrint(&want_buf, "auth: admin password file: stat {s}: file not found", .{missing});
    try expectMessage(error.FileNotFound, missing, want);
    try testing.expect(std.mem.startsWith(u8, want, "auth: admin password file: "));
}

test "a directory is not a password file" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    var dir_buf: [std.fs.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &dir_buf);
    const dir_path = dir_buf[0..n];
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expectError(error.PasswordFileNotRegular, fromFile(testing.allocator, testing.io, store, dir_path, rig.logger()));
    var want_buf: [std.fs.max_path_bytes + 96]u8 = undefined;
    const want = try std.fmt.bufPrint(&want_buf, "auth: admin password file {s}: {s}", .{ dir_path, not_regular_msg });
    try expectMessage(error.PasswordFileNotRegular, dir_path, want);
}

test "a short password names the file and not the secret" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    const rel = "admin_password";
    const secret = "nope-secret";
    try tmp.dir.writeFile(testing.io, .{ .sub_path = rel, .data = secret });
    try tmp.dir.setFilePermissions(testing.io, rel, .fromMode(0o600), .{});
    var path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const path = try absJoin(&tmp, rel, &path_buf);
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expectError(error.PasswordTooShort, fromFile(testing.allocator, testing.io, store, path, rig.logger()));
    var want_buf: [std.fs.max_path_bytes + 128]u8 = undefined;
    const want = try std.fmt.bufPrint(&want_buf, "auth: admin password file {s}: {s}", .{ path, password.passwordErrorMessage(error.PasswordTooShort) });
    try expectMessage(error.PasswordTooShort, path, want);
    try testing.expect(std.mem.indexOf(u8, want, secret) == null);
    try testing.expectEqual(@as(usize, 0), rig.written().len);
}

test "a group-readable file warns and still rejects a short password" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    const rel = "admin_password";
    const secret = "nope-secret";
    try tmp.dir.writeFile(testing.io, .{ .sub_path = rel, .data = secret });
    try tmp.dir.setFilePermissions(testing.io, rel, .fromMode(0o644), .{});
    var path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const path = try absJoin(&tmp, rel, &path_buf);
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expectError(error.PasswordTooShort, fromFile(testing.allocator, testing.io, store, path, rig.logger()));
    const raw = rig.written();
    try testing.expect(std.mem.indexOf(u8, raw, broad_msg) != null);
    try testing.expect(std.mem.indexOf(u8, raw, "0644") != null);
    try testing.expect(std.mem.indexOf(u8, raw, secret) == null);
    try testing.expect(std.mem.indexOf(u8, raw, created_msg) == null);
}

test "a file over 4 KiB is refused and its bytes stay out of the error" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    const rel = "admin_password";
    var big: [max_file_bytes + 1]u8 = undefined;
    @memset(&big, 'a');
    try tmp.dir.writeFile(testing.io, .{ .sub_path = rel, .data = &big });
    try tmp.dir.setFilePermissions(testing.io, rel, .fromMode(0o600), .{});
    var path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const path = try absJoin(&tmp, rel, &path_buf);
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expectError(error.PasswordFileTooLarge, fromFile(testing.allocator, testing.io, store, path, rig.logger()));
    var want_buf: [std.fs.max_path_bytes + 96]u8 = undefined;
    const want = try std.fmt.bufPrint(&want_buf, "auth: admin password file {s}: {s}", .{ path, too_large_msg });
    try expectMessage(error.PasswordFileTooLarge, path, want);
    try testing.expect(std.mem.indexOf(u8, want, "aaa") == null);
}

test "exactly 4 KiB is read, then the password length rule applies" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    const rel = "admin_password";
    var body: [max_file_bytes]u8 = undefined;
    @memset(&body, 'a');
    try tmp.dir.writeFile(testing.io, .{ .sub_path = rel, .data = &body });
    try tmp.dir.setFilePermissions(testing.io, rel, .fromMode(0o600), .{});
    var path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const path = try absJoin(&tmp, rel, &path_buf);
    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    try testing.expectError(error.PasswordTooLong, fromFile(testing.allocator, testing.io, store, path, rig.logger()));
    var want_buf: [std.fs.max_path_bytes + 128]u8 = undefined;
    const want = try std.fmt.bufPrint(&want_buf, "auth: admin password file {s}: {s}", .{ path, password.passwordErrorMessage(error.PasswordTooLong) });
    try expectMessage(error.PasswordTooLong, path, want);
}
