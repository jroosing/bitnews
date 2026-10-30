//! Adding Downloads (internal/app/add.go, add_request.go): the one way in for
//! the own API and, later, the SABnzbd and qBittorrent adapters.
//!
//! A caller has two steps. `receive` stages an uploaded file (a multipart
//! upload may send the file before the fields that describe it): the NZB is
//! parsed and kept in the new Download's directory
//! <usenet incomplete>/<download id>/ with mode 0600. `add` takes the whole
//! request, the received file or a link, and does the rest: checking, fetching
//! a link, the Category (found, or created by name), naming, storing and
//! queueing, and cleaning up on any error. Nothing is left behind when the NZB
//! is not valid or the add fails.
//!
//! Torrents are not handled here: magnet links, .torrent files and links to
//! them go to the `Torrents` seam, which is empty until the libtorrent
//! milestone (Z7); the answer is then `add_unavailable` (503).
//!
//! The failures a client can act on are `error.Rejected` with the details in
//! the `Failure` the caller passes (app_error.zig); anything else is a 500.
//! Results and their strings live in the arena the caller passes.
const std = @import("std");
const Io = std.Io;
const Allocator = std.mem.Allocator;
const app_error = @import("app_error.zig");
const Failure = app_error.Failure;
const dl = @import("download.zig");
const fsutil = @import("fsutil.zig");
const id_mod = @import("id.zig");
const log = @import("log.zig");
const nzb = @import("nzb.zig");
const secret_mod = @import("secret.zig");
const Secret = secret_mod.Secret;
const categories_types = @import("categories_types.zig");
const categories_service = @import("categories_service.zig");
const downloads_service = @import("downloads_service.zig");
const fetch = @import("add_fetch.zig");
const mime = @import("add_mime.zig");
const multipart = @import("add_multipart.zig");
const names = @import("add_names.zig");
const settings_service = @import("settings_service.zig");
const source_repo = @import("add_source_repo.zig");

/// The size cap of an NZB, uploaded or fetched (DefaultMaxNZBBytes).
pub const default_max_nzb_bytes: u64 = 64 << 20;
/// The size cap of a .torrent file (core.MaxMetainfoSize).
pub const max_torrent_bytes: u64 = 10 << 20;
pub const max_password_bytes = 1024;
/// The temporary name of the NZB while it is being received.
const staging_name = ".incoming.nzb";

const unavailable_message = "adding downloads is not available in this build yet";

// ---- seams ----

/// What the add service reads from the Settings in effect now (Go:
/// SettingsReader.Current). Strings are allocated in `arena`.
pub const SettingsSource = struct {
    ctx: ?*anyopaque,
    read_fn: *const fn (ctx: ?*anyopaque, arena: Allocator) anyerror!View,

    pub const View = struct {
        /// The Usenet incomplete root: a Download's directory is made in it.
        usenet_incomplete: []const u8,
        /// PostProcess.CleanNames: read at every add.
        clean_names: bool = false,
    };

    pub fn read(s: SettingsSource, arena: Allocator) anyerror!View {
        return s.read_fn(s.ctx, arena);
    }
};

/// The Settings service as the add service's SettingsSource. It must outlive
/// the AddService.
pub fn settingsFromService(svc: *settings_service.Service) SettingsSource {
    const A = struct {
        fn read(ctx: ?*anyopaque, arena: Allocator) anyerror!SettingsSource.View {
            const s: *settings_service.Service = @ptrCast(@alignCast(ctx.?));
            const snap = s.current();
            defer snap.release();
            return .{
                .usenet_incomplete = try arena.dupe(u8, snap.settings.paths.usenet.incomplete),
                .clean_names = snap.settings.postprocess.clean_names,
            };
        }
    };
    return .{ .ctx = svc, .read_fn = A.read };
}

