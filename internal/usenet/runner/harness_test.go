package runner_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// fakeServers is a mutable ServerSource.
type fakeServers struct {
	mu    sync.Mutex
	specs []runner.ServerSpec
	err   error
}

func (f *fakeServers) Servers(context.Context) ([]runner.ServerSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.ServerSpec(nil), f.specs...), f.err
}

// fakeSources is a mutable NZBSource.
type fakeSources struct {
	mu    sync.Mutex
	paths map[core.ID]string
}

func (f *fakeSources) Source(_ context.Context, id core.ID) (string, core.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.paths[id]
	if !ok {
		return "", "", fmt.Errorf("no source for %s: %w", id, core.ErrNotFound)
	}
	return p, "", nil
}

// harness wires a Runner to a real store, a bus and in-memory NNTP servers.
type harness struct {
	t        *testing.T
	db       *store.DB
	repo     *store.DownloadRepo
	bus      *core.Bus
	servers  *fakeServers
	sources  *fakeSources
	root     string
	nntp     map[string]*nntptest.Server
	mu       sync.Mutex
	settings core.Settings
	freeMu   sync.Mutex
	freeVal  uint64
	cfg      runner.Config // adjusted by tests before start
	runner   *runner.Runner
	cancel   context.CancelFunc
	done     chan error
	seq      int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := &harness{
		t:       t,
		db:      db,
		repo:    store.NewDownloadRepo(db),
		bus:     core.NewBus(),
		servers: &fakeServers{},
		sources: &fakeSources{paths: map[core.ID]string{}},
		root:    t.TempDir(),
		nntp:    map[string]*nntptest.Server{},
	}
	h.settings = core.DefaultSettings()
	h.settings.Paths.Usenet.Incomplete = filepath.Join(h.root, "incomplete")
	h.settings.Paths.Usenet.Complete = filepath.Join(h.root, "complete")
	h.cfg = runner.Config{
		Downloads:    h.repo,
		Bus:          h.bus,
		Servers:      h.servers,
		Sources:      h.sources,
		DirFor:       h.dirFor,
		Settings:     h.getSettings,
		Factory:      h.factory,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff:      func(int) time.Duration { return 0 },
		PollInterval: 50 * time.Millisecond,
		FreeSpace:    h.freeSpace,
	}
	h.freeVal = 1 << 40
	return h
}

func (h *harness) freeSpace(string) (uint64, error) {
	h.freeMu.Lock()
	defer h.freeMu.Unlock()
	return h.freeVal, nil
}

// setFree changes the free space the disk guard sees.
func (h *harness) setFree(n uint64) {
	h.freeMu.Lock()
	h.freeVal = n
	h.freeMu.Unlock()
}

func (h *harness) dirFor(d core.Download) string {
	return filepath.Join(h.settings.Paths.Usenet.Incomplete, string(d.ID))
}

func (h *harness) getSettings(context.Context) (core.Settings, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings, nil
}

// setSettings changes the settings and publishes SettingsChanged.
func (h *harness) setSettings(f func(*core.Settings)) {
	h.mu.Lock()
	f(&h.settings)
	h.mu.Unlock()
	h.bus.Publish(core.SettingsChanged{})
}

func (h *harness) factory(spec runner.ServerSpec) nntp.ConnFactory {
	return h.nntp[spec.Name].Factory()
}

// addServer registers an in-memory Server with the given priority and
// connection limit, and returns it.
func (h *harness) addServer(name string, priority, conns int) *nntptest.Server {
	h.t.Helper()
	srv := nntptest.NewServer(name)
	h.nntp[name] = srv
	h.servers.mu.Lock()
	h.servers.specs = append(h.servers.specs, runner.ServerSpec{
		Name: name, Host: name + ".example", Port: 563, User: "u", Pass: "p",
		Priority: priority, MaxConnections: conns, Enabled: true,
	})
	h.servers.mu.Unlock()
	return srv
}

