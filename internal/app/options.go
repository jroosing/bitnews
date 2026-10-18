package app

import (
	"context"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Publisher receives the events the services emit. *core.Bus is one.
type Publisher interface {
	Publish(core.Event)
}

// DataRemover deletes the data a Download left on disk: the incomplete
// directory, the output, any seeded files. Engines provide the real one in
// later milestones.
type DataRemover interface {
	// RemoveData is called before the Download row is deleted, so it can
	// still read the row's paths. An error keeps the row, so the user can
	// retry.
	RemoveData(ctx context.Context, d core.Download) error
}

type noopRemover struct{}

func (noopRemover) RemoveData(context.Context, core.Download) error { return nil }

type options struct {
	now     func() time.Time
	remover DataRemover
}

// Option configures a service constructor.
type Option func(*options)

// WithClock sets the time source used for UpdatedAt stamps.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithDataRemover sets the hook that deletes a Download's data. The default
// does nothing.
func WithDataRemover(r DataRemover) Option {
	return func(o *options) { o.remover = r }
}

func newOptions(opts []Option) options {
	o := options{now: func() time.Time { return time.Now().UTC() }, remover: noopRemover{}}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
