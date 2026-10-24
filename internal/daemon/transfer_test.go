package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/netbind"
	"github.com/jroosing/bitnews/internal/torrent"
	"github.com/jroosing/bitnews/internal/web"
)

func newSpeedApplier(s *core.Settings) (*speedApplier, *fakeUsenetLimiter, *fakeTorrentRunner, *webRuntime) {
	u, tq := &fakeUsenetLimiter{}, &fakeTorrentRunner{}
	rt := newWebRuntime(queues(&fakeQueue{}, &fakeQueue{}), u, tq, newTracker())
	return &speedApplier{settings: func() core.Settings { return *s }, target: rt}, u, tq, rt
}

func TestSpeedApplierGivesEachQueueTheTighterOfTheOverallAndItsOwnLimit(t *testing.T) {
	s := core.DefaultSettings()
	s.Speed = core.SpeedSettings{
		Global:  core.SpeedLimit{DownKBps: 1000, UpKBps: 400},
		Usenet:  core.SpeedLimit{DownKBps: 5000},
		Torrent: core.SpeedLimit{DownKBps: 200},
	}
	a, u, tq, rt := newSpeedApplier(&s)

	a.apply()

	assert.Equal(t, int64(1000*1024), u.limit)
	assert.Equal(t, int64(200*1024), tq.down)
	assert.Equal(t, int64(400*1024), tq.up)
	down, up := rt.SpeedLimits(core.SourceTorrent)
	assert.Equal(t, int64(200*1024), down, "the queue page shows what is in force")
	assert.Equal(t, int64(400*1024), up)
}

func TestSpeedApplierLeavesAQueuePageLimitAloneUnlessTheSpeedSettingsChange(t *testing.T) {
	s := core.DefaultSettings()
	a, u, _, rt := newSpeedApplier(&s)
	a.apply()
	rt.SetSpeedLimit(core.SourceNZB, 77*1024, 0) // typed on the queue page

	s.Paths.Usenet.Complete = "/elsewhere"
	a.apply()
	assert.Equal(t, int64(77*1024), u.limit, "an unrelated change keeps the temporary limit")

	s.Speed.Usenet.DownKBps = 300
	a.apply()
	assert.Equal(t, int64(300*1024), u.limit)

	s.Speed = core.SpeedSettings{}
	a.apply()
	assert.Zero(t, u.limit, "removing the limits lifts them")
}

// recordedLimits is a speedTarget that can be read while the applier runs.
type recordedLimits struct {
	mu   sync.Mutex
	down map[core.SourceKind]int64
}

func (r *recordedLimits) SetSpeedLimit(kind core.SourceKind, down, _ int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down == nil {
		r.down = map[core.SourceKind]int64{}
	}
	r.down[kind] = down
}

func (r *recordedLimits) get(kind core.SourceKind) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.down[kind]
}

func TestSpeedApplierFollowsSettingsChangedEvents(t *testing.T) {
	var mu sync.Mutex
	s := core.DefaultSettings()
	rec := &recordedLimits{}
	a := &speedApplier{settings: func() core.Settings { mu.Lock(); defer mu.Unlock(); return s }, target: rec}
	a.apply()
	bus := core.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.run(ctx, bus); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	mu.Lock()
	s.Speed.Usenet.DownKBps = 123
	mu.Unlock()
	require.Eventually(t, func() bool {
		bus.Publish(core.SettingsChanged{})
		return rec.get(core.SourceNZB) == 123*1024
	}, 5*time.Second, 10*time.Millisecond)
}

func TestTorrentNetworkFollowsTheSettings(t *testing.T) {
	s := core.DefaultSettings()
	s.Torrent = core.TorrentSettings{ListenPort: 6881, DisableDHT: true, DisablePEX: true, LSD: true, MSE: core.MSERequire}

	cfg := torrentNetwork(s, TorrentOptions{})

	assert.Equal(t, 6881, cfg.ListenPort)
	assert.False(t, cfg.DHT)
	assert.False(t, cfg.PEX)
	assert.True(t, cfg.LSD)
	assert.Equal(t, torrent.MSERequire, cfg.MSE)
}

func TestTorrentNetworkDefaultsAndTestOptions(t *testing.T) {
	cfg := torrentNetwork(core.Settings{}, TorrentOptions{})
	assert.Equal(t, DefaultTorrentPort, cfg.ListenPort)
	assert.True(t, cfg.DHT)
	assert.True(t, cfg.PEX)
	assert.Equal(t, torrent.MSEPrefer, cfg.MSE)

	s := core.DefaultSettings()
	s.Torrent.ListenPort = 6881
	cfg = torrentNetwork(s, TorrentOptions{Port: 7000, DisableDHT: true, DisableUTP: true})
	assert.Equal(t, 7000, cfg.ListenPort, "a test's port wins")
	assert.False(t, cfg.DHT, "a test can switch DHT off")
	assert.True(t, cfg.DisableUTP)
}

