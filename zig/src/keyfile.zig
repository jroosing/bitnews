//! The master key that encrypts recoverable secrets (Go: internal/store/keyfile.go).
//!
//! The key file holds 32 random bytes, base64-encoded, followed by a newline,
//! and is shared with the Go version: a file written by either opens in the
//! other. A `Key` is the one piece of key material in the process: it zeroes
//! itself on `deinit` and prints as "[redacted]" through `{f}` and JSON, so it
//! cannot reach a log by accident. The owner is whoever called `generate`,
//! `parse` or `loadOrCreate`; callers pass it on by pointer and never copy it.

const std = @import("std");
const Io = std.Io;
const Dir = Io.Dir;
const base64 = std.base64;

/// The size of the master key in bytes (XChaCha20-Poly1305).
pub const key_size = 32;

/// Bounds how much of a key file is read; a real one is 45 bytes.
pub const max_file_size = 1024;

/// Bounds how long (about a second) a starter waits for a concurrent creator
/// to finish writing.
pub const max_empty_reads = 50;

/// What a Key shows in place of its bytes.
pub const redacted = "[redacted]";

pub const Key = struct {
    bytes: [key_size]u8,

    /// Zeroes the key material. Call it when the owner is done with the key.
    pub fn deinit(self: *Key) void {
        std.crypto.secureZero(u8, &self.bytes);
    }

    /// A fresh random key. Fails only when the system has no entropy.
    pub fn generate(io: Io) Io.RandomSecureError!Key {
        var key: Key = .{ .bytes = undefined };
        try io.randomSecure(&key.bytes);
        return key;
    }

    /// Every formatting path shows `redacted`, never the bytes.
    pub fn format(_: Key, w: *Io.Writer) Io.Writer.Error!void {
        try w.writeAll(redacted);
    }

    /// A Key serializes as the string "[redacted]".
    pub fn jsonStringify(_: Key, jw: anytype) !void {
        try jw.write(redacted);
    }

    pub fn eql(a: *const Key, b: *const Key) bool {
        return std.crypto.timing_safe.eql([key_size]u8, a.bytes, b.bytes);
    }
};

pub const ParseError = error{
    /// The text is not valid base64.
    KeyNotBase64,
    /// The text decodes to something other than 32 bytes.
    KeyWrongLength,
};

/// Decodes a key in its key-file form: 32 bytes, base64-encoded. The caller
/// has trimmed surrounding whitespace.
///
/// Like Go's StdEncoding decoder, it ignores CR and LF inside the text and
/// does not insist that the unused bits of the last character are zero, so
/// every key file the Go version accepts is accepted here.
pub fn parse(encoded: []const u8) ParseError!Key {
    // 32 bytes are 44 characters; anything much longer is not a key. The
    // bound keeps the scratch copy on the stack.
    var clean: [max_file_size]u8 = undefined;
    defer std.crypto.secureZero(u8, &clean);
    if (encoded.len > clean.len) return error.KeyWrongLength;
    var n: usize = 0;
    for (encoded) |c| {
        if (c == '\r' or c == '\n') continue;
        clean[n] = c;
        n += 1;
    }
    const text = clean[0..n];

    // "...X=" with spare bits set in X: clear them, as Go does.
    if (text.len == 44 and text[43] == '=' and text[42] != '=') {
        const alphabet = base64.standard_alphabet_chars;
        if (std.mem.findScalar(u8, &alphabet, text[42])) |idx| text[42] = alphabet[idx & ~@as(usize, 3)];
    }

    const decoder = base64.standard.Decoder;
    const len = decoder.calcSizeForSlice(text) catch return error.KeyNotBase64;
    if (len > key_size * 2) return error.KeyWrongLength; // never decode into more than the scratch holds
    var raw: [key_size * 2]u8 = undefined;
    defer std.crypto.secureZero(u8, &raw);
    decoder.decode(raw[0..len], text) catch return error.KeyNotBase64;
    if (len != key_size) return error.KeyWrongLength;
    var key: Key = .{ .bytes = undefined };
    @memcpy(&key.bytes, raw[0..key_size]);
    return key;
}

