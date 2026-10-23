package runner_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/core/coretest"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/torrent/runner"
)

// fakeSources is an in-memory core.TorrentSourceRepo.
type fakeSources struct {
	mu sync.Mutex
	m  map[core.ID]core.TorrentSource
}

var _ core.TorrentSourceRepo = (*fakeSources)(nil)

func (f *fakeSources) Create(_ context.Context, s core.TorrentSource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.m {
		if o.InfoHash == s.InfoHash {
			return core.ErrConflict
		}
	}
	f.m[s.DownloadID] = s
	return nil
}

func (f *fakeSources) Get(_ context.Context, id core.ID) (core.TorrentSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[id]
	if !ok {
		return core.TorrentSource{}, fmt.Errorf("no source for %s: %w", id, core.ErrNotFound)
	}
	return s, nil
}

func (f *fakeSources) GetByInfoHash(_ context.Context, h core.InfoHash) (core.TorrentSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.m {
		if s.InfoHash == h {
			return s, nil
		}
	}
	return core.TorrentSource{}, core.ErrNotFound
}

func (f *fakeSources) SetTorrentPath(_ context.Context, id core.ID, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[id]
	if !ok {
		return core.ErrNotFound
	}
	s.TorrentPath = path
	f.m[id] = s
	return nil
}

// fakeRemover records Remove calls and deletes the Download like the daemon's
// Remover does.
type fakeRemover struct {
	mu    sync.Mutex
	calls []removeCall
	repo  core.DownloadRepo
	bus   *core.Bus
	err   error
}

type removeCall struct {
	ID         core.ID
	DeleteData bool
}

func (f *fakeRemover) Remove(ctx context.Context, id core.ID, deleteData bool) error {
	f.mu.Lock()
	f.calls = append(f.calls, removeCall{id, deleteData})
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if err := f.repo.Delete(ctx, id); err != nil {
		return err
	}
	f.bus.Publish(core.Removed{ID: id})
	return nil
}

func (f *fakeRemover) Calls() []removeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]removeCall(nil), f.calls...)
}

// harness wires a Runner to a real store, a bus and the fake engine.
type harness struct {
	t          *testing.T
	db         *store.DB
	downloads  *store.DownloadRepo
	records    *store.TorrentStateRepo
	sources    *fakeSources
	engine     *coretest.FakeTorrentEngine
	bus        *core.Bus
	remover    *fakeRemover
	root       string
	incomplete string
	complete   string

	mu       sync.Mutex
	settings core.Settings
	free     uint64
	clock    time.Time
	dest     func(context.Context, core.Download) (string, error)

	cfg    runner.Config
	runner *runner.Runner
	cancel context.CancelFunc
	done   chan error
	seq    int
	logs   *syncBuf

	stopOnCleanup bool
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	root := t.TempDir()
	h := &harness{
		t: t, db: db, downloads: store.NewDownloadRepo(db), records: store.NewTorrentStateRepo(db),
		sources: &fakeSources{m: map[core.ID]core.TorrentSource{}},
		engine:  coretest.NewFakeTorrentEngine(), bus: core.NewBus(), root: root,
		incomplete: filepath.Join(root, "incomplete"), complete: filepath.Join(root, "complete"),
		settings: core.DefaultSettings(), free: 1 << 40, clock: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		logs: &syncBuf{},
	}
	h.settings.Paths.Torrent = core.Roots{Incomplete: h.incomplete, Complete: h.complete}
	h.remover = &fakeRemover{repo: h.downloads, bus: h.bus}
	h.cfg = runner.Config{
		Downloads: h.downloads, Sources: h.sources, Records: h.records, Engine: h.engine, Bus: h.bus,
		Settings:  func(context.Context) (core.Settings, error) { h.mu.Lock(); defer h.mu.Unlock(); return h.settings, nil },
		MinFree:   func(context.Context) (uint64, error) { return 0, nil },
		FreeSpace: func(string) (uint64, error) { h.mu.Lock(); defer h.mu.Unlock(); return h.free, nil },
		Remover:   h.remover,
		Now:       func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
		Logger:    slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),

		PollInterval: 10 * time.Millisecond, SampleInterval: 10 * time.Millisecond,
		DoneBytesInterval: 10 * time.Millisecond, PersistInterval: 20 * time.Millisecond,
		LimitInterval: 20 * time.Millisecond, DiskCheckInterval: 10 * time.Millisecond,
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("runner log:\n%s", h.logs.String())
		}
	})
	return h
}

func (h *harness) setSettings(f func(*core.Settings)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.settings)
}

func (h *harness) setFree(v uint64) {
	h.mu.Lock()
	h.free = v
	h.mu.Unlock()
}

// start builds the Runner and runs it until the test ends.
func (h *harness) start() {
	h.t.Helper()
	h.startRunner()
	h.t.Cleanup(h.stop)
}

func (h *harness) build() {
	h.t.Helper()
	cfg := h.cfg
	if h.dest != nil {
		cfg.Destination = h.dest
	}
	r, err := runner.New(cfg)
	require.NoError(h.t, err)
	h.runner = r
}

func (h *harness) startRunner() {
	h.t.Helper()
	h.build()
	h.startRunnerBuilt()
}

// startRunnerBuilt runs the Runner that build made.
func (h *harness) startRunnerBuilt() {
	h.t.Helper()
	if h.stopOnCleanup == false {
		h.stopOnCleanup = true
		h.t.Cleanup(h.stop)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.runner.Run(ctx) }()
}

