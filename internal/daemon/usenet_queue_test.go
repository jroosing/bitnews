package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/netbind"
	"github.com/jroosing/bitnews/internal/queue"
)

// idleProtocol is a queue.Protocol with nothing running.
type idleProtocol struct{}

func (idleProtocol) Prepare(context.Context) bool               { return true }
func (idleProtocol) Exclude(core.ID) bool                       { return false }
func (idleProtocol) Running(core.ID) bool                       { return false }
func (idleProtocol) Start(context.Context, core.Download) error { return nil }
func (idleProtocol) Stop(context.Context, core.ID)              {}
func (idleProtocol) Abandon(context.Context, core.ID) error     { return nil }

func TestBindingSwitchPausesTheQueueForTheBindingNotAsTheUser(t *testing.T) {
	q, err := queue.New(queue.Config{
		Source: core.SourceNZB, Downloads: struct{ core.DownloadRepo }{}, Bus: core.NewBus(),
		Settings: func(context.Context) (core.Settings, error) { return core.DefaultSettings(), nil },
		Protocol: idleProtocol{},
	})
	require.NoError(t, err)
	onChange := bindingSwitch("usenet", q, slog.New(slog.NewTextHandler(io.Discard, nil)))

	onChange(netbind.Status{Available: false, Reason: "interface gone"})

	paused, reason, detail := q.Paused()
	assert.True(t, paused)
	assert.Equal(t, queue.ReasonBindUnavailable, reason)
	assert.Equal(t, "interface gone", detail)

	q.Pause(queue.ReasonUser, "")
	onChange(netbind.Status{Available: true})

	paused, reason, _ = q.Paused()
	assert.True(t, paused, "the user's own pause stays")
	assert.Equal(t, queue.ReasonUser, reason)
	q.Resume(queue.ReasonUser)
	paused, _, _ = q.Paused()
	assert.False(t, paused)
}
