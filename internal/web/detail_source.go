package web

import (
	"context"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// DetailSource is what the Download detail page asks the running daemon
// beyond the Download row: the Article report of a Usenet fetch and the
// current Post-processing stage. internal/daemon implements it
// (web_torrents.go). It may be nil.
type DetailSource interface {
	// FetchReport returns what the fetch of an NZB Download learned about the
	// state of its Articles. ok is false when there is no report (the
	// Download was not fetched yet, or it is a torrent).
	FetchReport(ctx context.Context, id core.ID) (report FetchReport, ok bool)
	// PostProcessStage returns the stage ("verifying", "repairing",
	// "unpacking" or "finishing") a Download in Post-processing is in. ok is
	// false when it is not known, for example right after a restart.
	PostProcessStage(id core.ID) (stage string, ok bool)
}

// FetchReport is the state of a Usenet fetch's Articles.
type FetchReport struct {
	// MissingArticles no Server had, with their encoded size.
	MissingArticles int
	MissingBytes    int64
	// DamagedArticles failed their CRC check; DamagedBytes is the decoded
	// size of the damaged ranges.
	DamagedArticles int
	DamagedBytes    int64
	// MissingFiles are files none of whose Articles arrived.
	MissingFiles int
}

// TorrentFile is one file of a torrent: a slash-separated path and its size.
type TorrentFile struct {
	Path string
	Size int64
}

// TorrentFacts is what the detail page shows of a torrent besides its
// Download row.
type TorrentFacts struct {
	// InfoHash is the lower-case hex info hash.
	InfoHash string
	// Registered says the torrent engine currently has the torrent; the live
	// numbers below are zero otherwise.
	Registered bool
	Private    bool
	// LimitPaused says the torrent was paused because it reached its Share
	// Limit.
	LimitPaused bool
	Size        int64
	Files       []TorrentFile
	Peers       int
	Seeds       int
	DownRate    int64 // bytes per second
	UpRate      int64 // bytes per second
	Uploaded    int64
	Ratio       float64
	SeedingTime time.Duration
	// OwnRatio and OwnSeedingTime are the Download's own Share Limit; nil
	// means the global default applies, a negative value means unlimited.
	OwnRatio       *float64
	OwnSeedingTime *time.Duration
}

// ShareLimitChange is a request to set a torrent's Share Limit.
type ShareLimitChange struct {
	// UseDefault removes the Download's own limit; the other fields are
	// ignored then.
	UseDefault bool
	// Ratio is the ratio after which seeding stops; negative means no ratio
	// limit.
	Ratio float64
	// Minutes is the seeding time after which seeding stops; negative means
	// no time limit.
	Minutes int
}
