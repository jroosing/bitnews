//go:build !unix

package fsutil

// SameFilesystem is not implemented on this platform and always returns
// ErrUnsupported.
func SameFilesystem(a, b string) (bool, error) {
	return false, ErrUnsupported
}
