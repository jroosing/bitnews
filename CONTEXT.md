# bitnews

A self-hosted download daemon for a homelab that fetches content from Usenet and BitTorrent and makes it ready for use.

## Language

### Core

**Download**:
One thing a user or media manager asked for, tracked from being added until it is removed. It is protocol-agnostic and has exactly one Source.
_Avoid_: Job, task, item, transfer, queue item

**Source**:
What a Download was created from: either an NZB or a Torrent. The Source decides which protocol fetches the Download.
_Avoid_: Input, origin

**NZB**:
A Source describing Usenet articles to fetch. An NZB that bundles several releases is still one Download.
_Avoid_: Nzb file, group

**Torrent**:
A Source describing BitTorrent content, given as a .torrent file or a magnet link. A magnet link whose metadata hasn't been fetched yet is still a Download.
_Avoid_: Magnet (as a generic term), transfer

**Category**:
An optional label on a Download that decides where its Ready output lands and which Media manager owns it. Categories are shared across Usenet and torrents.
_Avoid_: Label, tag, group

**Media manager**:
An external application (Sonarr, Radarr, Prowlarr, and similar) that adds Downloads and imports them once they are Ready.
_Avoid_: Arr, *arr, client

### Usenet

**Server**:
One NNTP endpoint of a Usenet provider, with its own credentials, connection limit, retention and Server Priority.
_Avoid_: Host, provider (when meaning the endpoint), news server

**Server Priority**:
The order in which Servers are tried for an Article. Servers with the same Server Priority share load, and a higher-numbered one is used only for Articles the lower ones don't have.
_Avoid_: Priority (bare), level, tier, backup server

**Article**:
One Usenet message carrying a yEnc-encoded part of a file named in an NZB.
_Avoid_: Segment, post, part

**Missing Article**:
An Article that no Server can supply. Missing Articles are left for Repair to recover.
_Avoid_: Failed article, 430

### Scheduling

**Queue**:
The ordered set of one protocol's Downloads that are not yet Fetched. Usenet and torrents each have their own Queue with its own concurrency limit.
_Avoid_: List, backlog

**Download Priority**:
The rank of a Download within its Queue: force, high, normal or low. A force Download ignores the Queue's concurrency limit.
_Avoid_: Priority (bare), order, position

### Torrent

**Seeding**:
Uploading a Torrent's data to peers after it is Fetched. Seeding continues independently of Post-processing.
_Avoid_: Sharing, uploading

**Share Limit**:
The ratio or seeding time after which Seeding stops for a Download.
_Avoid_: Seed goal, ratio limit

### Lifecycle

**Fetched**:
The state in which all of a download's data has been received and integrity-checked at the protocol level: all articles for Usenet, all pieces hash-verified for a torrent.
_Avoid_: Complete, finished, done

**Ready**:
The state in which Post-processing has succeeded and importable files exist. Media managers are told a download is complete only at this point.
_Avoid_: Complete, done, imported

**Failed**:
The state in which a download cannot become Ready, because data is missing beyond repair, an archive is unsupported, or a password is wrong. Media managers are told so they can pick another release.
_Avoid_: Error, broken

### Post-processing

**Post-processing**:
The steps run after a download's data has been fetched to turn it into files a media manager can import. For Usenet that is Verify, Repair, Unpack, then cleanup. For a torrent it is Unpack only, and the original files are never removed.
_Avoid_: Postproc, finishing

**Verify**:
Checking downloaded files against their PAR2 checksums to detect missing or damaged data.
_Avoid_: Check, validate

**Repair**:
Reconstructing missing or damaged data from PAR2 recovery blocks.
_Avoid_: Fix, recover

**Unpack**:
Extracting the payload from RAR or 7z archives into usable files.
_Avoid_: Extract, unrar, decompress
