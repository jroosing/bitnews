//! Tests of the sabnzbd group: import the files of the group here.
test {
    _ = @import("sabnzbd.zig");
    // Later slices fill these. Imported here so the group compiles them
    // without sabnzbd.zig owning their logic.
    _ = @import("sabnzbd_params.zig");
    _ = @import("sabnzbd_config.zig");
    _ = @import("sabnzbd_format.zig");
    _ = @import("sabnzbd_text.zig");
    _ = @import("sabnzbd_live.zig");
    _ = @import("sabnzbd_status.zig");
    _ = @import("sabnzbd_add.zig");
    _ = @import("sabnzbd_queue.zig");
    _ = @import("sabnzbd_history.zig");
    _ = @import("sabnzbd_shapes.zig");
}
