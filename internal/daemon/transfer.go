package daemon

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/netbind"
	"github.com/jroosing/bitnews/internal/torrent"
	"github.com/jroosing/bitnews/internal/web"
)

// This file wires the speed, torrent network, post-processing and disk
// settings (M6-07):
//
//   - speed limits and the Queue limits apply live: speedApplier pushes the
//     limits to the runners on every change, and the runners read the Queue
//     limits and min_free from the Settings on each pass;
//   - the torrent listen port applies live: torrentPort moves the engine's
//     sockets (port.go);
//   - the torrent engine's other network options and the post-processing
//     concurrency are read once, when Build constructs the component. A change
//     is stored at once and applied by the next start; restartWatcher says so
//     in the log.

// onSettingsChanged calls f for every core.SettingsChanged event until ctx
// ends.
func onSettingsChanged(ctx context.Context, bus *core.Bus, f func()) {
	for ctx.Err() == nil {
		sub := bus.Subscribe(ctx)
		for ev := range sub.Events() {
			if _, ok := ev.(core.SettingsChanged); ok {
				f()
			}
		}
		sub.Close()
	}
}

// speedTarget is where speed limits are applied: the web runtime, which sets
// them on the runners and remembers them for the queue page.
type speedTarget interface {
	SetSpeedLimit(kind core.SourceKind, down, up int64)
}

// speedApplier gives the runners the speed limits of the Settings. A limit
// typed on the queue page is temporary: it is replaced only when the speed
// settings themselves change, not on every settings write.
type speedApplier struct {
	settings func() core.Settings
	target   speedTarget

	mu      sync.Mutex
	last    core.SpeedSettings
	applied bool
}

func (a *speedApplier) apply() {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.settings().Speed
	if a.applied && cur == a.last {
		return
	}
	a.applied, a.last = true, cur
	for _, kind := range []core.SourceKind{core.SourceNZB, core.SourceTorrent} {
		down, up := cur.Effective(kind)
		a.target.SetSpeedLimit(kind, down, up)
	}
}

func (a *speedApplier) run(ctx context.Context, bus *core.Bus) {
	onSettingsChanged(ctx, bus, a.apply)
}

// torrentNetwork is the part of the engine's configuration that comes from
// the Settings, with the test options applied. Binding, piece store and
// logger are added by newTorrentEngine.
func torrentNetwork(s core.Settings, o TorrentOptions) torrent.Config {
	t := s.Torrent
	port := o.Port
	if port == 0 {
		port = t.EffectivePort()
	}
	return torrent.Config{
		ListenPort: port, DisableUTP: o.DisableUTP,
		DHT: t.DHTEnabled() && !o.DisableDHT, PEX: t.PEXEnabled(), LSD: t.LSD,
		MSE: torrent.MSE(t.EffectiveMSE()),
	}
}

// minFreeFunc returns the min_free the runners and the unpacker use. It reads
// the Settings on each call, so a change applies at once. A test's fixed
// reserve (Tuning.MinFree) wins.
func minFreeFunc(settings func() core.Settings, fixed *uint64) func(context.Context) (uint64, error) {
	return func(context.Context) (uint64, error) {
		if fixed != nil {
			return *fixed, nil
		}
		return settings().Queues.EffectiveMinFree(), nil
	}
}

// postprocConcurrency is how many Downloads are post-processed at once: a
// test's value, else the setting as it was at start-up.
func postprocConcurrency(s core.Settings, tune Tuning) int {
	if tune.PostProcConcurrency > 0 {
		return tune.PostProcConcurrency
	}
	return s.Queues.EffectivePostprocConcurrency()
}

// restartOnly is the part of the Settings that is read once at start-up.
type restartOnly struct {
	Torrent  core.TorrentSettings
	Postproc int
}

func restartOnlyOf(s core.Settings) restartOnly {
	t := s.Torrent
	t.ListenPort = 0 // applied live by torrentPort
	return restartOnly{Torrent: t, Postproc: s.Queues.EffectivePostprocConcurrency()}
}

// restartWatcher warns once per change when settings that need a restart
// differ from the ones the daemon started with.
type restartWatcher struct {
	start   restartOnly
	current func() core.Settings
	log     *slog.Logger

	mu     sync.Mutex
	warned restartOnly
}

func (w *restartWatcher) run(ctx context.Context, bus *core.Bus) {
	onSettingsChanged(ctx, bus, w.check)
}

func (w *restartWatcher) check() {
	now := restartOnlyOf(w.current())
	w.mu.Lock()
	defer w.mu.Unlock()
	if now == w.start {
		w.warned = now
		return
	}
	if now == w.warned {
		return
	}
	w.warned = now
	w.log.Warn("the torrent network or post-processing settings changed; restart bitnews to apply them")
}

// webConfig implements web.ConfigSource: the Settings the daemon was built
// with and the live status of the bindings it runs with.
type webConfig struct {
	started core.Settings
	// portOverride is the torrent port the operator fixed, or 0.
	portOverride int
	// port is the live torrent listen port; nil in tests.
	port *torrentPort
	// lookup lists the host's interfaces; nil means the system's.
	lookup netbind.Lookup
}

var _ web.ConfigSource = (*webConfig)(nil)

func (c *webConfig) Started() core.Settings { return c.started }

func (c *webConfig) TorrentPortOverride() int { return c.portOverride }

func (c *webConfig) ListenPort() (core.ListenPortStatus, bool) { return c.port.ListenPortStatus() }

// ApplyListenPort moves the engine to the saved port now, so the page that
// saved it shows the new port. The SettingsChanged watcher would do the same
// a moment later; moving twice is a no-op.
func (c *webConfig) ApplyListenPort() {
	if c.port != nil {
		c.port.apply()
	}
}

func (c *webConfig) Binding(kind core.SourceKind) web.BindingStatus {
	var b core.ProtocolBinding
	switch kind {
	case core.SourceNZB:
		b = c.started.Network.Usenet
	case core.SourceTorrent:
		b = c.started.Network.Torrent
	default:
		return web.BindingStatus{}
	}
	if !b.Enabled() {
		return web.BindingStatus{}
	}
	st := web.BindingStatus{Enabled: true, Interface: b.Interface, Address: b.Address}
	lookup := c.lookup
	if lookup == nil {
		lookup = netbind.SystemLookup
	}
	ifaces, err := lookup()
	if err != nil {
		st.Reason = "bitnews cannot list the network interfaces"
		return st
	}
	res := netbind.Binding{Interface: b.Interface, Address: b.Address}.Check(ifaces)
	st.Available, st.Reason = res.Available, res.Reason
	return st
}
