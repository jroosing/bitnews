package web

import (
	"context"

	"github.com/jroosing/bitnews/internal/core"
)

// RuntimeSource is what the UI asks the running daemon about the Queues:
// transfer speeds, whether a Queue is paused, and the speed limits. What is
// live about one Download (stage, rates, torrent counters) comes from the
// view module (Deps.View). internal/daemon implements it (web_runtime.go).
// The types are in queue_runtime.go.
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
}

// TorrentSource is what the UI asks about torrents that the view module does
// not give: the file list, and setting the Share Limit. The detail page owns
// this interface; internal/daemon implements it (web_torrents.go). The types
// are in detail_source.go.
type TorrentSource interface {
	// Files lists the files of the torrent of a Download, nil when the
	// torrent engine does not know them right now.
	Files(ctx context.Context, id core.ID) ([]TorrentFile, error)
	// SetShareLimit gives the torrent its own Share Limit, or with
	// UseDefault removes it so the global default applies again.
	SetShareLimit(ctx context.Context, id core.ID, change ShareLimitChange) error
}
