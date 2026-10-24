package web

import (
	"maps"
	"net/http"
	"strconv"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// torrentSection is the "Torrent network" entry of the settings registry (M6-07).
func torrentSection() SettingsSection {
	return SettingsSection{
		Slug: "torrent", Title: "Torrent network", Icon: "bolt", Order: 40,
		Description: "Listen port, peer discovery, encryption and network binding.",
		Template:    "settings_torrent.html", Data: settingsTorrentData, Routes: (*Server).registerTorrentRoutes,
	}
}

func settingsTorrentData(s *Server, r *http.Request) (any, error) {
	return s.cardPage(r, torrentCards, nil), nil
}

func (s *Server) registerTorrentRoutes() { s.registerCards(torrentCards) }

var torrentCards = []*settingsCard{torrentConnectionCard, torrentBindingCard, torrentSeedingCard}

const restartNote = "Restart bitnews for this to take effect."

// ---- connection ---------------------------------------------------------

// mseOption is one choice of the encryption policy.
type mseOption struct{ Value, Label string }

var mseOptions = []mseOption{
	{string(core.MSEPrefer), "Prefer encryption (recommended)"},
	{string(core.MSERequire), "Require encryption"},
	{string(core.MSEDisable), "No encryption"},
}

// connectionView is the connection card: the options of the MSE select are
// part of the view.
type connectionView struct {
	cardView
	MSE []mseOption
	// PortFixed is set when the operator fixed the port with
	// BITNEWS_TORRENT_PORT: the field then shows that port and is read-only.
	PortFixed bool
	// Port is the live state of the listen port.
	Port portView
}

// fixedPort is the listen port fixed by the operator, or 0.
func (s *Server) fixedPort() int {
	if s.Config == nil {
		return 0
	}
	return s.Config.TorrentPortOverride()
}

// torrentKey is the part of TorrentSettings that matters, in its effective
// form, so that 0 and the default port are the same.
type torrentKey struct {
	Port     int
	DHT, PEX bool
	LSD      bool
	MSE      core.MSEPolicy
}

func torrentKeyOf(t core.TorrentSettings) torrentKey {
	return torrentKey{t.EffectivePort(), t.DHTEnabled(), t.PEXEnabled(), t.LSD, t.EffectiveMSE()}
}

// torrentConnectionCard edits Settings.Torrent. The listen port moves live;
// the engine reads the rest when it starts, so a change to it needs a
// restart.
var torrentConnectionCard = &settingsCard{
	key: "connection", route: "/settings/torrent", slug: "torrent", partial: "torrent_connection_card",
	saved:       "Connection settings saved.",
	restartNote: restartNote,
	values: func(s core.Settings) map[string]string {
		t := s.Torrent
		return map[string]string{
			"listen_port": itoa(t.EffectivePort()),
			"dht":         checked(t.DHTEnabled()), "pex": checked(t.PEXEnabled()), "lsd": checked(t.LSD),
			"mse": string(t.EffectiveMSE()),
		}
	},
	read: func(s *Server, f map[string]string, next *core.Settings) map[string]string {
		errs := map[string]string{}
		port, blank, ok := wholeNumber(f["listen_port"], 65535)
		if s.fixedPort() != 0 {
			port, ok = int64(next.Torrent.ListenPort), true // not editable: keep the stored one
		} else if !ok || blank || port < 1 {
			errs["listen_port"] = "Enter a port from 1 to 65535, such as 51413."
		}
		mse := core.MSEPolicy(f["mse"])
		switch mse {
		case core.MSEPrefer, core.MSERequire, core.MSEDisable:
		default:
			errs["mse"] = "Choose one of the encryption options."
		}
		if len(errs) > 0 {
			return errs
		}
		next.Torrent = core.TorrentSettings{
			ListenPort: int(port), DisableDHT: !on(f, "dht"), DisablePEX: !on(f, "pex"), LSD: on(f, "lsd"), MSE: mse,
		}
		return nil
	},
	view: func(s *Server, v cardView, _ core.Settings) any {
		cv := connectionView{cardView: v, MSE: mseOptions, Port: s.portStatus()}
		if p := s.fixedPort(); p != 0 {
			cv.PortFixed = true
			cv.V = maps.Clone(v.V)
			cv.V["listen_port"] = itoa(p)
		}
		return cv
	},
	restart: func(_ *Server, started, saved core.Settings) bool {
		a, b := torrentKeyOf(started.Torrent), torrentKeyOf(saved.Torrent)
		a.Port, b.Port = 0, 0 // the port moves live (or is fixed by the operator)
		return a != b
	},
}

// ---- network binding ----------------------------------------------------

// bindingStatusView is the live state line of one protocol's binding.
type bindingStatusView struct {
	// Known is false without a daemon to ask.
	Known bool
	// State is off, bound or unavailable.
	State string
	// Label and Detail are the words shown.
	Label, Detail string
	// Pending is set when the saved binding is not the one that runs.
	Pending bool
}

// bindingView is the binding card: one status line per protocol.
type bindingView struct {
	cardView
	Usenet, Torrent bindingStatusView
}

func (s *Server) bindingStatus(kind core.SourceKind, queue string, saved core.ProtocolBinding, started core.ProtocolBinding) bindingStatusView {
	if s.Config == nil {
		return bindingStatusView{}
	}
	st := s.Config.Binding(kind)
	v := bindingStatusView{Known: true, Pending: saved != started}
	switch {
	case !st.Enabled:
		v.State, v.Label, v.Detail = "off", "Not bound", "Using your normal network route."
	case st.Available:
		v.State, v.Label, v.Detail = "bound", "Bound", "Sockets are limited to "+describeBinding(st)+"."
	default:
		v.State, v.Label = "unavailable", "Unavailable"
		v.Detail = "The " + queue + " queue is paused until " + describeBinding(st) + " is back."
		if st.Reason != "" {
			v.Detail = st.Reason + ". " + v.Detail
		}
	}
	return v
}

// describeBinding names what a binding is limited to, for example
// `interface wg0 and address 10.2.0.2`.
func describeBinding(st BindingStatus) string {
	var parts []string
	if st.Interface != "" {
		parts = append(parts, "interface "+st.Interface)
	}
	if st.Address != "" {
		parts = append(parts, "address "+st.Address)
	}
	return strings.Join(parts, " and ")
}

// torrentBindingCard edits Settings.Network. Sockets are bound when they are
// opened, so a change needs a restart.
var torrentBindingCard = &settingsCard{
	key: "binding", route: "/settings/torrent/binding", slug: "torrent", partial: "torrent_binding_card",
	saved:       "Network binding saved.",
	restartNote: restartNote,
	values: func(s core.Settings) map[string]string {
		n := s.Network
		return map[string]string{
			"usenet_interface": n.Usenet.Interface, "usenet_address": n.Usenet.Address,
			"torrent_interface": n.Torrent.Interface, "torrent_address": n.Torrent.Address,
		}
	},
	read: func(s *Server, f map[string]string, next *core.Settings) map[string]string {
		errs := map[string]string{}
		var out core.NetworkSettings
		for _, p := range []struct {
			proto string
			dst   *core.ProtocolBinding
		}{{"usenet", &out.Usenet}, {"torrent", &out.Torrent}} {
			iface, addr := strings.TrimSpace(f[p.proto+"_interface"]), strings.TrimSpace(f[p.proto+"_address"])
			if iface != "" {
				if err := core.ValidInterfaceName(iface); err != nil {
					errs[p.proto+"_interface"] = "Use at most 15 letters, digits and the characters _ . : - (for example wg0)."
				} else if !s.interfaceBinding {
					errs[p.proto+"_interface"] = "Binding to an interface only works on Linux, and bitnews would not start with it here. Use the address instead."
				}
			}
			if addr != "" {
				if core.ValidBindAddress(addr) != nil {
					errs[p.proto+"_address"] = "Enter one IP address of this machine, such as 10.2.0.2, without a port."
				}
			}
			*p.dst = core.ProtocolBinding{Interface: iface, Address: addr}
		}
		if len(errs) > 0 {
			return errs
		}
		next.Network = out
		return nil
	},
	view: func(s *Server, v cardView, cur core.Settings) any {
		var started core.NetworkSettings
		if s.Config != nil {
			started = s.Config.Started().Network
		}
		return bindingView{
			cardView: v,
			Usenet:   s.bindingStatus(core.SourceNZB, "Usenet", cur.Network.Usenet, started.Usenet),
			Torrent:  s.bindingStatus(core.SourceTorrent, "torrent", cur.Network.Torrent, started.Torrent),
		}
	},
	restart: func(_ *Server, started, saved core.Settings) bool { return started.Network != saved.Network },
}

// ---- Share Limit defaults ----------------------------------------------

// seedingOption is one choice of the action.
type seedingOption struct{ Value, Label string }

var seedingOptions = []seedingOption{
	{string(core.SeedingActionPause), "Pause the torrent (recommended)"},
	{string(core.SeedingActionRemove), "Remove the download, keep the files"},
	{string(core.SeedingActionRemoveData), "Remove the download and delete the files"},
}

type seedingView struct {
	cardView
	Actions []seedingOption
}

const (
	maxShareRatio     = 1000
	maxSeedingMinutes = 10 * 365 * 24 * 60
)

// torrentSeedingCard edits Settings.Seeding. It applies live.
var torrentSeedingCard = &settingsCard{
	key: "seeding", route: "/settings/torrent/seeding", slug: "torrent", partial: "torrent_seeding_card",
	saved: "Share Limit defaults saved. They apply right away.",
	values: func(s core.Settings) map[string]string {
		g := s.Seeding
		v := map[string]string{"seeding_action": string(g.EffectiveAction())}
		if g.RatioLimit > 0 {
			v["ratio_limit"] = strconv.FormatFloat(g.RatioLimit, 'f', -1, 64)
		}
		v["seeding_minutes"] = optNumber(g.SeedingTimeMinutes)
		return v
	},
	read: func(_ *Server, f map[string]string, next *core.Settings) map[string]string {
		errs := map[string]string{}
		ratio, _, ok := decimalNumber(f["ratio_limit"], maxShareRatio)
		if !ok {
			errs["ratio_limit"] = "Enter a ratio such as 2 or 1.5, or leave it empty for no ratio limit."
		}
		minutes, _, ok := wholeNumber(f["seeding_minutes"], maxSeedingMinutes)
		if !ok {
			errs["seeding_minutes"] = "Enter a whole number of minutes, such as 1440 for a day, or leave it empty for no time limit."
		}
		action := core.SeedingAction(f["seeding_action"])
		switch action {
		case core.SeedingActionPause, core.SeedingActionRemove, core.SeedingActionRemoveData:
		default:
			errs["seeding_action"] = "Choose one of the options."
		}
		if len(errs) > 0 {
			return errs
		}
		next.Seeding = core.SeedingSettings{RatioLimit: ratio, SeedingTimeMinutes: int(minutes), Action: action}
		return nil
	},
	view: func(_ *Server, v cardView, _ core.Settings) any {
		return seedingView{cardView: v, Actions: seedingOptions}
	},
}
