const std = @import("std");

/// A group of modules tested together. Each group has one root file in src/
/// that imports the files of the group, so a developer works in their files
/// and runs `zig build test-<name>` without building the rest.
const groups = [_][]const u8{ "bootstrap", "store", "crypto", "http", "io", "app", "core", "settings", "servers", "categories", "downloads", "events", "remover", "nzb", "yenc", "tls", "nntp", "scheduler", "filewriter", "queue", "add", "runner", "tool", "par2", "unpack", "finalize", "tidy", "postprocess", "sabnzbd", "html", "accounts", "web", "torrent", "qbit" };

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    // SQLite: the pinned amalgamation, compiled by Zig as C (ADR-0014).
    const sqlite = b.dependency("sqlite", .{});
    const sqlite_lib = b.addLibrary(.{
        .name = "sqlite3",
        .linkage = .static,
        .root_module = b.createModule(.{ .target = target, .optimize = optimize, .link_libc = true }),
    });
    sqlite_lib.root_module.addCSourceFile(.{
        .file = sqlite.path("sqlite3.c"),
        .flags = &.{ "-DSQLITE_DQS=0", "-DSQLITE_THREADSAFE=1", "-DSQLITE_OMIT_LOAD_EXTENSION", "-DSQLITE_DEFAULT_FOREIGN_KEYS=1" },
    });
    sqlite_lib.root_module.addIncludePath(sqlite.path("."));
    const sqlite_c = b.addTranslateC(.{
        .root_source_file = sqlite.path("sqlite3.h"),
        .target = target,
        .optimize = optimize,
    });

    // The SQL migrations, shared with the Go version (zig/sync.sh).
    const migrations = b.createModule(.{
        .root_source_file = b.path("migrations/migrations.zig"),
        .target = target,
        .optimize = optimize,
    });

    // Set at build time, as the Go build does with -ldflags -X main.version.
    const build_options = b.addOptions();
    build_options.addOption([]const u8, "version", b.option([]const u8, "version", "version shown by the daemon") orelse "dev");
    build_options.addOption([]const u8, "commit", b.option([]const u8, "commit", "commit shown by the daemon") orelse "unknown");

    // OpenSSL (NNTPS) and libcurl (NZB URLs): the C headers as modules, found
    // through pkg-config like the libraries are. On macOS the Homebrew
    // OpenSSL is keg-only: put its lib/pkgconfig in PKG_CONFIG_PATH (the
    // Makefile does).
    const openssl_c = b.addTranslateC(.{ .root_source_file = b.path("c/openssl.h"), .target = target, .optimize = optimize });
    const curl_c = b.addTranslateC(.{ .root_source_file = b.path("c/curl.h"), .target = target, .optimize = optimize });
    addPkgIncludes(b, openssl_c, "openssl");
    addPkgIncludes(b, curl_c, "libcurl");

    const imports: []const std.Build.Module.Import = &.{
        .{ .name = "build_options", .module = build_options.createModule() },
        .{ .name = "openssl_c", .module = openssl_c.createModule() },
        .{ .name = "curl_c", .module = curl_c.createModule() },
        .{ .name = "sqlite_c", .module = sqlite_c.createModule() },
        .{ .name = "migrations", .module = migrations },
    };

    // The daemon.
    const exe_mod = b.createModule(.{
        .root_source_file = b.path("src/main.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = imports,
    });
    exe_mod.linkLibrary(sqlite_lib);
    linkNative(exe_mod);
    const exe = b.addExecutable(.{ .name = "bitnews", .root_module = exe_mod });
    b.installArtifact(exe);
    const run_cmd = b.addRunArtifact(exe);
    run_cmd.addPassthruArgs();
    b.step("run", "Run the daemon").dependOn(&run_cmd.step);

    // Tests: one step per group, and `test` for all of them.
    const test_all = b.step("test", "Run all tests");
    inline for (groups) |name| {
        const mod = b.createModule(.{
            .root_source_file = b.path("src/" ++ name ++ "_tests.zig"),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .imports = imports,
        });
        mod.linkLibrary(sqlite_lib);
        linkNative(mod);
        // LLVM, not the self-hosted aarch64 backend: that one skips fuzz tests, so
        // their corpora would never run in a plain `zig build test`.
        const t = b.addTest(.{ .root_module = mod, .use_llvm = true });
        const run = b.addRunArtifact(t);
        b.step("test-" ++ name, "Run the " ++ name ++ " tests").dependOn(&run.step);
        test_all.dependOn(&run.step);
    }

    // check: formatting, tests and the shared files, as in `make check`.
    const fmt = b.addFmt(.{ .paths = &.{ b.path("build.zig"), b.path("build.zig.zon"), b.path("src"), b.path("spike") }, .check = true });
    const sync_check = b.addSystemCommand(&.{ "sh", "sync.sh", "--check" });
    sync_check.setCwd(b.path("."));
    const check = b.step("check", "Format check, tests and the shared-files check");
    check.dependOn(&fmt.step);
    check.dependOn(test_all);
    check.dependOn(&sync_check.step);

    // The Z0 spike: libtorrent through the C wrapper, built by the system
    // C++ compiler (lt/build-lt.sh). Not part of the default build.
    const build_lt = b.addSystemCommand(&.{"sh"});
    build_lt.addFileArg(b.path("lt/build-lt.sh"));
    const lt_name = if (target.result.os.tag == .macos) "libbitnews_lt.dylib" else "libbitnews_lt.so";
    const lt_lib = build_lt.addOutputFileArg(lt_name);
    build_lt.addFileInput(b.path("lt/bitnews_lt.cpp"));
    build_lt.addFileInput(b.path("lt/bitnews_lt.h"));
    const lt_header = b.addTranslateC(.{
        .root_source_file = b.path("lt/bitnews_lt.h"),
        .target = target,
        .optimize = optimize,
    });
    const spike_mod = b.createModule(.{
        .root_source_file = b.path("spike/main.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = &.{.{ .name = "bnlt", .module = lt_header.createModule() }},
    });
    spike_mod.linkLibrary(sqlite_lib);
    spike_mod.addIncludePath(sqlite.path("."));
    spike_mod.addObjectFile(lt_lib);
    spike_mod.linkSystemLibrary("curl", .{});
    spike_mod.linkSystemLibrary("ssl", .{});
    spike_mod.linkSystemLibrary("crypto", .{});
    spike_mod.addRPath(.{ .cwd_relative = if (target.result.os.tag == .macos) "@loader_path/../lib" else "$ORIGIN/../lib" });
    const spike = b.addExecutable(.{ .name = "bitnews-spike", .root_module = spike_mod });
    const check_mod = b.createModule(.{
        .root_source_file = b.path("spike/lt_check.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = &.{.{ .name = "bnlt", .module = lt_header.createModule() }},
    });
    check_mod.addObjectFile(lt_lib);
    check_mod.addRPath(.{ .cwd_relative = if (target.result.os.tag == .macos) "@loader_path/../lib" else "$ORIGIN/../lib" });
    const lt_check = b.addExecutable(.{ .name = "bitnews-lt-check", .root_module = check_mod });
    const spike_step = b.step("spike", "Build the Z0 libtorrent spike");
    spike_step.dependOn(&b.addInstallArtifact(spike, .{}).step);
    spike_step.dependOn(&b.addInstallArtifact(lt_check, .{}).step);
    spike_step.dependOn(&b.addInstallLibFile(lt_lib, lt_name).step);
    const run_check = b.addRunArtifact(lt_check);
    run_check.step.dependOn(&b.addInstallArtifact(lt_check, .{}).step);
    b.step("lt-check", "Local libtorrent session: listen, magnet, pause, peer, remove").dependOn(&run_check.step);

    // The daemon loads the same libtorrent session. Tests do not: they never
    // import torrent_session.zig.
    exe_mod.addImport("bnlt", lt_header.createModule());
    exe_mod.addObjectFile(lt_lib);
    exe_mod.addRPath(.{ .cwd_relative = if (target.result.os.tag == .macos) "@loader_path/../lib" else "$ORIGIN/../lib" });
    b.getInstallStep().dependOn(&b.addInstallLibFile(lt_lib, lt_name).step);
}

/// The system libraries the daemon links: OpenSSL for NNTPS and libcurl for
/// NZB URLs (ADR-0014).
fn linkNative(mod: *std.Build.Module) void {
    mod.linkSystemLibrary("ssl", .{});
    mod.linkSystemLibrary("crypto", .{});
    mod.linkSystemLibrary("curl", .{});
}

/// Adds the include directories pkg-config reports for `name` to a
/// translate-c step. Without pkg-config the compiler's defaults are used.
fn addPkgIncludes(b: *std.Build, tc: *std.Build.Step.TranslateC, name: []const u8) void {
    var code: u8 = 0;
    const out = b.runAllowFail(&.{ "pkg-config", "--cflags-only-I", name }, &code, .ignore) catch return;
    var it = std.mem.tokenizeAny(u8, out, " \t\r\n");
    while (it.next()) |flag| {
        if (std.mem.startsWith(u8, flag, "-I")) tc.addIncludePath(.{ .cwd_relative = flag[2..] });
    }
}
