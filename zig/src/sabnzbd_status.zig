//! SABnzbd status strings and nzo_id parsing.
const std = @import("std");

const download = @import("download.zig");
const event = @import("event.zig");

const nzo_prefix = "SABnzbd_nzo_";
const max_nzo_ids = 256;

/// Maps Download + optional post-process stage to SABnzbd status wire string.
pub fn sabStatus(d: download.Download, stage: ?event.Stage) []const u8 {
    return switch (d.state) {
        .ready => "Completed",
        .failed => "Failed",
        .fetched => "Verifying",
        .post_processing => if (stage) |s| switch (s) {
            .repairing => "Repairing",
            .unpacking => "Extracting",
            .finishing => "Moving",
            .verifying => "Verifying",
        } else "Verifying",
        .fetching => if (d.paused) "Paused" else "Downloading",
        .fetching_metadata, .added, .queued => if (d.paused) "Paused" else "Queued",
    };
}

/// NZB downloads currently in the SABnzbd queue listing.
pub fn inQueueListing(d: download.Download) bool {
    return d.source_kind == .nzb and d.state.inQueue();
}

/// NZB downloads in the SABnzbd history listing (completed, not in queue).
pub fn inHistoryListing(d: download.Download) bool {
    return d.source_kind == .nzb and !d.state.inQueue();
}

/// Extracts the Download ID from an nzo_id. Null when the prefix or ID shape is wrong.
/// The result is a subslice of `s`: trim only drops ends, then the prefix.
pub fn parseNzoID(s: []const u8) ?[]const u8 {
    const trimmed = trimSpace(s);
    if (!std.mem.startsWith(u8, trimmed, nzo_prefix)) return null;
    const id = trimmed[nzo_prefix.len..];
    if (id.len == 0 or id.len > 64) return null;
    // Stricter than id.isValid: `_` and `-` are not an nzo remainder.
    for (id) |c| switch (c) {
        'a'...'z', '0'...'9' => {},
        else => return null,
    };
    return id;
}

/// Builds SABnzbd `nzo_id` wire string from a Download ID.
pub fn nzoID(arena: std.mem.Allocator, id: []const u8) []const u8 {
    return std.fmt.allocPrint(arena, "{s}{s}", .{ nzo_prefix, id }) catch unreachable;
}

/// Comma-separated nzo_ids → Download IDs. Invalid tokens dropped; duplicate IDs dropped (first wins).
pub fn parseNzoIDs(arena: std.mem.Allocator, s: []const u8) []const []const u8 {
    var list: std.ArrayList([]const u8) = .empty;
    var it = std.mem.splitScalar(u8, s, ',');
    while (it.next()) |part| {
        if (list.items.len >= max_nzo_ids) break;
        const id = parseNzoID(part) orelse continue;
        if (hasID(list.items, id)) continue;
        list.append(arena, id) catch unreachable;
    }
    return list.items;
}

fn hasID(ids: []const []const u8, id: []const u8) bool {
    for (ids) |existing| {
        if (std.mem.eql(u8, existing, id)) return true;
    }
    return false;
}

/// Go's strings.TrimSpace. Invalid UTF-8 is not white space. Result is a subslice of `s`.
fn trimSpace(s: []const u8) []const u8 {
    var start: usize = 0;
    while (start < s.len) {
        const sp = spaceLen(s[start..]);
        if (sp == 0) break;
        start += sp;
    }
    var end = start;
    var i = start;
    while (i < s.len) {
        const sp = spaceLen(s[i..]);
        if (sp == 0) {
            i += 1;
            end = i;
        } else i += sp;
    }
    return s[start..end];
}

/// Length of the Unicode white space (Go's unicode.IsSpace) at the start of `s`, or 0.
fn spaceLen(s: []const u8) usize {
    if (s.len == 0) return 0;
    switch (s[0]) {
        '\t', '\n', 0x0b, 0x0c, '\r', ' ' => return 1,
        0xc2 => if (s.len >= 2 and (s[1] == 0x85 or s[1] == 0xa0)) return 2,
        0xe1 => if (s.len >= 3 and s[1] == 0x9a and s[2] == 0x80) return 3, // U+1680
        0xe2 => if (s.len >= 3) {
            if (s[1] == 0x80 and ((s[2] >= 0x80 and s[2] <= 0x8a) or s[2] == 0xa8 or s[2] == 0xa9 or s[2] == 0xaf)) return 3;
            if (s[1] == 0x81 and s[2] == 0x9f) return 3; // U+205F
        },
        0xe3 => if (s.len >= 3 and s[1] == 0x80 and s[2] == 0x80) return 3, // U+3000
        else => {},
    }
    return 0;
}

test "nzoID prefixes download id" {
    const testing = std.testing;
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    try testing.expectEqualStrings("SABnzbd_nzo_abc123", nzoID(arena.allocator(), "abc123"));
}

test "parseNzoIDs comma split drop bad and dup first wins" {
    const testing = std.testing;
    var arena = std.heap.ArenaAllocator.init(testing.allocator);
    defer arena.deinit();
    const ids = parseNzoIDs(arena.allocator(), "SABnzbd_nzo_abc,garbage,SABnzbd_nzo_abc,SABnzbd_nzo_def");
    try testing.expectEqual(@as(usize, 2), ids.len);
    try testing.expectEqualStrings("abc", ids[0]);
    try testing.expectEqualStrings("def", ids[1]);
}

test "parseNzoID trim prefix and reject bad shapes" {
    const testing = std.testing;
    try testing.expectEqualStrings("abc123", parseNzoID(" SABnzbd_nzo_abc123 ").?);
    const bad = [_][]const u8{
        "",
        "SABnzbd_nzo_",
        "abc123",
        "SABnzbd_nzo_UPPER",
        "SABnzbd_nzo_a/b",
        "SABnzbd_nzo_../x",
        "nzo_abc123",
    };
    for (bad) |b| {
        try testing.expect(parseNzoID(b) == null);
    }
}

test "sabStatus ready without stage returns Completed" {
    const testing = std.testing;
    const d = download.Download{
        .id = "x",
        .name = "n",
        .source_kind = .nzb,
        .state = .ready,
        .added_at = 1,
        .updated_at = 1,
    };
    try testing.expectEqualStrings("Completed", sabStatus(d, null));
}

test "inQueueListing true for queued nzb false for ready or torrent" {
    const testing = std.testing;
    const base = struct {
        fn dl(kind: download.SourceKind, state: download.State) download.Download {
            return .{
                .id = "x",
                .name = "n",
                .source_kind = kind,
                .state = state,
                .added_at = 1,
                .updated_at = 1,
            };
        }
    };
    try testing.expect(inQueueListing(base.dl(.nzb, .queued)));
    try testing.expect(!inQueueListing(base.dl(.nzb, .ready)));
    try testing.expect(!inQueueListing(base.dl(.torrent, .queued)));
}

test "inHistoryListing true for ready nzb false for queued or torrent" {
    const testing = std.testing;
    const base = struct {
        fn dl(kind: download.SourceKind, state: download.State) download.Download {
            return .{
                .id = "x",
                .name = "n",
                .source_kind = kind,
                .state = state,
                .added_at = 1,
                .updated_at = 1,
            };
        }
    };
    try testing.expect(inHistoryListing(base.dl(.nzb, .ready)));
    try testing.expect(!inHistoryListing(base.dl(.nzb, .queued)));
    try testing.expect(!inHistoryListing(base.dl(.torrent, .ready)));
}
