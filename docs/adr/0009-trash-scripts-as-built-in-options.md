# The two TRaSH SABnzbd scripts are built-in post-processing options

The TRaSH guides tell SABnzbd users to install two Python scripts: `Clean.py` (a pre-queue script that strips bot suffixes from the NZB name) and `replace_for.py` (a category script that replaces underscores with dots in file names). bitnews offers both as toggles in Settings instead of running user scripts.

## Decision

- **Native Go, no scripts.** bitnews runs no Python and no user-supplied programs. The only subprocesses are 7-Zip and par2 ([ADR-0007](0007-external-postproc-tools.md)). Each script is a few lines of string handling, so it is ported to Go in one small module (`internal/postproc/tidy`) with the same rules, tested against the scripts' own cases.
- **Two toggles, both off by default**, in Settings:
  - **Clean release names** (`Clean.py`): runs *when an NZB is added*, before anything is downloaded, like SABnzbd's pre-queue script. It strips the known suffixes and tags from the name (`-NZBGeek`, `-Obfuscated`, `-BUYMORE`, `-Scrambled`, `-xpost`, `[eztv]`, `[TGx]`, …) and a trailing `-xxxx` after a group name, and the cleaned name becomes the Download's name (so also its output folder). It applies to NZBs only; a torrent's name is its identity.
  - **Dots for underscores** (`replace_for.py`): runs *in Post-processing, after Unpack and before the move to the complete folder*, like SABnzbd's category script. It renames the files and folders at the top level of the result, replacing `_` with `.`. It applies to NZBs only: a torrent's files are never renamed, because they are being seeded.
- The guide scopes `replace_for.py` to the categories that need it. bitnews makes it a global toggle for now; a per-Category switch can follow if it is wanted.
- Neither rename may overwrite: a target name that already exists is skipped and logged.

## Consequences

- Behaviour matches what SABnzbd users get from the TRaSH scripts without Python, a script folder or `chmod`/`dos2unix` steps.
- The suffix list is a snapshot of the script's. New bot suffixes need a code change.
- Sonarr and Radarr see the cleaned name, as they do with SABnzbd plus `Clean.py`; they track a Download by its id, not its name.
