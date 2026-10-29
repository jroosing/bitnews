//! Password hashing and the password policy (Go: internal/auth/password.go).
//!
//! Passwords are hashed with argon2id into a PHC string
//! ("$argon2id$v=19$m=..,t=..,p=..$salt$hash", unpadded base64). The strings
//! are shared with the Go version: each verifies what the other hashed. The
//! parameters of a stored hash come from a database row, so they are hostile
//! input: they are bounded before any hashing, so a poisoned row cannot make a
//! login allocate gigabytes or burn CPU.
//!
//! Passwords are plain byte slices here; the caller keeps them out of logs
//! (core.Secret in the Go version) and zeroes its copy when it can.

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const argon2 = std.crypto.pwhash.argon2;
const KdfError = std.crypto.pwhash.KdfError;

/// The argon2id cost parameters. `memory` is in KiB.
pub const Params = struct {
    memory: u32,
    time: u32,
    threads: u8,
    salt_len: usize,
    key_len: usize,

    pub fn eql(a: Params, b: Params) bool {
        return a.memory == b.memory and a.time == b.time and a.threads == b.threads and
            a.salt_len == b.salt_len and a.key_len == b.key_len;
    }
};

/// The production parameters from the design: m=64 MiB, t=3, p=2, 16-byte
/// salt, 32-byte key.
pub const default_params: Params = .{ .memory = 64 * 1024, .time = 3, .threads = 2, .salt_len = 16, .key_len = 32 };

/// Bounds on parameters read from a stored hash.
pub const max_hash_len = 512;
pub const max_memory_kib = 1 << 20; // 1 GiB
pub const max_time = 10;
pub const max_threads = 16;
pub const min_salt_len = 8;
pub const max_salt_len = 64;
pub const min_key_len = 16;
pub const max_key_len = 64;

/// A stored hash is malformed, uses an unsupported variant, or carries
/// parameters outside the accepted range.
pub const InvalidHash = error.InvalidHash;

pub const HashError = KdfError || error{
    /// The parameters are outside the bounds that `verifyPassword` accepts, so
    /// the hash could not be verified later.
    InvalidParams,
} || Io.RandomSecureError;

/// Hashes `password` with argon2id under a fresh random salt and returns a PHC
/// string owned by the caller.
pub fn hashPassword(allocator: Allocator, io: Io, password: []const u8, params: Params) HashError![]u8 {
    if (params.salt_len < min_salt_len or params.salt_len > max_salt_len) return error.InvalidParams;
    var salt: [max_salt_len]u8 = undefined;
    try io.randomSecure(salt[0..params.salt_len]);
    return hashPasswordWithSalt(allocator, io, password, params, salt[0..params.salt_len]);
}

/// `hashPassword` with the salt given, for tests that need reproducible output.
pub fn hashPasswordWithSalt(allocator: Allocator, io: Io, password: []const u8, params: Params, salt: []const u8) HashError![]u8 {
    if (salt.len != params.salt_len or salt.len < min_salt_len or salt.len > max_salt_len) return error.InvalidParams;
    if (!paramsInBounds(params)) return error.InvalidParams;

    var key: [max_key_len]u8 = undefined;
    defer std.crypto.secureZero(u8, &key);
    try argon2.kdf(allocator, key[0..params.key_len], password, salt, .{
        .t = params.time,
        .m = params.memory,
        .p = params.threads,
    }, .argon2id, io);

    const enc = std.base64.standard_no_pad.Encoder;
    var salt_b64: [enc.calcSize(max_salt_len)]u8 = undefined;
    var key_b64: [enc.calcSize(max_key_len)]u8 = undefined;
    return std.fmt.allocPrint(allocator, "$argon2id$v=19$m={d},t={d},p={d}${s}${s}", .{
        params.memory,
        params.time,
        params.threads,
        enc.encode(&salt_b64, salt),
        enc.encode(&key_b64, key[0..params.key_len]),
    });
}

fn paramsInBounds(p: Params) bool {
    return p.time >= 1 and p.time <= max_time and p.threads >= 1 and p.threads <= max_threads and
        p.memory >= 8 * @as(u32, p.threads) and p.memory <= max_memory_kib and
        p.key_len >= min_key_len and p.key_len <= max_key_len;
}

