//! Tests of the par2 group: import the files of the group here. The name
//! guard comes first: `--fuzz` only runs the first fuzz test of a group (the
//! output parser's fuzz test is in par2_output.zig).
test {
    _ = @import("par2_names_test.zig");
    _ = @import("par2.zig");
    _ = @import("par2_output.zig");
    _ = @import("par2_test.zig");
    _ = @import("par2_real_test.zig");
}
