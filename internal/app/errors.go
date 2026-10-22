package app

import (
	"errors"
	"fmt"
	"io/fs"

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

// ErrUnavailable is the Kind of an Error for a feature this instance is not
// set up for, such as adding torrents without a torrent source store.
var ErrUnavailable = errors.New("unavailable")

// ErrDuplicate is returned by AddTorrent when a Download with the same info
// hash already exists. It matches core.ErrConflict (errors.Is) and is
// extracted with errors.As(err, &dup) for a dup of type ErrDuplicate.
type ErrDuplicate struct {
	// ExistingID is the Download that already has the torrent.
	ExistingID core.ID
	// Name is that Download's name, empty if it could not be read.
	Name string
}

func (e ErrDuplicate) Error() string {
	return fmt.Sprintf("app: duplicate_torrent: the torrent is already download %s", e.ExistingID)
}

// Is makes an ErrDuplicate match core.ErrConflict.
func (e ErrDuplicate) Is(target error) bool { return target == core.ErrConflict }

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

func unavailable(code, format string, args ...any) error {
	return &Error{Kind: ErrUnavailable, Code: code, Message: fmt.Sprintf(format, args...)}
}

// createDirError is the failure to create a Download's directory. A
// permission error is the user's to fix, so it names the folder and the user
// id bitnews runs as (code "folder_not_writable"); anything else is internal.
func createDirError(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return unavailable("folder_not_writable", "%s", err)
	}
	return fmt.Errorf("app: create download dir: %w", err)
}
