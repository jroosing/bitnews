//! Tests of the unpack group: import the files of the group here.
//! `zig build test-unpack --fuzz=N` runs only the first fuzz test of a group
//! (the listing parser's); to fuzz another target (cleanName in
//! unpack_name_test, the guard, the volume names in unpack_detect_test),
//! import only its file for the run.
test {
    _ = @import("unpack.zig");
    _ = @import("unpack_name.zig");
    _ = @import("unpack_listing.zig");
    _ = @import("unpack_guard.zig");
    _ = @import("unpack_detect.zig");
    _ = @import("unpack_sevenzip.zig");
    _ = @import("unpack_walk.zig");
    _ = @import("unpack_ports.zig");
    _ = @import("unpack_golden_test.zig");
    _ = @import("unpack_listing_test.zig");
    _ = @import("unpack_guard_test.zig");
    _ = @import("unpack_name_test.zig");
    _ = @import("unpack_detect_test.zig");
    _ = @import("unpack_fake_test.zig");
    _ = @import("unpack_e2e_test.zig");
    _ = @import("unpack_rar_test.zig");
    _ = @import("unpack_cancel_test.zig");
    _ = @import("unpack_ports_test.zig");
}