/// A validated PHC string, split into its parts.
pub const ParsedHash = struct {
    params: Params,
    salt_buf: [max_salt_len]u8,
    key_buf: [max_key_len]u8,

    pub fn salt(self: *const ParsedHash) []const u8 {
        return self.salt_buf[0..self.params.salt_len];
    }

    pub fn key(self: *const ParsedHash) []const u8 {
        return self.key_buf[0..self.params.key_len];
    }
};

/// Validates a PHC string without running argon2.
///
/// Go's base64 decoder skips CR and LF inside the text; this one rejects them.
/// No hash either version writes contains one.
pub fn parseHash(hash: []const u8) error{InvalidHash}!ParsedHash {
    if (hash.len > max_hash_len) return error.InvalidHash;

    var parts: [6][]const u8 = undefined;
    var it = std.mem.splitScalar(u8, hash, '$');
    for (&parts) |*part| part.* = it.next() orelse return error.InvalidHash;
    if (it.next() != null) return error.InvalidHash;
    if (parts[0].len != 0 or !std.mem.eql(u8, parts[1], "argon2id") or !std.mem.eql(u8, parts[2], "v=19")) return error.InvalidHash;

    var kv_it = std.mem.splitScalar(u8, parts[3], ',');
    const kv_m = kv_it.next() orelse return error.InvalidHash;
    const kv_t = kv_it.next() orelse return error.InvalidHash;
    const kv_p = kv_it.next() orelse return error.InvalidHash;
    if (kv_it.next() != null) return error.InvalidHash;
    const m = paramValue(kv_m, "m=") orelse return error.InvalidHash;
    const t = paramValue(kv_t, "t=") orelse return error.InvalidHash;
    const p = paramValue(kv_p, "p=") orelse return error.InvalidHash;
    if (t < 1 or t > max_time or p < 1 or p > max_threads or m < 8 * p or m > max_memory_kib) return error.InvalidHash;

    const dec = std.base64.standard_no_pad.Decoder;
    const salt_len = dec.calcSizeForSlice(parts[4]) catch return error.InvalidHash;
    const key_len = dec.calcSizeForSlice(parts[5]) catch return error.InvalidHash;
    if (salt_len < min_salt_len or salt_len > max_salt_len or key_len < min_key_len or key_len > max_key_len) return error.InvalidHash;

    var ph: ParsedHash = .{
        .params = .{ .memory = m, .time = t, .threads = @intCast(p), .salt_len = salt_len, .key_len = key_len },
        .salt_buf = undefined,
        .key_buf = undefined,
    };
    dec.decode(ph.salt_buf[0..salt_len], parts[4]) catch return error.InvalidHash;
    dec.decode(ph.key_buf[0..key_len], parts[5]) catch return error.InvalidHash;
    return ph;
}

/// Digits only, no sign, no separators, fits in 32 bits (Go: ParseUint(v, 10, 32)).
fn paramValue(s: []const u8, prefix: []const u8) ?u32 {
    if (!std.mem.startsWith(u8, s, prefix)) return null;
    const digits = s[prefix.len..];
    if (digits.len == 0) return null;
    var n: u32 = 0;
    for (digits) |c| {
        if (c < '0' or c > '9') return null;
        n = std.math.mul(u32, n, 10) catch return null;
        n = std.math.add(u32, n, c - '0') catch return null;
    }
    return n;
}

pub const VerifyError = KdfError || error{InvalidHash};

/// Reports whether `password` matches the PHC string `hash`. It reads the
/// parameters from `hash`, so hashes made with older settings keep working. A
/// wrong password is `false`; a bad hash is `error.InvalidHash`.
pub fn verifyPassword(allocator: Allocator, io: Io, hash: []const u8, password: []const u8) VerifyError!bool {
    const ph = try parseHash(hash);
    var got: [max_key_len]u8 = @splat(0);
    defer std.crypto.secureZero(u8, &got);
    try argon2.kdf(allocator, got[0..ph.params.key_len], password, ph.salt(), .{
        .t = ph.params.time,
        .m = ph.params.memory,
        .p = ph.params.threads,
    }, .argon2id, io);
    var want: [max_key_len]u8 = @splat(0);
    @memcpy(want[0..ph.params.key_len], ph.key());
    return std.crypto.timing_safe.eql([max_key_len]u8, got, want);
}

