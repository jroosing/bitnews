# M13 — Category folders per protocol

## Goal

Let a Category send its finished Usenet and torrent Downloads to different folders, the way the complete and incomplete roots are already set per protocol.

## Scope

**In:** a Usenet folder and a torrent folder per Category (each optional), the migration of the single folder override, the destination of finished Downloads, the folders the SABnzbd and qBittorrent adapters report, the own API, the Categories card in Settings → Paths, and the data remover's allowed roots.

**Out:** moving output that already landed (a Ready torrent keeps seeding where it is), per-protocol Category names.

## Design

- **Model.** `core.Category` has `UsenetDir` and `TorrentDir` instead of `DirOverride`; `DirOverride(kind)` returns the one for a Source kind. Each is an absolute, clean path or empty (`<complete root of the protocol>/<name>`).
- **Migration 0009.** `dir_override` becomes `usenet_dir`, and `torrent_dir` starts as a copy of it, so nothing moves until the user splits them.
- **Destination.** Finish and the torrent runner's relocation use the folder of the Download's protocol.
- **Adapters.** SABnzbd `get_config` reports the Usenet folder; qBittorrent `torrents/categories` reports the torrent folder as `savePath`.
- **Own API.** `usenet_dir` and `torrent_dir` replace `dir_override` in the Category JSON, in `POST` and in `PATCH` (`null` clears one). `dir_override` is refused as an unknown field.
- **Settings.** The Categories table has a Usenet folder and a Torrent folder column; adding and editing a Category take both, each checked next to its own field.
- **Data remover.** Both folders of every Category are allowed roots for either protocol, so output that landed in the shared folder before the split can still be removed.

## Issues

### M13-01: Category folders per protocol
- **Labels:** area:core, area:ui, area:api, area:adapter, ready-for-agent

**Goal:** A Category has a Usenet folder and a torrent folder, and finished Downloads use the one of their protocol.

**Acceptance criteria**
- [x] `core.Category` has `UsenetDir` and `TorrentDir`, both validated as absolute clean paths; migration 0009 copies the old override into both.
- [x] A finished Usenet Download goes to the Usenet folder, a torrent to the torrent folder; a torrent of a Category with only a Usenet folder goes to `<torrent complete root>/<name>`.
- [x] SABnzbd `get_config` shows the Usenet folder, qBittorrent `savePath` the torrent folder.
- [x] `/api/v1/categories` uses `usenet_dir` and `torrent_dir`; `docs/api-v1.md` is updated.
- [x] Settings → Paths shows, adds and edits both folders, each with its own error.
- [x] Output in either folder of a Category can be removed with its data.
- [x] `make check` is green.
