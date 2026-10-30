//! The download_sources table (internal/store/repo_downloads_source.go): what
//! the Usenet runner needs to fetch an NZB Download again after a restart, the
//! stored .nzb file and the archive password. The password is sealed with the
//! master key and the AAD "download_sources/password/<download id>" (NULL when
//! there is none), the same bytes the Go version writes and reads.
//!
//! Strings of results live in the allocator the caller passes.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const sqlite = @import("sqlite.zig");
const secretbox = @import("secretbox.zig");
const Key = @import("keyfile.zig").Key;

pub const Error = error{
    /// The Download has a source already (Go: core.ErrConflict).
    Conflict,
    /// The Download has no source (Go: core.ErrNotFound).
    NotFound,
} || sqlite.Error || Allocator.Error || Io.Cancelable || Io.RandomSecureError;

pub const Source = struct {
    nzb_path: []u8,
    /// The archive password; empty when there is none or it cannot be opened.
    /// Wipe it with `secretbox.wipeAndFree` when done.
    password: []u8,
    /// A password is stored but the key or the value is wrong (Go:
    /// core.ErrSecretUnavailable); the path is still good.
    password_unavailable: bool = false,

    pub fn deinit(s: *Source, alloc: Allocator) void {
        alloc.free(s.nzb_path);
        secretbox.wipeAndFree(alloc, s.password);
        s.* = undefined;
    }
};

pub const SourceRepo = struct {
    store: *sqlite.Store,
    /// Owned by whoever loaded it; must outlive the repo.
    key: *const Key,

    /// Stores the source of an existing Download. A second source for the
    /// same Download is `Conflict`.
    pub fn create(self: SourceRepo, alloc: Allocator, download_id: []const u8, nzb_path: []const u8, password: []const u8) Error!void {
        var sealed: ?[]u8 = null;
        defer if (sealed) |s| alloc.free(s);
        if (password.len > 0) {
            const aad = try secretbox.aad(alloc, "download_sources", "password", download_id);
            defer alloc.free(aad);
            sealed = try secretbox.seal(alloc, self.store.io, self.key, password, aad);
        }
        const w = try self.store.acquireWriter();
        defer w.release();
        w.db.execArgs(
            "INSERT INTO download_sources (download_id, nzb_path, password) VALUES (?, ?, ?)",
            .{ download_id, nzb_path, @as(?[]const u8, sealed) },
        ) catch |err| {
            if (err == error.Constraint and w.db.failure().isConflict()) return error.Conflict;
            return err;
        };
    }

    /// The source of a Download, `NotFound` when it has none.
    pub fn get(self: SourceRepo, alloc: Allocator, download_id: []const u8) Error!Source {
        const r = try self.store.acquireReader();
        defer r.release();
        var st = try sqlite.Stmt.prepare(r.db, "SELECT nzb_path, password FROM download_sources WHERE download_id = ?");
        defer st.finalize();
        try st.bindAll(.{download_id});
        if (!try st.step()) return error.NotFound;
        const path = try st.columnText(alloc, 0);
        errdefer alloc.free(path);
        if (st.columnIsNull(1)) return .{ .nzb_path = path, .password = try alloc.dupe(u8, "") };
        const sealed = try st.columnText(alloc, 1);
        defer alloc.free(sealed);
        const aad = try secretbox.aad(alloc, "download_sources", "password", download_id);
        defer alloc.free(aad);
        const plain = secretbox.open(alloc, self.key, sealed, aad) catch |err| switch (err) {
            error.Decrypt, error.UnsupportedFormat => return .{ .nzb_path = path, .password = try alloc.dupe(u8, ""), .password_unavailable = true },
            error.OutOfMemory => return error.OutOfMemory,
        };
        return .{ .nzb_path = path, .password = plain };
    }
};

const testing = std.testing;
const tu = @import("downloads_testutil.zig");

