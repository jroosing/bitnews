const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    // libbitnews_lt: the C face of libtorrent, built with the system C++
    // compiler (lt/build-lt.sh). Zig only sees its C header.
    const build_lt = b.addSystemCommand(&.{"sh"});
    build_lt.addFileArg(b.path("lt/build-lt.sh"));
    const lt_lib = build_lt.addOutputFileArg(if (target.result.os.tag == .macos) "libbitnews_lt.dylib" else "libbitnews_lt.so");
    build_lt.addFileInput(b.path("lt/bitnews_lt.cpp"));
    build_lt.addFileInput(b.path("lt/bitnews_lt.h"));

    const lt_header = b.addTranslateC(.{
        .root_source_file = b.path("lt/bitnews_lt.h"),
        .target = target,
        .optimize = optimize,
    });

    const sqlite = b.dependency("sqlite", .{});

    const exe_mod = b.createModule(.{
        .root_source_file = b.path("src/main.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = &.{.{ .name = "bnlt", .module = lt_header.createModule() }},
    });
    exe_mod.addCSourceFile(.{
        .file = sqlite.path("sqlite3.c"),
        .flags = &.{ "-DSQLITE_DQS=0", "-DSQLITE_THREADSAFE=1", "-DSQLITE_OMIT_LOAD_EXTENSION", "-DSQLITE_DEFAULT_FOREIGN_KEYS=1" },
    });
    exe_mod.addIncludePath(sqlite.path("."));
    exe_mod.addObjectFile(lt_lib);
    exe_mod.linkSystemLibrary("curl", .{});
    exe_mod.linkSystemLibrary("ssl", .{});
    exe_mod.linkSystemLibrary("crypto", .{});

    const exe = b.addExecutable(.{ .name = "bitnews-spike", .root_module = exe_mod });
    // The wrapper library sits next to the binary's lib dir after install.
    exe.root_module.addRPath(.{ .cwd_relative = if (target.result.os.tag == .macos) "@loader_path/../lib" else "$ORIGIN/../lib" });
    b.installArtifact(exe);
    const install_lt = b.addInstallLibFile(lt_lib, if (target.result.os.tag == .macos) "libbitnews_lt.dylib" else "libbitnews_lt.so");
    b.getInstallStep().dependOn(&install_lt.step);

    const run_cmd = b.addRunArtifact(exe);
    run_cmd.step.dependOn(b.getInstallStep());
    run_cmd.addPassthruArgs();
    b.step("run", "Run the Z0 spike").dependOn(&run_cmd.step);

    // Tests: the std.Io cancellation check and the fuzz-seed check.
    const test_mod = b.createModule(.{
        .root_source_file = b.path("src/io_cancel.zig"),
        .target = target,
        .optimize = optimize,
    });
    const tests = b.addTest(.{ .root_module = test_mod });
    const run_tests = b.addRunArtifact(tests);
    b.step("test", "Run tests").dependOn(&run_tests.step);
}
