# M3 — Post-processing

## Goal

By the end of M3, a Fetched Usenet Download runs through Verify → Repair (when needed) → Unpack → cleanup → move, and becomes **Ready** in `complete/<category>/<download>/`. If that can't be done, it becomes **Failed** with a precise reason. Recovery volumes are fetched only when Verify says they are needed. Everything is pure Go and nothing runs as a subprocess ([ADR-0002](../adr/0002-pure-go-post-processing.md)).

## Scope

**In:**
- the pipeline orchestrator
- PAR2 packet parsing, Verify (including renaming obfuscated files), GF(2^16) Reed-Solomon, Repair, golden fixtures and benchmarks
- deferred recovery volumes
- archive detection and Unpack (RAR/7z, multi-volume, passwords), Unpack guards
- cleanup and the move to complete
- a compatibility spike against real releases

**Out:**
- torrent Unpack wiring (M5-08, which reuses the Unpacker from here)
- searching for misaligned or displaced slices inside damaged files (par2cmdline "scan"). v1 only handles aligned slices plus whole-file renames.

## Design

### Pipeline orchestrator (`internal/core`)

- The ports defined in core are:
  - `Verifier.Verify(dir, files) (VerifyResult, error)`
  - `Repairer.Repair(dir, plan) error`
  - `Unpacker.Detect(dir) []ArchiveSet` and `Unpacker.Unpack(set, dst, opts) error`
  - `FS` (the `fsutil` functions)
- The stages run in a fixed order: `verifying` → `repairing` (only when damaged) → `unpacking` (only when archives are present) → `cleaning` → `moving`. Each stage emits `DownloadUpdated` with a `stage` field.
- **Concurrency:** `postproc_concurrency` sets how many run at once (default 1). Fetching other Downloads continues meanwhile.
- **Failures:** each stage maps its errors to the failure reasons from M1:
  - `repair_insufficient` and `missing_articles`
  - `archive_unsupported`, `archive_password`, `archive_corrupt`
  - `disk_full`, `path_violation`
- **No PAR2 set:** if a Download has none, Verify falls back to the yEnc CRC results from M2. Any damaged or Missing Article then means Failed (`missing_articles`). If there is no damage, Verify passes.
- **Restart:** Post-processing state is not persisted mid-stage. After a restart a `PostProcessing` Download starts again from `verifying`. Every stage must be idempotent; for example, Unpack clears its staging dir first.

### PAR2 packets (`internal/postproc/par2`)

- **Header (64 bytes):**

  | Field | Size |
  |---|---|
  | magic `PAR2\0PKT` | 8 |
  | length (u64 LE, multiple of 4, includes the header) | 8 |
  | packet MD5 (over set ID..end) | 16 |
  | recovery set ID | 16 |
  | type | 16 |

- **Packet types:**
  - Main (`PAR 2.0\0Main\0\0\0\0`): slice size, file count, file IDs.
  - FileDesc: file ID, MD5 of the full file, MD5 of the first 16 KiB, length, name.
  - IFSC: file ID plus MD5 and CRC32 for each slice.
  - RecvSlic: exponent plus slice data.
  - Creator.
  - Unknown types are ignored.
- **Validation:**
  - Check each packet's MD5. Drop duplicates.
  - Keep only packets of the main recovery set ID, which is the MD5 of the Main packet body.
  - Bounds-check every length before allocating.
  - Caps: packet length ≤ 1 GiB, file count ≤ 100 000.
- **Scanning:** read packets from every `.par2` file in the Download dir. Damaged par2 files are fine, because the scanner resyncs on the magic.

### Verify

- **Matching files to FileDesc entries:**
  - First by name.
  - Failing that, by MD5 of the first 16 KiB plus length. This catches obfuscated or renamed posts. A file matched this way is renamed to its FileDesc name (through `SafeName`).
- **Checking each matched file:**
  - If the length is right and the full-file MD5 matches, the file is OK.
  - Otherwise compare each aligned slice's CRC32 and MD5 against IFSC. The last slice is zero-padded up to the slice size.
