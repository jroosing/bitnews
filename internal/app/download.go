package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

// DownloadService manages existing Downloads: list, read, pause, resume,
// change priority, category and queue position, and delete. It never moves a
// Download between lifecycle States: engines do that. Adding Downloads comes
// with the Usenet and torrent milestones.
type DownloadService struct {
	downloads core.DownloadRepo
	cats      core.CategoryRepo
	events    Publisher
	opts      options

	// mu serializes the read-modify-write cycles of the mutations.
	mu sync.Mutex
}

// NewDownloadService returns a DownloadService.
func NewDownloadService(downloads core.DownloadRepo, categories core.CategoryRepo, events Publisher, opts ...Option) *DownloadService {
	return &DownloadService{downloads: downloads, cats: categories, events: events, opts: newOptions(opts)}
}

// Pause stops a Download from being active. Pausing a paused Download does
// nothing. Only Downloads that have not been Fetched can be paused: for the
// others there is nothing left to pause (conflict, code "not_pausable").
func (s *DownloadService) Pause(ctx context.Context, id core.ID) (core.Download, error) {
	return s.setPaused(ctx, id, true)
}

// Resume undoes Pause. Resuming a Download that is not paused does nothing.
func (s *DownloadService) Resume(ctx context.Context, id core.ID) (core.Download, error) {
	return s.setPaused(ctx, id, false)
}

func (s *DownloadService) setPaused(ctx context.Context, id core.ID, paused bool) (core.Download, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.downloads.Get(ctx, id)
	if err != nil {
		return core.Download{}, err
	}
	if d.Paused == paused {
		return d, nil
	}
	if paused && !pausable(d.State) {
		return core.Download{}, conflict("not_pausable", "a download in state %q cannot be paused", d.State)
	}
	d.Paused = paused
	d.UpdatedAt = s.opts.now()
	if err := s.downloads.Update(ctx, d); err != nil {
		return core.Download{}, fmt.Errorf("app: pause download: %w", err)
	}
	s.events.Publish(core.DownloadUpdated{Download: d})
	return d, nil
}

// pausable reports whether a Download in state st is still waiting for or
// receiving data, which is what Paused acts on.
func pausable(st core.State) bool {
	switch st {
	case core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching:
		return true
	}
	return false
}

// ListFilter selects one page of Downloads. Zero-valued fields match
// everything.
type ListFilter struct {
	State      core.State
	CategoryID core.ID
	Kind       core.SourceKind
	// After is the Next cursor of the previous page.
	After string
	// Limit is the page size; zero takes the repository default.
	Limit int
}

// Page is one page of Downloads, oldest first.
type Page struct {
	// Items is never nil.
	Items []core.Download
	// Next is the cursor of the following page, empty on the last page.
	Next string
}

var (
	knownStates = []core.State{
		core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching,
		core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed,
	}
	knownKinds      = []core.SourceKind{core.SourceNZB, core.SourceTorrent}
	knownPriorities = []core.DownloadPriority{
		core.DownloadPriorityForce, core.DownloadPriorityHigh, core.DownloadPriorityNormal, core.DownloadPriorityLow,
	}
)

// List returns one page of Downloads matching f.
func (s *DownloadService) List(ctx context.Context, f ListFilter) (Page, error) {
	switch {
	case f.State != "" && !slices.Contains(knownStates, f.State):
		return Page{}, invalid("invalid_state", "unknown state %q", f.State)
	case f.Kind != "" && !slices.Contains(knownKinds, f.Kind):
		return Page{}, invalid("invalid_kind", "unknown kind %q: use nzb or torrent", f.Kind)
	case f.Limit < 0:
		return Page{}, invalid("invalid_limit", "limit must not be negative")
	}
	items, next, err := s.downloads.List(ctx, core.DownloadQuery{
		State: f.State, CategoryID: f.CategoryID, SourceKind: f.Kind, After: f.After, Limit: f.Limit,
	})
	if errors.Is(err, store.ErrInvalidCursor) {
		return Page{}, invalid("invalid_cursor", "the cursor is not valid")
	}
	if err != nil {
		return Page{}, fmt.Errorf("app: list downloads: %w", err)
	}
	if items == nil {
		items = []core.Download{}
	}
	return Page{Items: items, Next: next}, nil
}

// Get returns the Download with the given ID, or an error matching
// core.ErrNotFound.
func (s *DownloadService) Get(ctx context.Context, id core.ID) (core.Download, error) {
	d, err := s.downloads.Get(ctx, id)
	if err != nil {
		return core.Download{}, fmt.Errorf("app: get download: %w", err)
	}
	return d, nil
}

// Move says where to put a Download within its Queue.
type Move string

// The moves a Patch can ask for.
const (
	MoveTop    Move = "top"
	MoveBottom Move = "bottom"
	MoveUp     Move = "up"
	MoveDown   Move = "down"
)

// CategoryChange says whether a Patch changes the Category, and to what.
// The zero value leaves it alone.
type CategoryChange struct {
	set bool
	id  core.ID
}

