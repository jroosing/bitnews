package torrent

import (
	"context"
	"log/slog"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/jroosing/bitnews/internal/core"
)

// torrentStorage is the storage of one torrent: the files below its
// directory (see filestorage.go), with every path built by relPath and
// checked by torrentPaths before anything is opened.
type torrentStorage struct {
	dir     string
	pieces  *SQLitePieceCompletion
	logger  *slog.Logger
	onError func(error) // called when the torrent's info is refused
}

// OpenTorrent implements storage.ClientImpl.
func (s *torrentStorage) OpenTorrent(_ context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	if err := s.pieces.Register(core.InfoHash(ih), info.NumPieces()); err != nil {
		s.onError(err)
		return storage.TorrentImpl{}, err
	}
	files, err := openTorrentFiles(s.dir, info, ih, sharedCompletion{s.pieces}, s.logger)
	if err != nil {
		s.onError(err)
		return storage.TorrentImpl{}, err
	}
	return files.TorrentImpl(), nil
}

// sharedCompletion hides Close of a completion owned by the engine.
type sharedCompletion struct{ *SQLitePieceCompletion }

func (sharedCompletion) Close() error { return nil }