/// Categories, for the checks and the by-name lookups of an add.
pub const Categories = struct {
    ctx: ?*anyopaque,
    exists_fn: *const fn (ctx: ?*anyopaque, arena: Allocator, id: []const u8) anyerror!bool,
    list_fn: *const fn (ctx: ?*anyopaque, arena: Allocator) anyerror![]const categories_types.Category,
    /// The Category named so, ignoring case, created when there is none.
    ensure_fn: *const fn (ctx: ?*anyopaque, arena: Allocator, f: *Failure, name: []const u8) anyerror!categories_types.Category,
};

/// The Categories service as the add service's Categories. It must outlive
/// the AddService.
pub fn categoriesFromService(svc: *categories_service.Service) Categories {
    const A = struct {
        fn service(ctx: ?*anyopaque) *categories_service.Service {
            return @ptrCast(@alignCast(ctx.?));
        }
        fn exists(ctx: ?*anyopaque, arena: Allocator, id: []const u8) anyerror!bool {
            _ = service(ctx).repo.get(arena, id) catch |err| switch (err) {
                error.NotFound => return false,
                else => return err,
            };
            return true;
        }
        fn list(ctx: ?*anyopaque, arena: Allocator) anyerror![]const categories_types.Category {
            return service(ctx).list(arena);
        }
        fn ensure(ctx: ?*anyopaque, arena: Allocator, f: *Failure, name: []const u8) anyerror!categories_types.Category {
            return service(ctx).ensure(arena, f, name);
        }
    };
    return .{ .ctx = svc, .exists_fn = A.exists, .list_fn = A.list, .ensure_fn = A.ensure };
}

/// Where the source row of a new Download goes (core.DownloadSourceRepo).
pub const Sources = struct {
    ctx: ?*anyopaque,
    create_fn: *const fn (ctx: ?*anyopaque, arena: Allocator, download_id: []const u8, nzb_path: []const u8, password: []const u8) anyerror!void,
};

/// The sources table as the add service's Sources. It must outlive the
/// AddService.
pub fn sourcesFromRepo(repo: *const source_repo.SourceRepo) Sources {
    const A = struct {
        fn create(ctx: ?*anyopaque, arena: Allocator, download_id: []const u8, nzb_path: []const u8, password: []const u8) anyerror!void {
            const r: *const source_repo.SourceRepo = @ptrCast(@alignCast(ctx.?));
            return r.create(arena, download_id, nzb_path, password);
        }
    };
    return .{ .ctx = @constCast(repo), .create_fn = A.create };
}

/// Called after a Download was added and announced, so the runner looks at
/// its Queue now instead of at its next pass. Must not block.
pub const OnAdded = struct {
    ctx: ?*anyopaque = null,
    added_fn: *const fn (ctx: ?*anyopaque, d: dl.Download) void = noop,

    fn noop(_: ?*anyopaque, _: dl.Download) void {}
};

/// Runs Clean.py's name cleaning (ADR-0009) on the final name when the
/// CleanNames option is on. The post-processing milestone (Z4) provides it;
/// without one names are kept as they are. The result is allocated in `arena`.
pub const NameCleaner = struct {
    ctx: ?*anyopaque = null,
    clean_fn: *const fn (ctx: ?*anyopaque, arena: Allocator, name: []const u8) anyerror![]const u8,
};

/// The torrent branch of adding (Z7, libtorrent). `add_fn` creates the whole
/// Download, publishes `download_added` and wakes the torrent runner; it is
/// null in this build, which answers `add_unavailable`.
pub const Torrents = struct {
    ctx: ?*anyopaque = null,
    add_fn: ?*const fn (ctx: ?*anyopaque, arena: Allocator, f: *Failure, req: TorrentRequest) anyerror!dl.Download = null,

    pub fn enabled(t: Torrents) bool {
        return t.add_fn != null;
    }
};

pub const TorrentSource = union(enum) {
    /// The bytes of a .torrent file (at most `max_torrent_bytes`).
    file: []const u8,
    /// A magnet link, as given (not parsed here).
    magnet: []const u8,
};

/// A torrent add, after the checks all adds share.
pub const TorrentRequest = struct {
    source: TorrentSource,
    name: []const u8,
    file_name: []const u8,
    /// Empty or a Category that exists.
    category_id: []const u8,
    priority: dl.Priority,
    paused: bool,
};

