//! Tests of the nntp group: import the files of the group here.
test {
    _ = @import("nntp_proto.zig");
    _ = @import("nntp_diag.zig");
    _ = @import("nntp_transport.zig");
    _ = @import("nntp_conn_test.zig");
    _ = @import("nntptest_fixture.zig");
    _ = @import("nntptest.zig");
    _ = @import("nntp_pool_test.zig");
    _ = @import("nntp_tls.zig");
    _ = @import("nntp_probe.zig");
    _ = @import("nntp.zig");
}
