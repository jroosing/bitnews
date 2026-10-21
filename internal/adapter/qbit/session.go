package qbit

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

const (
	// sessionIdle is how long a session lives without a request.
	sessionIdle = time.Hour
	// defaultMaxSessions caps the sessions kept in memory.
	defaultMaxSessions = 1000
	sidBytes           = 16 // 128 bits
)

// sessions is the table of SID sessions: random 128-bit tokens in memory,
// each bound to the adapter credential that logged in. A session lives one
// hour after its last use and the table holds a bounded number of them.
// Sessions are valid on /api/v2 only, since nothing else reads them.
type sessions struct {
	now func() time.Time
	max int

	mu sync.Mutex
	m  map[string]*session
}

type session struct {
	credential core.ID
	lastUsed   time.Time
}

func newSessions(now func() time.Time, limit int) *sessions {
	return &sessions{now: now, max: limit, m: make(map[string]*session)}
}

// create opens a session for the credential and returns its token. When the
// table is full, expired sessions go first, then the least recently used one.
func (t *sessions) create(credential core.ID) (string, error) {
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
	t.m[token] = &session{credential: credential, lastUsed: now}
	return token, nil
}

func (t *sessions) sweep(now time.Time) {
	for k, s := range t.m {
		if now.Sub(s.lastUsed) >= sessionIdle {
			delete(t.m, k)
		}
	}
}

// use returns the credential of a live session and refreshes its idle timer.
func (t *sessions) use(token string) (core.ID, bool) {
	if len(token) != 2*sidBytes {
		return "", false
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[token]
	if !ok {
		return "", false
	}
	if now.Sub(s.lastUsed) >= sessionIdle {
		delete(t.m, token)
		return "", false
	}
	s.lastUsed = now
	return s.credential, true
}

func (t *sessions) drop(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, token)
}

// dropCredential ends every session of a credential.
func (t *sessions) dropCredential(id core.ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.m {
		if s.credential == id {
			delete(t.m, k)
		}
	}
}
