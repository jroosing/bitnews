package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
)

// ServerSpec describes one configured Server. The runner builds one
// connection pool per Server from it. Name identifies the Server and must be
// unique.
type ServerSpec struct {
	Name string
	Host string
	Port int
	User string
	Pass string
	// Priority is the Server Priority: lower values are tried first.
	Priority       int
	MaxConnections int
	// RetentionDays is 0 for unlimited retention.
	RetentionDays int
	Enabled       bool
}

// String implements fmt.Stringer without the password, so a spec that is
// formatted by accident does not leak it.
func (s ServerSpec) String() string {
	return fmt.Sprintf("server %s (%s:%d, priority %d, %d connections)", s.Name, s.Host, s.Port, s.Priority, s.MaxConnections)
}

// GoString implements fmt.GoStringer for %#v.
func (s ServerSpec) GoString() string { return s.String() }

// LogValue implements slog.LogValuer, so slog never shows the password.
func (s ServerSpec) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", s.Name), slog.String("host", s.Host), slog.Int("port", s.Port),
		slog.Int("priority", s.Priority), slog.Int("connections", s.MaxConnections),
		slog.Bool("enabled", s.Enabled),
	)
}

// ServerSource lists the configured Servers, enabled or not. The runner
// reads it on every control-loop pass, so changes apply without a restart.
type ServerSource interface {
	Servers(ctx context.Context) ([]ServerSpec, error)
}

// NZBSource finds the stored NZB of a Download.
type NZBSource interface {
	// Source returns the path of the stored NZB file and the Download's
	// archive password (unused by the fetch itself; it is part of the
	// contract so one implementation serves post-processing too).
	Source(ctx context.Context, downloadID core.ID) (nzbPath string, password core.Secret, err error)
}

// ProgressStore persists the resume state of Downloads (M2-12). A nil store
// in Config keeps that state in memory only, which still lets a Download
// continue after a pause or a pre-emption, but not after a restart.
type ProgressStore interface {
	// Load returns the blob saved for the Download, or an error wrapping
	// core.ErrNotFound when there is none.
	Load(ctx context.Context, id core.ID) ([]byte, error)
	Save(ctx context.Context, id core.ID, blob []byte) error
	// Delete forgets the blob; deleting an unknown ID is not an error.
	Delete(ctx context.Context, id core.ID) error
}

// Clock is the time source of the runner and its rate limiter. It is the
// same interface the connection pool uses.
type Clock = nntp.Clock

// Config configures a Runner. Downloads, Bus, Servers, Sources, DirFor and
// Settings are required.
type Config struct {
	Downloads core.DownloadRepo
	Bus       *core.Bus
	Servers   ServerSource
	Sources   NZBSource
	// DirFor returns the working directory of a Download, normally
	// <incomplete root>/<download>. The stored .nzb lives in it too.
	DirFor func(core.Download) string
	// Settings returns the current Settings; the runner uses the Usenet
	// Queue limit and the Usenet incomplete root.
	Settings func(ctx context.Context) (core.Settings, error)
	// MinFree returns the free space in bytes that must stay free on top of
	// a Download's remaining bytes (the min_free setting). Nil means
	// DefaultMinFree.
	MinFree func(ctx context.Context) (uint64, error)
	// Factory opens connections to a Server. The default dials with strict
	// TLS (nntp.DialFactory); tests inject in-memory servers.
	Factory func(spec ServerSpec) nntp.ConnFactory
	// Progress persists resume state. Nil keeps it in memory only.
	Progress ProgressStore
	// Volumes persists which PAR2 recovery volumes were un-deferred (M3-07).
	// Nil keeps that in memory only.
	Volumes VolumeStore
	// Reports persists FetchReports so Post-processing still finds them
	// after a restart. Nil keeps them in memory only.
	Reports ReportStore
	// FreeSpace reports the free bytes of the filesystem holding path. The
	// default is fsutil.FreeBytes; where that is unsupported
	// the disk guard is off.
	FreeSpace func(path string) (uint64, error)
	Clock     Clock
	Logger    *slog.Logger
	// Backoff is passed to the Article scheduler as its retry backoff.
	Backoff func(attempt int) time.Duration

	// PollInterval is the control loop's fallback poll (default 10 s).
	PollInterval time.Duration
	// DiskCheckInterval is how often a running Download rechecks free space
	// (default 30 s).
	DiskCheckInterval time.Duration
	// SaveInterval is how often resume state is saved (default 10 s).
	SaveInterval time.Duration
	// DoneBytesInterval is how often Download.DoneBytes is written to the
	// repository (default 2 s).
	DoneBytesInterval time.Duration
	// FailureBackoff is how long a Download waits in Queued after every
	// Server turned out to be unusable (default 30 s).
	FailureBackoff time.Duration
}

// Defaults.
const (
	// DefaultMinFree is the min_free default: 5 GiB.
	DefaultMinFree uint64 = 5 << 30

	defaultPollInterval      = 10 * time.Second
	defaultDiskCheckInterval = 30 * time.Second
	defaultSaveInterval      = 10 * time.Second
	defaultDoneBytesInterval = 2 * time.Second
	defaultFailureBackoff    = 30 * time.Second
	noServersLogInterval     = time.Minute
	rateWindowSpan           = 5 * time.Second
)
