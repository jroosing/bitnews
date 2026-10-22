package auth_test

import (
	"context"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// fakeAdapterCreds is an in-memory core.AdapterCredentialRepo that counts
// touches.
type fakeAdapterCreds struct {
	mu      sync.Mutex
	rows    []core.AdapterCredential
	touches int
}

func (f *fakeAdapterCreds) Create(_ context.Context, c core.AdapterCredential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.SecretHash == c.SecretHash || (c.Username != "" && r.Kind == c.Kind && r.Username == c.Username) {
			return core.ErrConflict
		}
	}
	f.rows = append(f.rows, c)
	return nil
}

func (f *fakeAdapterCreds) List(context.Context) ([]core.AdapterCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.AdapterCredential(nil), f.rows...), nil
}

func (f *fakeAdapterCreds) GetByHash(_ context.Context, h core.TokenHash) (core.AdapterCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.SecretHash == h {
			return r, nil
		}
	}
	return core.AdapterCredential{}, core.ErrNotFound
}

func (f *fakeAdapterCreds) GetByUsername(_ context.Context, kind core.AdapterKind, username string) (core.AdapterCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Kind == kind && r.Username == username {
			return r, nil
		}
	}
	return core.AdapterCredential{}, core.ErrNotFound
}

func (f *fakeAdapterCreds) TouchLastUsed(_ context.Context, id core.ID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].LastUsedAt = at
			f.touches++
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fakeAdapterCreds) Delete(_ context.Context, id core.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fakeAdapterCreds) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touches
}
