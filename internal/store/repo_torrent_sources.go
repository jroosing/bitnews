package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// TorrentSourceRepo implements core.TorrentSourceRepo on SQLite. The magnet
// link is sealed with the master Key and the AAD
// torrent_sources/magnet/<download id>.
type TorrentSourceRepo struct {
	db  *DB
	key Key
}

// NewTorrentSourceRepo returns a TorrentSourceRepo that stores sources in db
// and encrypts magnet links with key.
func NewTorrentSourceRepo(db *DB, key Key) *TorrentSourceRepo {
	return &TorrentSourceRepo{db: db, key: key}
}

func magnetAAD(id core.ID) string { return AAD("torrent_sources", "magnet", string(id)) }

// Create stores the source of an existing Download. A source whose info hash
// is already stored, or a second source for the same Download, is
// core.ErrConflict. The source needs a TorrentPath or a Magnet.
func (r *TorrentSourceRepo) Create(ctx context.Context, s core.TorrentSource) error {
	if s.TorrentPath == "" && s.Magnet == "" {
		return fmt.Errorf("store: create torrent source: a torrent path or a magnet is required")
	}
	var path, sealed sql.NullString
	if s.TorrentPath != "" {
		path = sql.NullString{String: s.TorrentPath, Valid: true}
	}
	if s.Magnet != "" {
		v, err := Seal(r.key, s.Magnet.Reveal(), magnetAAD(s.DownloadID))
		if err != nil {
			return fmt.Errorf("store: seal magnet: %w", err)
		}
		sealed = sql.NullString{String: v, Valid: true}
	}
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO torrent_sources (download_id, info_hash, torrent_path, magnet, created_at) VALUES (?, ?, ?, ?, ?)`,
		string(s.DownloadID), s.InfoHash.String(), path, sealed, toMillis(time.Now()))
	return mapErr("create torrent source", err)
}

// Get returns the source of a Download. When the magnet cannot be decrypted
// the rest of the record is still returned, with an error matching
// core.ErrSecretUnavailable.
func (r *TorrentSourceRepo) Get(ctx context.Context, id core.ID) (core.TorrentSource, error) {
	return r.query(ctx, "get torrent source", `WHERE download_id = ?`, string(id))
}

// GetByInfoHash returns the source with the given info hash.
func (r *TorrentSourceRepo) GetByInfoHash(ctx context.Context, h core.InfoHash) (core.TorrentSource, error) {
	return r.query(ctx, "get torrent source by info hash", `WHERE info_hash = ?`, h.String())
}

func (r *TorrentSourceRepo) query(ctx context.Context, op, where string, arg any) (core.TorrentSource, error) {
	var (
		s            core.TorrentSource
		id, hash     string
		path, sealed sql.NullString
	)
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT download_id, info_hash, torrent_path, magnet FROM torrent_sources `+where, arg).
		Scan(&id, &hash, &path, &sealed)
	if err != nil {
		return core.TorrentSource{}, mapErr(op, err)
	}
	s.DownloadID = core.ID(id)
	if s.InfoHash, err = core.ParseInfoHash(hash); err != nil {
		return core.TorrentSource{}, fmt.Errorf("store: %s: stored info hash: %w", op, err)
	}
	s.TorrentPath = path.String
	if !sealed.Valid {
		return s, nil
	}
	plain, err := Unseal(r.key, sealed.String, magnetAAD(s.DownloadID))
	if errors.Is(err, ErrDecrypt) || errors.Is(err, ErrUnsupportedFormat) {
		return s, fmt.Errorf("store: download %s magnet: %w", s.DownloadID, core.ErrSecretUnavailable)
	}
	if err != nil {
		return core.TorrentSource{}, err
	}
	s.Magnet = core.Secret(plain)
	return s, nil
}

// SetTorrentPath records the .torrent file saved for a magnet Download.
func (r *TorrentSourceRepo) SetTorrentPath(ctx context.Context, id core.ID, path string) error {
	if path == "" {
		return fmt.Errorf("store: set torrent path: the path is empty")
	}
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE torrent_sources SET torrent_path = ? WHERE download_id = ?`, path, string(id))
	if err != nil {
		return mapErr("set torrent path", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("store: set torrent path: %w", core.ErrNotFound)
	}
	return nil
}
