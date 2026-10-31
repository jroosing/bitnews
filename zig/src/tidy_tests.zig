//! Tests of the tidy group: Clean.py's name cleaning, replace_for.py's dots
//! and the naming of obfuscated videos.
test {
    _ = @import("tidy.zig");
    _ = @import("tidy_clean_test.zig");
    _ = @import("tidy_files_test.zig");
}