/// The key-file form: 44 base64 characters and a newline. The result holds key
/// material; the caller zeroes it.
pub fn encodeLine(key: *const Key) [45]u8 {
    var line: [45]u8 = undefined;
    _ = base64.standard.Encoder.encode(line[0..44], &key.bytes);
    line[44] = '\n';
    return line;
}

pub const LoadError = ParseError || error{
    /// The path exists but is not a regular file (Docker creates a directory
    /// when a bind-mounted secret file is missing).
    KeyFileNotRegular,
    KeyFileTooLarge,
    /// The file exists but holds nothing yet, after waiting for a creator.
    KeyFileEmpty,
    /// The file exists but cannot be opened or read.
    KeyFileUnreadable,
    /// The file is missing and cannot be created.
    KeyFileCannotCreate,
} || Io.RandomSecureError;

pub const Loaded = struct {
    key: Key,
    /// True when the file did not exist and was generated. The caller logs a
    /// WARN with the path, because losing the file makes the encrypted
    /// secrets unrecoverable.
    generated: bool = false,
    /// The mode of an existing file whose permissions are broader than 0600
    /// (only a warning: Docker secrets are often 0444). The caller logs a
    /// WARN with the path and the mode.
    broad_mode: ?u32 = null,
};

/// Returns the master key stored at `path`. A missing file is generated with
/// mode 0600 (and its directory with 0700 if that is missing too). The caller
/// owns `Loaded.key` and calls `deinit` on it.
pub fn loadOrCreate(io: Io, path: []const u8) LoadError!Loaded {
    var empty_reads: u32 = 0;
    while (true) {
        if (readKeyFile(io, path)) |loaded| {
            return loaded;
        } else |err| switch (err) {
            error.KeyFileEmpty => {
                if (empty_reads >= max_empty_reads) return error.KeyFileEmpty;
                // Another process is creating the file right now.
                empty_reads += 1;
                Io.Clock.Duration.sleep(.{ .clock = .awake, .raw = .fromMilliseconds(20) }, io) catch return error.KeyFileUnreadable;
            },
            error.FileNotFound => switch (try createKeyFile(io, path)) {
                .created => |key| return .{ .key = key, .generated = true },
                // Lost a race with another process: read its file.
                .lost_race => {},
            },
            else => |e| return e,
        }
    }
}

const ReadError = LoadError || error{FileNotFound};

fn readKeyFile(io: Io, path: []const u8) ReadError!Loaded {
    var file = Dir.cwd().openFile(io, path, .{}) catch |err| switch (err) {
        error.FileNotFound => return error.FileNotFound,
        error.IsDir => return error.KeyFileNotRegular,
        else => return error.KeyFileUnreadable,
    };
    defer file.close(io);
    const st = file.stat(io) catch return error.KeyFileUnreadable;
    if (st.kind != .file) return error.KeyFileNotRegular;

    var content: [max_file_size + 1]u8 = undefined;
    defer std.crypto.secureZero(u8, &content);
    const n = file.readPositionalAll(io, &content, 0) catch return error.KeyFileUnreadable;
    if (n > max_file_size) return error.KeyFileTooLarge;
    const trimmed = std.mem.trim(u8, content[0..n], " \t\r\n\x0b\x0c");
    if (trimmed.len == 0) return error.KeyFileEmpty;
    const key = try parse(trimmed);

    const broad = broadMode(st.permissions);
    return .{ .key = key, .broad_mode = broad };
}

fn broadMode(permissions: Io.File.Permissions) ?u32 {
    if (!@hasDecl(Io.File.Permissions, "toMode")) return null;
    const perm: u32 = permissions.toMode() & 0o777;
    return if (perm & 0o077 != 0) perm else null;
}

const Created = union(enum) {
    created: Key,
    lost_race,
};