fn addDownload(rig: *tu.Rig, id: []const u8) !void {
    try rig.repo.create(.{ .id = id, .name = id, .source_kind = .nzb, .state = .added, .added_at = tu.epoch, .updated_at = tu.epoch });
}

test "a source round-trips, and the password is stored sealed for the Go AAD" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try addDownload(&rig, "d1");
    const key: Key = .{ .bytes = @splat(7) };
    const repo: SourceRepo = .{ .store = rig.store, .key = &key };

    try repo.create(testing.allocator, "d1", "/data/incomplete/d1/x.nzb", "s3cret");
    var got = try repo.get(testing.allocator, "d1");
    defer got.deinit(testing.allocator);
    try testing.expectEqualStrings("/data/incomplete/d1/x.nzb", got.nzb_path);
    try testing.expectEqualStrings("s3cret", got.password);
    try testing.expect(!got.password_unavailable);

    // The stored text opens with the AAD Go uses.
    const r = try rig.store.acquireReader();
    defer r.release();
    var st = try sqlite.Stmt.prepare(r.db, "SELECT password FROM download_sources WHERE download_id = 'd1'");
    defer st.finalize();
    try testing.expect(try st.step());
    const stored = try st.columnText(testing.allocator, 0);
    defer testing.allocator.free(stored);
    try testing.expect(std.mem.startsWith(u8, stored, "v1:"));
    try testing.expect(std.mem.indexOf(u8, stored, "s3cret") == null);
    const plain = try secretbox.open(testing.allocator, &key, stored, "download_sources/password/d1");
    defer secretbox.wipeAndFree(testing.allocator, plain);
    try testing.expectEqualStrings("s3cret", plain);
}

test "no password is NULL, a second source conflicts, a missing one is not found" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try addDownload(&rig, "d1");
    const key: Key = .{ .bytes = @splat(7) };
    const repo: SourceRepo = .{ .store = rig.store, .key = &key };

    try repo.create(testing.allocator, "d1", "/p/x.nzb", "");
    var got = try repo.get(testing.allocator, "d1");
    defer got.deinit(testing.allocator);
    try testing.expectEqualStrings("", got.password);
    try testing.expectEqual(@as(usize, 1), try rig.countWhere("id = 'd1'"));
    try testing.expectError(error.Conflict, repo.create(testing.allocator, "d1", "/p/y.nzb", ""));
    try testing.expectError(error.NotFound, repo.get(testing.allocator, "nope"));
}

test "a password that cannot be opened keeps the path" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try addDownload(&rig, "d1");
    const key: Key = .{ .bytes = @splat(7) };
    const other: Key = .{ .bytes = @splat(9) };
    try (SourceRepo{ .store = rig.store, .key = &key }).create(testing.allocator, "d1", "/p/x.nzb", "pw");

    var got = try (SourceRepo{ .store = rig.store, .key = &other }).get(testing.allocator, "d1");
    defer got.deinit(testing.allocator);
    try testing.expectEqualStrings("/p/x.nzb", got.nzb_path);
    try testing.expect(got.password_unavailable);
    try testing.expectEqualStrings("", got.password);
}

test "deleting the Download deletes its source" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    try addDownload(&rig, "d1");
    const key: Key = .{ .bytes = @splat(7) };
    const repo: SourceRepo = .{ .store = rig.store, .key = &key };
    try repo.create(testing.allocator, "d1", "/p/x.nzb", "pw");

    try rig.repo.delete("d1");

    try testing.expectError(error.NotFound, repo.get(testing.allocator, "d1"));
}

test "a source for a Download that does not exist is refused" {
    var rig: tu.Rig = undefined;
    try rig.open();
    defer rig.close();
    const key: Key = .{ .bytes = @splat(7) };
    const repo: SourceRepo = .{ .store = rig.store, .key = &key };
    try testing.expect(std.meta.isError(repo.create(testing.allocator, "ghost", "/p/x.nzb", "")));
}
