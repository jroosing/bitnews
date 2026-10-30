//! Tests of the app group: the pieces that join the others (app key, wiring).
test {
    _ = @import("appkey.zig");
    _ = @import("setup_gate.zig");
    _ = @import("services.zig");
    _ = @import("native_libs_test.zig");
}
