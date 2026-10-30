//! The real implementations of the Queue's seams that live in other modules:
//! the Downloads repo and the Settings service.
const std = @import("std");
const Allocator = std.mem.Allocator;
const dl = @import("download.zig");
const seams = @import("queue_seams.zig");
const repo_mod = @import("downloads_repo.zig");
const settings_service = @import("settings_service.zig");

/// The Downloads seam backed by the repo. `repo` must outlive the Queue.
pub fn downloadsFromRepo(repo: *const repo_mod.DownloadRepo) seams.Downloads {
    return .{
        .ctx = @constCast(repo),
        .list_fn = repoList,
        .get_fn = repoGet,
        .update_state_fn = repoUpdateState,
        .update_progress_fn = repoUpdateProgress,
    };
}

fn repoOf(ctx: *anyopaque) *const repo_mod.DownloadRepo {
    return @ptrCast(@alignCast(ctx));
}

fn repoList(ctx: *anyopaque, arena: Allocator, state: dl.State, kind: dl.SourceKind) anyerror![]dl.Download {
    return repoOf(ctx).listAll(arena, .{ .state = state, .source_kind = kind });
}

fn repoGet(ctx: *anyopaque, arena: Allocator, id: []const u8) anyerror!dl.Download {
    return repoOf(ctx).get(arena, id);
}

fn repoUpdateState(ctx: *anyopaque, arena: Allocator, id: []const u8, c: dl.StateChange) anyerror!dl.Download {
    return repoOf(ctx).updateState(arena, id, c);
}

fn repoUpdateProgress(ctx: *anyopaque, id: []const u8, done: i64, total: i64) anyerror!void {
    return repoOf(ctx).updateProgress(id, done, total);
}

/// The Policy of the `kind` Queue, read from the Settings in effect. The
/// service must outlive the Queue.
pub const SettingsPolicy = struct {
    service: *settings_service.Service,
    kind: dl.SourceKind,

    pub fn source(self: *SettingsPolicy) seams.PolicySource {
        return .{ .ctx = self, .read_fn = read };
    }

    fn read(ctx: *anyopaque, arena: Allocator) anyerror!seams.Policy {
        const self: *SettingsPolicy = @ptrCast(@alignCast(ctx));
        const snap = self.service.current();
        defer snap.release();
        const s = &snap.settings;
        return switch (self.kind) {
            .nzb => .{
                .limit = s.queues.usenet,
                .incomplete = try arena.dupe(u8, s.paths.usenet.incomplete),
                .min_free = s.queues.effectiveMinFree(),
            },
            .torrent => .{
                .limit = s.queues.torrent,
                .incomplete = try arena.dupe(u8, s.paths.torrent.incomplete),
                .min_free = s.queues.effectiveMinFree(),
            },
        };
    }
};
