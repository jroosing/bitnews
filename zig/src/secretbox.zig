//! Sealed secrets (Go: internal/store/secretbox.go).
//!
//! A secret that must be recoverable (a server password, the app key) is
//! stored as "v1:" followed by base64(nonce || ciphertext || tag) of
//! XChaCha20-Poly1305. The additional authenticated data binds the value to
//! the table, column and row it lives in, so ciphertexts cannot be swapped
//! between rows. The format is shared with the Go version: each opens what the
//! other sealed.
//!
//! Results are allocated by the caller's allocator. A plaintext is a secret:
//! free it with `wipeAndFree`.

const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const XChaCha = std.crypto.aead.chacha_poly.XChaCha20Poly1305;
const Key = @import("keyfile.zig").Key;

/// The format tag in front of every sealed value.
pub const format_v1 = "v1:";

pub const nonce_size = XChaCha.nonce_length;
pub const tag_size = XChaCha.tag_length;

/// Hard cap on a stored value, so a poisoned database row cannot make us
/// allocate without bound. Real values are a few hundred bytes. (Go has no cap.)
pub const max_stored_len = 64 * 1024;

pub const OpenError = error{
    /// The key or the AAD is wrong, or the value was tampered with. The causes
    /// are deliberately indistinguishable.
    Decrypt,
    /// The value is not in a known sealed format.
    UnsupportedFormat,
} || Allocator.Error;

pub const SealError = Allocator.Error || Io.RandomSecureError;

/// Builds the AAD that binds a ciphertext to its table, column and row.
pub fn aad(allocator: Allocator, table: []const u8, column: []const u8, row_id: []const u8) Allocator.Error![]u8 {
    return std.fmt.allocPrint(allocator, "{s}/{s}/{s}", .{ table, column, row_id });
}

/// Encrypts `plaintext` under `key` and a fresh random nonce. The result is
/// "v1:" followed by base64(nonce || ciphertext || tag).
pub fn seal(allocator: Allocator, io: Io, key: *const Key, plaintext: []const u8, aad_bytes: []const u8) SealError![]u8 {
    var nonce: [nonce_size]u8 = undefined;
    try io.randomSecure(&nonce);
    return sealWithNonce(allocator, key, nonce, plaintext, aad_bytes);
}

/// `seal` with the nonce given, for tests that need reproducible output. A
/// nonce must never be reused with the same key outside tests.
pub fn sealWithNonce(allocator: Allocator, key: *const Key, nonce: [nonce_size]u8, plaintext: []const u8, aad_bytes: []const u8) Allocator.Error![]u8 {
    const raw = try allocator.alloc(u8, nonce_size + plaintext.len + tag_size);
    defer allocator.free(raw);
    @memcpy(raw[0..nonce_size], &nonce);
    const ciphertext = raw[nonce_size..][0..plaintext.len];
    XChaCha.encrypt(ciphertext, raw[nonce_size + plaintext.len ..][0..tag_size], plaintext, aad_bytes, nonce, key.bytes);

    const encoder = std.base64.standard.Encoder;
    const out = try allocator.alloc(u8, format_v1.len + encoder.calcSize(raw.len));
    @memcpy(out[0..format_v1.len], format_v1);
    _ = encoder.encode(out[format_v1.len..], raw);
    return out;
}

/// Decrypts a value produced by `seal` (in Go or Zig) with the same key and
/// AAD. The caller frees the result with `wipeAndFree`.
pub fn open(allocator: Allocator, key: *const Key, stored: []const u8, aad_bytes: []const u8) OpenError![]u8 {
    if (!std.mem.startsWith(u8, stored, format_v1)) return error.UnsupportedFormat;
    if (stored.len > max_stored_len) return error.UnsupportedFormat;
    const body = stored[format_v1.len..];

    const decoder = std.base64.standard.Decoder;
    const raw_len = decoder.calcSizeForSlice(body) catch return error.UnsupportedFormat;
    const raw = try allocator.alloc(u8, raw_len);
    defer allocator.free(raw);
    decoder.decode(raw, body) catch return error.UnsupportedFormat;

    // Accept only the canonical spelling, as Go does: one secret must not have
    // many valid encodings (line breaks, spare padding bits).
    const encoder = std.base64.standard.Encoder;
    const canonical = try allocator.alloc(u8, encoder.calcSize(raw.len));
    defer allocator.free(canonical);
    if (!std.mem.eql(u8, encoder.encode(canonical, raw), body)) return error.UnsupportedFormat;

    if (raw.len < nonce_size) return error.UnsupportedFormat;
    if (raw.len < nonce_size + tag_size) return error.Decrypt;
    const nonce = raw[0..nonce_size].*;
    const ciphertext = raw[nonce_size .. raw.len - tag_size];
    const tag = raw[raw.len - tag_size ..][0..tag_size].*;

    const plain = try allocator.alloc(u8, ciphertext.len);
    errdefer allocator.free(plain);
    XChaCha.decrypt(plain, ciphertext, tag, aad_bytes, nonce, key.bytes) catch return error.Decrypt;
    return plain;
}

