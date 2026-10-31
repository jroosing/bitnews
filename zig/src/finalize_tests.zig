//! Tests of the finalize group: cleanup and the move into the complete tree.
test {
    _ = @import("finalize.zig");
    _ = @import("finalize_dest_test.zig");
    _ = @import("finalize_flow_test.zig");
    _ = @import("finalize_safety_test.zig");
    _ = @import("finalize_move_test.zig");
}
