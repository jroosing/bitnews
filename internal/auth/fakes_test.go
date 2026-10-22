package auth_test

import (
	"context"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// UpdatePasswordHash completes fakeUsers (setup_test.go) as a core.UserRepo.
func (f *fakeUsers) UpdatePasswordHash(_ context.Context, id core.ID, hash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.users {
		if f.users[i].ID == id {
			f.users[i].PasswordHash = hash
			return nil
		}
	}
	return core.ErrNotFound
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeSessions is an in-memory core.SessionRepo that counts calls.
type fakeSessions struct {
	mu      sync.Mutex
	rows    map[core.TokenHash]core.Session
	touches int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{rows: map[core.TokenHash]core.Session{}}
}

func (f *fakeSessions) Create(_ context.Context, s core.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.rows[s.TokenHash]; dup {
		return core.ErrConflict
	}
	f.rows[s.TokenHash] = s
	return nil
}

func (f *fakeSessions) Get(_ context.Context, h core.TokenHash) (core.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[h]
	if !ok {
		return core.Session{}, core.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessions) Touch(_ context.Context, h core.TokenHash, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[h]
	if !ok {
		return core.ErrNotFound
	}
	s.LastSeenAt = at
	f.rows[h] = s
	f.touches++
	return nil
}

func (f *fakeSessions) Delete(_ context.Context, h core.TokenHash) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[h]; !ok {
		return core.ErrNotFound
	}
	delete(f.rows, h)
	return nil
}

func (f *fakeSessions) DeleteExpired(_ context.Context, createdBefore, seenBefore time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for h, s := range f.rows {
		if s.CreatedAt.Before(createdBefore) || s.LastSeenAt.Before(seenBefore) {
			delete(f.rows, h)
			n++
		}
	}
	return n, nil
}

func (f *fakeSessions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeSessions) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touches
}

// fakeAPIKeys is an in-memory core.APIKeyRepo that counts touches.
type fakeAPIKeys struct {
	mu      sync.Mutex
	keys    []core.APIKey
	touches int
	// wrongHash makes GetByHash return the first key whatever the hash,
	// like a buggy repository would.
	wrongHash bool
}

func (f *fakeAPIKeys) Create(_ context.Context, k core.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, k)
	return nil
}

func (f *fakeAPIKeys) List(context.Context) ([]core.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.APIKey(nil), f.keys...), nil
}

func (f *fakeAPIKeys) GetByHash(_ context.Context, h core.TokenHash) (core.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wrongHash && len(f.keys) > 0 {
		return f.keys[0], nil
	}
	for _, k := range f.keys {
		if k.Hash == h {
			return k, nil
		}
	}
	return core.APIKey{}, core.ErrNotFound
}

func (f *fakeAPIKeys) TouchLastUsed(_ context.Context, id core.ID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.keys {
		if f.keys[i].ID == id {
			f.keys[i].LastUsedAt = at
			f.touches++
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fakeAPIKeys) Delete(_ context.Context, id core.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.keys {
		if f.keys[i].ID == id {
			f.keys = append(f.keys[:i], f.keys[i+1:]...)
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fakeAPIKeys) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touches
}
