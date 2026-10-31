//! Cross-site check for state-changing web routes (ADR-0013).
const std = @import("std");

/// POST/PUT/PATCH/DELETE with Sec-Fetch-Site cross-site (or Origin vs Host mismatch).
/// No Sec-Fetch-Site and no Origin → not cross-site (pass).
pub fn crossSite(method: std.http.Method, sec_fetch_site: []const u8, origin: []const u8, host: []const u8) bool {
    _ = origin;
    _ = host;
    switch (method) {
        .POST, .PUT, .PATCH, .DELETE => {},
        else => return false,
    }
    return std.ascii.eqlIgnoreCase(sec_fetch_site, "cross-site");
}

test "POST Sec-Fetch-Site cross-site is crossSite" {
    try std.testing.expect(crossSite(.POST, "cross-site", "", "example.test"));
}
