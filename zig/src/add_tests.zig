//! Tests of the add group: import the files of the group here.
test {
    _ = @import("add_mime.zig");
    _ = @import("add_mime_compat_test.zig");
    _ = @import("add_multipart.zig");
    _ = @import("add_multipart_compat_test.zig");
    _ = @import("add_names.zig");
    _ = @import("add_names_compat_test.zig");
    _ = @import("add_fetch.zig");
    _ = @import("add_curl.zig");
    _ = @import("add_service.zig");
    _ = @import("add_service_test.zig");
    _ = @import("add_api.zig");
    _ = @import("add_api_test.zig");
    _ = @import("add_source_repo.zig");
}
