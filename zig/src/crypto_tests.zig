//! Tests of the crypto group: import the files of the group here.
test {
    _ = @import("keyfile.zig");
    _ = @import("secretbox.zig");
    _ = @import("password.zig");
    _ = @import("token.zig");
    _ = @import("compat_crypto.zig");
}
