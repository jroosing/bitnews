//! Creates a torrent Download and hands the magnet to the engine (Z7).
//! The engine is a function pointer so tests do not link libtorrent.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const app_error = @import("app_error.zig");
const Failure = app_error.Failure;
const fsutil = @import("fsutil.zig");
const torrent_meta = @import("torrent_meta.zig");

pub const Engine = struct {
    ctx: ?*anyopaque,
    /// 0 on success. `magnet` and `save_path` are sentinel-terminated.
    add_magnet_fn: *const fn (ctx: ?*anyopaque, magnet: [*:0]const u8, save_path: [*:0]const u8) i32,
    /// 0 on success. Writes the v1 info-hash. Null until a .torrent add is wired.
    add_torrent_fn: ?*const fn (ctx: ?*anyopaque, bytes: [*]const u8, len: usize, save_path: [*:0]const u8, hash_out: *[20]u8) i32 = null,
    pause_fn: ?*const fn (ctx: ?*anyopaque, hash: *const [20]u8) void = null,
};

pub const Store = struct {
    ctx: ?*anyopaque,
    now_fn: *const fn (ctx: ?*anyopaque) i64,
    new_id_fn: *const fn (ctx: ?*anyopaque) [26]u8,
    /// Torrent incomplete root. Allocated in `arena`.
    incomplete_fn: *const fn (ctx: ?*anyopaque, arena: Allocator) anyerror![]const u8,
    create_fn: *const fn (ctx: ?*anyopaque, d: dl.Download) anyerror!void,
    publish_fn: *const fn (ctx: ?*anyopaque, d: dl.Download) void,
    /// Told the v1 info-hash after the Download row exists. Null skips it.
    remember_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8, hash: [20]u8) void = null,
    /// Writes the torrent_sources row for a magnet. Null skips it. Called after `create_fn`.
    source_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8, hash: [20]u8, magnet: []const u8) anyerror!void = null,
    /// Writes the torrent_sources row for a .torrent file (`path` is the stored file).
    file_source_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8, hash: [20]u8, path: []const u8) anyerror!void = null,
    /// Told the info-hash of a .torrent add so the caller can store the size. Null skips it.
    meta_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8, hash: [20]u8, replace_name: bool) void = null,
    /// Drops the Download row when `source_fn` fails.
    delete_fn: ?*const fn (ctx: ?*anyopaque, id: []const u8) void = null,
    io: Io,
};

/// `magnet:?xt=urn:btih:<40 hex>`. Name falls back to the `dn` parameter, then `magnet`.
pub fn addMagnet(
    arena: Allocator,
    f: *Failure,
    magnet_in: []const u8,
    name_in: []const u8,
    category_id: []const u8,
    priority: dl.Priority,
    paused: bool,
    engine: Engine,
    store: Store,
) !dl.Download {
    const magnet = std.mem.trim(u8, magnet_in, &std.ascii.whitespace);
    if (!isMagnet(magnet) or parseV1Hash(magnet) == null) {
        return f.setLiteral(.invalid_input, "invalid_magnet", "not a usable magnet link");
    }
    const id = store.new_id_fn(store.ctx);
    const id_text = try arena.dupe(u8, &id);
    const name = if (name_in.len > 0) name_in else displayName(magnet);
    const incomplete = try store.incomplete_fn(store.ctx, arena);
    const save = try fsutil.join(arena, &.{ incomplete, id_text });
    Io.Dir.cwd().createDirPath(store.io, save) catch {
        return f.setLiteral(.invalid_input, "torrent_dir", "the torrent incomplete directory could not be created");
    };
    const magnet_z = try arena.dupeSentinel(u8, magnet, 0);
    const save_z = try arena.dupeSentinel(u8, save, 0);
    if (engine.add_magnet_fn(engine.ctx, magnet_z, save_z) != 0) {
        return f.setLiteral(.unavailable, "torrent_add_failed", "the torrent engine refused the magnet");
    }
    const hash = parseV1Hash(magnet).?;
    const now = store.now_fn(store.ctx);
    const d: dl.Download = .{
        .id = id_text,
        .name = name,
        .source_kind = .torrent,
        .category_id = category_id,
        .state = .fetching_metadata,
        .priority = priority,
        .paused = paused,
        .added_at = now,
        .updated_at = now,
    };
    try store.create_fn(store.ctx, d);
    errdefer if (store.delete_fn) |del| del(store.ctx, id_text);
    if (store.source_fn) |src| try src(store.ctx, id_text, hash, magnet);
    if (store.remember_fn) |remember| remember(store.ctx, id_text, hash);
    store.publish_fn(store.ctx, d);
    return d;
}