/// Reports whether `hash` should be replaced after a successful login: it is
/// unparseable or was made with other parameters than `current`.
pub fn needsRehash(hash: []const u8, current: Params) bool {
    const ph = parseHash(hash) catch return true;
    return !ph.params.eql(current);
}

/// Password length limits. The minimum counts code points; the maximum counts
/// bytes and exists so a huge body cannot make argon2 hash megabytes.
pub const min_password_runes = 12;
pub const max_password_bytes = 1024;

pub const PasswordError = error{ PasswordTooShort, PasswordTooLong, PasswordInvalid };

/// Checks the length rules.
pub fn validatePassword(password: []const u8) PasswordError!void {
    if (password.len > max_password_bytes) return error.PasswordTooLong;
    if (!std.unicode.utf8ValidateSlice(password)) return error.PasswordInvalid;
    // Valid UTF-8, so counting cannot fail.
    const runes = std.unicode.utf8CountCodepoints(password) catch unreachable;
    if (runes < min_password_runes) return error.PasswordTooShort;
}

/// The user-facing text of a password error, as in the Go version. It never
/// contains the password.
pub fn passwordErrorMessage(err: PasswordError) []const u8 {
    return switch (err) {
        error.PasswordTooShort => "auth: password must be at least 12 characters",
        error.PasswordTooLong => "auth: password must be at most 1024 bytes",
        error.PasswordInvalid => "auth: password is not valid UTF-8",
    };
}

// ---------------------------------------------------------------- tests

const testing = std.testing;

/// The cheapest valid cost, so tests stay quick (Go: UseFastParams).
const fast: Params = .{ .memory = 8, .time = 1, .threads = 1, .salt_len = 16, .key_len = 32 };

test "the production parameters match the spec" {
    try testing.expectEqual(64 * 1024, default_params.memory);
    try testing.expectEqual(3, default_params.time);
    try testing.expectEqual(2, default_params.threads);
    try testing.expectEqual(16, default_params.salt_len);
    try testing.expectEqual(32, default_params.key_len);
}

test "a hashed password is a PHC string that verifies" {
    const hash = try hashPassword(testing.allocator, testing.io, "correct horse battery", fast);
    defer testing.allocator.free(hash);
    try testing.expect(std.mem.startsWith(u8, hash, "$argon2id$v=19$m=8,t=1,p=1$"));
    try testing.expect(try verifyPassword(testing.allocator, testing.io, hash, "correct horse battery"));
    try testing.expect(!try verifyPassword(testing.allocator, testing.io, hash, "correct horse batterY"));
    try testing.expect(!try verifyPassword(testing.allocator, testing.io, hash, ""));
}

test "every hash uses a fresh salt" {
    const a = try hashPassword(testing.allocator, testing.io, "correct horse battery", fast);
    defer testing.allocator.free(a);
    const b = try hashPassword(testing.allocator, testing.io, "correct horse battery", fast);
    defer testing.allocator.free(b);
    try testing.expect(!std.mem.eql(u8, a, b));
}

test "a given salt reproduces the same hash" {
    const salt = "0123456789abcdef";
    const a = try hashPasswordWithSalt(testing.allocator, testing.io, "pw", fast, salt);
    defer testing.allocator.free(a);
    const b = try hashPasswordWithSalt(testing.allocator, testing.io, "pw", fast, salt);
    defer testing.allocator.free(b);
    try testing.expectEqualStrings(a, b);
}

test "hashing refuses parameters that verification would refuse" {
    const bad = [_]Params{
        .{ .memory = 7, .time = 1, .threads = 1, .salt_len = 16, .key_len = 32 },
        .{ .memory = 8, .time = 0, .threads = 1, .salt_len = 16, .key_len = 32 },
        .{ .memory = 8, .time = 11, .threads = 1, .salt_len = 16, .key_len = 32 },
        .{ .memory = 8, .time = 1, .threads = 0, .salt_len = 16, .key_len = 32 },
        .{ .memory = 8, .time = 1, .threads = 1, .salt_len = 7, .key_len = 32 },
        .{ .memory = 8, .time = 1, .threads = 1, .salt_len = 16, .key_len = 65 },
        .{ .memory = 8, .time = 1, .threads = 1, .salt_len = 16, .key_len = 15 },
    };
    for (bad) |p| try testing.expectError(error.InvalidParams, hashPassword(testing.allocator, testing.io, "pw", p));
}

