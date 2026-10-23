package qbit

import (
	"context"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// What a media manager reads about a torrent comes from the Download view
// (Deps.View): its phase, sizes, progress, rates, ETA, ratio, uploaded bytes,
// seeding time, peers, seeds, private flag and effective Share Limit. The
// narrow ports below give only what the view does not hold: which Download
// an info hash belongs to, the torrent's name and paths, and its files. The
// daemon implements them over the torrent runner.

// ShareLimit is a Share Limit a client asks for. Negative Ratio and
// SeedingSeconds mean unlimited (-1, like core).
type ShareLimit struct {
	// Global means the global default applies; Ratio and SeedingSeconds
	// are then ignored.
	Global         bool
	Ratio          float64
	SeedingSeconds int64
}

// Torrent is what the adapter needs of a torrent besides its view.
type Torrent struct {
	// Name is the torrent's name: the engine's, else the Download's.
	Name string
	// SavePath is the directory that holds the torrent's content directory
	// or file; ContentPath is the content itself (the Download dir, after
	// the move to complete). Either may be empty when nothing is known.
	SavePath    string
	ContentPath string
}

// TorrentSource resolves torrents and describes them.
type TorrentSource interface {
	// Lookup returns the Download of the torrent with the (lowercase hex)
	// info hash; false means there is none.
	Lookup(ctx context.Context, hash string) (core.Download, bool, error)
	// Torrent describes the torrent of d; false means d has none. The
	// adapter calls it for every torrent on every poll of a media manager,
	// so without full it must be cheap (no disk or network access); full
	// may also look on disk for the content directory.
	Torrent(ctx context.Context, d core.Download, full bool) (Torrent, bool, error)
}

// TorrentFile is one file of a torrent.
type TorrentFile struct {
	// Name is the path within the torrent, with forward slashes.
	Name string
	Size int64
}

// FileSource lists the files of a torrent. It is optional: without it
// torrents/files answers an empty list.
type FileSource interface {
	// Files returns the files of the Download's torrent, empty while they
	// are not known; false means the Download has no torrent.
	Files(ctx context.Context, id core.ID) ([]TorrentFile, bool, error)
}

// AddOptions are the options of a torrents/add call that the engine honours.
type AddOptions struct {
	// CategoryID is the (already resolved or created) Category, or "".
	CategoryID core.ID
	// Paused adds the Download paused.
	Paused bool
	// SavePath is an absolute directory under one of the torrent roots
	// (already validated), or "" for the default.
	SavePath string
	// ShareLimit is the limit the call asked for; nil leaves the global
	// default.
	ShareLimit *ShareLimit
}

// TorrentAdder adds torrent Downloads. Both methods return the lowercase hex
// info hash. A torrent that exists already is an error matching
// core.ErrConflict; a bad torrent or URL is an error matching
// app.ErrInvalidInput. The URL may be a magnet link or an http(s) address of
// a .torrent file; it can hold a passkey, so it must never be logged or
// stored in clear.
type TorrentAdder interface {
	AddTorrentFile(ctx context.Context, data []byte, opts AddOptions) (hash string, err error)
	AddURL(ctx context.Context, url string, opts AddOptions) (hash string, err error)
}

// ShareLimiter sets the Share Limit of a Download. ratio and seedingSeconds
// are -1 for unlimited. useGlobal means the global default, and then the
// other two are ignored.
type ShareLimiter interface {
	SetShareLimit(ctx context.Context, id core.ID, ratio float64, seedingSeconds int64, useGlobal bool) error
}

// GlobalShare is the global Share Limit default, which preferences reports.
type GlobalShare struct {
	RatioEnabled bool
	Ratio        float64
	// SeedingTimeEnabled and SeedingMinutes are the seeding time limit.
	SeedingTimeEnabled bool
	SeedingMinutes     int
	// Action is what happens at the limit: "pause", "remove" or
	// "remove_files".
	Action string
}
