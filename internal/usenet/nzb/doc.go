// Package nzb parses NZB files into a normalized model.
//
// NZBs come from indexers and uploaders and are treated as hostile input.
// Parse streams the document through encoding/xml in strict mode, with no
// custom entity map, so DTD entity definitions (internal or external) are
// never expanded and nothing outside the input is read. Input size, file
// count and segment count are capped by Limits. A segment with an invalid
// number or message ID is skipped rather than failing the whole NZB.
//
// File names guessed from subjects are provisional and unsanitized: the
// yEnc name= header is authoritative, and callers must pass any name
// through fsutil before using it on disk.
package nzb
