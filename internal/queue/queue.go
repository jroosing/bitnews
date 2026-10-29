package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
)

// Pause reasons. A caller may use any other reason too; each one is held
// separately.
const (
	// ReasonUser is the user's pause of the whole Queue.
	ReasonUser = "user"
	// ReasonDiskFull is the disk guard's: the incomplete root is short of
	// space.
	ReasonDiskFull = "disk_full"
	// ReasonBindUnavailable is the network monitor's: the protocol's bound
	// interface or address is gone.
	ReasonBindUnavailable = "bind_unavailable"
)

// ErrNotNow is returned by Protocol.Start to skip a Download for this pass,
// for example while it cools down after a failure. The Queue leaves it as
// it is and tries again on a later pass.
var ErrNotNow = errors.New("queue: not now")

// Protocol is what one protocol's runner gives its Queue.
type Protocol interface {
	// Prepare runs at the start of each pass, before the Queue lists its
	// Downloads. canStart false keeps the pass from starting fetches (it
	// still stops the ones that must stop), for example while no Server is
	// configured.
	Prepare(ctx context.Context) (canStart bool)
	// Exclude reports whether the Download is out of the Queue's hands for
	// this pass, for example a torrent that is already Seeding.
	Exclude(id core.ID) bool
	// Running reports whether a fetch of the Download is running.
	Running(id core.ID) bool
	// Start starts the fetch of d, which the Queue has just moved to
	// Fetching (or found Fetching without a running fetch). An error other
	// than ErrNotNow is logged; the protocol records any failure itself.
	Start(ctx context.Context, d core.Download) error
	// Stop stops the fetch of the Download. The Queue moves it to Queued.
	Stop(ctx context.Context, id core.ID)
	// Abandon stops the fetch, forgets its resume state and returns once it
	// has stopped (ctx bounds the wait). It is called by Hold.
	Abandon(ctx context.Context, id core.ID) error
}

// Config is what New needs. Every field but Logger, MinFree, FreeSpace,
// Now, Wake and the intervals is required.
type Config struct {
	// Source is the kind of the Queue's Downloads.
	Source    core.SourceKind
	Downloads core.DownloadRepo
	Bus       *core.Bus
	Settings  func(context.Context) (core.Settings, error)
	Protocol  Protocol
	// MinFree is the free space to keep on the incomplete root. Nil uses
	// core.DefaultMinFree.
	MinFree func(context.Context) (uint64, error)
	// FreeSpace reports the free bytes of the filesystem holding path. Nil
	// turns the disk guard off; an error makes it pass (and is logged).
	FreeSpace func(path string) (uint64, error)
	Now       func() time.Time
	// Wake asks the runner for another pass.
	Wake   func()
	Logger *slog.Logger
	// ProgressInterval throttles the progress events of one Download; zero
	// is core.DefaultProgressInterval. DoneBytesInterval is how often its
	// DoneBytes is stored; zero is DefaultDoneBytesInterval.
	ProgressInterval  time.Duration
	DoneBytesInterval time.Duration
}

// Queue is the Queue of one Source kind. Its methods are safe for concurrent
// use; Reconcile must be called from one goroutine at a time.
type Queue struct {
	cfg Config
	log *slog.Logger

	throttle *core.ProgressThrottle

	mu      sync.Mutex
	reasons map[string]string // Pause reason -> detail
	held    map[core.ID]time.Time
	stored  map[core.ID]time.Time // when Progress last stored DoneBytes
}

