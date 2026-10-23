# M9 — Post-processing options

## Goal

Offer the two TRaSH SABnzbd scripts as built-in toggles ([ADR-0009](../adr/0009-trash-scripts-as-built-in-options.md)): clean release names when an NZB is added, and dots for underscores after Unpack.

## Scope

**In:** the rename module, the two Settings fields, the hooks at add time and in Post-processing, and the Settings page.

**Out:** running user scripts, per-Category switches, torrents.

## When they run

| Option | Script | When | Applies to |
|---|---|---|---|
| Clean release names | `Clean.py` (SABnzbd pre-queue script) | When an NZB is added, before it is queued | NZB names only |
| Dots for underscores | `replace_for.py` (SABnzbd category script) | After Unpack, before the move to the complete folder | NZB Downloads only; never torrents |

## Issues

### M9-01: The rename module
- **Labels:** area:postproc, ready-for-agent

**Goal:** `internal/postproc/tidy` with `Clean(name) string` (a port of `Clean.py`: the same suffix and tag list, the same trailing `-xxxx` rule, applied in the same order) and `DotUnderscores(dir) ([]Rename, error)` (a port of `replace_for.py`: the entries at the top level of dir, files and folders, `_` to `.`, no overwriting).

**Acceptance criteria**
- [ ] Table tests cover every pattern of `Clean.py`, including the case-insensitive match, the order of the substitutions and the trailing group rule; an empty result keeps the original name.
- [ ] `DotUnderscores` skips an existing target, does not recurse, and leaves names without `_` alone. `Clean` has a fuzz target.

### M9-02: Settings and hooks
- **Labels:** area:core, area:postproc, ready-for-agent
- **Depends on:** M9-01

**Goal:** `PostProcess.CleanNames` and `PostProcess.UnderscoresToDots` in the Settings (both off), applied at add time and in Post-processing.

**Work**
- `core.Settings.PostProcess` (JSON `postprocess`) with defaults, tests and docs/api-v1.md.
- `AddService` cleans the NZB's name when `CleanNames` is on, so every transport (own API, UI, SABnzbd, qBittorrent-less) gets it. A name that cleans to empty is kept.
- `finalize` runs `DotUnderscores` on the result before the move for NZB Downloads when `UnderscoresToDots` is on, and logs each rename.

**Acceptance criteria**
- [ ] An NZB added with `CleanNames` on is stored with the cleaned name; with it off the name is unchanged; torrents are never touched.
- [ ] A Ready NZB with `UnderscoresToDots` on has dotted names in its output; a torrent's files are untouched.

### M9-03: The Settings page
- **Labels:** area:ui, ready-for-agent
- **Depends on:** M9-02

**Goal:** a "Post-processing" section in Settings with the two switches, each saying in plain words what it does, when it runs and an example.

**Acceptance criteria**
- [ ] Both switches save through the card pattern and persist; the help text shows the before and after example and when each runs.
