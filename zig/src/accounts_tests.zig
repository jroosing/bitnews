//! Tests of the accounts group.
const std = @import("std");
const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");
const setup_gate = @import("setup_gate.zig");
const accounts_setup = @import("accounts_setup.zig");
const log = @import("log.zig");

test {
    _ = @import("accounts_setup.zig");
    _ = @import("accounts_bootstrap.zig");
}

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

fn setupTokenFormatOk(tok: []const u8) bool {
    if (tok.len != 52) return false;
    for (tok) |c| {
        const ok = (c >= 'A' and c <= 'Z') or (c >= '2' and c <= '7');
        if (!ok) return false;
    }
    return true;
}

test "fresh store logs one WARN setup token" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    var gate = try setup_gate.Gate.init(store);
    try testing.expect(gate.required());

    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    var setup = try accounts_setup.openSetup(testing.allocator, testing.io, &gate, rig.logger());
    defer setup.deinit();

    try testing.expect(setup.required());
    try testing.expect(gate.required());

    const raw = std.mem.trim(u8, rig.written(), &std.ascii.whitespace);
    var it = std.mem.splitScalar(u8, raw, '\n');
    var with_token: usize = 0;
    var token: []const u8 = undefined;
    while (it.next()) |line| {
        if (line.len == 0) continue;
        var parsed = try std.json.parseFromSlice(std.json.Value, testing.allocator, line, .{ .duplicate_field_behavior = .use_last });
        defer parsed.deinit();
        const obj = parsed.value.object;
        if (obj.get("setup_token") == null) continue;
        with_token += 1;
        try testing.expectEqualStrings("WARN", obj.get("level").?.string);
        const msg = obj.get("msg").?.string;
        try testing.expect(std.mem.indexOf(u8, msg, "first-run setup required: no admin exists yet.") != null);
        token = obj.get("setup_token").?.string;
        try testing.expect(setupTokenFormatOk(token));
        try testing.expect(std.mem.indexOf(u8, msg, token) == null);
    }
    try testing.expectEqual(@as(usize, 1), with_token);
}

test "user in store skips setup token log" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    {
        const w = try store.acquireWriter();
        defer w.release();
        try w.db.exec("INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'admin', x'24617267', 1700000000000)");
    }

    var gate = setup_gate.Gate{ .open = std.atomic.Value(bool).init(false) };

    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    var setup = try accounts_setup.openSetup(testing.allocator, testing.io, &gate, rig.logger());
    defer setup.deinit();

    try testing.expect(!setup.required());
    try testing.expect(!gate.required());

    const raw = std.mem.trim(u8, rig.written(), &std.ascii.whitespace);
    if (raw.len == 0) return;

    var it = std.mem.splitScalar(u8, raw, '\n');
    while (it.next()) |line| {
        if (line.len == 0) continue;
        var parsed = try std.json.parseFromSlice(std.json.Value, testing.allocator, line, .{ .duplicate_field_behavior = .use_last });
        defer parsed.deinit();
        const obj = parsed.value.object;
        try testing.expect(obj.get("setup_token") == null);
        if (obj.get("msg")) |msg_val| {
            const msg = msg_val.string;
            try testing.expect(std.mem.indexOf(u8, msg, "first-run setup required") == null);
        }
    }
}

test "bootstrap from password file creates admin" {
    const accounts_bootstrap = @import("accounts_bootstrap.zig");

    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    const pw_rel = "admin_password";
    try tmp.dir.writeFile(testing.io, .{ .sub_path = pw_rel, .data = "twelvechars!" });
    try tmp.dir.setFilePermissions(testing.io, pw_rel, .fromMode(0o600), .{});

    var dir_buf: [std.fs.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &dir_buf);
    var pw_path_buf: [std.fs.max_path_bytes + 32]u8 = undefined;
    const pw_path = try std.fmt.bufPrint(&pw_path_buf, "{s}/{s}", .{ dir_buf[0..dir_len], pw_rel });

    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    const created = try accounts_bootstrap.fromFile(testing.allocator, testing.io, store, pw_path, rig.logger());
    try testing.expect(created);

    const r = try store.acquireReader();
    defer r.release();
    var count_st = try sqlite.Stmt.prepare(r.db, "SELECT count(*) FROM users");
    defer count_st.finalize();
    try testing.expect(try count_st.step());
    try testing.expectEqual(@as(i64, 1), count_st.columnInt(0));

    var user_st = try sqlite.Stmt.prepare(r.db, "SELECT username FROM users");
    defer user_st.finalize();
    try testing.expect(try user_st.step());
    const username = try user_st.columnText(testing.allocator, 0);
    defer testing.allocator.free(username);
    try testing.expectEqualStrings("admin", username);
}

test "complete with matching token creates admin and closes gate" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    var gate = try setup_gate.Gate.init(store);
    try testing.expect(gate.required());

    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    var setup = try accounts_setup.openSetup(testing.allocator, testing.io, &gate, rig.logger());
    defer setup.deinit();
    try testing.expect(setup.required());
    try testing.expect(gate.required());

    try setup.complete(testing.allocator, testing.io, store, &gate, setup.token, "admin", "twelvechars!");

    try testing.expect(!setup.required());
    try testing.expect(!gate.required());

    const r = try store.acquireReader();
    defer r.release();
    var count_st = try sqlite.Stmt.prepare(r.db, "SELECT count(*) FROM users");
    defer count_st.finalize();
    try testing.expect(try count_st.step());
    try testing.expectEqual(@as(i64, 1), count_st.columnInt(0));

    var user_st = try sqlite.Stmt.prepare(r.db, "SELECT username FROM users");
    defer user_st.finalize();
    try testing.expect(try user_st.step());
    const username = try user_st.columnText(testing.allocator, 0);
    defer testing.allocator.free(username);
    try testing.expectEqualStrings("admin", username);
}

test "complete with wrong setup token creates no user" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    var gate = try setup_gate.Gate.init(store);
    try testing.expect(gate.required());

    var rig: LogRig = undefined;
    rig.init();
    defer rig.deinit();

    var setup = try accounts_setup.openSetup(testing.allocator, testing.io, &gate, rig.logger());
    defer setup.deinit();
    try testing.expect(setup.required());

    try testing.expectError(
        error.BadSetupToken,
        setup.complete(testing.allocator, testing.io, store, &gate, "wrong-token", "admin", "twelvechars!"),
    );

    const r = try store.acquireReader();
    defer r.release();
    var count_st = try sqlite.Stmt.prepare(r.db, "SELECT count(*) FROM users");
    defer count_st.finalize();
    try testing.expect(try count_st.step());
    try testing.expectEqual(@as(i64, 0), count_st.columnInt(0));
}
