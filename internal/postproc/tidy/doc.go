// Package tidy ports the two TRaSH SABnzbd scripts to Go (ADR-0009): Clean
// strips the suffixes and tags release bots add to an NZB's name (Clean.py, a
// pre-queue script), and DotUnderscores replaces underscores with dots in the
// names at the top level of a directory (replace_for.py, a category script).
// bitnews runs no scripts; these are the same rules, in the same order.
// NameVideo is SABnzbd's deobfuscation of final file names: the largest file,
// when its content is a video but its name has no video extension, is named
// after the Download.
package tidy