/// Zeroes a secret and frees it.
pub fn wipeAndFree(allocator: Allocator, secret: []u8) void {
    std.crypto.secureZero(u8, secret);
    allocator.free(secret);
}

// ---------------------------------------------------------------- tests

const testing = std.testing;

/// What the Go tests call goldenKey: 32 bytes of 7.
fn goldenKey() Key {
    return .{ .bytes = @splat(7) };
}
const golden_aad = "servers/password/srv1";

/// Values sealed by the Go code (internal/store/secretbox_test.go), so a
/// stored value stays readable across the two implementations and upgrades.
const golden_sealed = [_]struct { stored: []const u8, plaintext: []const u8 }{
    .{ .stored = "v1:DnxZtI7UwsmrHEICstYnHoH/WTzRhgoBzAvama/frmzffYEFabpmkw==", .plaintext = "" },
    .{ .stored = "v1:4+2w0atsQsGSkU/7N/MdXWq5Ep4DIWg1zkJadqh7TbPB+eZrwvH/tEQ=", .plaintext = "x" },
    .{ .stored = "v1:i4aH7d4+0/s9XhgzCrJFd18/dsZaJGZN3ozg+R6JDFf9eu4Cn44JPCUCChmlMzeYECm2ZglpkQ==", .plaintext = "s3cret-pa55word" },
    .{
        .stored = "v1:HgXd3WfoTCnWfaglAqLHWVLF1R4IY/OEiv78/JMh3TWXbmdoz77ACKuDt0JMZHR1mYzcGNhkX9859gq74DRa5GpyBA35tCp3FlLP6w4kkrbRP0AL+sQJ7wXKEPgXzxvyxE/c0kxnD2PaIQ8cNtfl3/EJ4v4qs+ql",
        .plaintext = "longlonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglong",
    },
};

fn otherKey() Key {
    return .{ .bytes = @splat(9) };
}

test "a sealed secret opens to the original plaintext" {
    var key = goldenKey();
    defer key.deinit();
    const a = try aad(testing.allocator, "servers", "password", "srv1");
    defer testing.allocator.free(a);
    try testing.expectEqualStrings(golden_aad, a);

    const sealed = try seal(testing.allocator, testing.io, &key, "s3cret-pa55word", a);
    defer testing.allocator.free(sealed);
    try testing.expect(std.mem.find(u8, sealed, "s3cret-pa55word") == null);

    const opened = try open(testing.allocator, &key, sealed, a);
    defer wipeAndFree(testing.allocator, opened);
    try testing.expectEqualStrings("s3cret-pa55word", opened);
}

test "every seal uses a fresh nonce" {
    const key = goldenKey();
    const x = try seal(testing.allocator, testing.io, &key, "same", "a/b/c");
    defer testing.allocator.free(x);
    const y = try seal(testing.allocator, testing.io, &key, "same", "a/b/c");
    defer testing.allocator.free(y);
    try testing.expect(!std.mem.eql(u8, x, y));
}

test "the format is a version tag and base64 of nonce, ciphertext and tag" {
    const key = goldenKey();
    const sealed = try sealWithNonce(testing.allocator, &key, @splat(1), "abc", "a/b/c");
    defer testing.allocator.free(sealed);
    try testing.expect(std.mem.startsWith(u8, sealed, "v1:"));
    var raw: [24 + 3 + 16]u8 = undefined;
    try std.base64.standard.Decoder.decode(&raw, sealed[3..]);
    try testing.expectEqualSlices(u8, &@as([24]u8, @splat(1)), raw[0..24]);
}

