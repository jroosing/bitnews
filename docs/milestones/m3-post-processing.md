# M3 — Post-processing

## Goal

By the end of M3, a Fetched Usenet Download runs through Verify → Repair (when needed) → Unpack → cleanup → move, and becomes **Ready** in `complete/<category>/<download>/`. If that can't be done, it becomes **Failed** with a precise reason. Recovery volumes are fetched only when Verify says they are needed. PAR2 and archives are handled by the external programs `par2` and `7zz` ([ADR-0007](../adr/0007-external-postproc-tools.md), which supersedes the pure-Go [ADR-0002](../adr/0002-pure-go-post-processing.md)).

## Scope

**In:**
- the pipeline orchestrator
- PAR2 Verify (including renaming obfuscated files) and Repair by running `par2` (ADR-0007; the pure-Go parser, GF(2^16) code and golden fixtures of the first version are gone), and the hostile-name check before it runs
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

### PAR2 with par2cmdline-turbo (`internal/postproc/par2`)

> Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md). The pure-Go packet parser, Verify, GF(2^16) arithmetic and Reed-Solomon Repair of M3-02 to M3-06 were replaced by running `par2` (par2cmdline-turbo 1.5.0). There is no fallback. The ports (`core.Verifier`, `core.Repairer`) and their outcome fields did not change, so the orchestrator did not either. The package doc (`internal/postproc/par2/doc.go`) is the reference for the details below.

- **What runs:** `par2 v|r -B<dir> -t<threads> -m<MB> [-O] -- <main.par2> @`, started only through `internal/postproc/tool` (no shell, scrubbed environment, `LC_ALL=C`, the context kills the process group). `-B` is the absolute Download directory (par2 ignores files outside it). `-t` is GOMAXPROCS, at most 16. `-m` is 512 MB by default, because par2's own default is half of the physical RAM and a container limit is invisible to it (it works down to `-m1`). Never used: `-p` (purges the `.par2` files), `-N`/`-S`, `-q`.
- **Files given to par2:** the smallest `.par2` file is the main file. Every other `.par2` file and every regular top-level file that is not already a target name go in as "extra files" through stdin (`@`), so the argument list cannot overflow. par2 itself only loads sibling volumes that share the main file's prefix; extras are how volumes with other names are found. Symlinks and directories are never given to it.
- **Verify** (`par2 v`):
  - exit 0: `Clean`. Exit 1: `NeedsRepair`, `Repairable`. Exit 2: `NeedsRepair`, not `Repairable`. Exit 4 ("Main packet not found"): no usable set, `HasPAR2` false. No `.par2` file at all: `HasPAR2` false without starting par2. Anything else is an error carrying the tail of the output.
  - `DamagedSlices` = Y - X from "You have X out of Y data blocks available." (what a repair consumes). `RecoveryOnDisk` = Z from "You have Z recovery blocks available." (the line is absent when there are none; a clean verify does not print it, so the "Loaded N new packets including M recovery blocks" lines are summed then). The adapter ORs in the deferred blocks (`Repairable` also when `DamagedSlices <= RecoveryOnDisk + deferred`).
  - **Renamed (obfuscated) files:** par2 finds a file under another name by hash, but only if the file is given as an extra file. `par2 v` only reports `File: "x" - is a match for "a.bin".` and never renames; `par2 r` renames, and `par2 r -O` (rename-only) does only that. So Verify, on seeing a match line, runs `r -O`, removes the backups it made and verifies again: a clean-but-renamed set ends `Clean` with the set's names, and a second Verify finds nothing left. A file that is renamed and also damaged is not a perfect match, so `-O` skips it: it counts as a data source (its good blocks are available) and Repair renames and repairs it.
  - A file with intact blocks but trailing bytes is `NeedsRepair` with 0 damaged slices (par2 exit 1, "Repair is possible"); Repair truncates it.
