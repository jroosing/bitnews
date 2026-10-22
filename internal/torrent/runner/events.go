package runner

import "gitlab.com/kyura4k/gitnews/internal/core"

// Queue pause reasons, as reported by Runner.QueuePaused.
const (
	// ReasonUser is the reason while PauseQueue is in effect.
	ReasonUser = "user"
	// ReasonDiskFull is the reason while the disk guard holds the Queue.
	ReasonDiskFull = "disk_full"
	// ReasonBindUnavailable is the reason the daemon gives PauseQueueFor
	// while the network binding of the protocol is unavailable.
	ReasonBindUnavailable = "bind_unavailable"
)

// The kinds of the Events this package publishes besides core's own.
const (
	KindQueuePaused       core.EventKind = "torrent_queue_paused"
	KindQueueResumed      core.EventKind = "torrent_queue_resumed"
	KindShareLimitReached core.EventKind = "torrent_share_limit_reached"
)

// QueuePaused says the torrent Queue stopped: its Downloads are left Queued.
// Seeding goes on.
type QueuePaused struct {
	// Reason is ReasonUser, ReasonDiskFull or the reason given to
	// PauseQueueFor.
	Reason string
	// Detail is free text, for example the free and needed bytes.
	Detail string
}

// QueueResumed says the torrent Queue runs again.
type QueueResumed struct{}

// ShareLimitReached says a torrent reached its Share Limit and Action was
// taken.
type ShareLimitReached struct {
	ID     core.ID
	Action core.SeedingAction
}

// Kind implements core.Event.
func (QueuePaused) Kind() core.EventKind { return KindQueuePaused }

// Kind implements core.Event.
func (QueueResumed) Kind() core.EventKind { return KindQueueResumed }

// Kind implements core.Event.
func (ShareLimitReached) Kind() core.EventKind { return KindShareLimitReached }
