package daemon

import (
	"context"
	"log/slog"
	"math"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// listenPorter is the part of the torrent engine that moves its listen port
// while it runs (*torrent.Engine).
type listenPorter interface {
	SetListenPort(port int) error
	ListenPortStatus() core.ListenPortStatus
}

// portSettings is the part of the SettingsService torrentPort uses.
type portSettings interface {
	Current() core.Settings
	Change(ctx context.Context, change func(*core.Settings) error) error
}

// torrentPort keeps the torrent engine on the listen port of the Settings,
// live: the port changes without a restart, whether it comes from the
// settings page or from a VPN's port-forwarding hook calling
// POST /api/v1/torrent/port. A port fixed by BITNEWS_TORRENT_PORT wins and is
// never moved.
type torrentPort struct {
	engine   listenPorter // nil when torrents are off
	settings portSettings
	override int // BITNEWS_TORRENT_PORT, or 0
	log      *slog.Logger

	mu sync.Mutex // serialises moves
}

// SetListenPort moves the engine to port and then stores the port, so a
// port that cannot be opened is never stored. It implements api.TorrentPort.
func (p *torrentPort) SetListenPort(ctx context.Context, port int) (core.ListenPortStatus, error) {
	if p.engine == nil {
		return core.ListenPortStatus{}, &app.Error{Kind: app.ErrUnavailable, Code: "torrents_unavailable", Message: "torrents are turned off on this instance"}
	}
	if port < 1 || port > math.MaxUint16 {
		return core.ListenPortStatus{}, &app.Error{Kind: app.ErrInvalidInput, Code: "invalid_port", Message: "port must be a number from 1 to 65535"}
	}
	if p.override != 0 {
		if port == p.override {
			return p.engine.ListenPortStatus(), nil
		}
		return core.ListenPortStatus{}, &app.Error{Kind: core.ErrConflict, Code: "port_fixed",
			Message: "the listen port is fixed by BITNEWS_TORRENT_PORT; remove that setting to let the port change"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.engine.ListenPortStatus().Port
	if err := p.engine.SetListenPort(port); err != nil {
		return core.ListenPortStatus{}, &app.Error{Kind: core.ErrConflict, Code: "port_unavailable",
			Message: "bitnews cannot listen on this port: " + err.Error()}
	}
	if err := p.settings.Change(ctx, func(s *core.Settings) error {
		s.Torrent.ListenPort = port
		return nil
	}); err != nil {
		if old != port {
			_ = p.engine.SetListenPort(old)
		}
		return core.ListenPortStatus{}, err
	}
	p.log.Info("torrent listen port set", "port", port)
	return p.engine.ListenPortStatus(), nil
}

// ListenPortStatus implements api.TorrentPort; ok is false when torrents are
// off.
func (p *torrentPort) ListenPortStatus() (core.ListenPortStatus, bool) {
	if p == nil || p.engine == nil {
		return core.ListenPortStatus{}, false
	}
	return p.engine.ListenPortStatus(), true
}

// apply moves the engine to the port of the Settings when they changed it.
func (p *torrentPort) apply() {
	if p.engine == nil || p.override != 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	want := p.settings.Current().Torrent.EffectivePort()
	if p.engine.ListenPortStatus().Port == want {
		return
	}
	if err := p.engine.SetListenPort(want); err != nil {
		p.log.Warn("torrent listen port not changed", "port", want, "error", err.Error())
		return
	}
	p.log.Info("torrent listen port set", "port", want)
}

func (p *torrentPort) run(ctx context.Context, bus *core.Bus) {
	onSettingsChanged(ctx, bus, p.apply)
}
