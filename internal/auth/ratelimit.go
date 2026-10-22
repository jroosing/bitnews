package auth

import (
	"container/list"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Login rate limiting parameters from the design.
const (
	// limiterFreeFailures is how many failures a key may collect before the
	// backoff starts: the 5th failure is the first one that blocks.
	limiterFreeFailures = 5
	limiterBaseDelay    = time.Second
	limiterMaxDelay     = 15 * time.Minute
	// limiterForgetAfter is how long after its block ended a key keeps its
	// failure count. It equals the longest block, so waiting out one full
	// backoff does not reset the next one.
	limiterForgetAfter = limiterMaxDelay
	// limiterDefaultMaxTracked caps the keys remembered per table.
	limiterDefaultMaxTracked = 10_000
	limiterSweepInterval     = time.Minute
	// maxUsernameKeyLen truncates hostile usernames before they become keys.
	maxUsernameKeyLen = 128
)

// LoginLimiter throttles password guessing. It counts failures per client IP
// and per username, in memory, and a login attempt must pass both. After
// five failures a key is blocked for 1 s, and every further failure doubles
// the block up to 15 min. A successful login clears both keys.
//
// Memory is bounded: each table (IPs, usernames) tracks at most a fixed
// number of keys, forgets quiet keys, and evicts the least recently failed
// one when full. A flood of distinct keys can therefore push out a victim's
// history. The cost is only that an attacker's own free attempts restart.
//
// Trade-off: because the username key is shared by every IP, anyone can
// block the admin's login for up to 15 min by failing it repeatedly. That is
// the price of stopping distributed guessing; a session that already exists
// is not affected.
//
// IPv6 clients are keyed by their /64, since a host usually owns the whole
// prefix. A LoginLimiter is safe for concurrent use.
type LoginLimiter struct {
	now func() time.Time

	mu    sync.Mutex
	ips   *keyTable
	users *keyTable
}

// LimiterOption configures NewLoginLimiter.
type LimiterOption func(*limiterConfig)

type limiterConfig struct {
	now        func() time.Time
	maxTracked int
}

// WithLimiterClock sets the time source, for tests.
func WithLimiterClock(now func() time.Time) LimiterOption {
	return func(c *limiterConfig) { c.now = now }
}

// WithMaxTracked sets how many IPs and how many usernames are remembered.
func WithMaxTracked(n int) LimiterOption {
	return func(c *limiterConfig) { c.maxTracked = max(n, 1) }
}

// NewLoginLimiter returns a limiter that remembers up to 10,000 IPs and
// 10,000 usernames unless WithMaxTracked says otherwise.
func NewLoginLimiter(opts ...LimiterOption) *LoginLimiter {
	cfg := limiterConfig{now: time.Now, maxTracked: limiterDefaultMaxTracked}
	for _, o := range opts {
		o(&cfg)
	}
	return &LoginLimiter{
		now:   cfg.now,
		ips:   newKeyTable(cfg.maxTracked),
		users: newKeyTable(cfg.maxTracked),
	}
}

// Check reports how long the caller must wait before this IP may try this
// username. Zero means go ahead. It is the longer of the IP's and the
// username's remaining blocks.
func (l *LoginLimiter) Check(ip netip.Addr, username string) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.ips.wait(ipKey(ip), now), l.users.wait(userKey(username), now))
}

// Fail records a failed attempt against both keys.
func (l *LoginLimiter) Fail(ip netip.Addr, username string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ips.fail(ipKey(ip), now)
	l.users.fail(userKey(username), now)
}

// Success clears both keys after a good login.
func (l *LoginLimiter) Success(ip netip.Addr, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ips.forget(ipKey(ip))
	l.users.forget(userKey(username))
}

// Tracked reports how many IP and username keys are remembered.
func (l *LoginLimiter) Tracked() (ips, users int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ips.len(), l.users.len()
}

func ipKey(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String() // the zero Addr gives "invalid IP": one shared bucket
}

func userKey(username string) string {
	if len(username) > maxUsernameKeyLen {
		username = username[:maxUsernameKeyLen]
	}
	return strings.ToLower(username)
}

type keyEntry struct {
	key          string
	failures     int
	blockedUntil time.Time
	expiresAt    time.Time
}

// keyTable is an LRU map of failure counters. Callers hold LoginLimiter.mu.
type keyTable struct {
	max       int
	byKey     map[string]*list.Element // value: *keyEntry
	lru       *list.List               // front = most recently failed
	lastSweep time.Time
}

func newKeyTable(max int) *keyTable {
	return &keyTable{max: max, byKey: make(map[string]*list.Element), lru: list.New()}
}

func (t *keyTable) len() int { return len(t.byKey) }

func (t *keyTable) wait(key string, now time.Time) time.Duration {
	el, ok := t.byKey[key]
	if !ok {
		return 0
	}
	e := el.Value.(*keyEntry)
	if !now.Before(e.expiresAt) {
		t.remove(el)
		return 0
	}
	return max(e.blockedUntil.Sub(now), 0)
}

func (t *keyTable) fail(key string, now time.Time) {
	el, ok := t.byKey[key]
	var e *keyEntry
	if ok {
		e = el.Value.(*keyEntry)
		if !now.Before(e.expiresAt) {
			*e = keyEntry{key: key} // went quiet: start over
		}
		t.lru.MoveToFront(el)
	} else {
		t.makeRoom(now)
		e = &keyEntry{key: key}
		t.byKey[key] = t.lru.PushFront(e)
	}
	e.failures++
	if e.failures >= limiterFreeFailures {
		e.blockedUntil = now.Add(backoff(e.failures))
	}
	last := now
	if e.blockedUntil.After(last) {
		last = e.blockedUntil
	}
	e.expiresAt = last.Add(limiterForgetAfter)
}

// backoff is the block after the n-th failure (n >= limiterFreeFailures).
func backoff(n int) time.Duration {
	shift := n - limiterFreeFailures
	if shift >= 20 { // 1 s << 20 is already past the cap; avoid overflow
		return limiterMaxDelay
	}
	return min(limiterBaseDelay<<shift, limiterMaxDelay)
}

func (t *keyTable) forget(key string) {
	if el, ok := t.byKey[key]; ok {
		t.remove(el)
	}
}

func (t *keyTable) remove(el *list.Element) {
	delete(t.byKey, el.Value.(*keyEntry).key)
	t.lru.Remove(el)
}

// makeRoom frees one slot when the table is full: first by dropping keys that
// went quiet (at most one sweep per minute), then by evicting the least
// recently failed key.
func (t *keyTable) makeRoom(now time.Time) {
	if len(t.byKey) < t.max {
		return
	}
	if now.Sub(t.lastSweep) >= limiterSweepInterval {
		t.lastSweep = now
		for el := t.lru.Back(); el != nil; {
			prev := el.Prev()
			if !now.Before(el.Value.(*keyEntry).expiresAt) {
				t.remove(el)
			}
			el = prev
		}
	}
	for len(t.byKey) >= t.max {
		t.remove(t.lru.Back())
	}
}