const test_salt_b64 = "c2FsdHNhbHRzYWx0";
const test_key_b64 = "aGFzaGhhc2hoYXNoaGFzaA";

test "a malformed hash is an error, not a panic" {
    const cases = [_][]const u8{
        "",
        "not a hash",
        "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHQ", // too few parts
        "$argon2id$v=19$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64 ++ "$x", // too many parts
        "$argon2i$v=19$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // wrong algorithm
        "$argon2id$v=16$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // wrong version
        "$argon2id$v=x$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64,
        "$argon2id$v=19$m=x,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64,
        "$argon2id$v=19$m=8,t=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // missing param
        "$argon2id$v=19$m=8,t=1,p=1,x=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // extra param
        "$argon2id$v=19$m=8,t=1,p=1x$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // trailing junk
        "$argon2id$v=19$m=-8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64,
        "$argon2id$v=19$m=+8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64,
        "$argon2id$v=19$m=,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64,
        "$argon2id$v=19$t=1,m=8,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // wrong order
        "$argon2id$v=19$m=8,t=1,p=1$!!!$" ++ test_key_b64, // bad salt base64
        "$argon2id$v=19$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$!!!", // bad hash base64
        "$argon2id$v=19$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64 ++ "==", // padded base64
        "$argon2id$v=19$m=8,t=1,p=1$$" ++ test_key_b64, // empty salt
        "$argon2id$v=19$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$", // empty key
        "$argon2id$v=19$m=8,t=1,p=0$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // zero threads
        "$argon2id$v=19$m=8,t=0,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // zero time
        "$argon2id$v=19$m=7,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64, // memory below 8*p
    };
    for (cases) |h| {
        try testing.expectError(error.InvalidHash, verifyPassword(testing.allocator, testing.io, h, "whatever password"));
    }
}

test "absurd parameters are rejected before any hashing" {
    // A failure here would try to allocate gigabytes or run for minutes.
    const params = [_][]const u8{
        "m=1048577,t=1,p=1",
        "m=4294967295,t=1,p=1",
        "m=65536,t=11,p=1",
        "m=8,t=4294967295,p=1",
        "m=65536,t=1,p=17",
        "m=65536,t=1,p=255",
        "m=99999999999999999999,t=1,p=1",
        "m=4294967296,t=1,p=1",
    };
    inline for (params) |p| {
        const h = "$argon2id$v=19$" ++ p ++ "$" ++ test_salt_b64 ++ "$" ++ test_key_b64;
        try testing.expectError(error.InvalidHash, verifyPassword(testing.failing_allocator, testing.io, h, "whatever password"));
    }
}

test "an oversized hash string is rejected" {
    const run4000: [4000]u8 = @splat('A');
    const run40: [40]u8 = @splat('A');
    const big = "$argon2id$v=19$m=8,t=1,p=1$" ++ run4000 ++ "$" ++ run40;
    try testing.expectError(error.InvalidHash, verifyPassword(testing.failing_allocator, testing.io, big, "whatever password"));
}

test "verification uses the parameters stored in the hash" {
    const hash = try hashPassword(testing.allocator, testing.io, "correct horse battery", .{ .memory = 16, .time = 2, .threads = 1, .salt_len = 16, .key_len = 32 });
    defer testing.allocator.free(hash);
    try testing.expect(std.mem.find(u8, hash, "m=16,t=2,p=1") != null);
    try testing.expect(try verifyPassword(testing.allocator, testing.io, hash, "correct horse battery"));
}

test "a hash with another salt or key length verifies" {
    const hash = try hashPassword(testing.allocator, testing.io, "pw", .{ .memory = 16, .time = 1, .threads = 2, .salt_len = 24, .key_len = 20 });
    defer testing.allocator.free(hash);
    try testing.expect(try verifyPassword(testing.allocator, testing.io, hash, "pw"));
    try testing.expect(!try verifyPassword(testing.allocator, testing.io, hash, "pW"));
}

