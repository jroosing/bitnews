// Package par2 parses PAR2 packets and implements Verify and Repair.
//
// # Packet parsing
//
// PAR2 files are hostile input: any uploader controls them. Scan reads a
// possibly corrupt or truncated byte stream (an io.ReaderAt, so RecvSlic
// slice data is located, never loaded) and yields only packets whose MD5
// verifies. It finds packets by their "PAR2\0PKT" magic, so garbage between
// packets and damaged packets cost only themselves: after a candidate fails
// (impossible length, MD5 mismatch, malformed body) scanning resumes one byte
// after its magic. Packets of unknown type are verified and ignored.
//
// Every length and count is checked before anything is allocated: packet
// length at most MaxPacketLen (1 GiB), at most MaxFiles file IDs in a Main
// packet, FileDesc names and Creator text at most MaxNameLen, IFSC pairs at
// most MaxSlicesPerFile (and always bounded by the packet's own length).
// Bodies that are held in memory are therefore a few megabytes at most. Work
// on corrupt input is bounded too: the bytes hashed for candidates that fail
// verification are limited to a small multiple of the stream size, so
// repeated fake headers cannot make a scan quadratic.
//
// Names are exactly what the uploader wrote, minus trailing NULs. They are
// not sanitized here; callers must run them through fsutil before touching
// the filesystem.
//
// # Sets
//
// LoadDir (or LoadFiles) merges the packets of every .par2 file of a
// download into a Set. The set is the one whose Main packet is
// self-consistent (recovery set ID equal to the MD5 of the Main body) and
// valid (slice size non-zero and a multiple of 4) and which is referenced by
// the most packets, smallest ID winning ties. Packets of other sets are
// dropped, duplicates are dropped (the first one in file-name order wins),
// FileDesc packets whose file ID is not MD5(MD5 of first 16 KiB, length,
// name) are dropped, and an IFSC is kept only when it has exactly one pair
// per slice of its file. Non-fatal problems are reported in Set.Warnings,
// which is bounded and contains only quoted, ASCII-only text.
package par2
