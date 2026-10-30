//! Tests of the core group: the domain types the services and handlers share.
test {
    _ = @import("id.zig");
    _ = @import("time_fmt.zig");
    _ = @import("download.zig");
    _ = @import("event.zig");
    _ = @import("app_error.zig");
    _ = @import("strict_json.zig");
    _ = @import("api_json.zig");
    _ = @import("download_json.zig");
}