/// A .torrent file already has its metadata, so the Download starts Queued.
/// The bytes are stored under the save directory (mode 0600) and handed to the engine.
pub fn addFile(
    arena: Allocator,
    f: *Failure,
    bytes: []const u8,
    name_in: []const u8,
    category_id: []const u8,
    priority: dl.Priority,
    paused: bool,
    engine: Engine,
    store: Store,
) !dl.Download {
    if (bytes.len == 0) return f.setLiteral(.invalid_input, "invalid_torrent", "not a usable torrent file");
    const add_torrent = engine.add_torrent_fn orelse return f.setLiteral(.unavailable, "torrent_add_failed", "the torrent engine refused the torrent file");
    const id = store.new_id_fn(store.ctx);
    const id_text = try arena.dupe(u8, &id);
    const name = if (name_in.len > 0) name_in else "torrent";
    const incomplete = try store.incomplete_fn(store.ctx, arena);
    const save = try fsutil.join(arena, &.{ incomplete, id_text });
    Io.Dir.cwd().createDirPath(store.io, save) catch {
        return f.setLiteral(.invalid_input, "torrent_dir", "the torrent incomplete directory could not be created");
    };
    const path = torrent_meta.write(arena, store.io, save, name, bytes) catch {
        return f.setLiteral(.invalid_input, "torrent_dir", "the torrent file could not be stored");
    };
    const save_z = try arena.dupeSentinel(u8, save, 0);
    var hash: [20]u8 = undefined;
    if (add_torrent(engine.ctx, bytes.ptr, bytes.len, save_z, &hash) != 0) {
        return f.setLiteral(.unavailable, "torrent_add_failed", "the torrent engine refused the torrent file");
    }
    if (paused) if (engine.pause_fn) |pause| pause(engine.ctx, &hash);
    const now = store.now_fn(store.ctx);
    const d: dl.Download = .{
        .id = id_text,
        .name = name,
        .source_kind = .torrent,
        .category_id = category_id,
        .state = .queued,
        .priority = priority,
        .paused = paused,
        .added_at = now,
        .updated_at = now,
    };
    try store.create_fn(store.ctx, d);
    errdefer if (store.delete_fn) |del| del(store.ctx, id_text);
    if (store.file_source_fn) |src| try src(store.ctx, id_text, hash, path);
    if (store.remember_fn) |remember| remember(store.ctx, id_text, hash);
    if (store.meta_fn) |meta| meta(store.ctx, id_text, hash, name_in.len == 0);
    store.publish_fn(store.ctx, d);
    return d;
}

fn isMagnet(s: []const u8) bool {
    return s.len >= 7 and std.ascii.eqlIgnoreCase(s[0..7], "magnet:");
}

pub fn v1Hash(magnet: []const u8) ?[20]u8 {
    return parseV1Hash(std.mem.trim(u8, magnet, &std.ascii.whitespace));
}

fn parseV1Hash(magnet: []const u8) ?[20]u8 {
    const key = "urn:btih:";
    const at = findIgnore(magnet, key) orelse return null;
    const rest = magnet[at + key.len ..];
    if (rest.len < 40) return null;
    var out: [20]u8 = undefined;
    _ = std.fmt.hexToBytes(&out, rest[0..40]) catch return null;
    return out;
}

fn findIgnore(hay: []const u8, needle: []const u8) ?usize {
    if (needle.len == 0 or hay.len < needle.len) return null;
    var i: usize = 0;
    while (i + needle.len <= hay.len) : (i += 1) {
        if (std.ascii.eqlIgnoreCase(hay[i..][0..needle.len], needle)) return i;
    }
    return null;
}

fn displayName(magnet: []const u8) []const u8 {
    const key = "dn=";
    const at = std.mem.indexOf(u8, magnet, key) orelse return "magnet";
    const rest = magnet[at + key.len ..];
    const end = std.mem.indexOfScalar(u8, rest, '&') orelse rest.len;
    if (end == 0) return "magnet";
    return rest[0..end];
}

