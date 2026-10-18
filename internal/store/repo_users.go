package store

import (
	"context"
	"database/sql"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// UserRepo implements core.UserRepo on SQLite.
//
// Usernames are matched exactly (case-sensitive): the UNIQUE constraint on
// users.username is byte-exact, so "Admin" and "admin" are different users.
// Callers that want case-insensitive logins must normalise before calling.
type UserRepo struct {
	db *DB
}

// NewUserRepo returns a UserRepo that stores Users in db.
func NewUserRepo(db *DB) *UserRepo { return &UserRepo{db: db} }

// Create stores u. A taken username is core.ErrConflict.
func (r *UserRepo) Create(ctx context.Context, u core.User) error {
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
			string(u.ID), u.Username, u.PasswordHash, toMillis(u.CreatedAt))
		return err
	})
	return mapErr("create user", err)
}

// Get returns the User with the given ID.
func (r *UserRepo) Get(ctx context.Context, id core.ID) (core.User, error) {
	return r.scanOne(ctx, "get user", `WHERE id = ?`, string(id))
}

// GetByUsername returns the User with exactly the given username.
func (r *UserRepo) GetByUsername(ctx context.Context, username string) (core.User, error) {
	return r.scanOne(ctx, "get user by username", `WHERE username = ?`, username)
}

// Count reports how many Users exist.
func (r *UserRepo) Count(ctx context.Context) (int, error) {
	var n int
	err := r.db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, mapErr("count users", err)
}

func (r *UserRepo) scanOne(ctx context.Context, op, where string, arg any) (core.User, error) {
	var (
		u  core.User
		ms int64
	)
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users `+where, arg).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &ms)
	if err != nil {
		return core.User{}, mapErr(op, err)
	}
	u.CreatedAt = fromMillis(ms)
	return u, nil
}