test "an empty plaintext round-trips" {
    const key = goldenKey();
    const sealed = try seal(testing.allocator, testing.io, &key, "", "a/b/c");
    defer testing.allocator.free(sealed);
    const opened = try open(testing.allocator, &key, sealed, "a/b/c");
    defer wipeAndFree(testing.allocator, opened);
    try testing.expectEqual(0, opened.len);
}

test "open reads the values sealed by the Go code" {
    const key = goldenKey();
    for (golden_sealed) |g| {
        const opened = try open(testing.allocator, &key, g.stored, golden_aad);
        defer wipeAndFree(testing.allocator, opened);
        try testing.expectEqualStrings(g.plaintext, opened);
    }
}

test "sealing with the Go nonce reproduces the Go value" {
    // The first golden value is the empty plaintext: its nonce is the first 24 bytes.
    const key = goldenKey();
    for (golden_sealed) |g| {
        var raw: [200]u8 = undefined;
        const n = try std.base64.standard.Decoder.calcSizeForSlice(g.stored[3..]);
        try std.base64.standard.Decoder.decode(raw[0..n], g.stored[3..]);
        const again = try sealWithNonce(testing.allocator, &key, raw[0..24].*, g.plaintext, golden_aad);
        defer testing.allocator.free(again);
        try testing.expectEqualStrings(g.stored, again);
    }
}

fn expectOpenError(expected: anyerror, key: *const Key, stored: []const u8, a: []const u8) !void {
    try testing.expectError(expected, open(testing.allocator, key, stored, a));
}

test "open fails with Decrypt when key, row or bytes are wrong" {
    const key = goldenKey();
    const other = otherKey();
    const sealed = try sealWithNonce(testing.allocator, &key, @splat(3), "s3cret", golden_aad);
    defer testing.allocator.free(sealed);

    var raw: [24 + 6 + 16]u8 = undefined;
    try std.base64.standard.Decoder.decode(&raw, sealed[3..]);
    const flip = struct {
        fn at(r: []const u8, i: usize) ![]u8 {
            var c = try testing.allocator.dupe(u8, r);
            defer testing.allocator.free(c);
            c[i] ^= 1;
            return reencode(c);
        }
        fn reencode(r: []const u8) ![]u8 {
            const enc = std.base64.standard.Encoder;
            const out = try testing.allocator.alloc(u8, 3 + enc.calcSize(r.len));
            @memcpy(out[0..3], "v1:");
            _ = enc.encode(out[3..], r);
            return out;
        }
    };

    try expectOpenError(error.Decrypt, &other, sealed, golden_aad); // wrong key
    try expectOpenError(error.Decrypt, &key, sealed, "servers/password/srv2"); // wrong row
    try expectOpenError(error.Decrypt, &key, sealed, "servers/username/srv1"); // another column
    for ([_]usize{ 0, 24, raw.len - 1 }) |i| { // nonce, ciphertext, tag
        const bad = try flip.at(&raw, i);
        defer testing.allocator.free(bad);
        try expectOpenError(error.Decrypt, &key, bad, golden_aad);
    }
    const truncated = try flip.reencode(raw[0 .. raw.len - 1]);
    defer testing.allocator.free(truncated);
    try expectOpenError(error.Decrypt, &key, truncated, golden_aad);
    const nonce_only = try flip.reencode(raw[0..24]);
    defer testing.allocator.free(nonce_only);
    try expectOpenError(error.Decrypt, &key, nonce_only, golden_aad);
}

test "a ciphertext cannot be swapped between rows" {
    const key = goldenKey();
    const sealed = try seal(testing.allocator, testing.io, &key, "pw-of-row-1", "servers/password/1");
    defer testing.allocator.free(sealed);
    try expectOpenError(error.Decrypt, &key, sealed, "servers/password/2");
}

