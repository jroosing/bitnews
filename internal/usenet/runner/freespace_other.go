//go:build !unix

package runner

// defaultFreeSpace is nil where there is no statfs: the disk guard is then
// disabled unless Config.FreeSpace is set.
var defaultFreeSpace func(path string) (uint64, error)
