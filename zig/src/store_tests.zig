//! Tests of the store group: import the files of the group here.
test {
    _ = @import("sqlite.zig");
    _ = @import("migrate.zig");
    _ = @import("appkey_repo.zig");
    _ = @import("store_compat.zig");
}
