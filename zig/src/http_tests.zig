//! Tests of the http group: import the files of the group here.
test {
    _ = @import("router.zig");
    _ = @import("respond.zig");
    _ = @import("http_redact.zig");
    _ = @import("http_head.zig");
    _ = @import("api.zig");
    _ = @import("http_server.zig");
}
