//! A Download as the API shows it, in responses and in SSE events
//! (internal/api/dto.go downloadJSON): category_id and the timestamps after
//! added_at are null when unset; failure_reason, failure_detail and
//! output_path are left out when empty. Fields keep the Go struct order.
const std = @import("std");
const dl = @import("download.zig");
const time_fmt = @import("time_fmt.zig");

/// Wrap a Download in this to serialize it: `std.json.Stringify.value(Json{ .d = d }, ...)`,
/// or nest it in any struct or slice given to `Response.json`.
pub const Json = struct {
    d: dl.Download,

    pub fn jsonStringify(self: Json, jw: anytype) !void {
        const d = self.d;
        try jw.beginObject();
        try jw.objectField("id");
        try jw.write(d.id);
        try jw.objectField("name");
        try jw.write(d.name);
        try jw.objectField("source_kind");
        try jw.write(@tagName(d.source_kind));
        try jw.objectField("category_id");
        try jw.write(if (d.category_id.len == 0) null else d.category_id);
        try jw.objectField("state");
        try jw.write(@tagName(d.state));
        try jw.objectField("priority");
        try jw.write(@tagName(d.priority));
        try jw.objectField("queue_position");
        try jw.write(d.queue_position);
        try jw.objectField("paused");
        try jw.write(d.paused);
        if (d.failure_reason) |r| {
            try jw.objectField("failure_reason");
            try jw.write(@tagName(r));
        }
        if (d.failure_detail.len > 0) {
            try jw.objectField("failure_detail");
            try jw.write(d.failure_detail);
        }
        try jw.objectField("total_bytes");
        try jw.write(d.total_bytes);
        try jw.objectField("done_bytes");
        try jw.write(d.done_bytes);
        try jw.objectField("added_at");
        try writeTime(jw, d.added_at);
        try jw.objectField("fetched_at");
        try writeTimeOrNull(jw, d.fetched_at);
        try jw.objectField("ready_at");
        try writeTimeOrNull(jw, d.ready_at);
        try jw.objectField("updated_at");
        try writeTime(jw, d.updated_at);
        if (d.output_path.len > 0) {
            try jw.objectField("output_path");
            try jw.write(d.output_path);
        }
        try jw.endObject();
    }
};

fn writeTimeOrNull(jw: anytype, ms: i64) !void {
    if (ms == 0) return jw.write(null);
    return writeTime(jw, ms);
}

fn writeTime(jw: anytype, ms: i64) !void {
    var buf: [40]u8 = undefined;
    var w: std.Io.Writer = .fixed(&buf);
    try time_fmt.writeRfc3339(&w, ms);
    try jw.write(w.buffered());
}

const testing = std.testing;

fn render(d: dl.Download) ![]u8 {
    var aw: std.Io.Writer.Allocating = .init(testing.allocator);
    errdefer aw.deinit();
    try std.json.Stringify.value(Json{ .d = d }, .{}, &aw.writer);
    return aw.toOwnedSlice();
}

// Expected strings are the output of the Go api.toDownloadJSON for the same values.
test "a queued Download without a category" {
    const out = try render(.{
        .id = "abc",
        .name = "Some.Show",
        .source_kind = .nzb,
        .state = .queued,
        .queue_position = 2,
        .total_bytes = 1000,
        .added_at = 1793365901873,
        .updated_at = 1793365901000,
    });
    defer testing.allocator.free(out);
    try testing.expectEqualStrings(
        "{\"id\":\"abc\",\"name\":\"Some.Show\",\"source_kind\":\"nzb\",\"category_id\":null,\"state\":\"queued\"," ++
            "\"priority\":\"normal\",\"queue_position\":2,\"paused\":false,\"total_bytes\":1000,\"done_bytes\":0," ++
            "\"added_at\":\"2026-10-30T13:11:41.873Z\",\"fetched_at\":null,\"ready_at\":null," ++
            "\"updated_at\":\"2026-10-30T13:11:41Z\"}",
        out,
    );
}

test "a failed Download shows the reason and detail, a ready one the output path" {
    const failed = try render(.{
        .id = "f",
        .name = "n",
        .source_kind = .torrent,
        .category_id = "cat1",
        .state = .failed,
        .failure_reason = .disk_full,
        .failure_detail = "no space",
        .added_at = 0,
        .updated_at = 0,
    });
    defer testing.allocator.free(failed);
    try testing.expect(std.mem.indexOf(u8, failed, "\"category_id\":\"cat1\"") != null);
    try testing.expect(std.mem.indexOf(u8, failed, "\"failure_reason\":\"disk_full\",\"failure_detail\":\"no space\"") != null);
    try testing.expect(std.mem.indexOf(u8, failed, "output_path") == null);

    const ready = try render(.{ .id = "r", .name = "n", .source_kind = .nzb, .state = .ready, .output_path = "/data/x", .ready_at = 1000, .added_at = 0, .updated_at = 0 });
    defer testing.allocator.free(ready);
    try testing.expect(std.mem.indexOf(u8, ready, "\"ready_at\":\"1970-01-01T00:00:01Z\"") != null);
    try testing.expect(std.mem.endsWith(u8, ready, "\"output_path\":\"/data/x\"}"));
}