- **Result:**
  - The damaged or missing slice indices.
  - The count of available recovery slices, split into those already on disk and those deferred (M3-07).
  - The files that will be OK once repaired.

### GF(2^16) and Reed-Solomon

- **The field:** GF(2^16) with generator polynomial `0x1100B` (x^16 + x^12 + x^3 + x + 1). Log and antilog tables have 65 536 entries.
- **Input constants:** input slice *i* has constant `c_i = 2^(n_i)`. Here `n_i` is the *i*-th integer *n* ≥ 0 with `n % 3 != 0`, `n % 5 != 0`, `n % 17 != 0` and `n % 257 != 0`, as in the PAR2 spec.
- **Recovery slices:** the recovery slice with exponent *e* is `Σ_i c_i^e · input_i`, word-wise. Words are 16-bit little-endian.
- **Repair:**
  - Pick *k* recovery slices for the *k* missing inputs.
  - Build the *k×k* matrix of `c_missing^e` and invert it with Gaussian elimination. Fail if it is singular, then retry with a different recovery-slice choice when spares are available.
  - Compute `missing = M⁻¹ · (recovery − Σ_known c_j^e · input_j)`.
- **Performance:**
  - Process slices in chunks of 64 KiB to 1 MiB so memory stays bounded.
  - Precompute split multiply tables (low and high byte, 2×256 entries per coefficient).
  - Run multiply-accumulate across goroutines, one per chunk range.
  - Assembly is out of scope for v1, but there must be a benchmark.

### Repair

- Check feasibility first: the number of available recovery slices must be at least the number of damaged ones. If enough exist only in deferred volumes, M3-07 fetches them first. Otherwise the Download is Failed (`repair_insufficient`).
- Write the reconstructed slices in place, at their offsets in the target files. Missing files are created with their FileDesc length.
- Run Verify again afterwards. Any remaining damage means Failed.
- Implementation notes (M3-05): `Repair(ctx, dir, set, verifyResult, opts...)` returns `*InsufficientError{Needed, Available}` (matches `ErrInsufficientRecovery`) before changing anything, so the orchestrator can fetch deferred volumes (M3-07) or fail with `repair_insufficient`. `Available` counts only readable recovery slices. Memory is about `(k+1) x chunk`, with the chunk halved down to 4 KiB under a configurable memory limit (default 256 MiB). Reed-Solomon correctness is checked against real `par2cmdline` 1.4.0 output (M3-06): recovery slices are byte-identical and Repair restores all 146 damage scenarios.
- Conformance results (M3-06), details in `internal/postproc/par2/testdata/CONFORMANCE.md`:
  - The Main packet orders file IDs as 128-bit little-endian integers (last byte most significant), not by byte comparison. That order is also the input slice order, and `Set.FileIDs` keeps the Main order.
  - par2cmdline skips empty files, so they are not in the set.
  - After appending bytes to a file shorter than one slice, par2cmdline counts its only block as lost while Verify reports 0 damaged slices and Repair truncates it. Both end with the correct file.
  - par2cmdline 1.4.0 rejects `-f` above about 32768, so exponents near 65535 cannot be generated for fixtures.
- Contract with Verify (M3-03):
  - A file with intact slices but the wrong length (for example trailing bytes) comes back `Damaged` with an empty slice list and needs 0 recovery slices. Repair must truncate it.
  - A missing zero-length file comes back `Missing` with 0 damaged slices. Repair must create it.
  - For `Missing` and `Unverifiable` files the damaged list is not built (`DamagedSlices` is nil and `DamagedCount` equals `TotalSlices`), so Repair iterates `0..TotalSlices`.
  - A wrong file sitting under the right name is reported `Damaged`. Searching for misaligned slices is out of scope for v1.

