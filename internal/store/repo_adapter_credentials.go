package store

import (
	"context"
	"database/sql"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// AdapterCredentialRepo implements core.AdapterCredentialRepo on SQLite. Only
// the SHA-256 hash of a key is stored; the lookup by hash uses the UNIQUE
// index on adapter_credentials.secret_hash, the lookup by username the
// unique (kind, username) index.
type AdapterCredentialRepo struct {
	db *DB
}

// NewAdapterCredentialRepo returns an AdapterCredentialRepo that stores
// credentials in db.
func NewAdapterCredentialRepo(db *DB) *AdapterCredentialRepo {
	return &AdapterCredentialRepo{db: db}
}

// Create stores c. A taken hash or a taken (kind, username) is
// core.ErrConflict. The schema rejects an unknown kind, a username on an
// adapter_sab credential and an adapter_qbit credential without one.
func (r *AdapterCredentialRepo) Create(ctx context.Context, c core.AdapterCredential) error {
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO adapter_credentials (id, kind, name, username, secret_hash, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(c.ID), string(c.Kind), c.Name, nullString(c.Username), c.SecretHash[:],
		toMillis(c.CreatedAt), nullMillis(c.LastUsedAt))
	return mapErr("create adapter credential", err)
}

const adapterCredentialColumns = `id, kind, name, username, secret_hash, created_at, last_used_at`

func scanAdapterCredential(row rowScanner) (core.AdapterCredential, error) {
	var (
		c        core.AdapterCredential
		kind     string
		username sql.NullString
		hash     []byte
		created  int64
		used     sql.NullInt64
	)
	if err := row.Scan(&c.ID, &kind, &c.Name, &username, &hash, &created, &used); err != nil {
		return core.AdapterCredential{}, err
	}
	c.Kind = core.AdapterKind(kind)
	c.Username = username.String
	copy(c.SecretHash[:], hash)
	c.CreatedAt = fromMillis(created)
	c.LastUsedAt = fromNullMillis(used)
	return c, nil
}

// List returns every credential, oldest first.
func (r *AdapterCredentialRepo) List(ctx context.Context) ([]core.AdapterCredential, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+adapterCredentialColumns+` FROM adapter_credentials ORDER BY created_at, id`)
	if err != nil {
		return nil, mapErr("list adapter credentials", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.AdapterCredential
	for rows.Next() {
		c, err := scanAdapterCredential(rows)
		if err != nil {
			return nil, mapErr("list adapter credentials", err)
		}
		out = append(out, c)
	}
	return out, mapErr("list adapter credentials", rows.Err())
}

// GetByHash returns the credential whose key hashes to h.
func (r *AdapterCredentialRepo) GetByHash(ctx context.Context, h core.TokenHash) (core.AdapterCredential, error) {
	c, err := scanAdapterCredential(r.db.Reader().QueryRowContext(ctx,
		`SELECT `+adapterCredentialColumns+` FROM adapter_credentials WHERE secret_hash = ?`, h[:]))
	return c, mapErr("get adapter credential", err)
}

// GetByUsername returns the credential of the kind with the username.
func (r *AdapterCredentialRepo) GetByUsername(ctx context.Context, kind core.AdapterKind, username string) (core.AdapterCredential, error) {
	c, err := scanAdapterCredential(r.db.Reader().QueryRowContext(ctx,
		`SELECT `+adapterCredentialColumns+` FROM adapter_credentials WHERE kind = ? AND username = ?`,
		string(kind), username))
	return c, mapErr("get adapter credential", err)
}

// TouchLastUsed records the last time the credential authenticated a request.
func (r *AdapterCredentialRepo) TouchLastUsed(ctx context.Context, id core.ID, at time.Time) error {
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE adapter_credentials SET last_used_at = ? WHERE id = ?`, toMillis(at), string(id))
	return mapExec("touch adapter credential", res, err)
}

// Delete revokes the credential. An unknown ID is core.ErrNotFound.
func (r *AdapterCredentialRepo) Delete(ctx context.Context, id core.ID) error {
	res, err := r.db.Writer().ExecContext(ctx, `DELETE FROM adapter_credentials WHERE id = ?`, string(id))
	return mapExec("delete adapter credential", res, err)
}