func TestMinFreeIsReadFromTheSettingsEachTime(t *testing.T) {
	s := core.DefaultSettings()
	f := minFreeFunc(func() core.Settings { return s }, nil)

	got, err := f(context.Background())
	require.NoError(t, err)
	assert.Equal(t, core.DefaultMinFree, got)

	s.Queues.MinFree = 3 << 30
	got, err = f(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(3<<30), got, "no restart needed")

	zero := uint64(0)
	got, err = minFreeFunc(func() core.Settings { return s }, &zero)(context.Background())
	require.NoError(t, err)
	assert.Zero(t, got, "a test's reserve wins")
}

func TestPostprocConcurrencyComesFromTheSettingsUnlessATestSetsIt(t *testing.T) {
	s := core.DefaultSettings()
	s.Queues.PostprocConcurrency = 3

	assert.Equal(t, 3, postprocConcurrency(s, Tuning{}))
	assert.Equal(t, 5, postprocConcurrency(s, Tuning{PostProcConcurrency: 5}))
	assert.Equal(t, 1, postprocConcurrency(core.Settings{}, Tuning{}))
}

func TestRestartWatcherWarnsOncePerChangeOfTheTorrentOrPostprocSettings(t *testing.T) {
	var buf syncBuf
	s := core.DefaultSettings()
	w := &restartWatcher{start: restartOnlyOf(s), current: func() core.Settings { return s }, log: slog.New(slog.NewTextHandler(&buf, nil))}

	w.check()
	assert.Empty(t, buf.String(), "nothing changed")

	s.Torrent.ListenPort = 6881
	w.check()
	assert.Empty(t, buf.String(), "the listen port moves live")

	s.Torrent.MSE = core.MSERequire
	w.check()
	w.check()
	assert.Equal(t, 1, countOf(buf.String(), "restart bitnews"))

	s.Queues.PostprocConcurrency = 4
	w.check()
	assert.Equal(t, 2, countOf(buf.String(), "restart bitnews"))

	s = core.DefaultSettings()
	w.check()
	assert.Equal(t, 2, countOf(buf.String(), "restart bitnews"), "back to what we started with")

	s.Queues.Usenet = 8
	w.check()
	assert.Equal(t, 2, countOf(buf.String(), "restart bitnews"), "queue limits apply live")
}

func TestWebConfigReportsTheStartedSettingsAndTheLiveBindings(t *testing.T) {
	s := core.DefaultSettings()
	s.Network.Usenet = core.ProtocolBinding{Interface: "wg0", Address: "10.2.0.2"}
	s.Network.Torrent = core.ProtocolBinding{Interface: "tun1"}
	ifaces := []netbind.Iface{{Name: "wg0", Up: true, Addrs: []netip.Addr{netip.MustParseAddr("10.2.0.2")}}}
	c := &webConfig{started: s, lookup: func() ([]netbind.Iface, error) { return ifaces, nil }}

	assert.Equal(t, s, c.Started())

	nzb := c.Binding(core.SourceNZB)
	assert.Equal(t, web.BindingStatus{Enabled: true, Interface: "wg0", Address: "10.2.0.2", Available: true}, nzb)

	tor := c.Binding(core.SourceTorrent)
	assert.True(t, tor.Enabled)
	assert.False(t, tor.Available)
	assert.Contains(t, tor.Reason, "tun1")

	ifaces = nil // the VPN went away
	assert.False(t, c.Binding(core.SourceNZB).Available, "the status is live")

	assert.Equal(t, web.BindingStatus{}, c.Binding("other"))
}

func TestWebConfigWithoutABindingIsNotBound(t *testing.T) {
	c := &webConfig{started: core.DefaultSettings(), lookup: func() ([]netbind.Iface, error) { return nil, errors.New("no netlink") }}

	assert.Equal(t, web.BindingStatus{}, c.Binding(core.SourceNZB))
	assert.Equal(t, web.BindingStatus{}, c.Binding(core.SourceTorrent))
}

func TestWebConfigLookupFailureReadsAsUnavailable(t *testing.T) {
	s := core.DefaultSettings()
	s.Network.Usenet.Interface = "wg0"
	c := &webConfig{started: s, lookup: func() ([]netbind.Iface, error) { return nil, errors.New("no netlink") }}

	st := c.Binding(core.SourceNZB)

	assert.True(t, st.Enabled)
	assert.False(t, st.Available)
	assert.NotEmpty(t, st.Reason)
}

func TestWebConfigReportsAPortFixedByTheOperator(t *testing.T) {
	assert.Zero(t, (&webConfig{}).TorrentPortOverride())
	assert.Equal(t, 7000, (&webConfig{portOverride: 7000}).TorrentPortOverride())
}
