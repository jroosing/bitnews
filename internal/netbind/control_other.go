//go:build !linux

package netbind

import (
	"fmt"
	"syscall"
)

// bindToDevice is unsupported off Linux (macOS has IP_BOUND_IF, not
// implemented). Failing keeps the binding a kill switch.
func bindToDevice(_ syscall.RawConn, name string) error {
	return fmt.Errorf("%w: interface %q: %w", ErrBind, name, ErrUnsupported)
}
