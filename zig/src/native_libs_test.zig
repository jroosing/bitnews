//! The system libraries the daemon links work from Zig: OpenSSL and libcurl
//! are found, their headers translate, and the versions are the ones the
//! packages promise. A failure here is a build-environment problem.
const std = @import("std");
const ssl = @import("openssl_c");
const curl = @import("curl_c");

test "OpenSSL 3 is linked and can make a TLS client context" {
    try std.testing.expect(ssl.OpenSSL_version_num() >= 0x30000000);
    const ctx = ssl.SSL_CTX_new(ssl.TLS_client_method());
    try std.testing.expect(ctx != null);
    defer ssl.SSL_CTX_free(ctx);
    // TLS 1.2 or newer, as the NNTP client will require.
    try std.testing.expectEqual(@as(c_long, 1), ssl.SSL_CTX_set_min_proto_version(ctx, ssl.TLS1_2_VERSION));
}

test "libcurl is linked" {
    const info = curl.curl_version_info(curl.CURLVERSION_NOW);
    try std.testing.expect(info != null);
    try std.testing.expect(info.*.version_num >= 0x075000); // 7.80
}