test "needsRehash compares every parameter" {
    const hash = try hashPassword(testing.allocator, testing.io, "correct horse battery", fast);
    defer testing.allocator.free(hash);
    try testing.expect(!needsRehash(hash, fast));
    try testing.expect(needsRehash(hash, .{ .memory = 16, .time = 1, .threads = 1, .salt_len = 16, .key_len = 32 }));
    try testing.expect(needsRehash(hash, .{ .memory = 8, .time = 2, .threads = 1, .salt_len = 16, .key_len = 32 }));
    try testing.expect(needsRehash(hash, .{ .memory = 8, .time = 1, .threads = 1, .salt_len = 16, .key_len = 64 }));
    try testing.expect(needsRehash(hash, .{ .memory = 8, .time = 1, .threads = 1, .salt_len = 32, .key_len = 32 }));
    try testing.expect(needsRehash("garbage", fast));
}

test "the minimum length counts code points" {
    try validatePassword("twelve chars");
    try validatePassword("日本語日本語日本語日本語"); // 12 runes, 36 bytes
    try testing.expectError(error.PasswordTooShort, validatePassword("eleven char"));
    try testing.expectError(error.PasswordTooShort, validatePassword("日本語日本語日本語日"));
    try testing.expectError(error.PasswordTooShort, validatePassword(""));
}

test "the maximum length is 1024 bytes" {
    const ok: [1024]u8 = @splat('a');
    try validatePassword(&ok);
    const long: [1025]u8 = @splat('a');
    try testing.expectError(error.PasswordTooLong, validatePassword(&long));
}

test "invalid UTF-8 is rejected" {
    try testing.expectError(error.PasswordInvalid, validatePassword("valid prefix \xff\xfe"));
    try testing.expectError(error.PasswordInvalid, validatePassword("\xed\xa0\x80 surrogate half........"));
}

test "password error messages never contain the password" {
    for ([_]PasswordError{ error.PasswordTooShort, error.PasswordTooLong, error.PasswordInvalid }) |e| {
        try testing.expect(std.mem.find(u8, passwordErrorMessage(e), "hunter2") == null);
    }
}

fn fuzzHash(_: void, smith: *testing.Smith) anyerror!void {
    var buf: [600]u8 = undefined;
    const n = smith.slice(&buf);
    const hash = buf[0..n];
    const ph = parseHash(hash) catch return;
    const p = ph.params;
    if (p.memory > max_memory_kib or p.time > max_time or p.threads > max_threads or
        p.memory < 8 * @as(u32, p.threads) or p.time < 1 or p.threads < 1) return error.AcceptedOutOfRangeParams;
    if (ph.salt().len != p.salt_len or ph.key().len != p.key_len) return error.LengthMismatch;
    // Verification only for hashes cheap enough to run in a fuzz loop.
    if (p.memory <= 256 and p.time <= 2) {
        _ = try verifyPassword(testing.allocator, testing.io, hash, "fuzz password");
    }
}

/// A Smith stream holding one slice: a u32 little-endian length and the bytes.
fn seed(comptime bytes: []const u8) []const u8 {
    const len: [4]u8 = .{ @truncate(bytes.len), @truncate(bytes.len >> 8), @truncate(bytes.len >> 16), @truncate(bytes.len >> 24) };
    return &len ++ bytes;
}

test "fuzz: parsing and verifying arbitrary PHC strings never crashes or leaks" {
    try testing.fuzz({}, fuzzHash, .{ .corpus = &.{
        seed("$argon2id$v=19$m=65536,t=3,p=2$" ++ test_salt_b64 ++ "$" ++ test_key_b64),
        seed("$argon2id$v=19$m=8,t=1,p=1$" ++ test_salt_b64 ++ "$" ++ test_key_b64),
        seed("$argon2id$v=19$m=99999999999,t=1,p=1$c2FsdA$aGFzaA"),
        seed("$argon2id$v=19$m=8,t=1,p=1$$"),
        seed(""),
        seed("$$$$$$"),
    } });
}
