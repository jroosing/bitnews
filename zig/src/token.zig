//! Generated tokens (Go: internal/auth/token.go).
//!
//! Every kind of token is the same mechanism: 256 random bits, only the SHA-256
//! hash stored, constant-time compare. The kind decides the text prefix. The
//! app key (ADR-0006) is the one kind that is not stored as a hash: it is
//! sealed, so Settings can show it.

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const Sha256 = std.crypto.hash.sha2.Sha256;

/// What a generated token authenticates.
pub const Kind = enum {
    session,
    app_key,

    /// The fixed text a token of this kind starts with, so a credential pasted
    /// in the wrong place is recognisable. Session tokens have none.
    pub fn prefix(kind: Kind) []const u8 {
        return switch (kind) {
            .session => "",
            .app_key => "bitnews_",
        };
    }

    /// The name used in golden files and logs.
    pub fn name(kind: Kind) []const u8 {
        return @tagName(kind);
    }
};

const all_kinds = [_]Kind{ .session, .app_key };

/// The entropy of every token: 256 bits.
pub const token_bytes = 32;

/// The length of the encoded random part: 32 bytes in unpadded base32.
pub const body_len = 52;

/// The SHA-256 hash under which a token is stored and looked up.
pub const Hash = [Sha256.digest_length]u8;

const alphabet = "abcdefghijklmnopqrstuvwxyz234567";

/// A freshly generated token. The plaintext is shown to its owner once; only
/// the hash is stored.
pub const Issued = struct {
    plaintext: []u8,
    hash: Hash,

    /// Zeroes and frees the plaintext.
    pub fn deinit(self: *Issued, allocator: Allocator) void {
        std.crypto.secureZero(u8, self.plaintext);
        allocator.free(self.plaintext);
    }
};

/// Generates a token of `kind` from 256 bits of system randomness, encoded as
/// the kind's prefix followed by lowercase base32 without padding.
pub fn newToken(allocator: Allocator, io: Io, kind: Kind) (Allocator.Error || Io.RandomSecureError)!Issued {
    var raw: [token_bytes]u8 = undefined;
    defer std.crypto.secureZero(u8, &raw);
    try io.randomSecure(&raw);
    return newTokenFromRaw(allocator, kind, raw);
}

/// `newToken` from given bytes, for tests that need reproducible output.
pub fn newTokenFromRaw(allocator: Allocator, kind: Kind, raw: [token_bytes]u8) Allocator.Error!Issued {
    const prefix = kind.prefix();
    const plaintext = try allocator.alloc(u8, prefix.len + body_len);
    @memcpy(plaintext[0..prefix.len], prefix);
    encodeBase32(plaintext[prefix.len..][0..body_len], &raw);
    return .{ .plaintext = plaintext, .hash = hashToken(plaintext) };
}

/// Lowercase base32 without padding (RFC 4648 alphabet, 5 bits per character,
/// the last character zero-padded).
fn encodeBase32(out: *[body_len]u8, raw: *const [token_bytes]u8) void {
    var acc: u16 = 0;
    var bits: u4 = 0;
    var n: usize = 0;
    for (raw) |byte| {
        acc = (acc << 8) | byte;
        bits += 8;
        while (bits >= 5) {
            bits -= 5;
            out[n] = alphabet[(acc >> bits) & 31];
            n += 1;
        }
    }
    if (bits > 0) {
        out[n] = alphabet[(acc << (5 - bits)) & 31];
        n += 1;
    }
    std.debug.assert(n == body_len);
}

/// Returns the SHA-256 hash under which a token is stored and looked up. A
/// fast hash is right here because tokens are high-entropy; passwords go
/// through argon2id instead.
pub fn hashToken(plaintext: []const u8) Hash {
    var out: Hash = undefined;
    Sha256.hash(plaintext, &out, .{});
    return out;
}

/// Compares two hashes in constant time.
pub fn hashEqual(a: Hash, b: Hash) bool {
    return std.crypto.timing_safe.eql(Hash, a, b);
}

/// Reports which kind of token `s` looks like, by prefix and shape. It only
/// checks the format: a result says nothing about whether the token is valid.
/// It lets a credential check reject garbage before hashing it.
pub fn kindOf(s: []const u8) ?Kind {
    // No prefix is a prefix of another that the body could confuse: each
    // non-empty one ends in "_", which the encoded body never contains.
    for (all_kinds) |kind| {
        if (!std.mem.startsWith(u8, s, kind.prefix())) continue;
        const body = s[kind.prefix().len..];
        if (body.len != body_len) continue;
        if (isLowerBase32(body)) return kind;
    }
    return null;
}

fn isLowerBase32(s: []const u8) bool {
    for (s) |c| {
        if ((c < 'a' or c > 'z') and (c < '2' or c > '7')) return false;
    }
    return true;
}

// ---------------------------------------------------------------- tests

const testing = std.testing;

