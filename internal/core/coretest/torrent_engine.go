package coretest

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// TorrentCall is one recorded call to a FakeTorrentEngine. Only the fields
// that belong to Method are set.
type TorrentCall struct {
	Method     string
	Hash       core.InfoHash
	Spec       core.TorrentSpec
	DeleteData bool
	Dir        string
	Limit      core.ShareLimit
	Down, Up   int64
}

type fakeTorrent struct {
	spec  core.TorrentSpec
	dir   string
	stats core.TorrentStats
	// stateBeforePause is restored by Resume.
	stateBeforePause core.TorrentState
}

// FakeTorrentEngine is an in-memory core.TorrentEngine. It records every call
// and lets a test drive stats and events. It is safe for concurrent use.
type FakeTorrentEngine struct {
	mu       sync.Mutex
	calls    []TorrentCall
	torrents map[core.InfoHash]*fakeTorrent
	failNext map[string]error
	events   chan core.TorrentEvent
}

var _ core.TorrentEngine = (*FakeTorrentEngine)(nil)

// NewFakeTorrentEngine returns an empty fake with a 1024-event buffer.
func NewFakeTorrentEngine() *FakeTorrentEngine {
	return &FakeTorrentEngine{
		torrents: make(map[core.InfoHash]*fakeTorrent),
		failNext: make(map[string]error),
		events:   make(chan core.TorrentEvent, 1024),
	}
}

// Calls returns a copy of the recorded calls, oldest first.
func (f *FakeTorrentEngine) Calls() []TorrentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TorrentCall(nil), f.calls...)
}

// FailNext makes the next call of method (for example "Add") return err.
func (f *FakeTorrentEngine) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[method] = err
}

// SetStats replaces the stats of a torrent the fake already knows.
func (f *FakeTorrentEngine) SetStats(h core.InfoHash, st core.TorrentStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.torrents[h]; ok {
		t.stats = st
	}
}

// StorageDir returns the directory the torrent currently lives in.
func (f *FakeTorrentEngine) StorageDir(h core.InfoHash) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.torrents[h]; ok {
		return t.dir
	}
	return ""
}

// Emit queues an event on Events. It drops the event when the buffer is full,
// as the real engine does.
func (f *FakeTorrentEngine) Emit(ev core.TorrentEvent) {
	select {
	case f.events <- ev:
	default:
	}
}

// Events returns the event channel.
func (f *FakeTorrentEngine) Events() <-chan core.TorrentEvent { return f.events }

// record notes the call and returns the injected failure for it, if any.
// The caller holds f.mu.
func (f *FakeTorrentEngine) record(c TorrentCall) error {
	f.calls = append(f.calls, c)
	if err := f.failNext[c.Method]; err != nil {
		delete(f.failNext, c.Method)
		return err
	}
	return nil
}

// fakeHash is the magnet's btih when it is 40 hex characters, and a SHA-1 of
// the metainfo or magnet otherwise.
func fakeHash(spec core.TorrentSpec) core.InfoHash {
	if spec.Magnet != "" {
		if u, err := url.Parse(spec.Magnet); err == nil {
			for _, xt := range u.Query()["xt"] {
				if h, err := core.ParseInfoHash(strings.TrimPrefix(xt, "urn:btih:")); err == nil {
					return h
				}
			}
		}
		return sha1.Sum([]byte(spec.Magnet))
	}
	return sha1.Sum(spec.Metainfo)
}

// Add registers the torrent. Magnets start in TorrentMetadataPending, other
// torrents in TorrentDownloading, and paused ones in TorrentPaused.
func (f *FakeTorrentEngine) Add(_ context.Context, spec core.TorrentSpec) (core.InfoHash, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := spec.Validate(); err != nil {
		return core.InfoHash{}, err
	}
	h := fakeHash(spec)
	if err := f.record(TorrentCall{Method: "Add", Hash: h, Spec: spec}); err != nil {
		return core.InfoHash{}, err
	}
	if _, dup := f.torrents[h]; dup {
		return h, fmt.Errorf("fake torrent engine: add %s: %w", h, core.ErrConflict)
	}
	state := core.TorrentDownloading
	if spec.Magnet != "" {
		state = core.TorrentMetadataPending
	}
	t := &fakeTorrent{spec: spec, dir: spec.StorageDir, stats: core.TorrentStats{State: state}, stateBeforePause: state}
	if spec.Paused {
		t.stats.State = core.TorrentPaused
	}
	f.torrents[h] = t
	return h, nil
}

// lookup records the call and finds the torrent. The caller holds f.mu.
func (f *FakeTorrentEngine) lookup(c TorrentCall) (*fakeTorrent, error) {
	if err := f.record(c); err != nil {
		return nil, err
	}
	t, ok := f.torrents[c.Hash]
	if !ok {
		return nil, fmt.Errorf("fake torrent engine: %s %s: %w", c.Method, c.Hash, core.ErrNotFound)
	}
	return t, nil
}

// Remove forgets the torrent.
func (f *FakeTorrentEngine) Remove(_ context.Context, h core.InfoHash, deleteData bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.lookup(TorrentCall{Method: "Remove", Hash: h, DeleteData: deleteData}); err != nil {
		return err
	}
	delete(f.torrents, h)
	return nil
}

// Pause sets the state to TorrentPaused.
func (f *FakeTorrentEngine) Pause(h core.InfoHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.lookup(TorrentCall{Method: "Pause", Hash: h})
	if err != nil {
		return err
	}
	if t.stats.State != core.TorrentPaused {
		t.stateBeforePause = t.stats.State
		t.stats.State = core.TorrentPaused
	}
	return nil
}

// Resume restores the state the torrent had before Pause.
func (f *FakeTorrentEngine) Resume(h core.InfoHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.lookup(TorrentCall{Method: "Resume", Hash: h})
	if err != nil {
		return err
	}
	if t.stats.State == core.TorrentPaused {
		t.stats.State = t.stateBeforePause
	}
	return nil
}

// Relocate records the new directory.
func (f *FakeTorrentEngine) Relocate(_ context.Context, h core.InfoHash, newDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.lookup(TorrentCall{Method: "Relocate", Hash: h, Dir: newDir})
	if err != nil {
		return err
	}
	t.dir = newDir
	return nil
}

// SetShareLimits records the limit.
func (f *FakeTorrentEngine) SetShareLimits(h core.InfoHash, l core.ShareLimit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.lookup(TorrentCall{Method: "SetShareLimits", Hash: h, Limit: l})
	return err
}

// SetRateLimits records the limits.
func (f *FakeTorrentEngine) SetRateLimits(down, up int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record(TorrentCall{Method: "SetRateLimits", Down: down, Up: up})
}

// Stats returns the stats a test set. It is not recorded as a call, because
// callers poll it.
func (f *FakeTorrentEngine) Stats(h core.InfoHash) (core.TorrentStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.torrents[h]
	if !ok {
		return core.TorrentStats{}, fmt.Errorf("fake torrent engine: stats %s: %w", h, core.ErrNotFound)
	}
	return t.stats, nil
}