- **Repair** (`par2 r`): par2 verifies again first, so the Verify outcome is not needed (its `Opaque` payload is only diagnostic). Exit 0 is success (`RepairedSlices` = "N recovery blocks will be used to repair."; 0 when nothing was left to do). Exit 2 is `*core.RecoveryShortfall{Needed: Y-X, Available: Z}` and par2 changes nothing. Exit 5 is `core.ErrRepairFailed`. Exit 6, 7 and 8 (file I/O, logic, memory) are internal errors with the output tail. A cancelled context kills par2 and returns the context error.
- **Backups:** when par2 replaces a damaged target it renames the old file to `name.1` (`name.2`, ...), in `r -O` as well as in a repair. Cleanup only deletes `.par2`, `.nzb` and archives, so these would be moved into the library. Verify (after a rename pass, and whenever the set is clean) and Repair (after success) delete the regular files `<target>.<digits>` next to a target unless that name is a target itself. After a successful Repair the damaged copies par2 reported as `File: "x" - found 9 of 10 data blocks from "a.bin".` are deleted too, but only when `x` is exactly a file we gave it as an extra. An interrupted repair leaves its `name.N` on purpose: the next Verify sees it as an extra file and uses it as a data source, so Repair can run again (tested by recreating that state).
- **Other layouts:** a missing or damaged main `.par2` is fine as long as the volumes carry the critical packets (they do). A truncated volume counts the recovery blocks that survive. Extra files (nfo, sample) are scanned but left alone. Targets in subdirectories (`sub/c.bin`) work, and a missing subdirectory is recreated by Repair (not by `r -O`).
- **Hostile input:** file names in a PAR2 set are attacker controlled and par2 writes files under them. par2cmdline-turbo 1.5.0 neutralises some of them itself, found by experiment: a `..` component and a leading `/` are percent-encoded (`%2E%2E`, `%2F`), `\` becomes `/`, a name stops at a NUL, a line break becomes `%0A`. A drive letter (`C:evil.bin`), a quote and a colon pass through. Other versions may differ, so we do not rely on it: before par2 starts, `par2.ReadNames` reads the File Description packets of every `.par2` file and `par2.CheckNames` refuses the whole set with `par2.ErrUnsafeSet` (mapped to `core.ErrPathViolation`, failure reason `path_violation`) when a name is empty, absolute, has a drive letter, a backslash, NUL or any control character (also so that no name can fake a line of the report we parse), has a `..`, `.` or empty component, is longer than 1024 bytes (255 per component), ends in `.par2`, resolves outside the Download directory (`fsutil.SafeJoin`, which also catches a symlinked parent), or when a target already exists as a symlink or anything but a regular file. `ReadNames` is deliberately more permissive than par2: it looks for the packet magic at every byte offset (inside recovery data too), takes every File Description whose MD5 verifies, and bounds its memory and its work on damaged candidates (a candidate it cannot bound refuses the file). `FuzzReadNames` covers it. Tests run a hostile set through the real par2 and check that nothing is created or moved outside the Download directory.
- **Known gap:** par2 writes the name the set gives, so a name with `:`, `?`, `*`, `"`, `<`, `>` or `|` ends up on disk as written, while the fetch side saves downloaded files through `fsutil.SafeName` (those become `_`). A set naming such a file renames the downloaded copy to the set's name. Fine on Linux; not SMB friendly. Not handled in v1.
- **Tests:** the package tests make real sets with `par2 c` (1000 byte blocks, a few KB of data, so the package runs in about 2 s under `-race`) and damage, delete, truncate and rename files, drop volumes, and compare repaired files byte for byte. They need the pinned binaries (`make tools`, then `bin/tools` first on `PATH`; `make test` does it).

### Repair and the orchestrator

- The orchestrator checks feasibility first: if `Repairable` is false it asks the fetch side for deferred volumes (M3-07) or fails with `repair_insufficient`. Repair runs only on a directory Verify found repairable. Any remaining damage after a repair is `repair_insufficient` (par2 exit 5).

