package web

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
