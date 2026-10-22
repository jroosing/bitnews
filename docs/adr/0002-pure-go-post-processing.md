# Pure-Go post-processing, no subprocesses

> **Superseded by [ADR-0007](0007-external-postproc-tools.md):** post-processing now runs 7-Zip and par2cmdline-turbo as subprocesses. This record stays for the history.

Post-processing (PAR2 verify and repair, RAR/7z unpack) is implemented entirely in Go and never shells out to `par2cmdline`, `unrar` or `7z`. Those inputs are hostile, since uploaders control the archives and par2 sets. nzbget and SABnzbd shell out to C/C++ tools here, and closed-source `unrar` has had path-traversal CVEs. A memory-safe, fuzzable Go implementation removes the last native attack surface.

## Considered Options

- **Shell out like nzbget/SABnzbd.** This is the most complete and fastest option (SIMD par2), but it keeps a C/C++ attack surface. Rejected.
- **Pure-Go verify with `par2cmdline-turbo` for repair, swapped out later.** Rejected in favour of going pure Go from the start.

## Consequences

- PAR2 repair needs our own Reed-Solomon over GF(2^16). `klauspost/reedsolomon` is GF(2^8) and so not PAR2-compatible.
- Repair is slower than SIMD par2 unless it is optimised with assembly later. This is acceptable because repair only runs on damaged downloads.
- Unpacking depends on `nwaples/rardecode/v2` and `bodgit/sevenzip`. Any RAR/7z feature they lack is unsupported; there is no fallback.
- Every decoder is fuzzed. Unpacking writes to a staging directory, rejects `..`, absolute and symlink paths, and enforces size caps.