- Implementation notes (M3-01, `internal/core/postprocess*.go`): the orchestrator publishes a `postprocess_stage` event for each stage that runs. When the working directory is missing it goes straight to `Finisher.Finish`, because a crash between the move and the Ready update must not turn a finished Download into a failed one. `FetchReporter` is a required port; torrent-only setups pass a stub.
- Implementation notes (M3-10, `internal/postproc/finalize`): the destination is resolved at move time. Cleanup deletes archive volumes, `.par2`, `.nzb` and `_unpack.tmp` before the move, merges the unpacked content into the root of the final directory, and writes a `.bitnews-done` marker holding the Download ID so a retry after a crash finds the moved directory. A deleted Category falls back to the complete root with a warning. Torrent originals are never modified.

- Implementation notes (since M8-08, `par2.Verifier`, `par2.Repairer` and `unpack.Unpacker` implement the core ports in their own packages, and the Usenet runner implements `core.DeferredVolumes`; the package `internal/postproc/adapt` is gone): the steps implement the core ports on the real PAR2 and Unpack packages. A missing archive volume is `archive_corrupt`, and bomb, too-large and too-many-entries limits map to `archive_corrupt` because core has no "too big" reason. Several archive sets are unpacked separately and merged, with `fsutil.Unique` names on collisions. `PlanVolumes` is exact (dynamic programming over blocks, cheapest by bytes, then fewest volumes) with a greedy fallback for hostile inputs; it returns `ok=false` when all volumes together cannot cover the need. The composition root (`internal/daemon`) wires them: the runner is the `DeferredController` and the supplier of `Verifier.DeferredBlocks`, and the `Requeue` moves the Download from PostProcessing to Fetching and publishes `DownloadUpdated`.
- Implementation notes (M3-07 fetch side, `internal/usenet/runner/volumes.go`):
  - `ListDeferred` returns the `.volNN+MM.par2` files of the stored NZB that are not fetched yet (in NZB order; `Undefer` indexes refer to this list), `Undefer` records the chosen ones in `usenet_undeferred` and is refused unless the Download is PostProcessing, and `DeferredBlocks` sums `MM` of the volumes not fetched yet.
  - A Download with `usenet_undeferred` rows was fetched before, so its next fetch is a supplement run: it plans only the un-deferred volumes that are not fetched yet, opens its writer without touching the earlier files, keeps its own resume blob, publishes no progress and leaves the stored FetchReport alone. When done it marks the volumes fetched (before the state change, so a crash in between completes harmlessly) and the Download is Fetched again. The orchestrator then starts over at `verifying` and sees the extra recovery slices. A volume counts as deferred until it is fetched, so repeated rounds fetch each volume once.
  - `TotalBytes` and `DoneBytes` are not extended by the volumes: they describe the main fetch.

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
- **Tool:** `7zz` (7-Zip), run through `internal/postproc/tool` (ADR-0007). It reads RAR4, RAR5, 7z and zip, multi-volume sets and passwords. *Superseded: the first version used `nwaples/rardecode/v2` and `bodgit/sevenzip` in pure Go (ADR-0002); they are gone, and there is no fallback.*
- **Passwords:** taken from the Download's encrypted password. A wrong password or missing password → `archive_password`.
- **Nested archives:** one level is allowed, for example a RAR containing a RAR set. Anything deeper is left as-is.
- **Output:** written to the staging dir `incomplete/<download>/_unpack.tmp/`. On success it is renamed to the final layout, `<download>/` with the archive contents at its root.