fn createKeyFile(io: Io, path: []const u8) LoadError!Created {
    const cwd = Dir.cwd();
    if (std.fs.path.dirname(path)) |dir| {
        _ = cwd.createDirPathStatus(io, dir, .fromMode(0o700)) catch return error.KeyFileCannotCreate;
    }

    var key = try Key.generate(io);
    errdefer key.deinit();
    var line = encodeLine(&key);
    defer std.crypto.secureZero(u8, &line);

    // Write the complete key to a private temp file first and publish it with
    // a hard link. The link fails when another process won the race, and the
    // published file is never visible half-written, so a loser can never read
    // an empty or truncated key.
    var atomic = cwd.createFileAtomic(io, path, .{ .permissions = .fromMode(0o600) }) catch return error.KeyFileCannotCreate;
    defer atomic.deinit(io);
    atomic.file.setPermissions(io, .fromMode(0o600)) catch return error.KeyFileCannotCreate;
    atomic.file.writeStreamingAll(io, &line) catch return error.KeyFileCannotCreate;
    atomic.file.sync(io) catch return error.KeyFileCannotCreate;
    atomic.link(io) catch |err| switch (err) {
        error.PathAlreadyExists => {
            key.deinit();
            return .lost_race;
        },
        // Some filesystems (certain network and FUSE mounts) have no hard
        // links. Fall back to an exclusive create; a concurrent starter may
        // then briefly see an empty file, which loadOrCreate waits out.
        else => {
            if (try writeExclusive(io, path, &line)) return .{ .created = key };
            key.deinit();
            return .lost_race;
        },
    };
    return .{ .created = key };
}

/// Returns false when the file already exists.
fn writeExclusive(io: Io, path: []const u8, line: []const u8) LoadError!bool {
    var file = Dir.cwd().createFile(io, path, .{ .exclusive = true, .permissions = .fromMode(0o600) }) catch |err| switch (err) {
        error.PathAlreadyExists => return false,
        else => return error.KeyFileCannotCreate,
    };
    defer file.close(io);
    const ok = blk: {
        file.writeStreamingAll(io, line) catch break :blk false;
        file.sync(io) catch break :blk false;
        break :blk true;
    };
    if (!ok) {
        // Do not leave a truncated key behind.
        Dir.cwd().deleteFile(io, path) catch {};
        return error.KeyFileCannotCreate;
    }
    return true;
}

/// Writes the message for a load failure, with the instructions of the Go
/// version. It never contains the key or the file content.
pub fn writeErrorMessage(w: *Io.Writer, err: LoadError, path: []const u8) Io.Writer.Error!void {
    switch (err) {
        error.KeyFileNotRegular => try w.print("key file {f} is not a regular file", .{std.zig.fmtString(path)}),
        error.KeyFileTooLarge => try w.print("key file {f} is too large to be a key", .{std.zig.fmtString(path)}),
        error.KeyFileEmpty => {
            try w.print("key file {f}: key file is empty\n", .{std.zig.fmtString(path)});
            try writeHelp(w, path);
        },
        error.KeyNotBase64 => {
            try w.print("key file {f} is invalid: key is not valid base64\n", .{std.zig.fmtString(path)});
            try writeHelp(w, path);
        },
        error.KeyWrongLength => {
            try w.print("key file {f} is invalid: key must be {d} bytes\n", .{ std.zig.fmtString(path), key_size });
            try writeHelp(w, path);
        },
        error.KeyFileUnreadable => {
            try w.print("opening key file {f}\n", .{std.zig.fmtString(path)});
            try writeHelp(w, path);
        },
        error.KeyFileCannotCreate => {
            try w.print("key file {f} does not exist and cannot be created\n", .{std.zig.fmtString(path)});
            try writeHelp(w, path);
        },
        error.EntropyUnavailable => try w.writeAll("no entropy available to generate a master key"),
        error.Canceled => try w.writeAll("canceled while loading the master key"),
    }
}

fn writeHelp(w: *Io.Writer, path: []const u8) Io.Writer.Error!void {
    const dir = std.fs.path.dirname(path) orelse ".";
    try w.print(
        "Create the key file yourself, for example:\n  install -d -m 700 {f} && head -c 32 /dev/urandom | base64 > {f} && chmod 600 {f}\n" ++
            "or mount it as a secret, or point --key-file / BITNEWS_KEY_FILE at a writable location.\n" ++
            "Do not replace the key of an existing installation: stored secrets could no longer be decrypted.",
        .{ std.zig.fmtString(dir), std.zig.fmtString(path), std.zig.fmtString(path) },
    );
}

// ---------------------------------------------------------------- tests

const testing = std.testing;

