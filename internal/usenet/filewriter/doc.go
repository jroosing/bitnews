// Package filewriter writes decoded yEnc parts straight into pre-allocated
// files inside one Download directory and keeps per-file range bookkeeping.
//
// A Writer is created for one directory. Files are identified by a
// caller-supplied FileKey (the index of the file within the NZB), because
// yEnc names are hostile and may differ between the parts of one file. The
// final on-disk name is chosen from the first part's name through
// fsutil.SafeName and fsutil.Unique, and every path is built with
// fsutil.SafeJoin, so nothing is ever written outside the directory.
//
// Byte ranges are half-open and zero-based: Range{Start, End} covers offsets
// Start <= i < End. Per file the Writer tracks which ranges were written
// correctly (Done), written from a part whose CRC32 did not match (Damaged),
// and permanently unavailable (Lost, set with MarkMissing). Missing is
// everything neither Done nor Damaged.
//
// Pre-allocation uses Truncate, which creates a sparse file on every
// supported filesystem. fallocate is deliberately not used: it is not
// portable and a sparse file is enough for the byte-exact result.
//
// Snapshot and Restore serialize the bookkeeping in a compact, versioned
// binary form so the Usenet Queue runner can persist it and resume.
package filewriter