test "addMagnet stores a fetching_metadata torrent and passes the save dir" {
    const testing = std.testing;
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const root = try std.fs.path.join(testing.allocator, &.{ ".zig-cache", "tmp", &tmp.sub_path });
    defer testing.allocator.free(root);
    const Seen = struct {
        var magnet: [*:0]const u8 = undefined;
        var path: [*:0]const u8 = undefined;
        var created: ?dl.Download = null;
        var incomplete_root: []const u8 = "";
        const Self = @This();
        fn add(_: ?*anyopaque, uri: [*:0]const u8, save_path: [*:0]const u8) i32 {
            Self.magnet = uri;
            Self.path = save_path;
            return 0;
        }
        fn now(_: ?*anyopaque) i64 {
            return 42;
        }
        fn newId(_: ?*anyopaque) [26]u8 {
            return "aaaaaaaaaaaaaaaaaaaaaaaaaa".*;
        }
        fn incomplete(_: ?*anyopaque, arena: Allocator) ![]const u8 {
            return arena.dupe(u8, Self.incomplete_root);
        }
        fn create(_: ?*anyopaque, d: dl.Download) !void {
            Self.created = d;
        }
        fn publish(_: ?*anyopaque, _: dl.Download) void {}
    };
    Seen.incomplete_root = root;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    var f: Failure = .{};
    const d = try addMagnet(
        arena_state.allocator(),
        &f,
        "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Show.S01",
        "",
        "",
        .normal,
        false,
        .{ .ctx = null, .add_magnet_fn = Seen.add },
        .{
            .ctx = null,
            .now_fn = Seen.now,
            .new_id_fn = Seen.newId,
            .incomplete_fn = Seen.incomplete,
            .create_fn = Seen.create,
            .publish_fn = Seen.publish,
            .io = testing.io,
        },
    );
    try testing.expectEqual(dl.State.fetching_metadata, d.state);
    try testing.expectEqual(dl.SourceKind.torrent, d.source_kind);
    try testing.expectEqualStrings("Show.S01", d.name);
    try testing.expect(std.mem.indexOf(u8, std.mem.span(Seen.path), "aaaaaaaaaaaaaaaaaaaaaaaaaa") != null);
    try testing.expect(Seen.created != null);
}

test "addFile queues the download and stores the torrent bytes" {
    const testing = std.testing;
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const root = try std.fs.path.join(testing.allocator, &.{ ".zig-cache", "tmp", &tmp.sub_path });
    defer testing.allocator.free(root);
    const FileSeen = struct {
        var path: []const u8 = "";
        var paused = false;
        var replace_name = false;
        var incomplete_root: []const u8 = "";
        const Self = @This();
        fn magnet(_: ?*anyopaque, _: [*:0]const u8, _: [*:0]const u8) i32 {
            return 1;
        }
        fn add(_: ?*anyopaque, bytes: [*]const u8, len: usize, _: [*:0]const u8, hash_out: *[20]u8) i32 {
            if (len == 0 or bytes[0] != 'd') return 1;
            hash_out.* = @splat(0x44);
            return 0;
        }
        fn pause(_: ?*anyopaque, _: *const [20]u8) void {
            Self.paused = true;
        }
        fn now(_: ?*anyopaque) i64 {
            return 7;
        }
        fn newId(_: ?*anyopaque) [26]u8 {
            return "bbbbbbbbbbbbbbbbbbbbbbbbbb".*;
        }
        fn incomplete(_: ?*anyopaque, arena: Allocator) ![]const u8 {
            return arena.dupe(u8, Self.incomplete_root);
        }
        fn create(_: ?*anyopaque, _: dl.Download) !void {}
        fn publish(_: ?*anyopaque, _: dl.Download) void {}
        fn fileSource(_: ?*anyopaque, _: []const u8, _: [20]u8, stored: []const u8) !void {
            Self.path = stored;
        }
        fn meta(_: ?*anyopaque, _: []const u8, _: [20]u8, replace: bool) void {
            Self.replace_name = replace;
        }
    };
    FileSeen.incomplete_root = root;
    var arena_state: std.heap.ArenaAllocator = .init(testing.allocator);
    defer arena_state.deinit();
    var f: Failure = .{};
    const d = try addFile(
        arena_state.allocator(),
        &f,
        "d4:infoe",
        "Movie.2026",
        "",
        .normal,
        true,
        .{
            .ctx = null,
            .add_magnet_fn = FileSeen.magnet,
            .add_torrent_fn = FileSeen.add,
            .pause_fn = FileSeen.pause,
        },
        .{
            .ctx = null,
            .now_fn = FileSeen.now,
            .new_id_fn = FileSeen.newId,
            .incomplete_fn = FileSeen.incomplete,
            .create_fn = FileSeen.create,
            .publish_fn = FileSeen.publish,
            .file_source_fn = FileSeen.fileSource,
            .meta_fn = FileSeen.meta,
            .io = testing.io,
        },
    );
    try testing.expectEqual(dl.State.queued, d.state);
    try testing.expect(d.paused);
    try testing.expect(std.mem.endsWith(u8, FileSeen.path, "Movie.2026.torrent"));
    try testing.expect(FileSeen.paused);
    try testing.expect(!FileSeen.replace_name);
}
