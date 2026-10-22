// Package unpack extracts RAR and 7z archives with path and size guards.
//
// Detect finds the archive sets in a directory by magic bytes; Unpack extracts
// one set into a staging directory and renames it into place. The package is
// pure Go (ADR-0002): it never starts a subprocess.
//
// # Hostile archives
//
// Every archive is hostile. Names go through the path guards, sizes through
// the byte, ratio and disk guards, and entry counts through WithMaxEntries,
// before anything is written.
//
// A 7z archive is checked before the 7z library sees it. The library
// allocates from counts the header declares, before it has read the bytes that
// should back them, so a tiny archive could otherwise make it request
// gigabytes. The pre-flight (sevenz_preflight.go) validates the signature
// header, decodes a compressed header itself under a 64 MiB cap, checks every
// declared count against the bytes that are present and against the entry,
// header-memory (WithMaxHeaderMemory) and dictionary (WithMaxDictionary) caps,
// and hands the library the checked header. What it guarantees and what it
// does not is described in docs/milestones/m3-post-processing.md (M3-12): in
// short, the memory the library takes for the header is bounded by
// WithMaxHeaderMemory, and a decoder may still allocate up to WithMaxDictionary
// for one folder.
package unpack
