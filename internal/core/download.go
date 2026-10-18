package core

import (
	"errors"
	"fmt"
	"time"
)

// SourceKind says what a Download was created from, which decides the
// protocol that fetches it.
type SourceKind string

// The kinds of Source.
const (
	SourceNZB     SourceKind = "nzb"
	SourceTorrent SourceKind = "torrent"
)

// DownloadPriority is the rank of a Download within its Queue. A force
// Download ignores the Queue's concurrency limit.
type DownloadPriority string

// The Download Priorities, highest first.
const (
	DownloadPriorityForce  DownloadPriority = "force"
	DownloadPriorityHigh   DownloadPriority = "high"
	DownloadPriorityNormal DownloadPriority = "normal"
	DownloadPriorityLow    DownloadPriority = "low"
)

// Download is one thing a user or Media manager asked for, tracked from being
// added until it is removed. It has exactly one Source.
type Download struct {
	ID         ID
	Name       string
	SourceKind SourceKind
	// CategoryID is empty when the Download has no Category.
	CategoryID    ID
	State         State
	Priority      DownloadPriority
	QueuePosition int
	// Paused is orthogonal to State: a paused Download stays Queued or
	// Fetching but is never active.
	Paused bool
	// FailureReason is empty unless State is StateFailed.
	FailureReason FailureReason
	// FailureDetail is free text explaining the failure. It must already be
	// redacted.
	FailureDetail string
	TotalBytes    int64
	DoneBytes     int64
	AddedAt       time.Time
	FetchedAt     time.Time // zero until Fetched
	ReadyAt       time.Time // zero until Ready
	UpdatedAt     time.Time
	// OutputPath is where the importable files are. It is set once Ready.
	OutputPath string
}

// StateChange is one lifecycle move of a Download.
type StateChange struct {
	To State
	At time.Time
	// OutputPath is required when To is StateReady.
	OutputPath string
	// FailureReason is required when To is StateFailed. FailureDetail is
	// optional, already-redacted free text.
	FailureReason FailureReason
	FailureDetail string
}

// ErrInvalidStateChange is returned by ChangeState for a legal move that
// lacks data the target State needs, or carries data it must not have.
var ErrInvalidStateChange = errors.New("invalid state change")

// ChangeState moves the Download to c.To. An illegal move returns an error
// and leaves the Download unchanged.
func (d *Download) ChangeState(c StateChange) error {
	if err := Transition(d.State, c.To); err != nil {
		return err
	}
	if c.To == StateReady && c.OutputPath == "" {
		return fmt.Errorf("core: ready without output path: %w", ErrInvalidStateChange)
	}
	if c.To == StateFailed && !c.FailureReason.known() {
		return fmt.Errorf("core: failed with unknown reason %q: %w", c.FailureReason, ErrInvalidStateChange)
	}
	if c.To != StateFailed && c.FailureReason != "" {
		return fmt.Errorf("core: failure reason %q on a move to %s: %w", c.FailureReason, c.To, ErrInvalidStateChange)
	}
	d.State = c.To
	d.UpdatedAt = c.At
	switch c.To {
	case StateFetched:
		d.FetchedAt = c.At
	case StateReady:
		d.ReadyAt = c.At
		d.OutputPath = c.OutputPath
	case StateFailed:
		d.FailureReason = c.FailureReason
		d.FailureDetail = c.FailureDetail
	}
	return nil
}
