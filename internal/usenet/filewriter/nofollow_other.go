//go:build !unix

package filewriter

// oNoFollow is unavailable here; openFile falls back to an Lstat comparison.
const oNoFollow = 0
