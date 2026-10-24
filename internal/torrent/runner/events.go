package runner

import "github.com/jroosing/bitnews/internal/core"

// KindShareLimitReached is the kind of ShareLimitReached. The Queue's own
// events are core.QueuePaused and core.QueueResumed.
const KindShareLimitReached core.EventKind = "torrent_share_limit_reached"

// ShareLimitReached says a torrent reached its Share Limit and Action was
// taken.
type ShareLimitReached struct {
	ID     core.ID
	Action core.SeedingAction
}

// Kind implements core.Event.
func (ShareLimitReached) Kind() core.EventKind { return KindShareLimitReached }
