package torrent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// DefaultFlushInterval is how often pending piece marks are written to the
// store. A crash loses at most this much; pieces whose mark was lost are
// simply hashed again.
const DefaultFlushInterval = 2 * time.Second

// ErrCompletionClosed is returned by a SQLitePieceCompletion after Close.
var ErrCompletionClosed = errors.New("torrent: piece completion closed")

// PieceStore is the row-level storage a SQLitePieceCompletion writes to;
// *store.TorrentPieceRepo implements it.
type PieceStore interface {
	Load(ctx context.Context, h core.InfoHash) (store.TorrentPieces, error)
	Save(ctx context.Context, h core.InfoHash, p store.TorrentPieces) error
	Delete(ctx context.Context, h core.InfoHash) error
}

// SQLitePieceCompletion implements anacrolix's storage.PieceCompletion over
// the torrent_pieces table: one bitmap row per torrent, kept in memory and
// written behind in batches (every DefaultFlushInterval and on Close).
//
// A verified piece is Ok and Complete. A piece this process has marked not
// complete is Ok and not Complete, which anacrolix needs: reporting it as
// unknown makes anacrolix ignore the piece for requests and the download
// stalls. That "known incomplete" state is memory only. After a restart, or
// after a mark was lost in a crash, a piece without a persisted mark is
// unknown and gets hashed, which costs time, never correctness. Because the
// row is keyed by info hash alone, a torrent moved to another directory
// keeps its completion and is not rehashed.
//
// It is safe for concurrent use.
type SQLitePieceCompletion struct {
	repo     PieceStore
	logger   *slog.Logger
	interval time.Duration

	flushMu sync.Mutex // serialises Flush and Forget against each other

	mu      sync.Mutex // guards m and closed
	m       map[core.InfoHash]*pieceEntry
	closed  bool
	stop    chan struct{}
	stopped chan struct{}
}

type pieceEntry struct {
	count int    // pieces tracked, at most store.MaxTorrentPieces
	bits  []byte // (count+7)/8 bytes: pieces verified; this is what is stored
	known []byte // same size: pieces marked either way by this process
	dirty bool
	gen   uint64 // bumped on every change, so a flush only clears what it wrote
}

// PieceCompletionOption configures NewSQLitePieceCompletion.
type PieceCompletionOption func(*SQLitePieceCompletion)

// WithFlushInterval sets how often pending marks are written; the default is
// DefaultFlushInterval.
func WithFlushInterval(d time.Duration) PieceCompletionOption {
	return func(p *SQLitePieceCompletion) { p.interval = d }
}

// WithPieceCompletionLogger sets the logger for flush and load problems.
func WithPieceCompletionLogger(l *slog.Logger) PieceCompletionOption {
	return func(p *SQLitePieceCompletion) { p.logger = l }
}

// NewSQLitePieceCompletion returns a completion that persists to repo and
// starts its flusher. Call Close to stop it and write what is pending.
func NewSQLitePieceCompletion(repo PieceStore, opts ...PieceCompletionOption) *SQLitePieceCompletion {
	p := &SQLitePieceCompletion{
		repo:     repo,
		logger:   slog.New(slog.DiscardHandler),
		interval: DefaultFlushInterval,
		m:        make(map[core.InfoHash]*pieceEntry),
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	go p.run()
	return p
}

func (p *SQLitePieceCompletion) run() {
	defer close(p.stopped)
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
			if err := p.Flush(); err != nil {
				p.logger.Warn("torrent piece completion: flush failed", "err", err)
			}
		}
	}
}

// entry returns the in-memory bitmap of h, loading it on first use. The
// caller holds p.mu.
func (p *SQLitePieceCompletion) entry(h core.InfoHash) (*pieceEntry, error) {
	if p.closed {
		return nil, ErrCompletionClosed
	}
	if e, ok := p.m[h]; ok {
		return e, nil
	}
	e := &pieceEntry{}
	switch stored, err := p.repo.Load(context.Background(), h); {
	case err == nil:
		e.count, e.bits = stored.PieceCount, stored.Bitmap
		e.known = append([]byte(nil), stored.Bitmap...)
	case errors.Is(err, core.ErrNotFound):
	case errors.Is(err, store.ErrInvalidPieces):
		p.logger.Warn("torrent piece completion: ignoring unreadable row", "info_hash", h.String(), "err", err)
	default:
		return nil, fmt.Errorf("torrent: load piece completion %s: %w", h, err)
	}
	p.m[h] = e
	return e, nil
}

