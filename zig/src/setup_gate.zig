//! First-run setup state: setup is open while no User exists (auth.Setup of
//! the Go version, without the token and the admin creation, which come with
//! the web UI). The server asks the gate on every request, so it is one atomic
//! load; it never queries the database after start.
const std = @import("std");
const sqlite = @import("sqlite.zig");
const common = @import("http_common.zig");

pub const Gate = struct {
    open: std.atomic.Value(bool),

    /// Counts the users once.
    pub fn init(store: *sqlite.Store) !Gate {
        const r = try store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT count(*) FROM users");
        defer st.finalize();
        const has_row = try st.step();
        std.debug.assert(has_row);
        return .{ .open = .init(st.columnInt(0) == 0) };
    }

    pub fn required(self: *const Gate) bool {
        return self.open.load(.acquire);
    }

    /// Setup finished: an admin User exists. One store.
    pub fn close(self: *Gate) void {
        self.open.store(false, .release);
    }

    /// The gate as the HTTP server takes it. `self` must outlive the server.
    pub fn asSetupGate(self: *Gate) common.SetupGate {
        return .{ .ctx = self, .required = requiredFn };
    }

    fn requiredFn(ctx: *anyopaque) bool {
        const self: *const Gate = @ptrCast(@alignCast(ctx));
        return self.required();
    }
};

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

test "setup is open until a user exists" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const store = try openStore(&tmp);
    defer store.close();

    const before = try Gate.init(store);
    try testing.expect(before.required());

    {
        const w = try store.acquireWriter();
        defer w.release();
        try w.db.exec("INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'admin', x'24617267', 1700000000000)");
    }
    const after = try Gate.init(store);
    try testing.expect(!after.required());
}
