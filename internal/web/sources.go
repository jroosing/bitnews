package web

import (
	"context"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// RuntimeSource is what the UI asks the running daemon about live state that
// does not live in a Download row: transfer speeds, whether a Queue is
// paused, and the post-processing stage. The queue page owns this interface
// and extends it; internal/daemon implements it (web_runtime.go). The types
// are in queue_runtime.go.
type RuntimeSource interface {
	// Rates is the transfer speed of the whole daemon in bytes per second.
	Rates() (down, up int64)
	// QueueState says whether the Queue of one Source kind exists and
	// whether it is paused.
	QueueState(kind core.SourceKind) QueueState
	// PauseQueue and ResumeQueue are the user's pause of a whole Queue.
	PauseQueue(kind core.SourceKind)
	ResumeQueue(kind core.SourceKind)
	// SpeedLimits are the limits in force in bytes per second, 0 meaning
	// unlimited. They are runtime-only: set through SetSpeedLimit and lost
	// on restart. A Usenet Queue only has a download limit.
	SpeedLimits(kind core.SourceKind) (down, up int64)
	SetSpeedLimit(kind core.SourceKind, down, up int64)
	// Live is what is known about one Download beyond its row: the
	// post-processing stage, its current rates and, for a torrent, peers.
	Live(id core.ID) DownloadLive
}

// TorrentSource is what the UI asks about torrents beyond their Download row:
// info hash, ratio, peers, seeding time and Share Limit. The detail page owns
// this interface and extends it; internal/daemon implements it
// (web_torrents.go). The types are in detail_source.go.
type TorrentSource interface {
	// Torrent describes the torrent of a Download. ok is false when the
	// Download is not a torrent or has no torrent source.
	Torrent(ctx context.Context, id core.ID) (facts TorrentFacts, ok bool, err error)
	// SetShareLimit gives the torrent its own Share Limit, or with
	// UseDefault removes it so the global default applies again.
	SetShareLimit(ctx context.Context, id core.ID, change ShareLimitChange) error
}
