//! Compatibility with the Go version, both directions (M15, ADR-0014).
//!
//! testdata/compat/crypto.json is written by the Go tests (internal/store and
//! internal/auth, `-update`): a master key and key file, sealed secrets,
//! password hashes and token hashes. Every entry must open or verify here.
//!
//! testdata/compat/zig_written.json is written by these tests when
//! BITNEWS_UPDATE_GOLDEN=1 is set, from fixed keys, nonces and salts, so it is
//! reproduced bit for bit. The Go tests read it and must open or verify every
//! entry with the Go code. Tests run with the working directory zig/.

const std = @import("std");
const Io = std.Io;
const Dir = Io.Dir;
const keyfile = @import("keyfile.zig");
const secretbox = @import("secretbox.zig");
const password = @import("password.zig");
const token = @import("token.zig");

const testing = std.testing;

const go_file = "testdata/compat/crypto.json";
const zig_file = "testdata/compat/zig_written.json";

const Sealed = struct {
    table: []const u8,
    column: []const u8,
    row_id: []const u8,
    plaintext: []const u8,
    stored: []const u8,
};

const PasswordEntry = struct {
    password: []const u8,
    phc: []const u8,
};

const TokenEntry = struct {
    kind: []const u8,
    plaintext: []const u8,
    hash_hex: []const u8,
};

/// The layout shared by both golden files. Field order is the file order.
const Golden = struct {
    master_key: []const u8,
    key_file: []const u8,
    sealed: []const Sealed,
    passwords: []const PasswordEntry,
    tokens: []const TokenEntry,
};

fn readGolden(arena: std.mem.Allocator, path: []const u8) !Golden {
    const text = try Dir.cwd().readFileAlloc(testing.io, path, arena, .limited(1 << 20));
    return std.json.parseFromSliceLeaky(Golden, arena, text, .{ .ignore_unknown_fields = true });
}

/// Opens and verifies every entry of a golden file with the Zig code.
fn checkGolden(arena: std.mem.Allocator, g: Golden) !void {
    var key = try keyfile.parse(g.master_key);
    defer key.deinit();
    try testing.expect(g.sealed.len > 0);
    for (g.sealed) |s| {
        const a = try secretbox.aad(arena, s.table, s.column, s.row_id);
        const plain = try secretbox.open(testing.allocator, &key, s.stored, a);
        defer secretbox.wipeAndFree(testing.allocator, plain);
        try testing.expectEqualStrings(s.plaintext, plain);

        const moved = try secretbox.aad(arena, s.table, s.column, try std.mem.concat(arena, u8, &.{ s.row_id, "x" }));
        try testing.expectError(error.Decrypt, secretbox.open(testing.allocator, &key, s.stored, moved));
    }

    try testing.expect(g.passwords.len > 0);
    for (g.passwords) |p| {
        try testing.expect(try password.verifyPassword(testing.allocator, testing.io, p.phc, p.password));
        const wrong = try std.mem.concat(arena, u8, &.{ p.password, "x" });
        try testing.expect(!try password.verifyPassword(testing.allocator, testing.io, p.phc, wrong));
    }

    try testing.expect(g.tokens.len > 0);
    for (g.tokens) |t| {
        const hex = std.fmt.bytesToHex(token.hashToken(t.plaintext), .lower);
        try testing.expectEqualStrings(t.hash_hex, &hex);
        if (t.kind.len > 0) {
            const kind = std.meta.stringToEnum(token.Kind, t.kind) orelse return error.UnknownTokenKind;
            try testing.expectEqual(kind, token.kindOf(t.plaintext).?);
        }
    }

    // The key file is read through the real loader, from a private temp dir.
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.writeFile(testing.io, .{ .sub_path = "bitnews.key", .data = g.key_file });
    try tmp.dir.setFilePermissions(testing.io, "bitnews.key", .fromMode(0o600), .{});
    var dir_buf: [Dir.max_path_bytes]u8 = undefined;
    const dir_len = try tmp.dir.realPath(testing.io, &dir_buf);
    const path = try std.fs.path.join(arena, &.{ dir_buf[0..dir_len], "bitnews.key" });
    var loaded = try keyfile.loadOrCreate(testing.io, path);
    defer loaded.key.deinit();
    try testing.expect(!loaded.generated);
    try testing.expect(loaded.key.eql(&key));
}

test "everything the Go code wrote opens and verifies here" {
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const g = try readGolden(arena_state.allocator(), go_file);
    try checkGolden(arena_state.allocator(), g);
}

// ---------------------------------------------------------------- Zig side