// SetCategory returns a CategoryChange that assigns the Category with the
// given ID, or removes the Download's Category when id is empty.
func SetCategory(id core.ID) CategoryChange { return CategoryChange{set: true, id: id} }

// Patch lists the changes Update applies. The zero value of a field means
// "leave alone". The lifecycle State is deliberately absent: only engines
// move a Download through its States.
type Patch struct {
	Priority *core.DownloadPriority
	Category CategoryChange
	Move     Move
}

// Update validates the whole Patch, then applies it. A move renumbers the
// other Downloads of the same Queue, and each Download whose stored data
// changed gets a DownloadUpdated event. The returned Download is the
// patched one.
func (s *DownloadService) Update(ctx context.Context, id core.ID, p Patch) (core.Download, error) {
	if p.Priority == nil && !p.Category.set && p.Move == "" {
		return core.Download{}, invalid("empty_patch", "nothing to change: give priority, category_id or queue_position")
	}
	if p.Priority != nil && !slices.Contains(knownPriorities, *p.Priority) {
		return core.Download{}, invalid("invalid_priority", "unknown priority %q: use force, high, normal or low", *p.Priority)
	}
	var move func([]core.Download, core.ID) (map[core.ID]int, error)
	switch p.Move {
	case "":
	case MoveTop:
		move = core.MoveToTop
	case MoveBottom:
		move = core.MoveToBottom
	case MoveUp:
		move = core.MoveUp
	case MoveDown:
		move = core.MoveDown
	default:
		return core.Download{}, invalid("invalid_move", "unknown move %q: use top, bottom, up or down", p.Move)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.downloads.Get(ctx, id)
	if err != nil {
		return core.Download{}, fmt.Errorf("app: update download: %w", err)
	}
	before := d
	if p.Category.set && p.Category.id != "" {
		if _, err := s.cats.Get(ctx, p.Category.id); errors.Is(err, core.ErrNotFound) {
			return core.Download{}, invalid("invalid_category", "category %q does not exist", p.Category.id)
		} else if err != nil {
			return core.Download{}, fmt.Errorf("app: update download: %w", err)
		}
	}
	var others []core.Download // Queue neighbours whose position changes
	if move != nil {
		positions, queue, err := s.reposition(ctx, d, move)
		if err != nil {
			return core.Download{}, err
		}
		d.QueuePosition = positions[d.ID]
		for _, q := range queue {
			if q.ID != d.ID && q.QueuePosition != positions[q.ID] {
				q.QueuePosition = positions[q.ID]
				others = append(others, q)
			}
		}
	}

	if p.Priority != nil {
		d.Priority = *p.Priority
	}
	if p.Category.set {
		d.CategoryID = p.Category.id
	}
	now := s.opts.now()
	if before != d {
		d.UpdatedAt = now
		if err := s.downloads.Update(ctx, d); err != nil {
			return core.Download{}, fmt.Errorf("app: update download: %w", err)
		}
		s.events.Publish(core.DownloadUpdated{Download: d})
	}
	for _, o := range others {
		o.UpdatedAt = now
		if err := s.downloads.Update(ctx, o); err != nil {
			return core.Download{}, fmt.Errorf("app: reorder queue: %w", err)
		}
		s.events.Publish(core.DownloadUpdated{Download: o})
	}
	return d, nil
}

// queueStates are the States of a Download that is in its Queue: not yet
// Fetched.
var queueStates = []core.State{core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching}

// reposition computes the new QueuePositions after moving d with move. It
// returns them with the Downloads of d's Queue (the same Source kind, not yet
// Fetched). A Download that is not in its Queue is a conflict.
func (s *DownloadService) reposition(ctx context.Context, d core.Download, move func([]core.Download, core.ID) (map[core.ID]int, error)) (map[core.ID]int, []core.Download, error) {
	if !slices.Contains(queueStates, d.State) {
		return nil, nil, conflict("not_queued", "a download in state %q is not in a queue", d.State)
	}
	var queue []core.Download
	for _, st := range queueStates {
		for after := ""; ; {
			page, next, err := s.downloads.List(ctx, core.DownloadQuery{State: st, SourceKind: d.SourceKind, After: after, Limit: store.MaxListLimit})
			if err != nil {
				return nil, nil, fmt.Errorf("app: list queue: %w", err)
			}
			queue = append(queue, page...)
			if next == "" {
				break
			}
			after = next
		}
	}
	positions, err := move(queue, d.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("app: reorder queue: %w", err)
	}
	return positions, queue, nil
}

// Delete removes the Download and publishes core.Removed. With deleteData it
// first asks the DataRemover to delete the Download's files; if that fails
// the Download stays so the user can retry.
func (s *DownloadService) Delete(ctx context.Context, id core.ID, deleteData bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.downloads.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("app: delete download: %w", err)
	}
	if deleteData {
		if err := s.opts.remover.RemoveData(ctx, d); err != nil {
			return fmt.Errorf("app: delete download data: %w", err)
		}
	}
	if err := s.downloads.Delete(ctx, id); err != nil {
		return fmt.Errorf("app: delete download: %w", err)
	}
	s.events.Publish(core.Removed{ID: id})
	return nil
}
