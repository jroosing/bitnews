package web

import (
	"context"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// DetailSource is what the Download detail page asks the running daemon
// beyond the Download row and its View: the Article report of a Usenet
// fetch. internal/daemon implements it (web_torrents.go). It may be nil.
type DetailSource interface {
	// FetchReport returns what the fetch of an NZB Download learned about the
	// state of its Articles. ok is false when there is no report (the
	// Download was not fetched yet, or it is a torrent).
	FetchReport(ctx context.Context, id core.ID) (report FetchReport, ok bool)
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