- Implementation notes (M3-01, `internal/core/postprocess*.go`): the orchestrator publishes a `postprocess_stage` event for each stage that runs. When the working directory is missing it goes straight to `Finisher.Finish`, because a crash between the move and the Ready update must not turn a finished Download into a failed one. `FetchReporter` is a required port; torrent-only setups pass a stub.
- Implementation notes (M3-10, `internal/postproc/finalize`): the destination is resolved at move time. Cleanup deletes archive volumes, `.par2`, `.nzb` and `_unpack.tmp` before the move, merges the unpacked content into the root of the final directory, and writes a `.bitnews-done` marker holding the Download ID so a retry after a crash finds the moved directory. A deleted Category falls back to the complete root with a warning. Torrent originals are never modified.

- Implementation notes (adapters, package `internal/postproc/adapt`): `Verifier`, `Repairer` and `Unpacker` implement the core ports on the real PAR2 and Unpack packages. A missing archive volume is `archive_corrupt`, and bomb, too-large and too-many-entries limits map to `archive_corrupt` because core has no "too big" reason. Several archive sets are unpacked separately and merged, with `fsutil.Unique` names on collisions. `PlanVolumes` is exact (dynamic programming over blocks, cheapest by bytes, then fewest volumes) with a greedy fallback for hostile inputs; it returns `ok=false` when all volumes together cannot cover the need. Still to wire in the composition root: the real `DeferredController`, the `Requeue` (PostProcessing to Fetching) and the supplier of deferred blocks for `Verifier.DeferredBlocks`.

### Deferred recovery volumes

- In M2, `.volNN+MM.par2` files are added to the Download but marked `deferred`. The Article scheduler skips them, and the small index `.par2` is fetched normally.
- After Verify, if repair is needed:
  - Choose the smallest set of deferred volumes whose recovery-slice count (from `+MM` in the name, confirmed after fetch) covers the damage plus a margin of 1.
  - Un-defer them, return the Download to `Fetching` (so the scheduler fetches only those), then re-enter `verifying`.
- The Download's `TotalBytes` excludes deferred volumes, so progress makes sense in media managers.

### Unpack (`internal/postproc/unpack`)

- **Detecting archive sets:**
  - RAR5/RAR3 new-style: `name.partNN.rar`.
  - Old-style: `name.rar` with `.r00`, `.r01`, ….
  - 7z: `name.7z`, or `name.7z.001` and onwards.
  - Detection is by magic bytes, not by extension alone.
- **Libraries:**
  - `github.com/nwaples/rardecode/v2` for RAR, including multi-volume and passwords. Check its exact API and options when implementing.
  - `github.com/bodgit/sevenzip` for 7z.
- **Passwords:** taken from the Download's encrypted password. A wrong password or missing password → `archive_password`.
- **Nested archives:** one level is allowed, for example a RAR containing a RAR set. Anything deeper is left as-is.
- **Output:** written to the staging dir `incomplete/<download>/_unpack.tmp/`. On success it is renamed to the final layout, `<download>/` with the archive contents at its root.

- Implementation notes (M3-08, M3-09, package `internal/postproc/unpack`):
  - `Unpack(ctx, set, dst, opts...)` stages in `dst + ".tmp"` and renames to `dst`. The orchestrator passes `<download dir>/_unpack` as `dst` for both protocols, and the Usenet finisher merges its content into the final directory.
  - Pass 1 applies every guard to every entry before any byte is written; pass 2 writes. Symlinks, hardlinks, devices and FIFOs are skipped and counted, archive modes are ignored, and files are created `0666&^umask`.
  - Every library call is wrapped so that a panic becomes `ErrCorrupt` (the fuzzer found one in `sevenzip`).
  - A 7z checksum mismatch maps to `ErrPassword` because 7z has no password check, so a corrupt unencrypted 7z can read as a password failure.
  - Real archives covered by tests: the 7z fixtures of `sevenzip` and one 87-byte stored RAR4. All other RAR cases are built by test helpers with the stored method only. Compressed RAR (LZ, PPM, filters, solid), header-encrypted RAR, real multi-volume releases and 7z BCJ2/ARM/brotli are not exercised and belong to the M3-11 compatibility spike.

### Unpack guards

