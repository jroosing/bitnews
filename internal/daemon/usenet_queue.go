package daemon

import (
	"context"
	"log/slog"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/queue"
)

// queueHolder gives the data remover the holds of a Queue.
type queueHolder struct{ q *queue.Queue }

var _ fetchHolder = queueHolder{}

func (h queueHolder) Hold(ctx context.Context, id core.ID) error { return h.q.Hold(ctx, id) }
func (h queueHolder) Release(id core.ID)                         { h.q.Release(id) }

// usenetKillswitch pauses the Usenet Queue with queue.ReasonBindUnavailable
// while the bound interface or address is gone, and releases that reason
// when it is back. Other reasons, such as the user's pause, are held
// separately and stay.
func usenetKillswitch(q *queue.Queue, log *slog.Logger) *bindingKillswitch {
	return &bindingKillswitch{
		name: "usenet", log: log,
		pause: func(reason string) bool {
			q.Pause(queue.ReasonBindUnavailable, reason)
			return true
		},
		resume: func() { q.Resume(queue.ReasonBindUnavailable) },
	}
}