// New returns the Queue described by cfg.
func New(cfg Config) (*Queue, error) {
	if cfg.Source == "" || cfg.Downloads == nil || cfg.Bus == nil || cfg.Settings == nil || cfg.Protocol == nil {
		return nil, errors.New("queue: Config is missing a required field")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wake == nil {
		cfg.Wake = func() {}
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = core.DefaultProgressInterval
	}
	if cfg.DoneBytesInterval <= 0 {
		cfg.DoneBytesInterval = DefaultDoneBytesInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Queue{
		cfg: cfg, log: log.With("queue", string(cfg.Source)),
		throttle: core.NewProgressThrottle(cfg.Now, cfg.ProgressInterval),
		reasons:  map[string]string{}, held: map[core.ID]time.Time{}, stored: map[core.ID]time.Time{},
	}, nil
}

// limit is the Queue's concurrency limit.
func (q *Queue) limit(s core.Settings) int {
	if q.cfg.Source == core.SourceTorrent {
		return s.Queues.Torrent
	}
	return s.Queues.Usenet
}

// root is the incomplete root the disk guard watches.
func (q *Queue) root(s core.Settings) string {
	if q.cfg.Source == core.SourceTorrent {
		return s.Paths.Torrent.Incomplete
	}
	return s.Paths.Usenet.Incomplete
}

// Reconcile makes the running fetches match the Queue's policy. It is one
// pass of the runner's control loop.
func (q *Queue) Reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	settings, err := q.cfg.Settings(ctx)
	if err != nil {
		q.log.Error("queue: cannot read settings", "err", err)
		return
	}
	canStart := q.cfg.Protocol.Prepare(ctx)
	ds, err := q.list(ctx)
	if err != nil {
		if ctx.Err() == nil {
			q.log.Error("queue: cannot list downloads", "err", err)
		}
		return
	}
	ds = slices.DeleteFunc(ds, func(d core.Download) bool {
		return q.Held(d.ID) || q.cfg.Protocol.Exclude(d.ID)
	})
	byID := make(map[core.ID]core.Download, len(ds))
	for _, d := range ds {
		byID[d.ID] = d
	}

	activate, deactivate := core.Select(ds, q.limit(settings))
	root := q.root(settings)
	if q.diskHeld() && q.nextFits(ctx, root, ds, activate) {
		q.Resume(ReasonDiskFull)
	}
	if paused, _, _ := q.Paused(); paused {
		activate, deactivate = nil, nil
		for _, d := range ds {
			if d.State == core.StateFetching {
				deactivate = append(deactivate, d.ID)
			}
		}
		canStart = false
	}
	for _, id := range deactivate {
		q.deactivate(ctx, id)
	}
	// A Queued Download whose fetch is running (a torrent whose metadata
	// just arrived) but was not chosen is stopped.
	for _, d := range ds {
		if d.State == core.StateQueued && !slices.Contains(activate, d.ID) && q.cfg.Protocol.Running(d.ID) {
			q.cfg.Protocol.Stop(ctx, d.ID)
		}
	}
	if !canStart {
		return
	}
	// Fetching Downloads without a running fetch (left by a crash, or by a
	// fetch that ended without a state change) are started again.
	chosen := slices.Clone(activate)
	for _, d := range ds {
		if d.State == core.StateFetching && !q.cfg.Protocol.Running(d.ID) && !slices.Contains(deactivate, d.ID) {
			chosen = append(chosen, d.ID)
		}
	}
	for _, id := range chosen {
		d := byID[id]
		if ok, detail := q.fits(ctx, root, d.TotalBytes-d.DoneBytes); !ok {
			q.Pause(ReasonDiskFull, detail)
			for _, other := range ds {
				if other.State == core.StateFetching || q.cfg.Protocol.Running(other.ID) {
					q.deactivate(ctx, other.ID)
				}
			}
			return
		}
		q.start(ctx, d)
	}
}

// start moves d to Fetching when it is Queued and asks the protocol to
// start it.
func (q *Queue) start(ctx context.Context, d core.Download) {
	if d.State == core.StateQueued {
		var err error
		d, err = q.SetState(ctx, d.ID, core.StateChange{To: core.StateFetching, At: q.cfg.Now()})
		if err != nil {
			q.log.Warn("queue: cannot activate download", "id", d.ID, "err", err)
			return
		}
	} else {
		// The list may be stale: a fetch that finished meanwhile left the
		// Download Fetched, and starting it again would wipe its files.
		cur, err := q.cfg.Downloads.Get(ctx, d.ID)
		if err != nil || cur.State != core.StateFetching || q.cfg.Protocol.Running(d.ID) {
			return
		}
		d = cur
	}
	if err := q.cfg.Protocol.Start(ctx, d); err != nil && !errors.Is(err, ErrNotNow) {
		q.log.Warn("queue: cannot start download", "id", d.ID, "err", err)
	}
}

// deactivate stops the fetch of id and moves the Download back to Queued.
func (q *Queue) deactivate(ctx context.Context, id core.ID) {
	q.cfg.Protocol.Stop(ctx, id)
	_, err := q.SetState(ctx, id, core.StateChange{To: core.StateQueued, At: q.cfg.Now()})
	if err != nil && !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrIllegalTransition) {
		q.log.Warn("queue: cannot deactivate download", "id", id, "err", err)
	}
}