- Implementation notes (M3-08, M3-09, package `internal/postproc/unpack`; rewritten for ADR-0007):
  - `Unpack(ctx, set, dst, opts...)` stages in `dst + ".tmp"` and renames to `dst`. The orchestrator passes `<download dir>/_unpack` as `dst` for both protocols, and the Usenet finisher merges its content into the final directory. `Detect` (magic bytes, volume order, missing volumes) and the nested-archive step did not change; only the extraction did.
  - Pass 1: `7zz l -slt -ba` lists the archive (first volume only; 7zz finds the rest) and the listing is parsed strictly (`listing.go`): a repeated field, a line that is not `Key = Value` or a second `Path` line inside one entry fails with `ErrCorrupt`, because a name or comment with a line break could otherwise forge the size or the encrypted flag. The guards then run on the whole listing before a byte is written (entry count, names, entry kinds, total declared size, ratio, free disk space).
  - Pass 2: `7zz x -y -bd -aos -o<staging> -- <first volume>` with the staging directory as working directory. Then the staging tree is walked: a symlink, hardlink, device, FIFO or socket, a name that `SafeName` would change (renamed in place; two names that collide once case is ignored fail) or more files or bytes than the listing declared fail the Unpack. Files are set to `0644` and directories to `0755`, so an archive cannot plant setuid or executable files or directories the daemon cannot delete.
  - **Passwords** go to `7zz` on stdin, never on the command line. 7zz prompts on stdin only when it meets encrypted data; without a password stdin is empty and 7zz gives up at the prompt (status 255, "Break signaled"). A password with a line break fails with `ErrPassword`.
  - **Exit status and messages** map as follows. Status 0 is success. Text containing "Wrong password" or "Cannot open encrypted archive" (or status 255 after "Enter password") is `ErrPassword`; "No space left" or "not enough space" is `ErrDiskFull`; status 8 or "Can't allocate required memory" is `ErrTooLarge`; "Unsupported Method" or "Unsupported Feature" is `ErrUnsupported`; status 1 (warning) and status 2 (fatal: headers error, data error, CRC failed, unexpected end of archive, not an archive) are `ErrCorrupt`; status 7 (a bug in our command line) and 255 are plain errors. Cancelling the context kills the 7zz process group and removes the staging directory.
  - The listing of an archive may be at most `entries * 600` bytes (at least 4 MiB, at most 512 MiB); a longer one is cut off by the runner and fails with `ErrTooManyEntries`.
  - **No dictionary or header-memory options.** The 7z header memory guard (M3-12) and `WithMaxDictionary`/`WithMaxHeaderMemory` are gone. A hostile archive that makes 7zz allocate gigabytes is contained by the container's memory limit, not by us; `7zz` exits with status 8 or 2 when an allocation fails, and that is `ErrTooLarge`/`ErrCorrupt`.
  - Real archives covered by tests: the 7z fixtures of `sevenzip` (copied from that project's test data, including four encrypted variants and a six-volume set), one 87-byte stored RAR4, archives built by 7zz in the tests (7z, zip, encrypted, split volumes, symlinks), zips written with `archive/zip` (hostile names, symlinks, a header that lies about the size) and stored RAR4/RAR5 built by test helpers (multi-volume old and new style, encrypted, links, special files, hostile names). RAR cannot be created without the RAR program, so compressed RAR (LZ, PPM, filters) and header-encrypted RAR are not covered by our tests; 7-Zip's own decoder handles them.

### Unpack guards

These rules apply to every archive entry before anything is written.
- Normalize the name with `/` separators and pass each segment through `SafeName`. Reject absolute paths, `..`, and drive letters. Then use `SafeJoin` into the staging dir.
- Refuse symlink, hardlink, device, FIFO and socket entries (`path_violation`), in the listing and again when walking the extracted tree. Only regular files and dirs are extracted. *Changed by ADR-0007: the pure-Go version skipped and logged them.*
- **Size caps:**
  - The declared total uncompressed size must fit into free space minus `min_free`.
  - 7zz writes at most the size the header declares (a lying header is a `Data Error`), and the walk after extraction fails when more files or bytes appeared than the listing declared.
  - Per archive, a compression ratio above 100:1 at more than 1 GiB declared output is refused as a bomb, before extraction.
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
  - A cross-device move emits a `Warning` event and logs a warning saying hardlinks will not work for this layout. *Changed by M7-01 / ADR-0006:* the warning now says the finished download was copied instead of renamed because the folders are on different filesystems (slower), and that Sonarr/Radarr can only hardlink if the library shares the complete folder's filesystem.
- Set `OutputPath`, move to **Ready**, and emit `Ready`.

## Exit criteria

- Tests with real `par2` sets (made by `par2 c` in the tests) pass:
  - an intact set
  - damage within recovery capacity, which is repaired byte-exactly
  - damage beyond capacity → Failed
  - obfuscated names, which are renamed
  - a hostile file name in the set → Failed (`path_violation`) before par2 starts
  - RAR3 old-style, RAR5 multi-volume, encrypted RAR, 7z multi-volume
  - a path-traversal archive → Failed
- Recovery volumes are fetched only when needed. A test shows a clean download fetches 0 volume bytes.
- The fuzz targets (PAR2 file names, archive entry names) run in CI.
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

> **Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md).** `par2` reads the packets now. All that is left of the parser is `par2.ReadNames`, a minimal File Description reader used to refuse hostile file names before par2 starts (with `FuzzReadNames`); see "PAR2 with par2cmdline-turbo". The merged `Set` model, resync and set selection are gone.
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

