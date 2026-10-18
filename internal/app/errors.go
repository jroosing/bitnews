package app

import (
	"errors"
	"fmt"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// ErrInvalidInput is matched (errors.Is) by every error the services return
// for input that is not acceptable.
var ErrInvalidInput = errors.New("invalid input")

// ErrTooLarge is the Kind of an Error for input over a size limit.
var ErrTooLarge = errors.New("too large")

// ErrUpstream is the Kind of an Error for a failure of something bitnews
// called, such as the server an NZB URL points at.
var ErrUpstream = errors.New("upstream failure")

// Error is a failure a client can act on. Kind says which class it belongs
// to and is matched by errors.Is: ErrInvalidInput, core.ErrConflict or
// core.ErrNotFound. Code is a stable snake_case identifier and Message is
// safe to show to the user.
type Error struct {
	Kind    error
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("app: %s: %s", e.Code, e.Message) }

// Is makes an Error match its Kind.
func (e *Error) Is(target error) bool { return target == e.Kind }

func invalid(code, format string, args ...any) error {
	return &Error{Kind: ErrInvalidInput, Code: code, Message: fmt.Sprintf(format, args...)}
}

func conflict(code, format string, args ...any) error {
	return &Error{Kind: core.ErrConflict, Code: code, Message: fmt.Sprintf(format, args...)}
}

func tooLarge(code, format string, args ...any) error {
	return &Error{Kind: ErrTooLarge, Code: code, Message: fmt.Sprintf(format, args...)}
}

func upstream(code, format string, args ...any) error {
	return &Error{Kind: ErrUpstream, Code: code, Message: fmt.Sprintf(format, args...)}
}
