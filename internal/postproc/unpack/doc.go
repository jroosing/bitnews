// Package unpack extracts RAR and 7z archives with path and size guards.
//
// Detect finds the archive sets in a directory by magic bytes; Unpack extracts
// one set into a staging directory and renames it into place. The package is
// pure Go (ADR-0002): it never starts a subprocess.
package unpack
