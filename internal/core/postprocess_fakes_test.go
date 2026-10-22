package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// ppLog records the order of port calls across all fakes.
type ppLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *ppLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, s)
}

func (l *ppLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// ppRepo is an in-memory DownloadRepo.
type ppRepo struct {
	mu  sync.Mutex
	m   map[core.ID]core.Download
	now time.Time
	// stateChanges records every StateChange applied, in order.
	stateChanges []core.StateChange
}

func newPPRepo() *ppRepo {
	return &ppRepo{m: map[core.ID]core.Download{}, now: time.Unix(1000, 0)}
}

func (r *ppRepo) Create(_ context.Context, d core.Download) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[d.ID] = d
	return nil
}

func (r *ppRepo) Get(_ context.Context, id core.ID) (core.Download, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.m[id]
	if !ok {
		return core.Download{}, core.ErrNotFound
	}
	return d, nil
}

func (r *ppRepo) List(_ context.Context, q core.DownloadQuery) ([]core.Download, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.Download
	for _, d := range r.m {
		if q.State != "" && d.State != q.State {
			continue
		}
		out = append(out, d)
	}
	return out, "", nil
}

func (r *ppRepo) Update(_ context.Context, d core.Download) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[d.ID] = d
	return nil
}

func (r *ppRepo) UpdateState(_ context.Context, id core.ID, c core.StateChange) (core.Download, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.m[id]
	if !ok {
		return core.Download{}, core.ErrNotFound
	}
	if err := d.ChangeState(c); err != nil {
		return core.Download{}, err
	}
	r.m[id] = d
	r.stateChanges = append(r.stateChanges, c)
	return d, nil
}

func (r *ppRepo) Delete(_ context.Context, id core.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
	return nil
}

func (r *ppRepo) changes() []core.StateChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.StateChange(nil), r.stateChanges...)
}

// Fakes for the ports. Each has an optional fn to script behaviour; the
// default is the happy path.

type ppVerifier struct {
	log *ppLog
	fn  func(ctx context.Context, dir string) (core.VerifyOutcome, error)
}

func (v *ppVerifier) Verify(ctx context.Context, dir string, _ core.VerifyOptions) (core.VerifyOutcome, error) {
	v.log.add("verify")
	if v.fn != nil {
		return v.fn(ctx, dir)
	}
	return core.VerifyOutcome{HasPAR2: true, Clean: true}, nil
}

type ppRepairer struct {
	log *ppLog
	fn  func(ctx context.Context, dir string, o core.VerifyOutcome) (core.RepairOutcome, error)
}

func (r *ppRepairer) Repair(ctx context.Context, dir string, o core.VerifyOutcome) (core.RepairOutcome, error) {
	r.log.add("repair")
	if r.fn != nil {
		return r.fn(ctx, dir, o)
	}
	return core.RepairOutcome{RepairedSlices: o.DamagedSlices}, nil
}

type ppUnpacker struct {
	log      *ppLog
	sets     []core.ArchiveSet
	detectFn func(ctx context.Context, dir string) ([]core.ArchiveSet, error)
	unpackFn func(ctx context.Context, dir, dst string, o core.UnpackOptions) (core.UnpackResult, error)

	mu       sync.Mutex
	gotDir   string
	gotDst   string
	gotSets  []core.ArchiveSet
	gotDLID  core.ID
	unpacked int
}

func (u *ppUnpacker) Detect(ctx context.Context, dir string) ([]core.ArchiveSet, error) {
	u.log.add("detect")
	if u.detectFn != nil {
		return u.detectFn(ctx, dir)
	}
	return u.sets, nil
}

func (u *ppUnpacker) Unpack(ctx context.Context, dir, dst string, o core.UnpackOptions) (core.UnpackResult, error) {
	u.log.add("unpack")
	u.mu.Lock()
	u.gotDir, u.gotDst, u.gotSets, u.gotDLID = dir, dst, o.Sets, o.DownloadID
	u.unpacked++
	u.mu.Unlock()
	if u.unpackFn != nil {
		return u.unpackFn(ctx, dir, dst, o)
	}
	return core.UnpackResult{Files: 1}, nil
}