pub const Deps = struct {
    settings: SettingsSource,
    categories: Categories,
    sources: Sources,
    fetcher: fetch.Fetcher,
    logger: log.Logger,
    on_added: OnAdded = .{},
    torrents: Torrents = .{},
    name_cleaner: ?NameCleaner = null,
    max_nzb_bytes: u64 = default_max_nzb_bytes,
    fetch_timeout_ms: u32 = fetch.default_timeout_ms,
};

// ---- requests ----

/// A file received for a new Download, staged while it was read: an NZB in the
/// Download's directory, or the bytes of a .torrent. One that is not given to
/// `add` must be discarded.
pub const Upload = struct {
    kind: dl.SourceKind,
    file_name: []const u8,
    id: []const u8 = "",
    dir: []const u8 = "",
    tmp_path: []const u8 = "",
    doc: nzb.Nzb = .{},
    torrent_file: []const u8 = "",
    done: bool = false,

    /// Removes the staged files. It does nothing after a successful `add`, so
    /// it is safe to defer.
    pub fn discard(u: *Upload, io: Io) void {
        if (u.done) return;
        u.done = true;
        if (u.dir.len > 0) Io.Dir.cwd().deleteTree(io, u.dir) catch {};
    }
};

pub const Request = struct {
    /// The display name. Empty takes the NZB's own name, then `file_name`,
    /// then the first file's name, then "nzb-<id>".
    name: []const u8 = "",
    /// The name of the uploaded file, if any. A trailing ".nzb" is ignored.
    file_name: []const u8 = "",
    /// Must name an existing Category, or be empty to use the NZB's category
    /// (matched by name; an unknown one is ignored).
    category_id: []const u8 = "",
    /// A Category to use by name, created when it does not exist (what
    /// SABnzbd and qBittorrent do). `category_id` wins when both are set.
    category_name: []const u8 = "",
    /// force, high, normal or low; empty is normal. Kept as text so a bad
    /// value can be shown.
    priority: []const u8 = "",
    /// The password of the release's archives. Empty takes the NZB's, then the
    /// "{{password}}" suffix of the name.
    password: ?Secret = null,
    paused: bool = false,
    /// The received file (`receive`) or a link, exactly one of them: a magnet
    /// link, or the http(s) URL of an NZB or a .torrent, fetched now (never
    /// stored, never logged).
    upload: ?*Upload = null,
    url: []const u8 = "",
    /// When set, accepts that kind of source alone.
    only: ?dl.SourceKind = null,
};