These rules apply to every archive entry before anything is written.
- Normalize the name with `/` separators and pass each segment through `SafeName`. Reject absolute paths, `..`, and drive letters. Then use `SafeJoin` into the staging dir.
- Skip symlink, hardlink, device and FIFO entries and log them. Only regular files and dirs are extracted.
- **Size caps:**
  - The declared total uncompressed size must fit into free space minus `min_free`.
  - Writing stops at the declared size + 1% (catches lying headers).
  - Per archive, a compression ratio above 100:1 at more than 1 GiB output is aborted as a bomb.
- When a guard trips, the Download fails with `path_violation` or `archive_corrupt`, and nothing outside staging is touched.

### Cleanup and move

- **Usenet cleanup** after a successful Unpack deletes:
  - the archive volumes
  - the `.par2` files
  - the `.nzb`
  - `_unpack.tmp` leftovers
- **Without archives,** cleanup deletes only the `.par2` and `.nzb` files.
- **Sample** files and `.nfo` files are kept, because media managers ignore them.
- **Move:**
  - The destination is the Category `DirOverride`, or `<usenet complete root>/<category>/`, or `<usenet complete root>/` without a Category. It is resolved when the move happens, so later Category changes apply.
  - Use `fsutil.MoveDir` with `Unique` for collisions.
  - A cross-device move emits a `Warning` event and logs a warning saying hardlinks will not work for this layout.
- Set `OutputPath`, move to **Ready**, and emit `Ready`.

## Exit criteria

- Golden fixtures pass:
  - an intact set
  - damage within recovery capacity, which is repaired byte-exactly
  - damage beyond capacity → Failed
  - obfuscated names, which are renamed
  - RAR3 old-style, RAR5 multi-volume, encrypted RAR, 7z multi-volume
  - a path-traversal archive → Failed
- Recovery volumes are fetched only when needed. A test shows a clean download fetches 0 volume bytes.
- The fuzz targets (PAR2 packets, archive entry names) run in CI.
- The compatibility spike (M3-11) is documented.

## Issues

### M3-01: Post-processing pipeline orchestrator
- **Labels:** area:core, area:postproc, ready-for-agent
- **Depends on:** M2-11

**Goal:** Implement the stage pipeline in core using ports, following the "Pipeline orchestrator" design.

**Work**
- Define the `Verifier`, `Repairer`, `Unpacker` and `FS` ports. Use stub implementations until M3-03, M3-05 and M3-08 land.
- Run the fixed stage order with conditional stages and `stage` events.
- Enforce the `postproc_concurrency` limit, and keep it independent of fetching.
- Map errors to failure reasons.
- Handle a Download with no PAR2 set using the M2 CRC results.
- On restart, re-enter at `verifying` with idempotent stages.

**Acceptance criteria**
- [ ] Orchestrator tests with fake ports cover every path: clean, repair, unpack, each failure reason, and restart in the middle of a stage.
- [ ] Fetched → PostProcessing → Ready/Failed transitions go through core `Transition` only.

### M3-02: PAR2 packet parser
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M1-01

**Goal:** Parse PAR2 packets safely, following the "PAR2 packets" design.

**Work**
- Parse the header, check MD5, resync on the magic after corruption, and drop duplicates.
- Main, FileDesc, IFSC, RecvSlic and Creator packets. Unknown types are ignored.
- Filter by recovery set ID, and bounds-check all lengths and counts before allocating.
- Expose a `Set` model that merges every `.par2` file in a directory.

**Acceptance criteria**
- [ ] Parses fixtures created with par2cmdline (index plus volumes).
- [ ] Truncated or corrupt files yield the packets that are still valid.
- [ ] A fuzz target runs in CI with bounded memory.

### M3-03: PAR2 Verify including obfuscated-file renaming
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-02, M2-01

**Goal:** Compute damage per the "Verify" design.

**Work**
- Match files by name, then by MD5 of the first 16 KiB plus length, and rename matches via `SafeName`.
- Use a whole-file MD5 fast path; otherwise verify each aligned slice with CRC32 and MD5, padding the last slice.
- Return the damaged slice indices and the recovery slice counts, split into on-disk and deferred.