const test_key_bytes: [key_size]u8 = blk: {
    var b: [key_size]u8 = undefined;
    for (&b, 0..) |*c, i| c.* = if (i % 2 == 0) 0xAB else 0xCD;
    break :blk b;
};
const test_key_b64 = "q82rzavNq82rzavNq82rzavNq82rzavNq82rzavNq80=";

fn testKey() Key {
    return .{ .bytes = test_key_bytes };
}

/// An absolute path of `name` inside the temp dir; the caller frees it.
fn tmpPath(tmp: *testing.TmpDir, name: []const u8) ![]u8 {
    var buf: [Dir.max_path_bytes]u8 = undefined;
    const n = try tmp.dir.realPath(testing.io, &buf);
    return std.fs.path.join(testing.allocator, &.{ buf[0..n], name });
}

fn writeKeyFile(tmp: *testing.TmpDir, name: []const u8, content: []const u8, mode: u32) !void {
    tmp.dir.deleteFile(testing.io, name) catch |err| switch (err) {
        error.FileNotFound => {},
        else => return err,
    };
    try tmp.dir.writeFile(testing.io, .{ .sub_path = name, .data = content });
    try tmp.dir.setFilePermissions(testing.io, name, .fromMode(@intCast(mode)), .{});
}

fn modeOf(dir: Dir, sub: []const u8) !u32 {
    const st = try dir.statFile(testing.io, sub, .{});
    return st.permissions.toMode() & 0o777;
}

test "a missing key file is generated with mode 0600 in a 0700 directory" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const path = try tmpPath(&tmp, "keys/bitnews.key");
    defer testing.allocator.free(path);

    var loaded = try loadOrCreate(testing.io, path);
    defer loaded.key.deinit();
    try testing.expect(loaded.generated);
    try testing.expectEqual(null, loaded.broad_mode);

    var buf: [64]u8 = undefined;
    const content = try tmp.dir.readFile(testing.io, "keys/bitnews.key", &buf);
    try testing.expectEqual(45, content.len);
    try testing.expectEqual('\n', content[44]);
    var again = try parse(std.mem.trim(u8, content, "\n"));
    defer again.deinit();
    try testing.expect(again.eql(&loaded.key));

    try testing.expectEqual(0o600, try modeOf(tmp.dir, "keys/bitnews.key"));
    try testing.expectEqual(0o700, try modeOf(tmp.dir, "keys"));
}

test "the next start returns the same key and does not overwrite the file" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);

    var first = try loadOrCreate(testing.io, path);
    defer first.key.deinit();
    var second = try loadOrCreate(testing.io, path);
    defer second.key.deinit();
    try testing.expect(first.generated);
    try testing.expect(!second.generated);
    try testing.expect(first.key.eql(&second.key));

    // An existing file is read, never replaced.
    try writeKeyFile(&tmp, "other.key", test_key_b64 ++ "\n", 0o600);
    const other = try tmpPath(&tmp, "other.key");
    defer testing.allocator.free(other);
    var loaded = try loadOrCreate(testing.io, other);
    defer loaded.key.deinit();
    try testing.expect(!loaded.generated);
    try testing.expectEqualSlices(u8, &test_key_bytes, &loaded.key.bytes);
}

test "surrounding whitespace is tolerated" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);
    const contents = [_][]const u8{
        test_key_b64,
        test_key_b64 ++ "\n",
        test_key_b64 ++ "\r\n",
        test_key_b64 ++ "\n\n  \n",
        "  " ++ test_key_b64 ++ "\n",
    };
    for (contents) |content| {
        try writeKeyFile(&tmp, "bitnews.key", content, 0o600);
        var loaded = try loadOrCreate(testing.io, path);
        defer loaded.key.deinit();
        try testing.expectEqualSlices(u8, &test_key_bytes, &loaded.key.bytes);
    }
}

