package qbit

import "github.com/jroosing/bitnews/internal/view"

// TorrentState exposes the state mapping to the table test.
func TorrentState(v view.View, stopped bool) (state string, progress float64) {
	return torrentState(v, stopped)
}

// HoldOffProgress is the progress shown while a Download is Fetched or
// Post-processing.
const HoldOffProgress = holdOffProgress

// HashList is the parsed hashes parameter.
type HashList struct {
	All, Given bool
	Hashes     []string
}

// ParseHashes exposes the hashes parser.
func ParseHashes(s string) (HashList, bool) {
	l, ok := parseHashes(s)
	return HashList{All: l.all, Given: l.given, Hashes: l.hashes}, ok
}

// WebAPIAtLeast exposes the version comparison.
var WebAPIAtLeast = webAPIAtLeast
