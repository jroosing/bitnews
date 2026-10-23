package daemon

import (
	"context"
	"log/slog"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/queue"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// usenetRuntime gives webRuntime the Usenet runner with the user's pause of
// its Queue, and the data remover the Queue's holds.
type usenetRuntime struct{ *runner.Runner }

var (
	_ usenetQueue = usenetRuntime{}
	_ fetchHolder = usenetRuntime{}
)

func (u usenetRuntime) PauseQueue()  { u.Queue().Pause(queue.ReasonUser, "") }
func (u usenetRuntime) ResumeQueue() { u.Queue().Resume(queue.ReasonUser) }
func (u usenetRuntime) QueuePaused() (paused bool, reason string) {
	paused, reason, _ = u.Queue().Paused()
	return paused, reason
}

func (u usenetRuntime) Hold(ctx context.Context, id core.ID) error { return u.Queue().Hold(ctx, id) }
func (u usenetRuntime) Release(id core.ID)                         { u.Queue().Release(id) }

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
