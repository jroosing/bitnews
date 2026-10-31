//! The per-entry rules, applied to a whole listing before a byte is written
//! (internal/postproc/unpack guard.go, the guard part): only files and
//! directories pass (a symlink, hardlink, device, FIFO or socket fails the
//! whole Unpack with PathViolation), every name goes through `cleanName`, no
//! two names may collide once case is ignored, and the entry count and the
//! declared total size are tracked for the limits.
const std = @import("std");
const Allocator = std.mem.Allocator;
const errors = @import("unpack_errors.zig");
const Error = errors.Error;
const env_mod = @import("unpack_env.zig");
const Env = env_mod.Env;
const listing = @import("unpack_listing.zig");
const name_mod = @import("unpack_name.zig");

pub const Guard = struct {
    env: *Env,
    /// Holds the lower-cased names; an arena, nothing is freed one by one.
    scratch: Allocator,
    /// Entries seen, files among them, and the declared size of the files.
    count: usize = 0,
    files: usize = 0,
    declared: i64 = 0,
    /// Lower-cased path to "is a directory".
    seen: std.StringHashMapUnmanaged(bool) = .empty,

    pub fn init(env: *Env, scratch: Allocator) Guard {
        return .{ .env = env, .scratch = scratch };
    }

    /// Checks one entry of the listing.
    pub fn admit(g: *Guard, e: listing.Entry) Error!void {
        const env = g.env;
        g.count += 1;
        if (g.count > env.cfg.max_entries) {
            return errors.fail(env.arena, env.diag, error.TooManyEntries, "more than {d}", .{env.cfg.max_entries});
        }
        const rel = switch (try name_mod.cleanName(env.arena, e.name)) {
            .rel => |r| r,
            .violation => |why| {
                env.diag.message = try name_mod.violationText(env.arena, why, e.name);
                return error.PathViolation;
            },
        };
        switch (e.kind) {
            .file, .dir => {},
            else => {
                const t = try name_mod.truncateForError(env.arena, e.name);
                return errors.fail(env.arena, env.diag, error.PathViolation, "the archive contains a {s} {f}", .{ e.kind.text(), name_mod.quoted(t) });
            },
        }
        if (rel.len == 0) {
            if (e.kind == .dir) return;
            const t = try name_mod.truncateForError(env.arena, e.name);
            return errors.fail(env.arena, env.diag, error.PathViolation, "file named after the root {f}", .{name_mod.quoted(t)});
        }
        try g.claim(rel, e.kind == .dir);
        if (e.kind == .file) {
            g.files += 1;
            g.declared = env_mod.addSat(g.declared, e.size);
        }
    }

    /// Records `rel` and its parents, refusing a name that collides with an
    /// earlier one once case is ignored.
    fn claim(g: *Guard, rel: []const u8, is_dir: bool) Error!void {
        const env = g.env;
        // Lower-casing is per code point and '/' is ASCII, so a prefix of the
        // lower-cased path is the lower-cased prefix.
        const key = try name_mod.lowerAlloc(g.scratch, rel);
        for (key, 0..) |c, i| {
            if (c != '/') continue;
            const parent = key[0..i];
            const gop = try g.seen.getOrPut(g.scratch, parent);
            if (gop.found_existing and !gop.value_ptr.*) {
                const t = try name_mod.truncateForError(env.arena, rel);
                return errors.fail(env.arena, env.diag, error.PathViolation, "{f} is both a file and a directory", .{name_mod.quoted(t)});
            }
            gop.value_ptr.* = true;
        }
        const gop = try g.seen.getOrPut(g.scratch, key);
        if (!gop.found_existing) {
            gop.value_ptr.* = is_dir;
        } else if (!(gop.value_ptr.* and is_dir)) {
            const t = try name_mod.truncateForError(env.arena, rel);
            return errors.fail(env.arena, env.diag, error.PathViolation, "duplicate entry {f}", .{name_mod.quoted(t)});
        }
    }
};