test "open fails with UnsupportedFormat for anything that is not a v1 value" {
    const key = goldenKey();
    const body = golden_sealed[2].stored[3..];
    const cases = [_][]const u8{
        "",
        body, // no version
        "v2:" ++ body,
        "v1:",
        "v1:!!!not-base64!!!",
        "v1:c2hvcnQ=", // shorter than a nonce
        "s3cret",
        "V1:" ++ body,
        "v1:++++++++////////", // 12 bytes: shorter than a nonce
    };
    for (cases) |c| try expectOpenError(error.UnsupportedFormat, &key, c, golden_aad);
}

test "open rejects non-canonical base64" {
    const key = goldenKey();
    // The Go test's plaintext "x" gives a body that ends in "=" with spare bits.
    const stored = golden_sealed[1].stored;
    try testing.expect(stored[stored.len - 1] == '=');
    const alphabet = std.base64.standard_alphabet_chars;
    const last = stored[stored.len - 2];
    const idx = std.mem.findScalar(u8, &alphabet, last).?;

    var variant = try testing.allocator.dupe(u8, stored);
    defer testing.allocator.free(variant);
    variant[variant.len - 2] = alphabet[idx ^ 1];
    try expectOpenError(error.UnsupportedFormat, &key, variant, golden_aad);

    const with_newline = try std.mem.concat(testing.allocator, u8, &.{ stored, "\n" });
    defer testing.allocator.free(with_newline);
    try expectOpenError(error.UnsupportedFormat, &key, with_newline, golden_aad);

    const crlf = try std.mem.concat(testing.allocator, u8, &.{ stored, "\r\n" });
    defer testing.allocator.free(crlf);
    try expectOpenError(error.UnsupportedFormat, &key, crlf, golden_aad);

    const embedded = try std.mem.concat(testing.allocator, u8, &.{ stored[0..10], "\n", stored[10..] });
    defer testing.allocator.free(embedded);
    try expectOpenError(error.UnsupportedFormat, &key, embedded, golden_aad);
}

test "open refuses a value over the size cap" {
    const key = goldenKey();
    const big = try testing.allocator.alloc(u8, max_stored_len + 1);
    defer testing.allocator.free(big);
    @memset(big, 'A');
    @memcpy(big[0..3], "v1:");
    try expectOpenError(error.UnsupportedFormat, &key, big, golden_aad);
}

test "error values never contain the plaintext" {
    // Errors are plain tags: there is nowhere for the secret to go.
    try testing.expectEqualStrings("Decrypt", @errorName(error.Decrypt));
}

fn fuzzOpen(_: void, smith: *testing.Smith) anyerror!void {
    const key = goldenKey();
    var buf: [4096]u8 = undefined;
    const n = smith.slice(&buf);
    const stored = buf[0..n];
    const plain = open(testing.allocator, &key, stored, golden_aad) catch |err| switch (err) {
        error.Decrypt, error.UnsupportedFormat => return,
        error.OutOfMemory => return err,
    };
    defer wipeAndFree(testing.allocator, plain);
    // Only a value that seal produced can open: sealing again with its own
    // nonce reproduces it byte for byte.
    var raw: [4096]u8 = undefined;
    const dec = std.base64.standard.Decoder;
    const len = try dec.calcSizeForSlice(stored[3..]);
    try dec.decode(raw[0..len], stored[3..]);
    const again = try sealWithNonce(testing.allocator, &key, raw[0..nonce_size].*, plain, golden_aad);
    defer testing.allocator.free(again);
    try testing.expectEqualStrings(stored, again);
}

/// A Smith stream holding one slice: a u32 little-endian length and the bytes.
fn seed(comptime bytes: []const u8) []const u8 {
    const len: [4]u8 = .{ @truncate(bytes.len), @truncate(bytes.len >> 8), @truncate(bytes.len >> 16), @truncate(bytes.len >> 24) };
    return &len ++ bytes;
}

test "fuzz: opening arbitrary stored values never crashes or leaks" {
    try testing.fuzz({}, fuzzOpen, .{ .corpus = &.{
        seed(golden_sealed[0].stored),
        seed(golden_sealed[1].stored),
        seed(golden_sealed[2].stored),
        seed(golden_sealed[3].stored),
        seed("v1:"),
        seed("v1:!!!"),
        seed(""),
        seed("v1:c2hvcnQ="),
    } });
}
