//! The app key: the one secret that authenticates every client (ADR-0006),
//! generated on the first start and stored as plain text so Settings can show
//! it. Port of internal/auth/appkey.go (load, make and check; rotation comes
//! with Settings). Requests are compared with the copy in memory, never with
//! the database, on the SHA-256 of both values so the comparison takes the
//! same time whatever the length of the candidate.
const std = @import("std");
const Allocator = std.mem.Allocator;
const Io = std.Io;
const AppKeyRepo = @import("appkey_repo.zig").AppKeyRepo;
const Secret = @import("secret.zig").Secret;
const log = @import("log.zig");
const token = @import("token.zig");

pub const Service = struct {
    key: Secret,
    hash: token.Hash,

    /// Loads the key from `repo`, or makes and stores one when there is none
    /// (the first start). A stored value that is not shaped like an app key
    /// (the row was edited by hand) is replaced by a new one; every client then
    /// needs the new key. The key is never logged.
    pub fn init(gpa: Allocator, io: Io, repo: AppKeyRepo, logger: log.Logger) !Service {
        if (repo.get(gpa)) |stored| {
            defer {
                std.crypto.secureZero(u8, stored);
                gpa.free(stored);
            }
            if (token.kindOf(stored)) |kind| if (kind == .app_key) {
                return fromPlaintext(gpa, stored);
            };
            logger.warn("auth: the stored API key is malformed; making a new one", &.{});
        } else |err| switch (err) {
            error.NotFound => {},
            else => return err,
        }
        var fresh = try token.newToken(gpa, io, .app_key);
        defer fresh.deinit(gpa);
        try repo.set(fresh.plaintext);
        return fromPlaintext(gpa, fresh.plaintext);
    }

    fn fromPlaintext(gpa: Allocator, plaintext: []const u8) !Service {
        return .{ .key = try Secret.init(gpa, plaintext), .hash = token.hashToken(plaintext) };
    }

    pub fn deinit(self: *Service) void {
        self.key.deinit();
    }

    /// Reports whether `candidate` is the current key, in constant time.
    pub fn authenticate(self: *const Service, candidate: []const u8) bool {
        return token.hashEqual(self.hash, token.hashToken(candidate));
    }
};

const testing = std.testing;
const sqlite = @import("sqlite.zig");
const migrate = @import("migrate.zig");

const Rig = struct {
    tmp: testing.TmpDir,
    store: *sqlite.Store,
    out: Io.Writer.Allocating,
    core: log.Core,

    fn open(rig: *Rig) !void {
        rig.tmp = testing.tmpDir(.{});
        var buf: [std.fs.max_path_bytes]u8 = undefined;
        const n = try rig.tmp.dir.realPath(testing.io, &buf);
        var path_buf: [std.fs.max_path_bytes + 16]u8 = undefined;
        const path = try std.fmt.bufPrint(&path_buf, "{s}/test.db", .{buf[0..n]});
        rig.store = try sqlite.Store.open(testing.allocator, testing.io, path, .{ .readers = 2 }, null);
        const w = try rig.store.acquireWriter();
        defer w.release();
        var diag: migrate.Diagnostic = .{};
        try migrate.run(testing.allocator, w.db, migrate.embedded, 0, &diag);
        rig.out = .init(testing.allocator);
        rig.core = .init(testing.io, &rig.out.writer, .{ .level = .debug, .format = .text });
    }

    fn close(rig: *Rig) void {
        rig.out.deinit();
        rig.store.close();
        rig.tmp.cleanup();
    }
};

test "the first start makes an app key, stores it and accepts only it" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const repo: AppKeyRepo = .{ .store = rig.store };

    var svc = try Service.init(testing.allocator, testing.io, repo, rig.core.logger());
    defer svc.deinit();

    const stored = try repo.get(testing.allocator);
    defer testing.allocator.free(stored);
    try testing.expectEqual(token.Kind.app_key, token.kindOf(stored).?);
    try testing.expect(svc.authenticate(stored));
    try testing.expect(!svc.authenticate("bitnews_wrong"));
    try testing.expect(!svc.authenticate(""));
}

test "a stored app key is used as it is" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const repo: AppKeyRepo = .{ .store = rig.store };
    var issued = try token.newToken(testing.allocator, testing.io, .app_key);
    defer issued.deinit(testing.allocator);
    try repo.set(issued.plaintext);

    var svc = try Service.init(testing.allocator, testing.io, repo, rig.core.logger());
    defer svc.deinit();

    try testing.expect(svc.authenticate(issued.plaintext));
    try testing.expectEqual(@as(usize, 0), rig.out.written().len);
}

test "a malformed stored key is replaced and the replacement is stored" {
    var rig: Rig = undefined;
    try rig.open();
    defer rig.close();
    const repo: AppKeyRepo = .{ .store = rig.store };
    try repo.set("edited-by-hand");

    var svc = try Service.init(testing.allocator, testing.io, repo, rig.core.logger());
    defer svc.deinit();

    try testing.expect(!svc.authenticate("edited-by-hand"));
    const stored = try repo.get(testing.allocator);
    defer testing.allocator.free(stored);
    try testing.expect(svc.authenticate(stored));
    try testing.expect(std.mem.indexOf(u8, rig.out.written(), "malformed") != null);
    try testing.expect(std.mem.indexOf(u8, rig.out.written(), stored) == null);
}
