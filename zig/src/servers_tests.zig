//! Tests of the servers group: import the files of the group here.
test {
    _ = @import("servers_types.zig");
    _ = @import("servers_repo.zig");
    _ = @import("servers_service.zig");
    _ = @import("servers_api.zig");
}
