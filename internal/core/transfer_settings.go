package core

import (
	"fmt"
	"math"
)

// MaxSpeedKBps bounds a speed limit: 1 TB/s is far beyond any link.
const MaxSpeedKBps = 1 << 30

// SpeedLimit is a download and an upload limit in KB/s (1 KB = 1024 bytes).
// Zero means unlimited.
type SpeedLimit struct {
	DownKBps int `json:"down_kbps"`
	UpKBps   int `json:"up_kbps"`
}

// SpeedSettings are the speed limits kept in the Settings. They are applied
// to the runners at start-up and whenever the Settings change.
//
// Global holds the overall limit and Usenet and Torrent one limit per
// protocol. A protocol's runner is given the tighter of the overall limit and
// its own (see Effective). The overall limit is therefore a cap on each
// protocol, not one shared budget: with both Queues busy the total can be up
// to twice the overall limit.
//
// The Usenet Queue only receives, so Usenet.UpKBps must stay zero. The zero
// value is the default: no limit at all.
type SpeedSettings struct {
	Global  SpeedLimit `json:"global"`
	Usenet  SpeedLimit `json:"usenet"`
	Torrent SpeedLimit `json:"torrent"`
}

// Effective returns the limits in bytes per second that the Queue of kind
// runs with: the tighter of the overall and the protocol's own limit, 0 for
// unlimited. A Usenet Queue never uploads, so its upload is always 0.
func (s SpeedSettings) Effective(kind SourceKind) (down, up int64) {
	switch kind {
	case SourceNZB:
		return tighter(s.Global.DownKBps, s.Usenet.DownKBps), 0
	case SourceTorrent:
		return tighter(s.Global.DownKBps, s.Torrent.DownKBps), tighter(s.Global.UpKBps, s.Torrent.UpKBps)
	}
	return 0, 0
}

// tighter converts two KB/s limits to bytes per second and returns the lower
// one that is set.
func tighter(a, b int) int64 {
	switch {
	case a <= 0 && b <= 0:
		return 0
	case a <= 0:
		return int64(b) * 1024
	case b <= 0:
		return int64(a) * 1024
	}
	return int64(min(a, b)) * 1024
}

func (s SpeedSettings) validate() []error {
	var problems []error
	for _, l := range []struct {
		name string
		v    int
	}{
		{"speed.global.down_kbps", s.Global.DownKBps}, {"speed.global.up_kbps", s.Global.UpKBps},
		{"speed.usenet.down_kbps", s.Usenet.DownKBps}, {"speed.usenet.up_kbps", s.Usenet.UpKBps},
		{"speed.torrent.down_kbps", s.Torrent.DownKBps}, {"speed.torrent.up_kbps", s.Torrent.UpKBps},
	} {
		if l.v < 0 || l.v > MaxSpeedKBps {
			problems = append(problems, fmt.Errorf("%s %d is not between 0 and %d", l.name, l.v, MaxSpeedKBps))
		}
	}
	if s.Usenet.UpKBps != 0 {
		problems = append(problems, fmt.Errorf("speed.usenet.up_kbps must be 0: Usenet only downloads"))
	}
	return problems
}

// DefaultTorrentPort is the torrent listen port when none is set. Peers need
// it forwarded to reach bitnews: there is no UPnP.
const DefaultTorrentPort = 51413

// MSEPolicy is the message stream encryption (header obfuscation) policy of
// torrent connections.
type MSEPolicy string

// The MSE policies. The empty value means MSEPrefer.
const (
	// MSEPrefer offers obfuscation and still accepts plaintext peers.
	MSEPrefer MSEPolicy = "prefer"
	// MSERequire refuses plaintext peers.
	MSERequire MSEPolicy = "require"
	// MSEDisable talks plaintext only.
	MSEDisable MSEPolicy = "disable"
)

// TorrentSettings are the network options of the torrent engine. The engine
// reads them once, when it starts: a change needs a restart of bitnews.
//
// The zero value is the default, so a client that does not know the key and
// sends none gets the defaults: the default port, DHT and PEX on, local peer
// discovery off and encryption preferred. That is why DHT and PEX are
// switched off with Disable fields.
type TorrentSettings struct {
	// ListenPort is the TCP and uTP port. Zero means DefaultTorrentPort.
	ListenPort int `json:"listen_port"`
	// DisableDHT and DisablePEX switch the peer discovery methods off. Both
	// are used for public torrents only, whatever these say.
	DisableDHT bool `json:"disable_dht"`
	DisablePEX bool `json:"disable_pex"`
	// LSD turns local peer discovery on. The engine has no support for it
	// yet, so it has no effect today.
	LSD bool `json:"lsd"`
	// MSE is the encryption policy. Empty means MSEPrefer.
	MSE MSEPolicy `json:"mse"`
}

// EffectivePort is the listen port, with 0 read as DefaultTorrentPort.
func (t TorrentSettings) EffectivePort() int {
	if t.ListenPort == 0 {
		return DefaultTorrentPort
	}
	return t.ListenPort
}

// DHTEnabled and PEXEnabled say whether the discovery method is used.
func (t TorrentSettings) DHTEnabled() bool { return !t.DisableDHT }
func (t TorrentSettings) PEXEnabled() bool { return !t.DisablePEX }

// EffectiveMSE is the policy, with the empty value read as MSEPrefer.
func (t TorrentSettings) EffectiveMSE() MSEPolicy {
	if t.MSE == "" {
		return MSEPrefer
	}
	return t.MSE
}

func (t TorrentSettings) validate() []error {
	var problems []error
	if t.ListenPort < 0 || t.ListenPort > math.MaxUint16 {
		problems = append(problems, fmt.Errorf("torrent.listen_port %d is not between 0 and %d", t.ListenPort, math.MaxUint16))
	}
	switch t.MSE {
	case "", MSEPrefer, MSERequire, MSEDisable:
	default:
		problems = append(problems, fmt.Errorf("torrent.mse %q is not prefer, require or disable", t.MSE))
	}
	return problems
}

// Limits of the disk and post-processing settings.
const (
	// DefaultMinFree is the free space kept on the disk, on top of what a
	// Download still needs: 5 GiB.
	DefaultMinFree uint64 = 5 << 30
	// MaxMinFree bounds min_free to 1 PiB.
	MaxMinFree = 1 << 50
	// MaxPostprocConcurrency bounds how many Downloads are post-processed at
	// once; unpacking is disk bound, so more is never useful.
	MaxPostprocConcurrency = 16
)

// EffectivePostprocConcurrency is how many Downloads are post-processed at
// once, with 0 read as 1.
func (q QueueLimits) EffectivePostprocConcurrency() int {
	return max(q.PostprocConcurrency, 1)
}

// EffectiveMinFree is the free space to keep in bytes, with 0 read as
// DefaultMinFree.
func (q QueueLimits) EffectiveMinFree() uint64 {
	if q.MinFree <= 0 {
		return DefaultMinFree
	}
	return uint64(q.MinFree)
}

func (s Settings) validateTransfer() []error {
	problems := append(s.Speed.validate(), s.Torrent.validate()...)
	if s.Queues.PostprocConcurrency < 0 || s.Queues.PostprocConcurrency > MaxPostprocConcurrency {
		problems = append(problems, fmt.Errorf("postproc_concurrency %d is not between 0 and %d", s.Queues.PostprocConcurrency, MaxPostprocConcurrency))
	}
	if s.Queues.MinFree < 0 || s.Queues.MinFree > MaxMinFree {
		problems = append(problems, fmt.Errorf("min_free %d is not between 0 and %d", s.Queues.MinFree, int64(MaxMinFree)))
	}
	return problems
}
