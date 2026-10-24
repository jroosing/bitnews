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

	// mu serializes the read-modify-write cycles of the mutations and guards
	// deleting. It is never held while files are deleted.
	mu       sync.Mutex
	deleting map[core.ID]struct{} // Downloads a Delete is working on
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

// queueOf returns the Downloads of the Queue of one Source kind: those not
// yet Fetched.
func (s *DownloadService) queueOf(ctx context.Context, kind core.SourceKind) ([]core.Download, error) {
	var queue []core.Download
	for _, st := range queueStates {
		for after := ""; ; {
			page, next, err := s.downloads.List(ctx, core.DownloadQuery{State: st, SourceKind: kind, After: after, Limit: store.MaxListLimit})
			if err != nil {
				return nil, fmt.Errorf("app: list queue: %w", err)
			}
			queue = append(queue, page...)
			if next == "" {
				break
			}
			after = next
		}
	}
	return queue, nil
}

// reposition computes the new QueuePositions after moving d with move. It
// returns them with the Downloads of d's Queue (the same Source kind, not yet
// Fetched). A Download that is not in its Queue is a conflict.
func (s *DownloadService) reposition(ctx context.Context, d core.Download, move func([]core.Download, core.ID) (map[core.ID]int, error)) (map[core.ID]int, []core.Download, error) {
	if !slices.Contains(queueStates, d.State) {
		return nil, nil, conflict("not_queued", "a download in state %q is not in a queue", d.State)
	}
	queue, err := s.queueOf(ctx, d.SourceKind)
	if err != nil {
		return nil, nil, err
	}
	positions, err := move(queue, d.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("app: reorder queue: %w", err)
	}
	return positions, queue, nil
}

// busyStates are the States in which Post-processing owns a Download's
// files: Fetched (it is about to start) and PostProcessing (par2 or 7zz may
// be running). A Download in one of them is not deleted.
var busyStates = []core.State{core.StateFetched, core.StatePostProcessing}

func busyError() error {
	return conflict("download_busy", "This download is being post-processed (repaired or unpacked) and its files are in use. Remove it once it is ready or has failed.")
}

// IsBusy reports whether err is Delete refusing a Download that
// Post-processing is working on (code "download_busy").
func IsBusy(err error) bool {
	appErr, ok := errors.AsType[*Error](err)
	return ok && appErr.Code == "download_busy"
}

// Delete removes the Download and publishes core.Removed. A Download that
// is being post-processed is refused (conflict, code "download_busy"), with
// or without its data, because its files are in use; a second Delete of a
// Download that is still being deleted is a conflict too (code
// "download_deleting").
//
// With deleteData the DataRemover first stops and holds whatever still works
// on the files, then the state is checked again (the fetch may have finished
// just before the hold), and only then the files are deleted. If that fails,
// the hold is released and the Download stays, so the user can retry. The
// row goes last, and only if Post-processing has not claimed it meanwhile.
//
// Deleting files can take long; no lock is held meanwhile, so other
// Downloads are not kept waiting.
func (s *DownloadService) Delete(ctx context.Context, id core.ID, deleteData bool) error {
	if !s.startDeleting(id) {
		return conflict("download_deleting", "the download is already being removed")
	}
	defer s.doneDeleting(id)
	d, err := s.downloads.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("app: delete download: %w", err)
	}
	if slices.Contains(busyStates, d.State) {
		return busyError()
	}
	if deleteData {
		if err := s.removeData(ctx, d); err != nil {
			return err
		}
	}
	if err := s.downloads.DeleteUnlessIn(ctx, id, busyStates); err != nil {
		if errors.Is(err, core.ErrConflict) {
			return busyError()
		}
		return fmt.Errorf("app: delete download: %w", err)
	}
	s.events.Publish(core.Removed{ID: id})
	return nil
}

// removeData holds d, checks that Post-processing has not taken it, and
// deletes its files.
func (s *DownloadService) removeData(ctx context.Context, d core.Download) error {
	release, err := s.opts.remover.Hold(ctx, d)
	if err != nil {
		return fmt.Errorf("app: delete download data: %w", err)
	}
	now, err := s.downloads.Get(ctx, d.ID)
	if err == nil && slices.Contains(busyStates, now.State) {
		err = busyError()
	}
	if err == nil {
		if err = s.opts.remover.RemoveData(ctx, now); err != nil {
			err = fmt.Errorf("app: delete download data: %w", err)
		}
	}
	if err != nil {
		release()
		return err
	}
	return nil
}

func (s *DownloadService) startDeleting(id core.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.deleting[id]; busy {
		return false
	}
	if s.deleting == nil {
		s.deleting = map[core.ID]struct{}{}
	}
	s.deleting[id] = struct{}{}
	return true
}

func (s *DownloadService) doneDeleting(id core.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deleting, id)
}
