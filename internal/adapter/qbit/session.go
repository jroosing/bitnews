package qbit

import (
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
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
// ends them all. A session lives one hour after its last use and the table
// holds a bounded number of them; when it is full, the least recently used
// session goes, which is also the one idle longest, so expired sessions make
// room first. Sessions are valid on /api/v2 only, since nothing else reads
// them.
type sessions struct {
	now func() time.Time
	m   *lru.Cache[string, *session]
}

type session struct {
	generation uint64
	// lastUsed is in unix nanoseconds. use updates it in place rather than
	// adding the session again, so it cannot bring back a session that a
	// concurrent drop (a logout) just removed.
	lastUsed atomic.Int64
}

func newSessions(now func() time.Time, limit int) *sessions {
	m, err := lru.New[string, *session](limit)
	if err != nil {
		panic(err) // limit is defaultMaxSessions or a positive Deps.MaxSessions
	}
	return &sessions{now: now, m: m}
}

// create opens a session for the key generation and returns its token.
func (t *sessions) create(generation uint64) (string, error) {
	raw := make([]byte, sidBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	s := &session{generation: generation}
	s.lastUsed.Store(t.now().UnixNano())
	t.m.Add(token, s)
	return token, nil
}

// use returns the key generation of a live session and refreshes its idle
// timer.
func (t *sessions) use(token string) (uint64, bool) {
	if len(token) != 2*sidBytes {
		return 0, false
	}
	s, ok := t.m.Get(token)
	if !ok {
		return 0, false
	}
	now := t.now().UnixNano()
	if time.Duration(now-s.lastUsed.Load()) >= sessionIdle {
		t.m.Remove(token)
		return 0, false
	}
	s.lastUsed.Store(now)
	return s.generation, true
}

func (t *sessions) drop(token string) { t.m.Remove(token) }
