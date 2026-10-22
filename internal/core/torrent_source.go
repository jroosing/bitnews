package core

import "context"

// TorrentSource says what a torrent Download was created from. Like an NZB's
// stored source it is written once, when the Download is added.
//
// A magnet link and a .torrent file can both carry tracker URLs with a
// personal passkey, so the magnet is a Secret (stored sealed) and the
// .torrent file is kept on disk with mode 0600, never in the database.
type TorrentSource struct {
	DownloadID ID
	InfoHash   InfoHash
	// TorrentPath is the stored .torrent file. It is empty for a magnet link
	// until its metadata has been fetched and saved with SetTorrentPath.
	TorrentPath string
	// Magnet is the magnet URI the Download was added with; empty when it was
	// added from a .torrent file.
	Magnet Secret
}

// TorrentSourceRepo stores TorrentSources. Deleting a Download deletes its
// source.
type TorrentSourceRepo interface {
	// Create stores the source. A second source with the same info hash is
	// ErrConflict, which is how duplicate torrents are detected.
	Create(ctx context.Context, s TorrentSource) error
	Get(ctx context.Context, id ID) (TorrentSource, error)
	GetByInfoHash(ctx context.Context, h InfoHash) (TorrentSource, error)
	// SetTorrentPath records the .torrent file saved for a magnet Download
	// once its metadata arrived.
	SetTorrentPath(ctx context.Context, id ID, path string) error
}
