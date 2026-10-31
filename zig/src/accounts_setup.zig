//! First-run setup token (Z6). Go: internal/auth/setup.go NewSetup.
//!
//! Token is 32 random bytes encoded as uppercase unpadded base32 (52 chars
//! A-Z2-7). Logged once at WARN. Memory only; deinit zeroes it.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const log = @import("log.zig");
const id = @import("id.zig");
const password = @import("password.zig");
const servers_types = @import("servers_types.zig");
const setup_gate = @import("setup_gate.zig");
const sqlite = @import("sqlite.zig");

const token_bytes = 32;
const token_len = 52;
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

/// Go: MaxUsernameBytes.
pub const max_username_bytes = 64;

const setup_msg = "first-run setup required: no admin exists yet. Use this one-time setup token to create the admin; it is kept in memory only and changes on every restart";
const complete_msg = "first-run setup complete: admin account created";

pub const CompleteError = password.PasswordError || error{
    SetupClosed,
    BadSetupToken,
    InvalidUsername,
    /// `SELECT count(*)` failed.
    CountUsers,
    /// argon2 or the salt failed.
    HashPassword,
    /// The insert failed and it was not a lost race.
    CreateAdmin,
    OutOfMemory,
    Canceled,
};

/// Process-lifetime setup state. `token` is empty once setup is closed.
/// `log` is borrowed: the caller owns the `Core` and it must outlive `Setup`.
pub const Setup = struct {
    gpa: Allocator,
    token: []u8,
    log: log.Logger,
    /// Serializes `complete`, including the argon2 hash, so two callers cannot
    /// both observe an empty users table. Go: Setup.mu.
    mutex: std.Io.Mutex = .init,

    pub fn required(self: *const Setup) bool {
        return self.token.len != 0;
    }

    pub fn deinit(self: *Setup) void {
        self.forgetToken();
        self.* = undefined;
    }

    /// Go Complete. Holds `mutex` for the whole call. Order: open, token,
    /// username, password, user count, hash (no DB lock), insert, forget
    /// token, close gate, info log.
    /// A user that appeared after startup, or a unique conflict, closes
    /// setup and returns `error.SetupClosed`.
    pub fn complete(
        self: *Setup,
        gpa: Allocator,
        io: Io,
        store: *sqlite.Store,
        gate: *setup_gate.Gate,
        token: []const u8,
        username: []const u8,
        password_plain: []const u8,
    ) CompleteError!void {
        self.mutex.lockUncancelable(io);
        defer self.mutex.unlock(io);

        if (self.token.len == 0) return error.SetupClosed;
        if (!tokenEqual(self.token, token)) return error.BadSetupToken;
        try validateUsername(username);
        try password.validatePassword(password_plain);

        if (try countUsers(store) > 0) {
            self.shut(gate);
            return error.SetupClosed;
        }

        const hash = password.hashPassword(gpa, io, password_plain, password.default_params) catch |err| return mapHash(err);
        defer gpa.free(hash);

        if (!try insertUser(io, store, username, hash)) {
            self.shut(gate);
            return error.SetupClosed;
        }
        self.shut(gate);
        self.log.info(complete_msg, &.{log.Attr.string("username", username)});
    }

    fn forgetToken(self: *Setup) void {
        if (self.token.len == 0) return;
        std.crypto.secureZero(u8, self.token);
        self.gpa.free(self.token);
        self.token = &.{};
    }

    fn shut(self: *Setup, gate: *setup_gate.Gate) void {
        self.forgetToken();
        gate.close();
    }
};

/// Go ValidateUsername. Empty, longer than 64 bytes, invalid UTF-8, leading
/// or trailing Unicode space, or a control character (unicode.IsControl).
pub fn validateUsername(name: []const u8) error{InvalidUsername}!void {
    if (name.len == 0 or name.len > max_username_bytes) return error.InvalidUsername;
    if (!std.unicode.utf8ValidateSlice(name)) return error.InvalidUsername;
    if (!std.mem.eql(u8, servers_types.trimSpace(name), name)) return error.InvalidUsername;
    if (servers_types.hasControl(name)) return error.InvalidUsername;
}

fn tokenEqual(stored: []const u8, presented: []const u8) bool {
    if (stored.len != presented.len) {
        var dummy: [52]u8 = undefined;
        const n = @min(presented.len, dummy.len);
        @memcpy(dummy[0..n], presented[0..n]);
        _ = std.crypto.timing_safe.eql([52]u8, dummy, dummy);
        return false;
    }
    var a: [52]u8 = undefined;
    var b: [52]u8 = undefined;
    @memcpy(a[0..stored.len], stored);
    @memcpy(b[0..presented.len], presented);
    if (stored.len != 52) return std.mem.eql(u8, stored, presented);
    return std.crypto.timing_safe.eql([52]u8, a, b);
}

/// If the gate is open, mint a token and log it once. Else return a closed Setup.
pub fn openSetup(gpa: Allocator, io: Io, gate: *setup_gate.Gate, logger: log.Logger) !Setup {
    if (!gate.required()) return .{ .gpa = gpa, .token = &.{}, .log = logger };

    var raw: [token_bytes]u8 = undefined;
    defer std.crypto.secureZero(u8, &raw);
    try io.randomSecure(&raw);

    const tok = try gpa.alloc(u8, token_len);
    errdefer gpa.free(tok);
    encodeUpperBase32(tok[0..token_len], &raw);

    logger.warn(setup_msg, &.{log.Attr.string("setup_token", tok)});
    return .{ .gpa = gpa, .token = tok, .log = logger };
}

fn countUsers(store: *sqlite.Store) CompleteError!i64 {
    const r = try store.acquireReader();
    defer r.release();
    var st = sqlite.Stmt.prepare(r.db, "SELECT count(*) FROM users") catch |err| return mapCount(err);
    defer st.finalize();
    const has_row = st.step() catch |err| return mapCount(err);
    if (!has_row) return error.CountUsers;
    return st.columnInt(0);
}

fn mapCount(err: sqlite.Error) CompleteError {
    return switch (err) {
        error.OutOfMemory => error.OutOfMemory,
        else => error.CountUsers,
    };
}

fn mapHash(err: password.HashError) CompleteError {
    return switch (err) {
        error.OutOfMemory => error.OutOfMemory,
        error.Canceled => error.Canceled,
        else => error.HashPassword,
    };
}

/// Same row as accounts_bootstrap insertAdmin. `false` is a unique conflict
/// (someone else created the user). The hash is a PHC string stored as a BLOB.
fn insertUser(io: Io, store: *sqlite.Store, username: []const u8, hash: []const u8) CompleteError!bool {
    const w = try store.acquireWriter();
    defer w.release();
    const user_id = id.new(io);
    const now_ms = Io.Clock.now(.real, io).toMilliseconds();
    w.db.execArgs(
        "INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)",
        .{ user_id, username, sqlite.Blob{ .bytes = hash }, now_ms },
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

fn encodeUpperBase32(out: *[token_len]u8, raw: *const [token_bytes]u8) void {
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
    std.debug.assert(n == token_len);
}
