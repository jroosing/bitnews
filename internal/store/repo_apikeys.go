package store

import (
	"context"
	"database/sql"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// APIKeyRepo implements core.APIKeyRepo on SQLite. Only the SHA-256 hash of a
// key is stored; the lookup by hash uses the UNIQUE index on api_keys.hash.
type APIKeyRepo struct {
	db *DB
}

// NewAPIKeyRepo returns an APIKeyRepo that stores API keys in db.
func NewAPIKeyRepo(db *DB) *APIKeyRepo { return &APIKeyRepo{db: db} }

// Create stores k. A taken hash is core.ErrConflict; a scope other than read
// or write is rejected by the schema.
func (r *APIKeyRepo) Create(ctx context.Context, k core.APIKey) error {
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO api_keys (id, name, scope, hash, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?)`,
		string(k.ID), k.Name, string(k.Scope), k.Hash[:], toMillis(k.CreatedAt), nullMillis(k.LastUsedAt))
	return mapErr("create api key", err)
}

const apiKeyColumns = `id, name, scope, hash, created_at, last_used_at`

func scanAPIKey(row rowScanner) (core.APIKey, error) {
	var (
		k       core.APIKey
		scope   string
		hash    []byte
		created int64
		used    sql.NullInt64
	)
	if err := row.Scan(&k.ID, &k.Name, &scope, &hash, &created, &used); err != nil {
		return core.APIKey{}, err
	}
	k.Scope = core.APIKeyScope(scope)
	copy(k.Hash[:], hash)
	k.CreatedAt = fromMillis(created)
	k.LastUsedAt = fromNullMillis(used)
	return k, nil
}

// List returns every API key, oldest first.
func (r *APIKeyRepo) List(ctx context.Context) ([]core.APIKey, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, mapErr("list api keys", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, mapErr("list api keys", err)
		}
		out = append(out, k)
	}
	return out, mapErr("list api keys", rows.Err())
}

// GetByHash returns the API key with the given hash.
func (r *APIKeyRepo) GetByHash(ctx context.Context, h core.TokenHash) (core.APIKey, error) {
	k, err := scanAPIKey(r.db.Reader().QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE hash = ?`, h[:]))
	return k, mapErr("get api key", err)
}

// TouchLastUsed records the last time the key authenticated a request.
func (r *APIKeyRepo) TouchLastUsed(ctx context.Context, id core.ID, at time.Time) error {
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, toMillis(at), string(id))
	return mapExec("touch api key", res, err)
}

// Delete revokes the key. An unknown ID is core.ErrNotFound.
func (r *APIKeyRepo) Delete(ctx context.Context, id core.ID) error {
	res, err := r.db.Writer().ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, string(id))
	return mapExec("delete api key", res, err)
}
