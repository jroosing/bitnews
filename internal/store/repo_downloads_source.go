package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jroosing/bitnews/internal/core"
)

// DownloadSourceRepo implements core.DownloadSourceRepo on SQLite. The
// archive password is sealed with the master Key and the AAD
// download_sources/password/<download id>.
type DownloadSourceRepo struct {
	db  *DB
	key Key
}

// NewDownloadSourceRepo returns a DownloadSourceRepo that stores sources in
// db and encrypts passwords with key.
func NewDownloadSourceRepo(db *DB, key Key) *DownloadSourceRepo {
	return &DownloadSourceRepo{db: db, key: key}
}

func sourceAAD(id core.ID) string { return AAD("download_sources", "password", string(id)) }

// Create stores the source of an existing Download. A second source for the
// same Download is a conflict.
func (r *DownloadSourceRepo) Create(ctx context.Context, downloadID core.ID, nzbPath string, password core.Secret) error {
	var sealed sql.NullString
	if password != "" {
		s, err := Seal(r.key, password.Reveal(), sourceAAD(downloadID))
		if err != nil {
			return fmt.Errorf("store: seal download password: %w", err)
		}
		sealed = sql.NullString{String: s, Valid: true}
	}
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO download_sources (download_id, nzb_path, password) VALUES (?, ?, ?)`,
		string(downloadID), nzbPath, sealed)
	return mapErr("create download source", err)
}

// Get returns the NZB path and the password of a Download's source. When the
// password cannot be decrypted the path is still returned, with an error
// matching core.ErrSecretUnavailable.
func (r *DownloadSourceRepo) Get(ctx context.Context, downloadID core.ID) (string, core.Secret, error) {
	var (
		path   string
		sealed sql.NullString
	)
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT nzb_path, password FROM download_sources WHERE download_id = ?`, string(downloadID)).
		Scan(&path, &sealed)
	if err != nil {
		return "", "", mapErr("get download source", err)
	}
	if !sealed.Valid {
		return path, "", nil
	}
	plain, err := Unseal(r.key, sealed.String, sourceAAD(downloadID))
	if errors.Is(err, ErrDecrypt) || errors.Is(err, ErrUnsupportedFormat) {
		return path, "", fmt.Errorf("store: download %s password: %w", downloadID, core.ErrSecretUnavailable)
	}
	if err != nil {
		return "", "", err
	}
	return path, core.Secret(plain), nil
}
