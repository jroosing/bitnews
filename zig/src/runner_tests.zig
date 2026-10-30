//! Tests of the runner group: import the files of the group here.
test {
    _ = @import("runner_blob.zig");
    _ = @import("runner_plan.zig");
    _ = @import("runner_limiter.zig");
    _ = @import("runner_pools.zig");
    _ = @import("runner_volumes.zig");
    _ = @import("runner_seams.zig");
    _ = @import("runner_store.zig");
    _ = @import("runner.zig");
    _ = @import("runner_job.zig");
    _ = @import("runner_test.zig");
    _ = @import("runner_resume_test.zig");
}
