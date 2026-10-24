package web

import (
	"strconv"
	"time"
)

// portView is the live state of the torrent listen port, shown on the
// torrent Queue and on the Torrent network settings page.
type portView struct {
	// Known is false without a daemon to ask, or with torrents off.
	Known bool
	// State is open or unconfirmed.
	State string
	Port  int
	// Label is the badge text and Detail the sentence after it.
	Label, Detail string
}

// portHelp explains in the info popover why the port matters.
const portHelp = "Port forwarding is only needed for seeding. Downloading works without it, because bitnews connects out to peers itself. " +
	"Peers that cannot accept connections can only receive from you if they can reach this port, so forward it (TCP and UDP) on your router, " +
	"or let your VPN set it: gluetun, for example with ProtonVPN, can call POST /api/v1/torrent/port with your API key whenever it gets a new forwarded port."

// Help is the info popover text, for the templates.
func (portView) Help() string { return portHelp }

// portStatus asks the daemon for the listen port's state.
func (s *Server) portStatus() portView {
	if s.Config == nil {
		return portView{}
	}
	st, ok := s.Config.ListenPort()
	if !ok {
		return portView{}
	}
	v := portView{Known: true, Port: st.Port}
	port := strconv.Itoa(st.Port)
	if st.Open() {
		v.State, v.Label = "open", "Port "+port+" open"
		v.Detail = "A peer connected in on port " + port + " " + histRelative(time.Now(), st.LastIncoming) + "."
		return v
	}
	v.State, v.Label = "unconfirmed", "Port "+port+" not confirmed"
	v.Detail = "No peer has connected in on port " + port + " yet. If that stays so while you seed popular torrents, the port is probably not forwarded."
	return v
}