test "an invalid key file is rejected and left alone" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);

    const too_long = "q82rzavNq82rzavNq82rzavNq82rzavNq82rzavNq82rzavN\n";
    const huge: [16384]u8 = @splat('A');
    const Case = struct { content: []const u8, err: anyerror };
    const cases = [_]Case{
        .{ .content = "q82rzavNq82rzavNq82rzavNq82rzavNq82rzavN\n", .err = error.KeyWrongLength }, // too short
        .{ .content = too_long, .err = error.KeyWrongLength },
        .{ .content = "this is not base64 at all!!\n", .err = error.KeyNotBase64 },
        .{ .content = test_key_b64 ++ "\n" ++ test_key_b64 ++ "\n", .err = error.KeyWrongLength }, // two keys
        .{ .content = &test_key_bytes, .err = error.KeyNotBase64 }, // binary
        .{ .content = &huge, .err = error.KeyFileTooLarge },
        .{ .content = "abababababababababababababababababababababababababababababab\n", .err = error.KeyWrongLength }, // hex-ish
        .{ .content = "-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_=\n", .err = error.KeyNotBase64 }, // url-safe alphabet
    };
    for (cases) |c| {
        try writeKeyFile(&tmp, "bitnews.key", c.content, 0o600);
        try testing.expectError(c.err, loadOrCreate(testing.io, path));
        var buf: [1 << 15]u8 = undefined;
        const after = try tmp.dir.readFile(testing.io, "bitnews.key", &buf);
        try testing.expectEqualSlices(u8, c.content, after);
    }
}

test "an empty key file is reported after waiting for a concurrent creator" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try writeKeyFile(&tmp, "bitnews.key", "", 0o600);
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);
    try testing.expectError(error.KeyFileEmpty, loadOrCreate(testing.io, path));
}

test "broad permissions only produce a mode to warn about" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);
    for ([_]u32{ 0o644, 0o444, 0o640, 0o604 }) |mode| {
        try writeKeyFile(&tmp, "bitnews.key", test_key_b64 ++ "\n", mode);
        var loaded = try loadOrCreate(testing.io, path);
        defer loaded.key.deinit();
        try testing.expectEqual(mode, loaded.broad_mode.?);
        try testing.expectEqualSlices(u8, &test_key_bytes, &loaded.key.bytes);
    }
    for ([_]u32{ 0o600, 0o400 }) |mode| {
        try writeKeyFile(&tmp, "bitnews.key", test_key_b64 ++ "\n", mode);
        var loaded = try loadOrCreate(testing.io, path);
        defer loaded.key.deinit();
        try testing.expectEqual(null, loaded.broad_mode);
    }
}

test "a directory at the path is rejected" {
    // Docker creates a directory when a bind-mounted secret file is missing.
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.createDir(testing.io, "bitnews.key", .default_dir);
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);
    try testing.expectError(error.KeyFileNotRegular, loadOrCreate(testing.io, path));
}

test "a path below a regular file fails with instructions" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try writeKeyFile(&tmp, "blocker", "i am a file", 0o600);
    const path = try tmpPath(&tmp, "blocker/sub/bitnews.key");
    defer testing.allocator.free(path);
    // As in Go, the open fails with ENOTDIR, which is not "does not exist".
    const err = loadOrCreate(testing.io, path);
    try testing.expectError(error.KeyFileUnreadable, err);

    var aw: Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    try writeErrorMessage(&aw.writer, error.KeyFileUnreadable, path);
    try testing.expect(std.mem.find(u8, aw.written(), "BITNEWS_KEY_FILE") != null);
    try testing.expect(std.mem.find(u8, aw.written(), "chmod 600") != null);
    try testing.expect(std.mem.find(u8, aw.written(), path) != null);
}

test "a read-only directory fails with instructions and creates nothing" {
    if (std.posix.system.geteuid() == 0) return error.SkipZigTest;
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    try tmp.dir.createDir(testing.io, "ro", .default_dir);
    try tmp.dir.setFilePermissions(testing.io, "ro", .fromMode(0o500), .{});
    defer tmp.dir.setFilePermissions(testing.io, "ro", .fromMode(0o700), .{}) catch {};
    const path = try tmpPath(&tmp, "ro/bitnews.key");
    defer testing.allocator.free(path);
    try testing.expectError(error.KeyFileCannotCreate, loadOrCreate(testing.io, path));
    try testing.expectError(error.FileNotFound, tmp.dir.statFile(testing.io, "ro/bitnews.key", .{}));
}

