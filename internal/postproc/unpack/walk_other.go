//go:build !unix

package unpack

import "io/fs"

// linkCount is the number of hard links to the file; unknown here.
func linkCount(fs.FileInfo) uint64 { return 1 }