func (e *pieceEntry) grow(count int) {
	if count <= e.count {
		return
	}
	e.bits = append(e.bits, make([]byte, (count+7)/8-len(e.bits))...)
	e.known = append(e.known, make([]byte, (count+7)/8-len(e.known))...)
	e.count = count
	e.touch()
}

func (e *pieceEntry) touch() {
	e.dirty = true
	e.gen++
}

// Get reports whether piece pk is verified. Pieces without a mark are not
// Ok, so that they are hashed.
func (p *SQLitePieceCompletion) Get(pk metainfo.PieceKey) (storage.Completion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, err := p.entry(core.InfoHash(pk.InfoHash))
	if err != nil {
		return storage.Completion{Err: err}, err
	}
	if pk.Index < 0 || pk.Index >= e.count {
		return storage.Completion{}, nil
	}
	mask := byte(1) << (pk.Index % 8)
	switch {
	case e.bits[pk.Index/8]&mask != 0:
		return storage.Completion{Ok: true, Complete: true}, nil
	case e.known[pk.Index/8]&mask != 0:
		return storage.Completion{Ok: true}, nil
	}
	return storage.Completion{}, nil
}

// Set marks piece pk verified or not. The mark reaches the store at the next
// flush.
func (p *SQLitePieceCompletion) Set(pk metainfo.PieceKey, complete bool) error {
	if pk.Index < 0 || pk.Index >= store.MaxTorrentPieces {
		return fmt.Errorf("%w: piece index %d", store.ErrInvalidPieces, pk.Index)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, err := p.entry(core.InfoHash(pk.InfoHash))
	if err != nil {
		return err
	}
	e.grow(pk.Index + 1)
	mask := byte(1) << (pk.Index % 8)
	e.known[pk.Index/8] |= mask
	if has := e.bits[pk.Index/8]&mask != 0; has != complete {
		e.bits[pk.Index/8] ^= mask
		e.touch()
	}
	return nil
}

// Register tells the completion how many pieces torrent h has, so the stored
// row has the exact count rather than the highest marked piece.
func (p *SQLitePieceCompletion) Register(h core.InfoHash, pieceCount int) error {
	if pieceCount < 0 || pieceCount > store.MaxTorrentPieces {
		return fmt.Errorf("%w: piece count %d", store.ErrInvalidPieces, pieceCount)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, err := p.entry(h)
	if err != nil {
		return err
	}
	switch {
	case pieceCount > e.count:
		e.grow(pieceCount)
	case pieceCount < e.count:
		e.bits = e.bits[:(pieceCount+7)/8]
		e.known = e.known[:len(e.bits)]
		if r := pieceCount % 8; r != 0 {
			e.bits[len(e.bits)-1] &= byte(1)<<r - 1
			e.known[len(e.known)-1] &= byte(1)<<r - 1
		}
		e.count = pieceCount
		e.touch()
	}
	return nil
}

// Flush writes every pending change to the store now.
func (p *SQLitePieceCompletion) Flush() error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	return p.flush()
}

type flushJob struct {
	h      core.InfoHash
	pieces store.TorrentPieces
	gen    uint64
}

func (p *SQLitePieceCompletion) flush() error {
	var jobs []flushJob
	p.mu.Lock()
	for h, e := range p.m {
		if e.dirty {
			jobs = append(jobs, flushJob{h, store.TorrentPieces{PieceCount: e.count, Bitmap: append([]byte(nil), e.bits...)}, e.gen})
		}
	}
	p.mu.Unlock()

	var errs []error
	for _, j := range jobs {
		if err := p.repo.Save(context.Background(), j.h, j.pieces); err != nil {
			errs = append(errs, fmt.Errorf("torrent: save piece completion %s: %w", j.h, err))
			continue
		}
		p.mu.Lock()
		if e := p.m[j.h]; e != nil && e.gen == j.gen {
			e.dirty = false
		}
		p.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Forget drops the completion of h from memory and deletes its row. Call it
// when the torrent is removed, after the torrent has stopped marking pieces.
func (p *SQLitePieceCompletion) Forget(ctx context.Context, h core.InfoHash) error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	p.mu.Lock()
	delete(p.m, h)
	p.mu.Unlock()
	return p.repo.Delete(ctx, h)
}

// Close stops the flusher and writes pending marks. It is safe to call twice.
func (p *SQLitePieceCompletion) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	close(p.stop)
	<-p.stopped
	return p.Flush()
}
