package runner

import (
	"context"
	"log/slog"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// Remover removes a Download when its Share Limit action says so. The daemon
// implements it with the same code as DELETE /api/v1/downloads/{id}: the
// Download goes away and the Removed event follows, which makes the runner
// drop the torrent from the engine.
type Remover interface {
	Remove(ctx context.Context, id core.ID, deleteData bool) error
}

// Config configures a Runner. Downloads, Sources, Records, Engine, Bus and
// Settings are required.
type Config struct {
	Downloads core.DownloadRepo
	Sources   core.TorrentSourceRepo
	// Records keeps the Seeding counters, the per-Download Share Limit and the
	// location of the data (migration 0007).
	Records core.TorrentRecordRepo
	Engine  core.TorrentEngine
	Bus     *core.Bus
	// Settings returns the current Settings; the runner uses the torrent
	// Queue limit, the torrent roots and the Seeding defaults.
	Settings func(ctx context.Context) (core.Settings, error)

	// Destination returns the directory a Download's torrent is moved to
	// when it is Fetched, before the name is made unique. The daemon passes
	// finalize.Finisher.Destination, so the runner and the Finisher agree on
	// <complete root>/<category>/<name>. Nil disables the move.
	Destination func(ctx context.Context, d core.Download) (string, error)
	// Remover carries out the remove and remove_data Share Limit actions.
	// Nil turns them into pause.
	Remover Remover

	// MinFree returns the free space in bytes that must stay free on top of
	// a Download's remaining bytes. Nil means core.DefaultMinFree.
	MinFree func(ctx context.Context) (uint64, error)
	// FreeSpace reports the free bytes of the filesystem holding path. The
	// default is fsutil.FreeBytes; where that is unsupported
	// the disk guard is off.
	FreeSpace func(path string) (uint64, error)
	// Now is the clock. Nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger

	// PollInterval is the control loop's fallback poll (default 10 s).
	PollInterval time.Duration
	// SampleInterval is how often the engine's stats of running torrents are
	// read for progress events (default 1 s).
	SampleInterval time.Duration
	// ProgressInterval is the throttle of DownloadProgress events per
	// Download (default one second).
	ProgressInterval time.Duration
	// DoneBytesInterval is how often Download.DoneBytes is written to the
	// repository (default 2 s).
	DoneBytesInterval time.Duration
	// PersistInterval is how often the Seeding counters are written to the
	// Records (default 30 s). They are also written when a torrent is
	// Fetched and on shutdown.
	PersistInterval time.Duration
	// LimitInterval is how often Share Limits are evaluated (default 30 s).
	LimitInterval time.Duration
	// DiskCheckInterval is how often a running Download rechecks free space
	// (default queue.DefaultDiskCheckInterval).
	DiskCheckInterval time.Duration
}

// Defaults.
const (
	defaultPollInterval      = 10 * time.Second
	defaultSampleInterval    = time.Second
	defaultDoneBytesInterval = 2 * time.Second
	defaultPersistInterval   = 30 * time.Second
	defaultLimitInterval     = 30 * time.Second
)

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
