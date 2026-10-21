package qbit

import (
	"context"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// The torrent-specific facts a media manager reads (info hash, ratio,
// uploaded bytes, seeding time, share limits, paths, peers, speeds) do not
// live in core.Download. The adapter asks the narrow ports below for them and
// joins the answer to the Download by DownloadID; the daemon implements them
// over the torrent engine and its stores.

// ShareLimit is the Share Limit of one torrent as the engine applies it.
// Negative Ratio and SeedingSeconds mean unlimited (-1, like core).
type ShareLimit struct {
	// Global means the global default applies; Ratio and SeedingSeconds
	// are then ignored.
	Global         bool
	Ratio          float64
	SeedingSeconds int64
}

// TorrentView is the live state of one torrent.
type TorrentView struct {
	DownloadID core.ID
	// InfoHash is the 40 character lowercase hex v1 info hash (a v2-only
	// torrent: the truncated v2 hash).
	InfoHash string
	Name     string
	// Size is the size of the content in bytes; 0 while the metadata of a
	// magnet link is missing.
	Size int64
	// DoneBytes is what has been received and verified.
	DoneBytes int64
	// DownRate and UpRate are bytes per second.
	DownRate int64
	UpRate   int64
	// Uploaded is the total uploaded bytes.
	Uploaded int64
	// Ratio is uploaded bytes divided by Size.
	Ratio float64
	// SeedingTime is how long the torrent has been seeding.
	SeedingTime time.Duration
	// Peers is the number of connected leechers and Seeds of connected seeds.
	Peers int
	Seeds int
	// SavePath is the directory that holds the torrent's content directory
	// or file; ContentPath is the content itself (the Download dir, after
	// the move to complete).
	SavePath    string
	ContentPath string
	AddedAt     time.Time
	// CompletedAt is zero until the torrent is Fetched.
	CompletedAt time.Time
	ShareLimit  ShareLimit
	// Private is the BEP 27 flag.
	Private bool
	// Paused is the engine's pause: a Download that was paused by the user
	// before it was Fetched, or one that reached its Share Limit and stopped
	// seeding.
	Paused bool
}

// TorrentSource reads the torrents of the engine.
type TorrentSource interface {
	// List returns every torrent. The adapter may call it on every poll of a
	// media manager, so it must be cheap (no disk or network access).
	List(ctx context.Context) ([]TorrentView, error)
	// Get returns the torrent with the (lowercase hex) info hash; false
	// means there is none.
	Get(ctx context.Context, hash string) (TorrentView, bool, error)
}

// TorrentFile is one file of a torrent.
type TorrentFile struct {
	// Name is the path within the torrent, with forward slashes.
	Name     string
	Size     int64
	Progress float64
}

// FileSource lists the files of a torrent. It is optional: without it
// torrents/files answers an empty list.
type FileSource interface {
	// Files returns the files of the torrent; false means there is no such
	// torrent.
	Files(ctx context.Context, hash string) ([]TorrentFile, bool, error)
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