test "error messages never contain the key or the file content" {
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    const errors = [_]LoadError{ error.KeyNotBase64, error.KeyWrongLength, error.KeyFileEmpty, error.KeyFileNotRegular, error.KeyFileTooLarge, error.KeyFileUnreadable };
    for (errors) |e| try writeErrorMessage(&aw.writer, e, "/data/bitnews.key");
    try testing.expect(std.mem.find(u8, aw.written(), test_key_b64) == null);
    try testing.expect(std.mem.find(u8, aw.written(), "/data/bitnews.key") != null);
}

fn loadInThread(path: []const u8, out: *Key, failed: *bool) void {
    const loaded = loadOrCreate(testing.io, path) catch {
        failed.* = true;
        return;
    };
    out.* = loaded.key;
}

test "concurrent starters on a missing file all end up with the same key" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const path = try tmpPath(&tmp, "bitnews.key");
    defer testing.allocator.free(path);

    var keys: [4]Key = undefined;
    var failed: [4]bool = @splat(false);
    var threads: [4]std.Thread = undefined;
    for (&threads, 0..) |*t, i| t.* = try std.Thread.spawn(.{}, loadInThread, .{ path, &keys[i], &failed[i] });
    for (threads) |t| t.join();
    defer for (&keys) |*k| k.deinit();
    for (failed) |f| try testing.expect(!f);
    for (keys[1..]) |*k| try testing.expect(k.eql(&keys[0]));
}

test "a key never reveals itself" {
    var key = testKey();
    defer key.deinit();
    var aw: Io.Writer.Allocating = .init(testing.allocator);
    defer aw.deinit();
    try aw.writer.print("{f}", .{key});
    try testing.expectEqualStrings(redacted, aw.written());

    aw.clearRetainingCapacity();
    var jw: std.json.Stringify = .{ .writer = &aw.writer };
    try jw.write(key);
    try testing.expectEqualStrings("\"[redacted]\"", aw.written());
}

test "deinit zeroes the key material" {
    var key = testKey();
    key.deinit();
    try testing.expectEqualSlices(u8, &@as([key_size]u8, @splat(0)), &key.bytes);
}

test "parse accepts what Go's decoder accepts and nothing else" {
    var k = try parse(test_key_b64);
    k.deinit();
    // CR and LF inside the text are ignored by Go's decoder.
    var wrapped = try parse(test_key_b64[0..20] ++ "\n" ++ test_key_b64[20..]);
    wrapped.deinit();
    // Spare bits in the last character are ignored by Go's decoder.
    var spare = try parse("q82rzavNq82rzavNq82rzavNq82rzavNq82rzavNq81=");
    try testing.expectEqualSlices(u8, &test_key_bytes, &spare.bytes);
    spare.deinit();

    try testing.expectError(error.KeyNotBase64, parse("!!!"));
    try testing.expectError(error.KeyNotBase64, parse(test_key_b64[0..43] ++ "-")); // url-safe alphabet
    try testing.expectError(error.KeyWrongLength, parse(""));
    try testing.expectError(error.KeyWrongLength, parse("AAAA"));
    try testing.expectError(error.KeyWrongLength, parse(&@as([2000]u8, @splat('A'))));
}

fn fuzzParse(_: void, smith: *testing.Smith) anyerror!void {
    var buf: [2048]u8 = undefined;
    const n = smith.slice(&buf);
    var key = parse(buf[0..n]) catch return;
    key.deinit();
}

test "fuzz: parsing key file contents never crashes" {
    try testing.fuzz({}, fuzzParse, .{ .corpus = &.{
        seed(""),
        seed(test_key_b64),
        seed(test_key_b64 ++ "\n"),
        seed("q82rzavNq82rzavNq82rzavNq82rzavNq82rzavNq81="),
        seed("not base64"),
        seed(&@as([100]u8, @splat('A'))),
    } });
}

/// A Smith stream holding one slice: a u32 little-endian length and the bytes.
fn seed(comptime bytes: []const u8) []const u8 {
    const len: [4]u8 = .{ @truncate(bytes.len), @truncate(bytes.len >> 8), @truncate(bytes.len >> 16), @truncate(bytes.len >> 24) };
    return &len ++ bytes;
}
