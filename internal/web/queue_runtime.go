package web

import "gitlab.com/kyura4k/gitnews/internal/core"

// Why a Queue is paused, as RuntimeSource.QueueState reports it. They match
// the reasons of the runners; the web package does not import those.
const (
	PauseReasonUser            = "user"
	PauseReasonDiskFull        = "disk_full"
	PauseReasonBindUnavailable = "bind_unavailable"
)

// QueueState is the state of one Queue as the runtime reports it.
type QueueState struct {
	// Available is false when the Queue does not exist on this instance,
	// for example the torrent Queue when torrents are turned off.
	Available bool
	Paused    bool
	// Reason is one of the PauseReason constants (or a reason this package
	// does not know) while Paused.
	Reason string
}

// DownloadLive is the live facts about one Download that the database does
// not hold.
type DownloadLive struct {
	// Stage is the Post-processing stage, empty when none is known.
	Stage core.Stage
	// DownRate and UpRate are bytes per second.
	DownRate, UpRate int64
	// Torrent says Peers, Seeds and Ratio are valid.
	Torrent      bool
	Peers, Seeds int
	Ratio        float64
}
