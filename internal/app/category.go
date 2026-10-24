package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jroosing/bitnews/internal/core"
)

// CategoryService manages Categories.
type CategoryService struct {
	cats      core.CategoryRepo
	downloads core.DownloadRepo
	events    Publisher
	opts      options

	mu sync.Mutex
}

// NewCategoryService returns a CategoryService. downloads is read when a
// Category is deleted, to announce the Downloads that lose it.
func NewCategoryService(cats core.CategoryRepo, downloads core.DownloadRepo, events Publisher, opts ...Option) *CategoryService {
	return &CategoryService{cats: cats, downloads: downloads, events: events, opts: newOptions(opts)}
}

// List returns every Category ordered by name. The slice is never nil.
func (s *CategoryService) List(ctx context.Context) ([]core.Category, error) {
	cs, err := s.cats.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list categories: %w", err)
	}
	if cs == nil {
		cs = []core.Category{}
	}
	return cs, nil
}

// Create validates and stores a new Category. A name that is already taken
// is a conflict (code "name_taken").
func (s *CategoryService) Create(ctx context.Context, name, dirOverride string) (core.Category, error) {
	c := core.Category{ID: core.NewID(), Name: name, DirOverride: dirOverride}
	if err := validateCategory(c); err != nil {
		return core.Category{}, err
	}
	if err := s.cats.Create(ctx, c); err != nil {
		return core.Category{}, categoryWriteError("create", err)
	}
	return c, nil
}

// CategoryPatch lists the changes Update applies; nil fields are left alone.
// A DirOverride that points at the empty string removes the override.
type CategoryPatch struct {
	Name        *string
	DirOverride *string
}

// Update validates and stores the changes to a Category.
func (s *CategoryService) Update(ctx context.Context, id core.ID, p CategoryPatch) (core.Category, error) {
	if p.Name == nil && p.DirOverride == nil {
		return core.Category{}, invalid("empty_patch", "nothing to change: give name or dir_override")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cats.Get(ctx, id)
	if err != nil {
		return core.Category{}, fmt.Errorf("app: update category: %w", err)
	}
	if p.Name != nil {
		c.Name = *p.Name
	}
	if p.DirOverride != nil {
		c.DirOverride = *p.DirOverride
	}
	if err := validateCategory(c); err != nil {
		return core.Category{}, err
	}
	if err := s.cats.Update(ctx, c); err != nil {
		return core.Category{}, categoryWriteError("update", err)
	}
	return c, nil
}

// Delete removes a Category. Downloads that used it lose it, and each of them
// is announced with a DownloadUpdated event.
func (s *CategoryService) Delete(ctx context.Context, id core.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	affected, err := s.downloadsIn(ctx, id)
	if err != nil {
		return err
	}
	if err := s.cats.Delete(ctx, id); err != nil {
		return fmt.Errorf("app: delete category: %w", err)
	}
	for _, did := range affected {
		d, err := s.downloads.Get(ctx, did)
		if err != nil {
			continue // removed meanwhile: its Removed event covers it
		}
		s.events.Publish(core.DownloadUpdated{Download: d})
	}
	return nil
}

// downloadsIn returns the IDs of the Downloads that use the Category.
func (s *CategoryService) downloadsIn(ctx context.Context, id core.ID) ([]core.ID, error) {
	var out []core.ID
	for after := ""; ; {
		page, next, err := s.downloads.List(ctx, core.DownloadQuery{CategoryID: id, After: after, Limit: 200})
		if err != nil {
			return nil, fmt.Errorf("app: delete category: %w", err)
		}
		for _, d := range page {
			out = append(out, d.ID)
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

func validateCategory(c core.Category) error {
	err := c.Validate()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrInvalidCategoryName):
		return invalid("invalid_name", "category name must use only a-z, 0-9, '.', '_' and '-', be 1 to 64 characters long, and not be '.' or '..'")
	default:
		return invalid("invalid_dir_override", "dir_override must be an absolute path")
	}
}

func categoryWriteError(op string, err error) error {
	if errors.Is(err, core.ErrConflict) {
		return &Error{Kind: core.ErrConflict, Code: "name_taken", Message: "a category with this name already exists"}
	}
	return fmt.Errorf("app: %s category: %w", op, err)
}