> **Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md).** `Verifier` runs `par2 v`; obfuscated files are found by par2 (by hash, through the extra-file list), renamed with `par2 r -O`, and the `name.1` backups removed. Renaming no longer goes through `SafeName`: names are checked, not rewritten (see the known gap in "PAR2 with par2cmdline-turbo"). Verify writes nothing except those renames and the removal of leftover backups.
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

> **Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md).** No GF(2^16) code, no benchmarks: par2cmdline-turbo does the Reed-Solomon work (multi-threaded, SIMD). `internal/postproc/par2/gf16` is deleted.
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

> **Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md).** `Repairer` runs `par2 r`; exit 2 becomes the `RecoveryShortfall`, exit 5 `ErrRepairFailed`. Memory is bounded by `-m` (512 MB by default), not by our chunking. A repair that is cancelled leaves a consistent state to run again (see "PAR2 with par2cmdline-turbo").
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

> **Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md).** The golden fixtures, their generator script, the conformance suite and the benchmarks are deleted: par2 is the reference implementation. One fixture set (`internal/postproc/par2/testdata/golden/explicit_uniform`, made by par2cmdline 1.4.0) stays because the daemon end-to-end tests serve it from the fake NNTP server. The package tests build their sets with `par2 c` instead.
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

> Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md): the Unpacker runs `7zz` instead of the pure-Go libraries named below. The port, detection, error mapping and nested-archive behaviour did not change. The acceptance criterion about `os/exec` now reads: only `internal/postproc/tool` imports it (`TestOnlyTheToolRunnerImportsOsExec`).

**Goal:** Implement the Unpacker port with pure-Go libraries, per the "Unpack" design.

**Work**
- Detect archive sets by magic bytes (RAR new-style and old-style, 7z single and multi-volume).
- Unpack with `nwaples/rardecode/v2` and `bodgit/sevenzip`, including multi-volume sets and passwords.
- Allow one level of nested archives.
- Output into `_unpack.tmp/`, then rename into place. Clear staging at the start so the stage is idempotent.
- Map errors to `archive_unsupported`, `archive_password` and `archive_corrupt`.

**Acceptance criteria**
- [ ] Fixture tests cover RAR3 old-style, RAR5 multi-volume, encrypted RAR (right and wrong password), 7z multi-volume, and nested RAR.
- [ ] No subprocess is spawned. A test asserts that `os/exec` is not imported in `internal/`. *(ADR-0007: only `internal/postproc/tool` may import it.)*

