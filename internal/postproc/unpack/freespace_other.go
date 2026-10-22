//go:build !unix

package unpack

import "errors"

// freeSpace is not implemented off Unix; the disk check is skipped.
func freeSpace(string) (uint64, error) { return 0, errors.ErrUnsupported }
