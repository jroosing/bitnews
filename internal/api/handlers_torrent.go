package api

import (
	"context"
	"math"
	"net/http"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// TorrentPort moves the torrent engine's listen port while it runs and says
// whether peers reach it. A VPN with port forwarding (gluetun, for example)
// calls POST /api/v1/torrent/port with the app key whenever it is handed a
// new port.
type TorrentPort interface {
	// SetListenPort moves the engine to port and stores it in the Settings.
	SetListenPort(ctx context.Context, port int) (core.ListenPortStatus, error)
	// ListenPortStatus is the current port and whether a peer has connected
	// in on it; ok is false when torrents are off.
	ListenPortStatus() (st core.ListenPortStatus, ok bool)
}

// portResponse is the body of both torrent port routes.
type portResponse struct {
	Port int `json:"port"`
	// Open is true once a peer has connected in on Port.
	Open         bool       `json:"open"`
	Since        time.Time  `json:"since"`
	LastIncoming *time.Time `json:"last_incoming"`
}

func portResponseOf(st core.ListenPortStatus) portResponse {
	out := portResponse{Port: st.Port, Open: st.Open(), Since: st.Since}
	if st.Open() {
		t := st.LastIncoming
		out.LastIncoming = &t
	}
	return out
}

func (s *Server) torrentsOff(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "torrents_unavailable", "torrents are turned off on this instance")
}

func (s *Server) getTorrentPort(w http.ResponseWriter, _ *http.Request) {
	if s.TorrentPort == nil {
		s.torrentsOff(w)
		return
	}
	st, ok := s.TorrentPort.ListenPortStatus()
	if !ok {
		s.torrentsOff(w)
		return
	}
	writeJSON(w, http.StatusOK, portResponseOf(st))
}

// setTorrentPort moves the listen port: {"port": 45678}.
func (s *Server) setTorrentPort(w http.ResponseWriter, r *http.Request) {
	if s.TorrentPort == nil {
		s.torrentsOff(w)
		return
	}
	var req struct {
		Port *int `json:"port"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Port == nil || *req.Port < 1 || *req.Port > math.MaxUint16 {
		writeError(w, http.StatusBadRequest, "invalid_port", "port must be a number from 1 to 65535")
		return
	}
	st, err := s.TorrentPort.SetListenPort(r.Context(), *req.Port)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, portResponseOf(st))
}
