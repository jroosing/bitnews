package queue_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/queue"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

// fakeProtocol records what the Queue asks of it. Running fetches are the
// ones it was told to Start and not told to Stop or Abandon.
type fakeProtocol struct {
	mu        sync.Mutex
	running   map[core.ID]bool
	excluded  map[core.ID]bool
	notNow    map[core.ID]bool
	failStart map[core.ID]error
	canStart  bool
	calls     []string
}

func newFakeProtocol() *fakeProtocol {
	return &fakeProtocol{running: map[core.ID]bool{}, excluded: map[core.ID]bool{}, notNow: map[core.ID]bool{}, failStart: map[core.ID]error{}, canStart: true}
}

func (p *fakeProtocol) record(format string, args ...any) {
	p.calls = append(p.calls, fmt.Sprintf(format, args...))
}

func (p *fakeProtocol) Prepare(context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canStart
}

func (p *fakeProtocol) Exclude(id core.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.excluded[id]
}

func (p *fakeProtocol) Running(id core.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running[id]
}

func (p *fakeProtocol) Start(_ context.Context, d core.Download) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notNow[d.ID] {
		return queue.ErrNotNow
	}
	if err := p.failStart[d.ID]; err != nil {
		return err
	}
	p.record("start %s %s", d.Name, d.State)
	p.running[d.ID] = true
	return nil
}

func (p *fakeProtocol) Stop(_ context.Context, id core.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("stop %s", id)
	delete(p.running, id)
}

func (p *fakeProtocol) Abandon(_ context.Context, id core.ID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("abandon %s", id)
	delete(p.running, id)
	return nil
}

// isRunning lists the names of the running Downloads, sorted.
func (h *harness) running() []string {
	h.proto.mu.Lock()
	defer h.proto.mu.Unlock()
	var out []string
	for id := range h.proto.running {
		out = append(out, h.names[id])
	}
	slices.Sort(out)
	return out
}

type harness struct {
	t         *testing.T
	ctx       context.Context
	downloads *store.DownloadRepo
	bus       *core.Bus
	events    *core.Subscription
	proto     *fakeProtocol
	q         *queue.Queue
	settings  core.Settings
	now       time.Time
	free      map[string]uint64
	freeErr   error
	minFree   uint64
	woken     int
	names     map[core.ID]string
	seq       int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := &harness{
		t: t, ctx: ctx, downloads: store.NewDownloadRepo(db), bus: core.NewBus(),
		proto: newFakeProtocol(), settings: core.DefaultSettings(),
		now:  time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		free: map[string]uint64{}, names: map[core.ID]string{},
	}
	h.settings.Queues.Usenet = 2
	h.settings.Paths.Usenet.Incomplete = "/data/usenet/incomplete"
	h.events = h.bus.Subscribe(ctx)
	q, err := queue.New(queue.Config{
		Source:    core.SourceNZB,
		Downloads: h.downloads,
		Bus:       h.bus,
		Settings:  func(context.Context) (core.Settings, error) { return h.settings, nil },
		MinFree:   func(context.Context) (uint64, error) { return h.minFree, nil },
		FreeSpace: func(path string) (uint64, error) {
			if h.freeErr != nil {
				return 0, h.freeErr
			}
			if v, ok := h.free[path]; ok {
				return v, nil
			}
			return 1 << 50, nil
		},
		Now:      func() time.Time { return h.now },
		Wake:     func() { h.woken++ },
		Protocol: h.proto,
	})
	require.NoError(t, err)
	h.q = q
	return h
}

type dlOpt func(*core.Download)

func withState(s core.State) dlOpt               { return func(d *core.Download) { d.State = s } }
func withPriority(p core.DownloadPriority) dlOpt { return func(d *core.Download) { d.Priority = p } }
func withSize(total, done int64) dlOpt {
	return func(d *core.Download) { d.TotalBytes, d.DoneBytes = total, done }
}
func withSource(k core.SourceKind) dlOpt { return func(d *core.Download) { d.SourceKind = k } }
func paused() dlOpt                      { return func(d *core.Download) { d.Paused = true } }

// add stores a Download named name, Queued and normal priority by default,
// added in call order.
func (h *harness) add(name string, opts ...dlOpt) core.ID {
	h.t.Helper()
	h.seq++
	at := time.Date(2026, 10, 1, 0, 0, h.seq, 0, time.UTC)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceNZB,
		State: core.StateQueued, Priority: core.DownloadPriorityNormal,
		TotalBytes: 1000, AddedAt: at, UpdatedAt: at,
	}
	for _, o := range opts {
		o(&d)
	}
	require.NoError(h.t, h.downloads.Create(h.ctx, d))
	h.names[d.ID] = name
	return d.ID
}

func (h *harness) state(id core.ID) core.State {
	h.t.Helper()
	d, err := h.downloads.Get(h.ctx, id)
	require.NoError(h.t, err)
	return d.State
}

// queueEvents drains the bus and returns the Queue events, rendered as
// "paused <reason>" and "resumed".
func (h *harness) queueEvents() []string {
	var out []string
	for {
		select {
		case ev := <-h.events.Events():
			switch e := ev.(type) {
			case core.QueuePaused:
				out = append(out, fmt.Sprintf("paused %s %s", e.Source, e.Reason))
			case core.QueueResumed:
				out = append(out, fmt.Sprintf("resumed %s", e.Source))
			}
		default:
			return out
		}
	}
}

var errBoom = errors.New("boom")
