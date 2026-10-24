package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"

	"github.com/jroosing/bitnews/internal/core"
)

// CategoryRepo implements core.CategoryRepo on SQLite.
type CategoryRepo struct {
	db *DB
}

// NewCategoryRepo returns a CategoryRepo that stores Categories in db.
func NewCategoryRepo(db *DB) *CategoryRepo { return &CategoryRepo{db: db} }

// SQLite extended result codes for the constraint violations the
// repositories report as core.ErrConflict.
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// mapExec is mapErr for statements that must affect a row: no affected row
// means core.ErrNotFound.
func mapExec(op string, res sql.Result, err error) error {
	if err != nil {
		return mapErr(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return mapErr(op, sql.ErrNoRows)
	}
	return nil
}

// mapErr translates SQLite errors into the core sentinel errors and wraps
// everything else with the operation name. It returns nil for a nil err.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, core.ErrNotFound)
	}
	if serr, ok := errors.AsType[*sqlite.Error](err); ok {
		switch serr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return fmt.Errorf("store: %s: %w: %w", op, core.ErrConflict, err)
		}
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

// Create validates and stores c.
func (r *CategoryRepo) Create(ctx context.Context, c core.Category) error {
	if err := c.Validate(); err != nil {
		return err
	}
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO categories (id, name, dir_override) VALUES (?, ?, ?)`,
		string(c.ID), c.Name, c.DirOverride)
	return mapErr("create category", err)
}

// Get returns the Category with the given ID.
func (r *CategoryRepo) Get(ctx context.Context, id core.ID) (core.Category, error) {
	var c core.Category
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT id, name, dir_override FROM categories WHERE id = ?`, string(id)).
		Scan(&c.ID, &c.Name, &c.DirOverride)
	return c, mapErr("get category", err)
}

// List returns every Category ordered by name.
func (r *CategoryRepo) List(ctx context.Context) ([]core.Category, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT id, name, dir_override FROM categories ORDER BY name`)
	if err != nil {
		return nil, mapErr("list categories", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Category
	for rows.Next() {
		var c core.Category
		if err := rows.Scan(&c.ID, &c.Name, &c.DirOverride); err != nil {
			return nil, mapErr("list categories", err)
		}
		out = append(out, c)
	}
	return out, mapErr("list categories", rows.Err())
}

// Update validates c and stores its name and dir override.
func (r *CategoryRepo) Update(ctx context.Context, c core.Category) error {
	if err := c.Validate(); err != nil {
		return err
	}
	res, err := r.db.Writer().ExecContext(ctx,
		`UPDATE categories SET name = ?, dir_override = ? WHERE id = ?`,
		c.Name, c.DirOverride, string(c.ID))
	return mapExec("update category", res, err)
}

// Delete removes the Category. Downloads that use it keep existing with no
// Category (the foreign key sets downloads.category_id to NULL).
func (r *CategoryRepo) Delete(ctx context.Context, id core.ID) error {
	res, err := r.db.Writer().ExecContext(ctx, `DELETE FROM categories WHERE id = ?`, string(id))
	return mapExec("delete category", res, err)
}
