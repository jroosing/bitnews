package daemon

import (
	"context"
	"log/slog"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/netbind"
	"github.com/jroosing/bitnews/internal/queue"
)

// queueHolder gives the data remover the holds of a Queue. It only drops
// the result of Queue.Release, which fetchHolder does not have.
type queueHolder struct{ q *queue.Queue }

var _ fetchHolder = queueHolder{}

func (h queueHolder) Hold(ctx context.Context, id core.ID) error { return h.q.Hold(ctx, id) }
func (h queueHolder) Release(id core.ID)                         { h.q.Release(id) }

// bindingSwitch is the network kill switch of a protocol: it pauses the
// protocol's Queue with queue.ReasonBindUnavailable while the bound interface
// or address is gone, and releases that reason when it is back. Other
// reasons, such as the user's pause, are held separately and stay.
func bindingSwitch(name string, q *queue.Queue, log *slog.Logger) func(netbind.Status) {
	return func(s netbind.Status) {
		if !s.Available {
			log.Warn("network binding unavailable, queue paused", "protocol", name, "reason", s.Reason)
			q.Pause(queue.ReasonBindUnavailable, s.Reason)
			return
		}
		if paused, reason, _ := q.Paused(); paused && reason == queue.ReasonBindUnavailable {
			log.Info("network binding available again", "protocol", name)
		}
		q.Resume(queue.ReasonBindUnavailable)
	}
}
