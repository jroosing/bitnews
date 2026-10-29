# M14 — Fully obfuscated Usenet posts

## Goal

A Usenet Download whose files, PAR2 files included, are posted under random names ends Ready with a video file the media managers import, as with SABnzbd.

## Scope

**In:** finding PAR2 files by their content; naming the largest file after the Download when its content is a video and its name has no video extension.

**Out:** renaming anything of a torrent (its files are being seeded), guessing names from NZB subjects, detecting non-video content (archives are already found by content by Unpack).

## Design

- **PAR2 by content.** Before `par2 r`, every top-level file that starts with `PAR2\0PKT` but has no `.par2` name gets `<name>.par2` (made safe and unique). The set is then used, par2 renames the data to its real names, and the cleanup stage deletes the PAR2 files as usual.
- **Video naming** (`tidy.NameVideo`, SABnzbd's "deobfuscate final filenames"). In Finish, for NZB Downloads after the cleanup and the underscore option: when the largest file below the Download's folder has no video extension but starts like a video container (Matroska, MP4/QuickTime, AVI, ASF, MPEG-PS, MPEG-TS, M2TS), it is renamed to the Download's name (made safe) plus that container's extension. Never overwrites, never follows links, logged at info, never fails the Download. The container sniffer has a fuzz target.

## Issues

### M14-01: Fully obfuscated posts end with an importable video file
- **Labels:** area:postproc, ready-for-agent

**Goal:** A Download posted with random names for every file, PAR2 files included, ends with its video under a name with a video extension.

**Acceptance criteria**
- [x] Hash-named PAR2 files are used for verify and repair and removed afterwards; the data gets its real names.
- [x] Without a usable PAR2 set, the largest file is named `<Download name>.<ext>` when its content is a video and its name has no video extension; anything else is left alone.
- [x] Torrent files are never renamed.
- [x] `VideoExt` has a fuzz target; `make check` is green.
