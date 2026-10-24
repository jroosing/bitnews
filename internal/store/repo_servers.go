package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jroosing/bitnews/internal/core"
)

// ServerRepo implements core.ServerRepo on SQLite. Passwords are stored
// sealed with the master Key and the AAD servers/password/<id>.
type ServerRepo struct {
	db  *DB
	key Key
}

// NewServerRepo returns a ServerRepo that stores Servers in db and encrypts
// their passwords with key.
func NewServerRepo(db *DB, key Key) *ServerRepo { return &ServerRepo{db: db, key: key} }

const serverColumns = `id, name, host, port, username, password, max_connections,
	retention_days, priority, enabled, created_at`

func serverAAD(id core.ID) string { return AAD("servers", "password", string(id)) }

// sealPassword returns the stored form of a password; none is stored as "".
func (r *ServerRepo) sealPassword(s core.Server) (string, error) {
	if s.Password == "" {
		return "", nil
	}
	sealed, err := Seal(r.key, s.Password.Reveal(), serverAAD(s.ID))
	if err != nil {
		return "", fmt.Errorf("store: seal server password: %w", err)
	}
	return sealed, nil
}

// Create validates s and stores it with its password encrypted.
func (r *ServerRepo) Create(ctx context.Context, s core.Server) error {
	if err := s.Validate(); err != nil {
		return err
	}
	sealed, err := r.sealPassword(s)
	if err != nil {
		return err
	}
	_, err = r.db.Writer().ExecContext(ctx,
		`INSERT INTO servers (`+serverColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(s.ID), s.Name, s.Host, s.Port, s.Username, sealed, s.MaxConnections,
		s.RetentionDays, s.Priority, s.Enabled, toMillis(s.CreatedAt))
	return mapErr("create server", err)
}

func (r *ServerRepo) scan(row rowScanner) (core.Server, error) {
	var (
		s       core.Server
		id      string
		sealed  string
		created int64
	)
	if err := row.Scan(&id, &s.Name, &s.Host, &s.Port, &s.Username, &sealed, &s.MaxConnections,
		&s.RetentionDays, &s.Priority, &s.Enabled, &created); err != nil {
		return core.Server{}, err
	}
	s.ID = core.ID(id)
	s.CreatedAt = fromMillis(created)
	if sealed != "" {
		plain, err := Unseal(r.key, sealed, serverAAD(s.ID))
		switch {
		case err == nil:
			s.Password = core.Secret(plain)
		case errors.Is(err, ErrDecrypt), errors.Is(err, ErrUnsupportedFormat):
			// Wrong or missing key, or a damaged value: the Server stays
			// usable for listing, and the user enters the password again.
			s.PasswordUnavailable = true
		default:
			return core.Server{}, err
		}
	}
	return s, nil
}

// Get returns the Server with the given ID. A password that cannot be
// decrypted is reported through Server.PasswordUnavailable, not as an error.
func (r *ServerRepo) Get(ctx context.Context, id core.ID) (core.Server, error) {
	s, err := r.scan(r.db.Reader().QueryRowContext(ctx,
		`SELECT `+serverColumns+` FROM servers WHERE id = ?`, string(id)))
	return s, mapErr("get server", err)
}

// List returns every Server ordered by Priority, then Name.
func (r *ServerRepo) List(ctx context.Context) ([]core.Server, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+serverColumns+` FROM servers ORDER BY priority, name`)
	if err != nil {
		return nil, mapErr("list servers", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Server
	for rows.Next() {
		s, err := r.scan(rows)
		if err != nil {
			return nil, mapErr("list servers", err)
		}
		out = append(out, s)
	}
	return out, mapErr("list servers", rows.Err())
}

// Update validates s and stores every field except ID and CreatedAt. The
// password is sealed again, except when s.PasswordUnavailable is set: then
// the stored one stays as it is.
func (r *ServerRepo) Update(ctx context.Context, s core.Server) error {
	if err := s.Validate(); err != nil {
		return err
	}
	const common = `UPDATE servers SET name = ?, host = ?, port = ?, username = ?,
		max_connections = ?, retention_days = ?, priority = ?, enabled = ?`
	args := []any{s.Name, s.Host, s.Port, s.Username, s.MaxConnections, s.RetentionDays, s.Priority, s.Enabled}
	query := common + ` WHERE id = ?`
	if !s.PasswordUnavailable {
		sealed, err := r.sealPassword(s)
		if err != nil {
			return err
		}
		query = common + `, password = ? WHERE id = ?`
		args = append(args, sealed)
	}
	res, err := r.db.Writer().ExecContext(ctx, query, append(args, string(s.ID))...)
	return mapExec("update server", res, err)
}

// Delete removes the Server.
func (r *ServerRepo) Delete(ctx context.Context, id core.ID) error {
	res, err := r.db.Writer().ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, string(id))
	return mapExec("delete server", res, err)
}
