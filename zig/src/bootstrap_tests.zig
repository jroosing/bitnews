//! Tests of the bootstrap group: import the files of the group here.
test {
    _ = @import("secret.zig");
    _ = @import("log_text.zig");
    _ = @import("log.zig");
    _ = @import("log_test.zig");
    _ = @import("config.zig");
    _ = @import("config_test.zig");
}