/// The values the Go tests seal, with the same shapes (empty, non-ASCII,
/// empty row id, text that needs JSON escapes).
const sealed_inputs = [_]struct { table: []const u8, column: []const u8, row_id: []const u8, plaintext: []const u8 }{
    .{ .table = "servers", .column = "password", .row_id = "srv1", .plaintext = "s3cret-pa55word" },
    .{ .table = "servers", .column = "username", .row_id = "srv1", .plaintext = "" },
    .{ .table = "servers", .column = "password", .row_id = "srv-é", .plaintext = "pässwörd-日本語-🔑" },
    .{ .table = "app_key", .column = "key", .row_id = "1", .plaintext = "bitnews_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqr" },
    .{ .table = "t", .column = "c", .row_id = "", .plaintext = "x" },
    .{ .table = "settings", .column = "value", .row_id = "long", .plaintext = "0123456789012345678901234567890123456789012345678901234567890123456789" },
    .{ .table = "t", .column = "c", .row_id = "r", .plaintext = "line1\nline2\t\"quoted\" \\ <&>" },
};

const password_inputs = [_]struct { password: []const u8, params: password.Params }{
    .{ .password = "correct horse battery staple", .params = .{ .memory = 64, .time = 2, .threads = 2, .salt_len = 16, .key_len = 32 } },
    .{ .password = "pässwörd-日本語-🔑-längd", .params = .{ .memory = 32, .time = 1, .threads = 1, .salt_len = 24, .key_len = 24 } },
    .{ .password = "twelve chars", .params = .{ .memory = 16, .time = 3, .threads = 1, .salt_len = 8, .key_len = 16 } },
};

/// Builds the golden content from fixed keys, nonces and salts.
fn buildZigGolden(arena: std.mem.Allocator) !Golden {
    var key: keyfile.Key = .{ .bytes = undefined };
    defer key.deinit();
    for (&key.bytes, 0..) |*b, i| b.* = @intCast(i * 7 + 1);
    const master_key = try arena.dupe(u8, std.base64.standard.Encoder.encode(try arena.alloc(u8, 44), &key.bytes));
    const key_file = try arena.dupe(u8, &keyfile.encodeLine(&key));

    const sealed = try arena.alloc(Sealed, sealed_inputs.len);
    for (sealed_inputs, sealed, 0..) |in, *out, i| {
        var nonce: [secretbox.nonce_size]u8 = undefined;
        for (&nonce, 0..) |*b, j| b.* = @intCast((i * 31 + j) % 256);
        const a = try secretbox.aad(arena, in.table, in.column, in.row_id);
        out.* = .{
            .table = in.table,
            .column = in.column,
            .row_id = in.row_id,
            .plaintext = in.plaintext,
            .stored = try secretbox.sealWithNonce(arena, &key, nonce, in.plaintext, a),
        };
    }

    const passwords = try arena.alloc(PasswordEntry, password_inputs.len);
    for (password_inputs, passwords, 0..) |in, *out, i| {
        var salt: [password.max_salt_len]u8 = undefined;
        for (salt[0..in.params.salt_len], 0..) |*b, j| b.* = @intCast((i * 17 + j * 3 + 5) % 256);
        out.* = .{
            .password = in.password,
            .phc = try password.hashPasswordWithSalt(arena, testing.io, in.password, in.params, salt[0..in.params.salt_len]),
        };
    }

    var tokens: std.ArrayList(TokenEntry) = .empty;
    for (std.enums.values(token.Kind), 0..) |kind, i| {
        var raw: [token.token_bytes]u8 = undefined;
        for (&raw, 0..) |*b, j| b.* = @intCast((i * 91 + j * 13 + 7) % 256);
        const issued = try token.newTokenFromRaw(arena, kind, raw);
        try tokens.append(arena, .{
            .kind = kind.name(),
            .plaintext = issued.plaintext,
            .hash_hex = try arena.dupe(u8, &std.fmt.bytesToHex(issued.hash, .lower)),
        });
    }
    try tokens.append(arena, .{
        .kind = "",
        .plaintext = "abc",
        .hash_hex = try arena.dupe(u8, &std.fmt.bytesToHex(token.hashToken("abc"), .lower)),
    });

    return .{
        .master_key = master_key,
        .key_file = key_file,
        .sealed = sealed,
        .passwords = passwords,
        .tokens = tokens.items,
    };
}

test "what the Zig code writes is reproduced bit for bit and opens with the Zig code" {
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    const arena = arena_state.allocator();

    const g = try buildZigGolden(arena);
    try checkGolden(arena, g);

    var json = try std.json.Stringify.valueAlloc(arena, g, .{ .whitespace = .indent_2 });
    json = try std.mem.concat(arena, u8, &.{ json, "\n" });

    if (testing.environ.getPosix("BITNEWS_UPDATE_GOLDEN")) |v| {
        if (std.mem.eql(u8, v, "1")) {
            try Dir.cwd().createDirPath(testing.io, "testdata/compat");
            try Dir.cwd().writeFile(testing.io, .{ .sub_path = zig_file, .data = json });
        }
    }

    const committed = try Dir.cwd().readFileAlloc(testing.io, zig_file, arena, .limited(1 << 20));
    try testing.expectEqualStrings(committed, json);
    try checkGolden(arena, try readGolden(arena, zig_file));
}
