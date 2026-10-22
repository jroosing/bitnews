# Post-processing runs 7-Zip and par2cmdline-turbo

Post-processing is done by two external programs, 7-Zip (`7zz`) and par2cmdline-turbo (`par2`), which bitnews starts as subprocesses. This reverses [ADR-0002](0002-pure-go-post-processing.md) (pure Go, no subprocesses). The owner judged the reference tools to be better and faster than the Go reimplementations, and accepts the native attack surface.

## What changed

- **Unpack:** `7zz` extracts RAR4, RAR5, 7z, zip, multi-volume and encrypted archives. It replaces `nwaples/rardecode` and `bodgit/sevenzip`, the 7z header guard (M3-12) and the whole pure-Go extraction code.
- **PAR2:** `par2` verifies and repairs. It replaces the pure-Go PAR2 parser, the GF(2^16) arithmetic and the repair code. It is multi-threaded with SIMD, and it is the reference implementation, so conformance with other PAR2 clients is no longer our problem.
- **No fallback.** There is one implementation of each, not a pure-Go fallback.
- **Pinned and verified.** `scripts/get-postproc-tools.sh` holds one version and one SHA-256 per platform. The Docker build and CI use it. Both Linux binaries are static (the 7-Zip `7zzs` build), so the image stays on distroless static. The binaries are installed as `/usr/local/bin/7zz` and `/usr/local/bin/par2`.
- **Missing tools are a startup error.** The daemon looks for `7zz` and `par2` on `PATH` at start and refuses to run without them, with a message saying how to install them.

## How the tools are run

- One small runner package starts them: the argument list is built by us (no shell, no string concatenation), the environment is scrubbed (`LC_ALL=C`, a private `TMPDIR`), the working directory is the Download's folder, there is a timeout, the context cancels the process group, and stdout is size-capped.
- Passwords go through stdin, never the command line, so they do not show up in `ps` or `/proc/*/cmdline`.
- Archives are still hostile input, and we keep our own guards around the tool: list the archive first and enforce the entry-count, total-size and ratio caps, extract into the staging directory, then walk the result and refuse symlinks, hardlinks, special files and any path that escapes (`fsutil`). A tool error or exit code is mapped to the same core errors as before (`archive_unsupported`, `archive_password`, `archive_corrupt`, `path_violation`, `disk_full`, `ErrInsufficientRecovery`, `ErrRepairFailed`).
- The container keeps its hardening: a non-root user, a read-only root with a `tmpfs` at `/tmp`, `cap_drop: ALL` and `no-new-privileges`. A bug in 7-Zip or par2 is contained to what that user may write.

## Consequences

- A memory-corruption bug in 7-Zip or par2 now runs C/C++ code on hostile archives. The mitigation is the container, not the language. Rebuild the image when either tool publishes a security fix.
- The 7-Zip RAR decoder carries the unRAR license restriction. It may be used to extract but not to build a RAR compressor, which we do not do.
- The fuzz targets and the conformance suites of the pure-Go code are gone, and the dependencies `rardecode`, `sevenzip` and their transitive libraries leave `go.mod`.
- Developers run `make tools` (the tests do it) to get the pinned binaries into `bin/tools/`, or have `7zz` and `par2` on `PATH`.
