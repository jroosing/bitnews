package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// Publisher receives the events the services emit. *core.Bus is one.
type Publisher interface {
	Publish(core.Event)
}

// DataRemover deletes the data a Download left on disk: the incomplete
// directory, the output, any seeded files.
type DataRemover interface {
	// Hold stops whatever still works on the Download's files (its fetch,
	// or the torrent engine) and keeps it stopped. release undoes the hold;
	// it is called when the removal does not go ahead. After a removal the
	// hold ends when core.Removed is published.
	Hold(ctx context.Context, d core.Download) (release func(), err error)
	// RemoveData deletes the files. It is called with the hold in place and
	// before the Download row is deleted, so it can still read the row's
	// paths. An error keeps the row, so the user can retry.
	RemoveData(ctx context.Context, d core.Download) error
}

type noopRemover struct{}

func (noopRemover) Hold(context.Context, core.Download) (func(), error) { return func() {}, nil }
func (noopRemover) RemoveData(context.Context, core.Download) error     { return nil }

type options struct {
	now     func() time.Time
	remover DataRemover

	// Used by the AddService.
	log          *slog.Logger
	httpClient   *http.Client
	fetchTimeout time.Duration
	maxNZBBytes  int64

	// torrentSources enables adding torrent Downloads.
	torrentSources core.TorrentSourceRepo
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

// WithLogger sets the logger of services that log. The default is
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.log = l }
}

// WithHTTPClient sets the client that fetches NZB URLs. Its redirect policy
// and cookie jar are always replaced by the AddService's own, so a custom
// client can change the transport (and tests the timeout), not the safety
// rules.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithFetchTimeout sets the total time an NZB URL fetch may take, including
// reading the body. The default is 30 seconds.
func WithFetchTimeout(d time.Duration) Option {
	return func(o *options) { o.fetchTimeout = d }
}

// WithMaxNZBBytes sets the size cap of an NZB, uploaded or fetched. The
// default is 64 MiB.
func WithMaxNZBBytes(n int64) Option {
	return func(o *options) { o.maxNZBBytes = n }
}

// WithTorrentSources enables torrent Downloads in the AddService: it stores
// their sources (the info hash, the .torrent path and the sealed magnet link)
// in repo. Without it StageTorrent, StageMagnet and AddTorrent fail with an
// error of kind ErrUnavailable.
func WithTorrentSources(repo core.TorrentSourceRepo) Option {
	return func(o *options) { o.torrentSources = repo }
}

func newOptions(opts []Option) options {
	o := options{
		now: func() time.Time { return time.Now().UTC() }, remover: noopRemover{},
		fetchTimeout: DefaultFetchTimeout, maxNZBBytes: DefaultMaxNZBBytes,
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.log == nil {
		o.log = slog.Default()
	}
	return o
}
