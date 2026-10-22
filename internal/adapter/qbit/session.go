package qbit

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// sessionIdle is how long a session lives without a request.
	sessionIdle = time.Hour
	// defaultMaxSessions caps the sessions kept in memory.
	defaultMaxSessions = 1000
	sidBytes           = 16 // 128 bits
)

// sessions is the table of SID sessions: random 128-bit tokens in memory,
// each bound to the generation of the app key that logged in, so a refresh
// ends them all. A session lives one
// hour after its last use and the table holds a bounded number of them.
// Sessions are valid on /api/v2 only, since nothing else reads them.
type sessions struct {
	now func() time.Time
	max int

	mu sync.Mutex
	m  map[string]*session
}

type session struct {
	generation uint64
	lastUsed   time.Time
}

func newSessions(now func() time.Time, limit int) *sessions {
	return &sessions{now: now, max: limit, m: make(map[string]*session)}
}

// create opens a session for the key generation and returns its token. When the
// table is full, expired sessions go first, then the least recently used one.
func (t *sessions) create(generation uint64) (string, error) {
	raw := make([]byte, sidBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.m) >= t.max {
		t.sweep(now)
	}
	for len(t.m) >= t.max {
		var oldest string
		var oldestAt time.Time
		for k, s := range t.m {
			if oldest == "" || s.lastUsed.Before(oldestAt) {
				oldest, oldestAt = k, s.lastUsed
			}
		}
		delete(t.m, oldest)
	}
	t.m[token] = &session{generation: generation, lastUsed: now}
	return token, nil
}

func (t *sessions) sweep(now time.Time) {
	for k, s := range t.m {
		if now.Sub(s.lastUsed) >= sessionIdle {
			delete(t.m, k)
		}
	}
}

// use returns the key generation of a live session and refreshes its idle
// timer.
func (t *sessions) use(token string) (uint64, bool) {
	if len(token) != 2*sidBytes {
		return 0, false
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[token]
	if !ok {
		return 0, false
	}
	if now.Sub(s.lastUsed) >= sessionIdle {
		delete(t.m, token)
		return 0, false
	}
	s.lastUsed = now
	return s.generation, true
}

func (t *sessions) drop(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, token)
}
