package runner

import "gitlab.com/kyura4k/gitnews/internal/core"

// Queue pause reasons, as reported by Runner.QueuePaused.
const (
	// ReasonUser is the reason while PauseQueue is in effect.
	ReasonUser = "user"
	// ReasonDiskFull is the reason while the disk guard holds the Queue.
	ReasonDiskFull = "disk_full"
)

// The kinds of the Events this package publishes besides core's own.
const (
	KindQueuePaused  core.EventKind = "usenet_queue_paused"
	KindQueueResumed core.EventKind = "usenet_queue_resumed"
)

// QueuePaused says the Usenet Queue stopped: its Downloads are left Queued.
type QueuePaused struct {
	// Reason is ReasonUser or ReasonDiskFull.
	Reason string
	// Detail is free text, for example the free and needed bytes.
	Detail string
}

// QueueResumed says the Usenet Queue runs again.
type QueueResumed struct{}

// Kind implements core.Event.
func (QueuePaused) Kind() core.EventKind { return KindQueuePaused }

// Kind implements core.Event.
func (QueueResumed) Kind() core.EventKind { return KindQueueResumed }