// list returns the Queue's Queued and Fetching Downloads.
func (q *Queue) list(ctx context.Context) ([]core.Download, error) {
	var out []core.Download
	for _, st := range []core.State{core.StateQueued, core.StateFetching} {
		page, err := core.ListAll(ctx, q.cfg.Downloads, core.DownloadQuery{State: st, SourceKind: q.cfg.Source})
		if err != nil {
			return nil, fmt.Errorf("queue: list %s downloads: %w", st, err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// SetState applies c to the stored Download and publishes DownloadUpdated.
// It must not be tied to a context that is being cancelled.
func (q *Queue) SetState(ctx context.Context, id core.ID, c core.StateChange) (core.Download, error) {
	d, err := q.cfg.Downloads.UpdateState(ctx, id, c)
	if err != nil {
		return core.Download{}, err
	}
	q.cfg.Bus.Publish(core.DownloadUpdated{Download: d})
	return d, nil
}

// Held reports whether Hold keeps the Download from being started.
func (q *Queue) Held(id core.ID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	until, ok := q.held[id]
	if !ok {
		return false
	}
	if !q.cfg.Now().Before(until) {
		delete(q.held, id)
		return false
	}
	return true
}

// Pause adds a Pause reason. The Queue starts nothing while any reason is
// held, and its next pass stops the running fetches. QueuePaused is published
// when the reason is new.
func (q *Queue) Pause(reason, detail string) {
	q.mu.Lock()
	_, had := q.reasons[reason]
	q.reasons[reason] = detail
	q.mu.Unlock()
	if had {
		return
	}
	q.log.Info("queue paused", "reason", reason, "detail", detail)
	q.cfg.Bus.Publish(core.QueuePaused{Source: q.cfg.Source, Reason: reason, Detail: detail})
	q.cfg.Wake()
}

// Resume releases a Pause reason. QueueResumed is published when it was the
// last one.
func (q *Queue) Resume(reason string) {
	q.mu.Lock()
	_, had := q.reasons[reason]
	delete(q.reasons, reason)
	last := len(q.reasons) == 0
	q.mu.Unlock()
	if !had {
		return
	}
	if last {
		q.log.Info("queue resumed")
		q.cfg.Bus.Publish(core.QueueResumed{Source: q.cfg.Source})
	}
	q.cfg.Wake()
}

// Paused reports whether a Pause reason is held, and the one to show: the
// disk guard first, then any reason other than the user's (alphabetically),
// then the user's.
func (q *Queue) Paused() (paused bool, reason, detail string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.reasons) == 0 {
		return false, "", ""
	}
	if d, ok := q.reasons[ReasonDiskFull]; ok {
		return true, ReasonDiskFull, d
	}
	for r, d := range q.reasons {
		if r != ReasonUser && (reason == "" || r < reason) {
			reason, detail = r, d
		}
	}
	if reason == "" {
		return true, ReasonUser, q.reasons[ReasonUser]
	}
	return true, reason, detail
}

// Fits applies the disk guard to a running fetch that still needs remaining
// bytes: it pauses the Queue with ReasonDiskFull when the incomplete root no
// longer has remaining plus min_free bytes free.
func (q *Queue) Fits(ctx context.Context, remaining int64) bool {
	settings, err := q.cfg.Settings(ctx)
	if err != nil {
		return true
	}
	ok, detail := q.fits(ctx, q.root(settings), remaining)
	if !ok {
		q.Pause(ReasonDiskFull, detail)
	}
	return ok
}

// DiskFull pauses the Queue with ReasonDiskFull, for a fetch that failed
// with "no space left".
func (q *Queue) DiskFull(detail string) { q.Pause(ReasonDiskFull, detail) }

func (q *Queue) diskHeld() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.reasons[ReasonDiskFull]
	return ok
}

// nextFits reports whether the Download that would be started next fits on
// disk again: the first one Select chose, else the first unpaused one in queue
// order (a fetch that failed with "no space left" is still Fetching when the
// guard is checked).
func (q *Queue) nextFits(ctx context.Context, root string, ds []core.Download, activate []core.ID) bool {
	var remaining int64
	var next *core.Download
	if len(activate) > 0 {
		next = downloadByID(ds, activate[0])
	} else {
		for i := range ds {
			if !ds[i].Paused && (next == nil || core.CompareQueueOrder(ds[i], *next) < 0) {
				next = &ds[i]
			}
		}
	}
	if next != nil {
		remaining = next.TotalBytes - next.DoneBytes
	}
	ok, _ := q.fits(ctx, root, remaining)
	return ok
}

func downloadByID(ds []core.Download, id core.ID) *core.Download {
	for i := range ds {
		if ds[i].ID == id {
			return &ds[i]
		}
	}
	return nil
}

// fits applies the disk guard: root must have at least remaining plus
// min_free bytes free. It passes when the guard is off or free space cannot
// be read.
func (q *Queue) fits(ctx context.Context, root string, remaining int64) (ok bool, detail string) {
	if q.cfg.FreeSpace == nil {
		return true, ""
	}
	free, err := q.cfg.FreeSpace(root)
	if errors.Is(err, fsutil.ErrUnsupported) {
		return true, ""
	}
	if err != nil {
		q.log.Warn("queue: disk guard cannot read free space", "path", root, "err", err)
		return true, ""
	}
	need := uint64(max(remaining, 0)) + q.minFree(ctx)
	if free >= need {
		return true, ""
	}
	return false, fmt.Sprintf("%d bytes free, %d needed", free, need)
}

func (q *Queue) minFree(ctx context.Context) uint64 {
	if q.cfg.MinFree == nil {
		return core.DefaultMinFree
	}
	v, err := q.cfg.MinFree(ctx)
	if err != nil {
		q.log.Warn("queue: min_free unavailable, using the default", "err", err)
		return core.DefaultMinFree
	}
	return v
}

// holdTTL is how long a Hold lasts unless Release ends it first. It keeps a
// Download whose removal failed half-way from staying parked for ever.
const holdTTL = 2 * time.Minute

// Hold keeps the Download from being started until Release or holdTTL, and
// abandons its running fetch, waiting until it has stopped (ctx bounds the
// wait). Call it before deleting the files of a Download that still exists.
func (q *Queue) Hold(ctx context.Context, id core.ID) error {
	q.mu.Lock()
	q.held[id] = q.cfg.Now().Add(holdTTL)
	q.mu.Unlock()
	if !q.cfg.Protocol.Running(id) {
		return nil
	}
	if err := q.cfg.Protocol.Abandon(ctx, id); err != nil {
		return fmt.Errorf("queue: hold %s: %w", id, err)
	}
	return nil
}

// Release undoes Hold. It reports whether the Download was held.
func (q *Queue) Release(id core.ID) bool {
	q.mu.Lock()
	_, was := q.held[id]
	delete(q.held, id)
	q.mu.Unlock()
	if was {
		q.cfg.Wake()
	}
	return was
}

// Recover moves the Queue's Fetching Downloads back to Queued. Call it once
// at start-up, before the first pass: those Downloads were interrupted by a
// shutdown or a crash.
func (q *Queue) Recover(ctx context.Context) error {
	var errs []error
	fetching, err := core.ListAll(ctx, q.cfg.Downloads, core.DownloadQuery{State: core.StateFetching, SourceKind: q.cfg.Source})
	if err != nil {
		return fmt.Errorf("queue: recover: %w", err)
	}
	for _, d := range fetching {
		if _, err := q.SetState(ctx, d.ID, core.StateChange{To: core.StateQueued, At: q.cfg.Now()}); err != nil {
			errs = append(errs, fmt.Errorf("queue: recover %s: %w", d.ID, err))
		}
	}
	return errors.Join(errs...)
}
