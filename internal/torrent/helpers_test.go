package torrent_test

import (
	"context"
	"sync"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// memStore is an in-memory torrent.PieceStore.
type memStore struct {
	mu   sync.Mutex
	rows map[core.InfoHash]store.TorrentPieces
}

func newMemStore() *memStore { return &memStore{rows: map[core.InfoHash]store.TorrentPieces{}} }

func (m *memStore) Load(_ context.Context, h core.InfoHash) (store.TorrentPieces, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[h]
	if !ok {
		return store.TorrentPieces{}, core.ErrNotFound
	}
	return p, nil
}

func (m *memStore) Save(_ context.Context, h core.InfoHash, p store.TorrentPieces) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[h] = p
	return nil
}

func (m *memStore) Delete(_ context.Context, h core.InfoHash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, h)
	return nil
}