**Acceptance criteria**
- [ ] Tests cover an intact set, a single flipped byte, a truncated file, a missing file, and an obfuscated file name, which gets renamed.
- [ ] Verify never writes anything except the rename.

### M3-04: GF(2^16) arithmetic and Reed-Solomon solver
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M1-01

**Goal:** A pure-Go GF(2^16) field and the PAR2 Reed-Solomon math from the design.

**Work**
- Log and antilog tables for `0x1100B`, with mul, div, pow and inverse.
- Generate the input constants `c_i` with the exclusion rules from the spec.
- Matrix inversion by Gaussian elimination, detecting singular matrices.
- A chunked multiply-accumulate kernel with split tables, parallelized across goroutines.

**Acceptance criteria**
- [ ] Field axioms hold under property tests.
- [ ] Recovery slices computed by our encoder (test-only) for small inputs match the ones par2cmdline produces byte for byte.
- [ ] Benchmarks are included, and their results are recorded in the package doc.

### M3-05: PAR2 Repair
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-03, M3-04

**Goal:** Reconstruct damaged slices in place, then verify again.

**Work**
- Check feasibility, signalling the deferred-volume need to the orchestrator (M3-07).
- Choose recovery slices, falling back to other choices when the matrix is singular.
- Compute the missing slices in bounded-memory chunks and write them at their offsets. Create missing files at their FileDesc length.
- Re-verify; any remaining damage means Failed.

**Acceptance criteria**
- [ ] Golden fixture tests: damage at 1, 50 and 100% of capacity is repaired byte-exactly, and damage beyond capacity yields `repair_insufficient`.
- [ ] Peak memory is bounded and independent of file size (test with 2 GiB sparse files, or document it in the benchmark).

### M3-06: PAR2 golden fixtures and benchmarks
- **Labels:** area:postproc, area:ci, ready-for-human
- **Depends on:** M1-01

**Goal:** Create the committed fixture corpus that M3-02 through M3-05 test against, using par2cmdline offline and never at runtime.

**Work**
- Write a script `scripts/gen-par2-fixtures.sh`, documented and run manually, that generates sets: different slice sizes, multiple files, obfuscated names, and varied volume layouts.
- Write damage scenarios as small deterministic patch files applied in tests (byte flips, truncation, deletion).
- Keep the fixtures small (under 5 MiB in total) and put a large-file benchmark behind a build tag.

**Acceptance criteria**
- [ ] Fixtures and the script are committed.
- [ ] `testdata/README.md` explains how to regenerate them and which par2cmdline version produced them.

### M3-07: Deferred recovery volumes
- **Labels:** area:usenet, area:postproc, ready-for-agent
- **Depends on:** M3-01, M3-03, M2-09

**Goal:** Fetch `.volNN+MM.par2` files only when Verify needs them, as designed.

**Work**
- Mark volume files `deferred` when the Download is added, have the Article scheduler skip them, and exclude them from `TotalBytes`.
- After Verify, pick the minimal covering set plus a margin of 1, un-defer it, go back to Fetching, then return to verifying.
- If all volumes together still can't cover the damage, fail with `repair_insufficient` without fetching them.

**Acceptance criteria**
- [ ] An integration test with the fake NNTP server shows a clean download fetches no volume bytes.
- [ ] With 3% damage, only the volumes needed are fetched and the Download becomes Ready.
- [ ] Damage beyond the total capacity fails without fetching.

### M3-08: Archive detection and Unpack (RAR and 7z)
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-01, M2-01

**Goal:** Implement the Unpacker port with pure-Go libraries, per the "Unpack" design.

**Work**
- Detect archive sets by magic bytes (RAR new-style and old-style, 7z single and multi-volume).
- Unpack with `nwaples/rardecode/v2` and `bodgit/sevenzip`, including multi-volume sets and passwords.
- Allow one level of nested archives.
- Output into `_unpack.tmp/`, then rename into place. Clear staging at the start so the stage is idempotent.
- Map errors to `archive_unsupported`, `archive_password` and `archive_corrupt`.