pub const AddService = struct {
    gpa: Allocator,
    io: Io,
    downloads: *downloads_service.Service,
    deps: Deps,

    /// `downloads` must outlive the service; Downloads are created through it
    /// so they share its lock and its repository.
    pub fn init(gpa: Allocator, io: Io, downloads: *downloads_service.Service, deps: Deps) AddService {
        return .{ .gpa = gpa, .io = io, .downloads = downloads, .deps = deps };
    }

    // ---- receiving ----

    /// Stages an uploaded file given as bytes (at most `max_nzb_bytes + 1` of
    /// them: more are `too_large` either way). `kind` says what it must be, or
    /// null to decide by the content: a .torrent is a bencoded dictionary and
    /// starts with "d", anything else is taken to be an NZB. Undecided, a file
    /// whose extension says the other kind is refused ("not_a_torrent",
    /// "not_an_nzb"), and an empty one is "empty_file".
    pub fn receive(self: *AddService, arena: Allocator, f: *Failure, bytes: []const u8, file_name: []const u8, kind: ?dl.SourceKind) !*Upload {
        const u = try arena.create(Upload);
        u.* = .{ .kind = kind orelse .nzb, .file_name = if (file_name.len == 0) "" else try arena.dupe(u8, multipart.goBase(try replaceBackslashes(arena, file_name))) };
        var k: dl.SourceKind = undefined;
        if (kind) |given| {
            k = given;
        } else {
            if (bytes.len == 0) return f.setLiteral(.invalid_input, "empty_file", "That file is empty.");
            k = if (bytes[0] == 'd') .torrent else .nzb;
            const ext = try std.ascii.allocLowerString(arena, extOf(u.file_name));
            if (std.mem.eql(u8, ext, ".torrent") and k != .torrent) {
                return f.setLiteral(.invalid_input, "not_a_torrent", "That file ends in .torrent but is not a torrent file.");
            }
            if (std.mem.eql(u8, ext, ".nzb") and k == .torrent) {
                return f.setLiteral(.invalid_input, "not_an_nzb", "That file ends in .nzb but is a torrent file.");
            }
        }
        u.kind = k;
        switch (k) {
            .nzb => try self.stage(arena, f, u, bytes),
            .torrent => {
                if (!self.deps.torrents.enabled()) return f.setLiteral(.unavailable, "add_unavailable", unavailable_message);
                if (bytes.len > max_torrent_bytes) return torrentTooLarge(f);
                u.torrent_file = bytes;
            },
        }
        return u;
    }

    fn torrentTooLarge(f: *Failure) error{Rejected} {
        return f.setLiteral(.too_large, "torrent_too_large", "the torrent file is larger than 10 MiB");
    }

    /// Makes the Download's directory, parses `bytes` and keeps them there. An
    /// NZB over the cap is `too_large` ("nzb_too_large"); one that does not
    /// parse is invalid ("invalid_nzb"). Nothing is left on disk on failure.
    fn stage(self: *AddService, arena: Allocator, f: *Failure, u: *Upload, bytes: []const u8) !void {
        const view = try self.deps.settings.read(arena);
        const id = try arena.dupe(u8, &id_mod.new(self.io));
        const dir = try fsutil.join(arena, &.{ view.usenet_incomplete, id });
        // 0o777 before the umask, like every directory the daemon creates.
        Io.Dir.cwd().createDirPath(self.io, dir) catch |err| {
            if (fsutil.isPermissionError(err)) {
                const detail = try fsutil.writeErrorDetail(arena, fsutil.deepestExistingParent(self.io, dir), fsutil.effectiveUid());
                return f.setLiteral(.unavailable, "folder_not_writable", detail);
            }
            return err;
        };
        u.id = id;
        u.dir = dir;
        errdefer u.discard(self.io);

        const lim: nzb.Limits = .{ .max_bytes = self.deps.max_nzb_bytes };
        var diag: nzb.Diag = .{};
        u.doc = nzb.parseWithLimits(arena, bytes, lim, &diag) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.TooLarge => return f.set(arena, .too_large, "nzb_too_large", "the NZB is larger than {d} MiB", .{self.deps.max_nzb_bytes >> 20}),
            error.Malformed, error.NoFiles, error.TooManyFiles, error.TooManySegments => {
                const why = try nzb.errorMessage(arena, err, lim, diag);
                return f.set(arena, .invalid_input, "invalid_nzb", "not a valid NZB: {s}", .{why});
            },
        };
        const tmp = try fsutil.join(arena, &.{ dir, staging_name });
        try writeNew(self.io, tmp, bytes);
        u.tmp_path = tmp;
    }

    // ---- adding ----

    /// Creates the Download for `req` and returns it, Queued at the end of its
    /// Queue, announced with `download_added`. On any error whatever was
    /// staged is removed, `req.upload` included.
    pub fn add(self: *AddService, arena: Allocator, f: *Failure, req_in: Request) !dl.Download {
        var req = req_in;
        var up = req.upload;
        defer if (up) |u| u.discard(self.io); // does nothing after a successful add
        if (up == null and req.url.len == 0) return f.setLiteral(.invalid_input, "missing_source", "give an NZB or torrent file, or a link");
        if (up != null and req.url.len > 0) return f.setLiteral(.invalid_input, "ambiguous_source", "give either a file or a link, not both");
        try self.check(arena, f, req);

        var magnet: ?[]const u8 = null;
        if (up == null) {
            const got = try self.fetchSource(arena, f, mime.trimSpace(req.url), req.only);
            up = got.upload;
            magnet = got.magnet;
        }
        const kind: dl.SourceKind = if (magnet != null) .torrent else up.?.kind;
        if (req.only) |only| {
            if (only == .nzb and kind != .nzb) return f.setLiteral(.invalid_input, "not_an_nzb", "this is a torrent, and only NZBs can be added here");
            if (only == .torrent and kind != .torrent) return f.setLiteral(.invalid_input, "not_a_torrent", "this is an NZB, and only torrents can be added here");
        }
        if (req.file_name.len == 0) {
            if (up) |u| req.file_name = u.file_name;
        }
        if (req.category_id.len == 0 and mime.trimSpace(req.category_name).len > 0) {
            const cat = try self.deps.categories.ensure_fn(self.deps.categories.ctx, arena, f, req.category_name);
            req.category_id = cat.id;
        }
        if (up) |u| {
            if (u.kind == .nzb) return self.addNzb(arena, f, u, req);
        }

        // A torrent: the seam does it all.
        const add_fn = self.deps.torrents.add_fn orelse return f.setLiteral(.unavailable, "add_unavailable", unavailable_message);
        const source: TorrentSource = if (magnet) |m| .{ .magnet = m } else .{ .file = up.?.torrent_file };
        const d = try add_fn(self.deps.torrents.ctx, arena, f, .{
            .source = source,
            .name = req.name,
            .file_name = req.file_name,
            .category_id = req.category_id,
            .priority = std.meta.stringToEnum(dl.Priority, req.priority) orelse .normal,
            .paused = req.paused,
        });
        if (up) |u| u.done = true;
        return d;
    }

    /// The fields that do not depend on the NZB: priority, password length and
    /// the requested Category. `add` does it too; doing it before a fetch saves
    /// fetching what would be refused anyway.
    fn check(self: *AddService, arena: Allocator, f: *Failure, req: Request) !void {
        if (req.priority.len > 0 and std.meta.stringToEnum(dl.Priority, req.priority) == null) {
            const q = try downloads_service.quote(arena, req.priority);
            return f.set(arena, .invalid_input, "invalid_priority", "unknown priority {s}: use force, high, normal or low", .{q});
        }
        if (req.password) |pw| {
            if (pw.len > max_password_bytes) return f.setLiteral(.invalid_input, "invalid_password", "the password is longer than 1024 bytes");
        }
        if (req.category_id.len > 0) {
            if (!try self.deps.categories.exists_fn(self.deps.categories.ctx, arena, req.category_id)) {
                const q = try downloads_service.quote(arena, req.category_id);
                return f.set(arena, .invalid_input, "unknown_category", "category {s} does not exist", .{q});
            }
        }
    }

    // ---- fetching a link ----

    const Fetched = struct { upload: ?*Upload = null, magnet: ?[]const u8 = null };

    fn fetchSource(self: *AddService, arena: Allocator, f: *Failure, link: []const u8, only: ?dl.SourceKind) !Fetched {
        if (isMagnet(link)) {
            if (only == dl.SourceKind.nzb) return f.setLiteral(.invalid_input, "not_an_nzb", "a magnet link is a torrent, and only NZBs can be added here");
            if (!self.deps.torrents.enabled()) return f.setLiteral(.unavailable, "add_unavailable", unavailable_message);
            return .{ .magnet = link };
        }
        const parsed = fetch.parseHttpUrl(link) orelse return f.setLiteral(.invalid_input, "invalid_url", "url must be an absolute http or https URL");
        const nzb_only = only == dl.SourceKind.nzb;
        const what = if (nzb_only) "NZB" else "NZB or torrent";

        const outcome = try self.deps.fetcher.fetch(self.gpa, self.io, .{
            .url = link,
            .accept = if (nzb_only) fetch.accept_nzb else fetch.accept_any,
            .max_bytes = self.deps.max_nzb_bytes,
            .timeout_ms = self.deps.fetch_timeout_ms,
        });
        defer outcome.deinit(self.gpa);
        const host_attr = log.Attr.string("host", parsed.host);
        switch (outcome) {
            .body => |bytes| {
                const is_torrent = !nzb_only and bytes.len > 0 and bytes[0] == 'd';
                if (is_torrent) {
                    if (bytes.len > max_torrent_bytes) return torrentTooLarge(f);
                    if (!self.deps.torrents.enabled()) return f.setLiteral(.unavailable, "add_unavailable", unavailable_message);
                    const u = try arena.create(Upload);
                    u.* = .{ .kind = .torrent, .file_name = "", .torrent_file = try arena.dupe(u8, bytes) };
                    self.deps.logger.info("torrent fetched", &.{host_attr});
                    return .{ .upload = u };
                }
                const u = try arena.create(Upload);
                u.* = .{ .kind = .nzb, .file_name = "" };
                try self.stage(arena, f, u, bytes);
                self.deps.logger.info("nzb fetched", &.{host_attr});
                return .{ .upload = u };
            },
            .magnet => |location| {
                if (nzb_only) return self.fetchFailed(arena, f, what, "refused a redirect to a \"magnet\" URL: only http and https are allowed", host_attr);
                if (!self.deps.torrents.enabled()) return f.setLiteral(.unavailable, "add_unavailable", unavailable_message);
                self.deps.logger.info("fetch redirected to a magnet link", &.{host_attr});
                return .{ .magnet = try arena.dupe(u8, location) };
            },
            .status => |code| {
                self.deps.logger.warn("nzb or torrent fetch failed", &.{ host_attr, log.Attr.int("status", code) });
                return f.set(arena, .upstream, "fetch_failed", "could not fetch the {s}: the server answered {d} {s}", .{ what, code, fetch.statusText(code) });
            },
            .too_large => return f.set(arena, .too_large, "nzb_too_large", "the NZB is larger than {d} MiB", .{self.deps.max_nzb_bytes >> 20}),
            .failed => |reason| return self.fetchFailed(arena, f, what, reason, host_attr),
            .timeout => {
                var buf: [32]u8 = undefined;
                const text = try std.fmt.allocPrint(arena, "timed out after {s}", .{fetch.durationText(&buf, self.deps.fetch_timeout_ms)});
                return self.fetchFailed(arena, f, what, text, host_attr);
            },
        }
    }

    fn fetchFailed(self: *AddService, arena: Allocator, f: *Failure, what: []const u8, reason: []const u8, host_attr: log.Attr) error{ Rejected, OutOfMemory } {
        // The reason holds no URL (add_fetch.zig); the log line does not
        // either.
        self.deps.logger.warn("nzb fetch failed", &.{ host_attr, log.Attr.string("reason", reason) });
        return f.set(arena, .upstream, "fetch_failed", "could not fetch the {s}: {s}", .{ what, reason });
    }

    // ---- an NZB ----

    fn addNzb(self: *AddService, arena: Allocator, f: *Failure, u: *Upload, req: Request) !dl.Download {
        errdefer u.discard(self.io);
        const view = try self.deps.settings.read(arena);
        const priority: dl.Priority = if (req.priority.len == 0) .normal else std.meta.stringToEnum(dl.Priority, req.priority).?;

        const category_id = try self.resolveCategory(arena, f, req.category_id, u.doc.meta.category);

        const resolved = try names.resolveName(arena, req.name, u.doc.meta.name, req.file_name, u.doc.files, u.id);
        var name = resolved.name;
        if (view.clean_names) {
            if (self.deps.name_cleaner) |nc| {
                // Clean.py as a built-in option (ADR-0009): applied to the
                // final name, which feeds the Download, the stored NZB file
                // and the output folder.
                name = try nc.clean_fn(nc.ctx, arena, name);
            }
        }
        const password = firstNonEmpty(if (req.password) |pw| pw.reveal() else "", mime.trimSpace(u.doc.meta.password), resolved.suffix_password);

        const final_path = try fsutil.join(arena, &.{ u.dir, try names.nzbFileName(arena, name) });
        try Io.Dir.renameAbsolute(u.tmp_path, final_path, self.io);

        const now = self.downloads.deps.now_fn(self.io);
        var d: dl.Download = .{
            .id = u.id,
            .name = name,
            .source_kind = .nzb,
            .category_id = category_id,
            .state = .queued,
            .priority = priority,
            .paused = req.paused,
            .total_bytes = u.doc.upFrontBytes(),
            .added_at = now,
            .updated_at = now,
        };
        d = try self.insert(arena, d, final_path, password);
        u.done = true;
        self.downloads.deps.publisher.publish(.{ .download_added = d });
        self.deps.on_added.added_fn(self.deps.on_added.ctx, d);
        return d;
    }

    /// Stores the Download at the end of the NZB Queue together with its
    /// source, so that the runner, which only looks at Queued Downloads, never
    /// sees one without its source:
    ///
    ///  1. the Download is created in state Added (with its Queue position);
    ///  2. its source row is stored;
    ///  3. the Download moves to Queued.
    ///
    /// A failure after step 1 deletes the Download again (the source row goes
    /// with it by cascade). Returns the stored, Queued Download.
    fn insert(self: *AddService, arena: Allocator, d_in: dl.Download, nzb_path: []const u8, password: []const u8) !dl.Download {
        const svc = self.downloads;
        try svc.mu.lock(self.io);
        defer svc.mu.unlock(self.io);
        var d = d_in;
        const queue = try svc.queueOf(arena, .nzb);
        d.queue_position = 0;
        for (queue) |q| d.queue_position = @max(d.queue_position, q.queue_position + 1);
        d.state = .added;
        try svc.repo.create(d);
        errdefer svc.repo.delete(d.id) catch {};
        try self.deps.sources.create_fn(self.deps.sources.ctx, arena, d.id, nzb_path, password);
        return svc.repo.updateState(arena, d.id, .{ .to = .queued, .at = svc.deps.now_fn(self.io) });
    }

    /// Validates the requested Category, or else looks the NZB's category up
    /// by name. A name no Category has is ignored: indexers put free text
    /// there, and the Download is better added than refused.
    fn resolveCategory(self: *AddService, arena: Allocator, f: *Failure, id: []const u8, meta_name: []const u8) ![]const u8 {
        if (id.len > 0) {
            if (!try self.deps.categories.exists_fn(self.deps.categories.ctx, arena, id)) {
                const q = try downloads_service.quote(arena, id);
                return f.set(arena, .invalid_input, "unknown_category", "category {s} does not exist", .{q});
            }
            return id;
        }
        if (meta_name.len == 0) return "";
        const cats = try self.deps.categories.list_fn(self.deps.categories.ctx, arena);
        if (categories_types.findIgnoreCase(cats, meta_name)) |c| return c.id;
        return "";
    }
};

fn firstNonEmpty(a: []const u8, b: []const u8, c: []const u8) []const u8 {
    if (a.len > 0) return a;
    if (b.len > 0) return b;
    return c;
}

fn isMagnet(s: []const u8) bool {
    return s.len >= 7 and std.ascii.eqlIgnoreCase(s[0..7], "magnet:");
}

fn replaceBackslashes(arena: Allocator, s: []const u8) Allocator.Error![]const u8 {
    if (std.mem.findScalar(u8, s, '\\') == null) return s;
    const out = try arena.dupe(u8, s);
    for (out) |*c| if (c.* == '\\') {
        c.* = '/';
    };
    return out;
}

/// Go's path.Ext of a base name.
fn extOf(base: []const u8) []const u8 {
    const i = std.mem.findScalarLast(u8, base, '.') orelse return "";
    return base[i..];
}

/// Creates `path` (which must not exist) with mode 0600, writes `bytes` and
/// syncs it.
fn writeNew(io: Io, path: []const u8, bytes: []const u8) !void {
    const file = try Io.Dir.cwd().createFile(io, path, .{ .exclusive = true, .permissions = .fromMode(0o600) });
    defer file.close(io);
    try file.writeStreamingAll(io, bytes);
    try file.sync(io);
}
