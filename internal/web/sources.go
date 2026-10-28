package web

import (
	"github.com/jroosing/bitnews/internal/core"
)

// RuntimeSource is what the UI asks the running daemon about the Queues:
// transfer speeds, whether a Queue is paused, and the speed limits. What is
// live about one Download (stage, rates, torrent counters) comes from the
// view module (Deps.View). internal/daemon implements it (web_runtime.go).
// The types are in queue_runtime.go.
type RuntimeSource interface {
	// Rates is the transfer speed of the whole daemon in bytes per second.
	Rates() (down, up int64)
	// QueueState says whether the Queue of one Source kind exists and
	// whether it is paused.
	QueueState(kind core.SourceKind) QueueState
	// PauseQueue and ResumeQueue are the user's pause of a whole Queue.
	PauseQueue(kind core.SourceKind)
	ResumeQueue(kind core.SourceKind)
	// SpeedLimits are the limits in force in bytes per second, 0 meaning
	// unlimited. They are runtime-only: set through SetSpeedLimit and lost
	// on restart. A Usenet Queue only has a download limit.
	SpeedLimits(kind core.SourceKind) (down, up int64)
	SetSpeedLimit(kind core.SourceKind, down, up int64)
}