// addDownload stores a Queued NZB Download whose NZB is fx.NZB and announces
// it on the bus.
func (h *harness) addDownload(name string, fx nntptest.Fixture, mod ...func(*core.Download)) core.Download {
	h.t.Helper()
	h.seq++
	at := time.Date(2026, 3, 1, 12, 0, h.seq, 0, time.UTC)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at,
		QueuePosition: h.seq,
	}
	for _, m := range mod {
		m(&d)
	}
	dir := h.dirFor(d)
	require.NoError(h.t, os.MkdirAll(dir, 0o777))
	p := filepath.Join(dir, name+".nzb")
	require.NoError(h.t, os.WriteFile(p, fx.NZB, 0o600))
	h.sources.mu.Lock()
	h.sources.paths[d.ID] = p
	h.sources.mu.Unlock()
	require.NoError(h.t, h.repo.Create(context.Background(), d))
	h.bus.Publish(core.DownloadAdded{Download: d})
	return d
}

// start builds the Runner (once) and runs it until the test ends.
func (h *harness) start() *runner.Runner {
	h.t.Helper()
	r, err := runner.New(h.cfg)
	require.NoError(h.t, err)
	h.runner = r
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- r.Run(ctx) }()
	h.t.Cleanup(h.stop)
	return r
}

// stop cancels Run and waits for it.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("Run did not return after cancel")
	}
}

func (h *harness) get(id core.ID) core.Download {
	h.t.Helper()
	d, err := h.repo.Get(context.Background(), id)
	require.NoError(h.t, err)
	return d
}

// waitState blocks until the Download is in state.
func (h *harness) waitState(id core.ID, state core.State) core.Download {
	h.t.Helper()
	var d core.Download
	require.Eventually(h.t, func() bool {
		var err error
		d, err = h.repo.Get(context.Background(), id)
		return err == nil && d.State == state
	}, 15*time.Second, 5*time.Millisecond, "download %s never reached %s", id, state)
	return d
}

// recorder collects bus events.
type recorder struct {
	mu     sync.Mutex
	events []core.Event
}

func (h *harness) record() *recorder {
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	sub := h.bus.Subscribe(ctx)
	h.t.Cleanup(cancel)
	go func() {
		for ev := range sub.Events() {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *recorder) all() []core.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Event(nil), r.events...)
}

// readDir returns name -> content of the regular files in dir, without .nzb.
func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string][]byte{}
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) == ".nzb" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out[e.Name()] = b
	}
	return out
}

// payload returns n deterministic pseudo-random bytes.
func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// update changes a stored Download and announces it, as the API would.
func (h *harness) update(id core.ID, f func(*core.Download)) core.Download {
	h.t.Helper()
	d := h.get(id)
	f(&d)
	require.NoError(h.t, h.repo.Update(context.Background(), d))
	d = h.get(id)
	h.bus.Publish(core.DownloadUpdated{Download: d})
	return d
}

// stable waits until f() stops changing for 150 ms and returns the value.
func stable(t *testing.T, f func() int) int {
	t.Helper()
	last, since := f(), time.Now()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		if v := f(); v != last {
			last, since = v, time.Now()
		} else if time.Since(since) > 150*time.Millisecond {
			return last
		}
	}
	t.Fatal("value never settled")
	return 0
}

// fakeProgress is a runner.ProgressStore in memory. Freeze makes later saves
// vanish, which plays a crash that happens between two periodic saves.
type fakeProgress struct {
	mu      sync.Mutex
	blobs   map[core.ID][]byte
	frozen  bool
	saves   int
	deletes int
}

func newFakeProgress() *fakeProgress { return &fakeProgress{blobs: map[core.ID][]byte{}} }

func (f *fakeProgress) Load(_ context.Context, id core.ID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[id]
	if !ok {
		return nil, fmt.Errorf("progress %s: %w", id, core.ErrNotFound)
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeProgress) Save(_ context.Context, id core.ID, blob []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return nil
	}
	f.saves++
	f.blobs[id] = append([]byte(nil), blob...)
	return nil
}

func (f *fakeProgress) Delete(_ context.Context, id core.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozen {
		return nil
	}
	f.deletes++
	delete(f.blobs, id)
	return nil
}

func (f *fakeProgress) freeze() {
	f.mu.Lock()
	f.frozen = true
	f.mu.Unlock()
}

func (f *fakeProgress) thaw() {
	f.mu.Lock()
	f.frozen = false
	f.mu.Unlock()
}

func (f *fakeProgress) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

func (f *fakeProgress) has(id core.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.blobs[id]
	return ok
}