### M3-09: Unpack guards — path traversal, entry types and size caps
- **Labels:** area:postproc, ready-for-agent
- **Depends on:** M3-08

> Superseded in part by [ADR-0007](../adr/0007-external-postproc-tools.md): the guards now run on the `7zz` listing before extraction and on the extracted tree afterwards. Symlinks, hardlinks and special entries are refused (`path_violation`) instead of skipped.

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

> **Superseded / dropped by [ADR-0007](../adr/0007-external-postproc-tools.md).** The risk this spike was to check, whether the pure-Go libraries handle real releases, is gone with the libraries: `7zz` is the reference decoder for RAR and 7z. Nothing is left to do here.

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

> **Superseded by [ADR-0007](../adr/0007-external-postproc-tools.md).** The guard existed because `bodgit/sevenzip` allocated from counts in the archive header. `bodgit/sevenzip` and the whole pre-flight (`sevenz_preflight.go`, `sevenz_header.go`, `WithMaxHeaderMemory`, `WithMaxDictionary`) are deleted; 7-Zip parses the header. The text below is kept as the record of what was done.

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

- Implementation notes (M3-12):
  - Measured on the unguarded path (sevenzip v1.6.5, darwin/arm64, allocation requested by the library): `numFiles` 2^31 asked for 240 GiB (2^32-1: 480 GiB), 2^31 sub streams 16 GiB, a 4 GiB coder property blob 4 GiB, a coder with 2^31 input streams 16 GiB, a pack-stream CRC table for 2^31 streams 10 GiB allocated and 2 GiB resident in 0.65 s, an LZMA2 dictionary byte of 38 two GiB for a 17-byte stream, PPMd with 3 GiB of memory 3 GiB. A compressed header that inflates into 3 million empty names (6 MB, packed to a few KB) cost 410 MiB. Folder counts and coder counts were already safe in the library. After the fix every one of these fails in under 100 ms with less than 64 MiB allocated (`TestHostile7zHeadersFailFastWithoutAllocating` asserts it for 30 crafted archives; the costliest, a 6 MB plain header of empty names, allocates 14 MiB).
  - The pre-flight is a view, not a filter: `headerOverlay` serves the checked signature header and header from memory and the packed streams from the volumes, so a file that changes after the check cannot change what the library parses. The encoded header is decoded with the stdlib and `ulikunitz/xz` (LZMA, LZMA2, Deflate, BZip2, Copy) and, for encrypted headers, our own 7zAES (the password is used twice, once for the header and once, by the library, for the data). Any other coder in an encoded header fails with `ErrUnsupported`; 7-Zip itself always writes LZMA.
  - Failure classes: counts or sizes over a cap `ErrTooManyEntries` or `ErrTooLarge`, a header that contradicts itself or its bytes `ErrCorrupt`, a coder or feature the library lacks `ErrUnsupported`, a wrong or missing password for an encrypted header `ErrPassword`.
  - No bug fix upstream is needed to be safe, but `sevenzip` v1.6.5 has these gaps worth reporting: `readFilesInfo` allocates `make([]FileHeader, n)` from `n <= MaxUint32` (120 bytes each) before reading any byte, `readCRC` and `readOptionalBool` allocate `n` entries from the declared count, `readSubStreamsInfo` allocates `make([]uint64, files)`, `readCoder` allocates the declared property length, `readFolder` allocates `f.packedStreams` entries from the declared input counts, and `lzma2.NewReader` and `ppmd.NewReader` allocate the declared dictionary and model memory. The same rule fixes all of them: no count may exceed the bytes left in the header.
  - `FuzzSevenZipPreflight` (checksum-aware: it also fuzzes the header behind a correct signature header, plain and compressed) asserts no panic, a classified error, less than 64 MiB allocated and 5 s per input, and also opens whatever the pre-flight accepts with the library. `FuzzUnpack` now fails when one input allocates more than 128 MiB.