/// `n` copies of `c`, known at compile time (Zig 0.17 has no `**`).
fn run(comptime c: u8, comptime n: usize) *const [n]u8 {
    const a: [n]u8 = @splat(c);
    return &a;
}

test "a token is 256 bits of lowercase base32 without padding" {
    var t = try newToken(testing.allocator, testing.io, .session);
    defer t.deinit(testing.allocator);
    try testing.expectEqual(52, t.plaintext.len);
    try testing.expect(isLowerBase32(t.plaintext));
    try testing.expect(hashEqual(hashToken(t.plaintext), t.hash));
}

test "tokens are unique" {
    var a = try newToken(testing.allocator, testing.io, .session);
    defer a.deinit(testing.allocator);
    var b = try newToken(testing.allocator, testing.io, .session);
    defer b.deinit(testing.allocator);
    try testing.expect(!std.mem.eql(u8, a.plaintext, b.plaintext));
}

test "the encoding is base32 of the random bytes" {
    var zero = try newTokenFromRaw(testing.allocator, .session, @splat(0));
    defer zero.deinit(testing.allocator);
    try testing.expectEqualStrings(run('a', 52), zero.plaintext);

    // python3: base64.b32encode(bytes([0xff] * 32)).lower() -> 51 x '7' and 'q'
    var ones = try newTokenFromRaw(testing.allocator, .session, @splat(0xff));
    defer ones.deinit(testing.allocator);
    try testing.expectEqualStrings(run('7', 51) ++ "q", ones.plaintext);

    // base64.b32encode(bytes(range(32))).lower().rstrip('=')
    var seq: [32]u8 = undefined;
    for (&seq, 0..) |*b, i| b.* = @intCast(i);
    var counted = try newTokenFromRaw(testing.allocator, .session, seq);
    defer counted.deinit(testing.allocator);
    try testing.expectEqualStrings("aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq", counted.plaintext);
}

test "the hash is SHA-256" {
    // echo -n abc | sha256sum
    const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
    const got = hashToken("abc");
    try testing.expectEqualStrings(want, &std.fmt.bytesToHex(got, .lower));
}

test "hashes compare equal only when equal" {
    const a = hashToken("a");
    const b = hashToken("b");
    try testing.expect(hashEqual(a, a));
    try testing.expect(!hashEqual(a, b));
}

test "the kinds have distinct prefixes" {
    try testing.expectEqualStrings("bitnews_", Kind.app_key.prefix());
    try testing.expectEqualStrings("", Kind.session.prefix());
}

test "a kinded token carries its prefix and is recognised" {
    var t = try newToken(testing.allocator, testing.io, .app_key);
    defer t.deinit(testing.allocator);
    try testing.expectEqual(8 + 52, t.plaintext.len);
    try testing.expect(std.mem.startsWith(u8, t.plaintext, "bitnews_"));
    try testing.expect(isLowerBase32(t.plaintext[8..]));
    try testing.expectEqual(Kind.app_key, kindOf(t.plaintext).?);
    try testing.expect(hashEqual(hashToken(t.plaintext), t.hash));
}

test "kindOf recognises every kind and rejects garbage" {
    for (all_kinds) |kind| {
        var t = try newToken(testing.allocator, testing.io, kind);
        defer t.deinit(testing.allocator);
        try testing.expectEqual(kind, kindOf(t.plaintext).?);
    }
    try testing.expectEqual(null, kindOf("bitnews_short"));
    try testing.expectEqual(null, kindOf(""));
    try testing.expectEqual(null, kindOf(run('A', 52))); // uppercase
    try testing.expectEqual(null, kindOf(run('1', 52))); // digits 0, 1, 8, 9 are not base32
    try testing.expectEqual(null, kindOf("bitnews_" ++ run('a', 51)));
    try testing.expectEqual(null, kindOf(run('a', 51) ++ "\n"));
}

fn fuzzKindOf(_: void, smith: *testing.Smith) anyerror!void {
    var buf: [128]u8 = undefined;
    const n = smith.slice(&buf);
    const s = buf[0..n];
    const kind = kindOf(s) orelse return;
    // A recognised token has exactly the shape the generator produces.
    try testing.expectEqual(kind.prefix().len + body_len, s.len);
    try testing.expect(isLowerBase32(s[kind.prefix().len..]));
}

/// A Smith stream holding one slice: a u32 little-endian length and the bytes.
fn seed(comptime bytes: []const u8) []const u8 {
    const len: [4]u8 = .{ @truncate(bytes.len), @truncate(bytes.len >> 8), @truncate(bytes.len >> 16), @truncate(bytes.len >> 24) };
    return &len ++ bytes;
}

test "fuzz: classifying arbitrary credentials never crashes" {
    try testing.fuzz({}, fuzzKindOf, .{ .corpus = &.{
        seed("aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq"),
        seed("bitnews_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq"),
        seed("bitnews_"),
        seed(""),
    } });
}
