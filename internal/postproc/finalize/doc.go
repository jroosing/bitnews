// Package finalize runs the last step of Post-processing: cleanup of
// known-garbage files, then the move of the finished Download into the
// complete tree. The Finisher resolves the destination at move time, so
// later Category and settings changes apply.
//
// # Usenet
//
// With unpacked archives (unpackedDir is not empty) the Download's dir is
// cleaned of archive volumes (.rar, .rNN, .sNN, .7z, .7z.NNN), .par2 files,
// the .nzb and any _unpack.tmp leftover. Sample files and .nfo files stay.
// The entries of unpackedDir are then moved to the root of dir, and dir is
// moved to the destination. The final dir therefore holds what Unpack
// produced at its root plus the remaining non-archive files. If an unpacked
// entry has the name of a file that stayed, the unpacked entry gets a Unique
// name; nothing is overwritten.
//
// Without archives only the .par2 files and the .nzb are deleted.
//
// Cleanup deletes only regular files directly inside dir (plus the
// _unpack.tmp dir). It never follows symlinks and never leaves dir. All
// deletion and merging happens before the move, so nothing inside the final
// dir changes afterwards; this matters for hardlinks made by media managers.
//
// # Torrent
//
// Torrent originals are never deleted or modified, because they are being
// seeded. dir (with its _unpack/ dir, if any) is moved as a whole to the
// destination, unless it already is the destination, in which case nothing
// happens. The returned path is the final dir; _unpack/ is inside it.
//
// # Restart safety
//
// Before the move a .bitnews-done marker holding the Download ID is written
// inside dir, so it travels with it. If Finish is retried after dir was
// already moved, the missing dir is detected and the destination (or its
// Unique'd variants name.1, name.2, ...) is searched for a marker with this
// Download's ID; if found, that path is returned without error.
package finalize