type ppFinisher struct {
	log *ppLog
	fn  func(ctx context.Context, d core.Download, dir, unpacked string) (string, error)

	mu          sync.Mutex
	gotDownload core.Download
	gotDir      string
	gotUnpacked string
}

func (f *ppFinisher) Finish(ctx context.Context, d core.Download, dir, unpacked string) (string, error) {
	f.log.add("finish")
	f.mu.Lock()
	f.gotDownload, f.gotDir, f.gotUnpacked = d, dir, unpacked
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, d, dir, unpacked)
	}
	return "/complete/" + d.Name, nil
}

type ppReporter struct {
	log    *ppLog
	report core.FetchReport
	err    error
}

func (r *ppReporter) FetchReport(context.Context, core.Download) (core.FetchReport, error) {
	r.log.add("fetchreport")
	return r.report, r.err
}

type ppDeferred struct {
	log  *ppLog
	more bool
	err  error

	mu        sync.Mutex
	minBlocks []int
}

func (d *ppDeferred) FetchDeferred(_ context.Context, _ core.Download, minBlocks int) (bool, error) {
	d.log.add("deferred")
	d.mu.Lock()
	d.minBlocks = append(d.minBlocks, minBlocks)
	d.mu.Unlock()
	return d.more, d.err
}

// ppHarness wires a PostProcessor to fakes and an event subscription.
type ppHarness struct {
	t        *testing.T
	repo     *ppRepo
	bus      *core.Bus
	sub      *core.Subscription
	log      *ppLog
	verifier *ppVerifier
	repairer *ppRepairer
	unpacker *ppUnpacker
	finisher *ppFinisher
	reporter *ppReporter
	deferred *ppDeferred
	pp       *core.PostProcessor
}

func newPPHarness(t *testing.T, mods ...func(*core.PostProcessorConfig)) *ppHarness {
	t.Helper()
	log := &ppLog{}
	h := &ppHarness{
		t:        t,
		repo:     newPPRepo(),
		bus:      core.NewBus(),
		log:      log,
		verifier: &ppVerifier{log: log},
		repairer: &ppRepairer{log: log},
		unpacker: &ppUnpacker{log: log},
		finisher: &ppFinisher{log: log},
		reporter: &ppReporter{log: log},
		deferred: &ppDeferred{log: log},
	}
	h.sub = h.bus.Subscribe(context.Background())
	t.Cleanup(h.sub.Close)
	cfg := core.PostProcessorConfig{
		Downloads:       h.repo,
		Bus:             h.bus,
		Verifier:        h.verifier,
		Repairer:        h.repairer,
		Unpacker:        h.unpacker,
		Finisher:        h.finisher,
		FetchReporter:   h.reporter,
		DeferredVolumes: h.deferred,
		DirFor:          func(d core.Download) string { return "/work/" + d.Name },
		Clock:           func() time.Time { return time.Unix(2000, 0) },
	}
	for _, m := range mods {
		m(&cfg)
	}
	pp, err := core.NewPostProcessor(cfg)
	require.NoError(t, err)
	h.pp = pp
	return h
}

// add stores a Download in the given state.
func (h *ppHarness) add(name string, state core.State, kind core.SourceKind) core.Download {
	h.t.Helper()
	d := core.Download{ID: core.NewID(), Name: name, SourceKind: kind, State: state}
	require.NoError(h.t, h.repo.Create(context.Background(), d))
	return d
}

func (h *ppHarness) get(id core.ID) core.Download {
	h.t.Helper()
	d, err := h.repo.Get(context.Background(), id)
	require.NoError(h.t, err)
	return d
}

// events drains the events published so far.
func (h *ppHarness) events() []core.Event {
	var out []core.Event
	for {
		select {
		case ev, ok := <-h.sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}