// stop ends the Run in progress and waits for it.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("runner did not stop")
	}
	h.cancel = nil
}

type dlOpt func(*core.Download)

func withState(s core.State) dlOpt   { return func(d *core.Download) { d.State = s } }
func withPosition(n int) dlOpt       { return func(d *core.Download) { d.QueuePosition = n } }
func withPaused() dlOpt              { return func(d *core.Download) { d.Paused = true } }
func withName(n string) dlOpt        { return func(d *core.Download) { d.Name = n } }
func withTotal(n int64) dlOpt        { return func(d *core.Download) { d.TotalBytes = n } }
func withFetched(at time.Time) dlOpt { return func(d *core.Download) { d.FetchedAt = at } }

// torrentFile is the metainfo the fake engine hashes.
func torrentBytes(id core.ID) []byte { return []byte("metainfo-of-" + string(id)) }

func hashOf(id core.ID) core.InfoHash { return sha1.Sum(torrentBytes(id)) }

// addTorrent stores a torrent Download (default: Queued, 1000 bytes) with a
// .torrent file in its own directory below the incomplete root.
func (h *harness) addTorrent(opts ...dlOpt) core.Download {
	h.t.Helper()
	h.seq++
	id := core.NewID()
	d := core.Download{
		ID: id, Name: fmt.Sprintf("Torrent %d", h.seq), SourceKind: core.SourceTorrent,
		State: core.StateQueued, Priority: core.DownloadPriorityNormal, TotalBytes: 1000,
		AddedAt: time.Date(2026, 4, 1, 0, 0, h.seq, 0, time.UTC), UpdatedAt: time.Date(2026, 4, 1, 0, 0, h.seq, 0, time.UTC),
	}
	for _, o := range opts {
		o(&d)
	}
	if d.State == core.StateFetched || d.State == core.StatePostProcessing || d.State == core.StateReady {
		if d.FetchedAt.IsZero() {
			d.FetchedAt = d.AddedAt
		}
		if d.State == core.StateReady {
			d.ReadyAt = d.AddedAt
			d.OutputPath = "/out"
		}
	}
	dir := filepath.Join(h.incomplete, string(id))
	require.NoError(h.t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "source.torrent")
	require.NoError(h.t, os.WriteFile(path, torrentBytes(id), 0o600))
	require.NoError(h.t, h.downloads.Create(context.Background(), d))
	require.NoError(h.t, h.sources.Create(context.Background(), core.TorrentSource{
		DownloadID: id, InfoHash: hashOf(id), TorrentPath: path,
	}))
	return d
}

// addMagnet stores a magnet Download in FetchingMetadata: no .torrent file yet.
func (h *harness) addMagnet(opts ...dlOpt) core.Download {
	h.t.Helper()
	h.seq++
	id := core.NewID()
	hash := hashOf(id)
	d := core.Download{
		ID: id, Name: fmt.Sprintf("Magnet %d", h.seq), SourceKind: core.SourceTorrent,
		State: core.StateFetchingMetadata, Priority: core.DownloadPriorityNormal,
		AddedAt: time.Date(2026, 4, 1, 0, 0, h.seq, 0, time.UTC), UpdatedAt: time.Date(2026, 4, 1, 0, 0, h.seq, 0, time.UTC),
	}
	for _, o := range opts {
		o(&d)
	}
	require.NoError(h.t, h.downloads.Create(context.Background(), d))
	require.NoError(h.t, h.sources.Create(context.Background(), core.TorrentSource{
		DownloadID: id, InfoHash: hash,
		Magnet: core.Secret("magnet:?xt=urn:btih:" + hash.String() + "&dn=x"),
	}))
	return d
}

func (h *harness) get(id core.ID) core.Download {
	h.t.Helper()
	d, err := h.downloads.Get(context.Background(), id)
	require.NoError(h.t, err)
	return d
}

func (h *harness) states(ids ...core.ID) []core.State {
	out := make([]core.State, len(ids))
	for i, id := range ids {
		out[i] = h.get(id).State
	}
	return out
}

func (h *harness) eventually(cond func() bool, msg string, args ...any) {
	h.t.Helper()
	require.Eventually(h.t, cond, 10*time.Second, 5*time.Millisecond, append([]any{msg}, args...)...)
}

// waitState waits until the Download is in state want. For Fetching it also
// waits until the engine runs the torrent: the Queue moves a Download to
// Fetching before it asks the runner to start it.
func (h *harness) waitState(id core.ID, want core.State) {
	h.t.Helper()
	h.eventually(func() bool {
		return h.get(id).State == want && (want != core.StateFetching || h.runningInEngine(id))
	}, "download %s did not reach %s (now %s)", id, want, h.get(id).State)
}

// runningInEngine reports whether the runner has registered the torrent and
// the engine is not holding it paused.
func (h *harness) runningInEngine(id core.ID) bool {
	if h.runner == nil || !h.runner.Managed(id) {
		return false
	}
	st, err := h.engine.Stats(hashOf(id))
	return err == nil && st.State != core.TorrentPaused
}

// calls returns the engine calls of one method.
func (h *harness) calls(method string) []coretest.TorrentCall {
	var out []coretest.TorrentCall
	for _, c := range h.engine.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (h *harness) callsFor(method string, id core.ID) []coretest.TorrentCall {
	var out []coretest.TorrentCall
	for _, c := range h.calls(method) {
		if c.Hash == hashOf(id) {
			out = append(out, c)
		}
	}
	return out
}
