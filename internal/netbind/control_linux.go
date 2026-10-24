package netbind

import (
	"fmt"
	"syscall"

	"github.com/jroosing/bitnews/internal/core"
)

// bindToDevice sets SO_BINDTODEVICE. The name only ever goes into the
// setsockopt call, after validation.
func bindToDevice(c syscall.RawConn, name string) error {
	if err := core.ValidInterfaceName(name); err != nil {
		return fmt.Errorf("%w: interface %q: %w", ErrBind, name, err)
	}
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, name)
	}); err != nil {
		return fmt.Errorf("%w: interface %q: %w", ErrBind, name, err)
	}
	if serr != nil {
		return fmt.Errorf("%w: SO_BINDTODEVICE %q: %w", ErrBind, name, serr)
	}
	return nil
}
