package core

// EventKind names the type of an Event. The values are stable: the SSE
// stream uses them as event names.
type EventKind string

// The kinds of Event.
const (
	KindDownloadAdded    EventKind = "download_added"
	KindDownloadUpdated  EventKind = "download_updated"
	KindDownloadProgress EventKind = "download_progress"
	KindFetched          EventKind = "fetched"
	KindReady            EventKind = "ready"
	KindFailed           EventKind = "failed"
	KindRemoved          EventKind = "removed"
	KindSettingsChanged  EventKind = "settings_changed"
)

// Event is something that happened that other parts of the daemon, and SSE
// clients, want to hear about. Events are published on a Bus and are never
// persisted. Every Event is a progress event or a lifecycle event: see
// EventKind.IsProgress.
type Event interface {
	Kind() EventKind
}

// IsProgress reports whether events of this kind are progress events.
// Progress events are high-frequency and superseded by the next one, so a Bus
// may drop them for a slow subscriber. Every other kind is a lifecycle event
// and must not be lost silently.
func (k EventKind) IsProgress() bool {
	return k == KindDownloadProgress
}

// DownloadAdded says a Download was created.
type DownloadAdded struct {
	Download Download
}

// DownloadUpdated says a Download changed: its State, Priority, Category or
// Paused flag. Download is the new snapshot.
type DownloadUpdated struct {
	Download Download
}

// DownloadProgress reports how much of a Download has been received. Engines
// pass it through a ProgressThrottle before publishing it.
type DownloadProgress struct {
	ID         ID
	DoneBytes  int64
	TotalBytes int64
	// BytesPerSecond is the current transfer rate.
	BytesPerSecond int64
}

// Fetched says a Download's data was received and integrity-checked.
type Fetched struct {
	ID ID
}

// Ready says Post-processing succeeded and importable files exist.
type Ready struct {
	ID ID
	// OutputPath is where the importable files are.
	OutputPath string
}

// Failed says a Download cannot become Ready.
type Failed struct {
	ID     ID
	Reason FailureReason
	// Detail is free text explaining the failure. It must already be
	// redacted.
	Detail string
}

// Removed says a Download was deleted.
type Removed struct {
	ID ID
}

// SettingsChanged says the Settings were written. It carries no payload:
// receivers read the current Settings, which may include secrets.
type SettingsChanged struct{}

// Kind implements Event.
func (DownloadAdded) Kind() EventKind { return KindDownloadAdded }

// Kind implements Event.
func (DownloadUpdated) Kind() EventKind { return KindDownloadUpdated }

// Kind implements Event.
func (DownloadProgress) Kind() EventKind { return KindDownloadProgress }

// Kind implements Event.
func (Fetched) Kind() EventKind { return KindFetched }

// Kind implements Event.
func (Ready) Kind() EventKind { return KindReady }

// Kind implements Event.
func (Failed) Kind() EventKind { return KindFailed }

// Kind implements Event.
func (Removed) Kind() EventKind { return KindRemoved }

// Kind implements Event.
func (SettingsChanged) Kind() EventKind { return KindSettingsChanged }