**Acceptance criteria**
- [ ] Fixture tests cover RAR3 old-style, RAR5 multi-volume, encrypted RAR (right and wrong password), 7z multi-volume, and nested RAR.
- [ ] No subprocess is spawned. A test asserts that `os/exec` is not imported in `internal/`.

### M3-09: Unpack guards — path traversal, entry types and size caps
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-08

**Goal:** Enforce the "Unpack guards" design on every entry.

**Work**
- Normalize names and apply `SafeName` per segment, then `SafeJoin` into staging. Reject absolute paths, `..` and drive letters.
- Skip and log symlink, hardlink, device and FIFO entries.
- Size caps: declared size against free space, writing stopped at declared size + 1%, and the ratio cap for bombs.
- Make sure nothing is written outside staging, even on partial failure.

**Acceptance criteria**
- [ ] Malicious fixture archives (traversal, absolute path, symlink to `/etc`, a lying size header, a high-ratio bomb) all fail safely.
- [ ] A fuzz target over entry names feeds the guard functions and checks that the result never escapes staging.

### M3-10: Cleanup and move to complete
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-01, M2-01

**Goal:** Finish Usenet Post-processing per the "Cleanup and move" design.

**Work**
- Apply the cleanup rules with and without archives, keeping samples and `.nfo` files.
- Resolve the destination when the move happens (Category override, then the complete root with the Category, then the root), and handle collisions with `Unique`.
- Use `MoveDir`, emitting a warning event on a cross-device move.
- Set `OutputPath`, then Ready, and emit the `Ready` event.

**Acceptance criteria**
- [ ] Tests cover each destination rule, a collision, a cross-device warning (tmpfs in Linux CI), and that cleanup never deletes files outside the Download dir.

### M3-11: Compatibility spike — pure-Go unpack against real releases
- **Labels:** area:postproc, ready-for-human
- **Depends on:** M3-08

**Goal:** Check ADR-0002's main risk: whether `rardecode` and `sevenzip` handle what real Usenet and scene releases use.

**Work**
- Gather a private corpus of archive variants: RAR4/RAR5, solid archives, recovery records, multi-volume old and new style, encrypted headers, unicode names, large dictionaries (RAR5 up to 4 GiB+), and 7z with BCJ2 or LZMA2.
- Run the Unpacker against each one and record pass/fail, speed, and any missing features.
- Upstream issues or patches where there are gaps.

**Acceptance criteria**
- [ ] `docs/compat/unpack.md` lists each variant with its result and the library version used.
- [ ] Gaps get follow-up issues, or are explicitly accepted as unsupported (a release that fails shows up as `archive_unsupported` and the media manager picks another).

### M3-12: 7z header memory guard
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-08

**Goal:** Prevent a hostile 7z archive from making the daemon allocate gigabytes before any Unpack guard runs.

**Background:** `github.com/bodgit/sevenzip` v1.6.5 allocates from counts declared in the archive header (up to about 4 GiB in its `checkUint64`) before `internal/postproc/unpack` sees a single entry. A 45 s fuzz run found only a nil-pointer panic (already converted to `ErrCorrupt` and stored as a regression seed), but the memory cap is not enforced. An OOM kill would take the whole daemon down, and archives come from uploaders we do not trust.

**Work**
- Reproduce with a crafted header that declares a huge file or folder count and measure the allocation.
- Add a cheap guard before calling the library, for example a hard cap on the 7z volume size we are willing to open and a sanity check of the signature header (`NextHeaderOffset` and `NextHeaderSize` against the file size and a small maximum).
- If the library cannot be guarded from outside, send a fix upstream, or vendor a minimal patched reader with the bounds checks.
- Add the crafted archives as regression fixtures and a test that opening them fails with `ErrCorrupt` or `ErrTooLarge` within a fixed heap budget.

**Acceptance criteria**
- [ ] A crafted 7z with an absurd declared count fails fast and the process heap grows by less than 64 MiB (test with `runtime.ReadMemStats`).
- [ ] `FuzzUnpack` runs 60 s with no out-of-memory and no panic.
- [ ] The behaviour is documented in `internal/postproc/unpack` and the M3 doc.
