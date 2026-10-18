package nntp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Clock lets tests control time. The zero PoolConfig uses the system clock.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the part of *time.Timer the pool uses.
type Timer interface {
	// C delivers the time when the timer fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing and reports whether it was still
	// pending.
	Stop() bool
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (s systemTimer) C() <-chan time.Time { return s.t.C }

func (s systemTimer) Stop() bool { return s.t.Stop() }

// ErrPoolClosed is returned by Acquire once the Pool is closed.
var ErrPoolClosed = errors.New("nntp: pool closed")

// DefaultIdleTimeout is how long an unused connection stays open.
const DefaultIdleTimeout = 60 * time.Second

// ErrServerDisabled is returned by Acquire while the Pool is disabled
// because the Server rejected the credentials. It wraps the cause, so
// errors.Is also matches ErrAuth. Reconfigure with a new Factory re-enables
// the Pool.
var ErrServerDisabled = errors.New("nntp: server disabled")

// Default reconnect backoff bounds.
const (
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = 60 * time.Second
)

// ServerErrorKind classifies a ServerError.
type ServerErrorKind int

const (
	// ServerErrorConnect means connecting failed. It is reported once per
	// streak of consecutive failures, not for every retry.
	ServerErrorConnect ServerErrorKind = iota + 1
	// ServerErrorAuth means the Server rejected the credentials and the Pool
	// is now disabled.
	ServerErrorAuth
)

// ServerError is reported through PoolConfig.OnError.
type ServerError struct {
	Server string
	Kind   ServerErrorKind
	Err    error
}

// PoolConfig configures a Pool.
type PoolConfig struct {
	// Name identifies the Server in events and errors.
	Name string
	// MaxConnections is the most connections open at once, 1 to 100;
	// values outside are clamped.
	MaxConnections int
	// Factory opens connections.
	Factory ConnFactory
	// IdleTimeout closes connections unused for this long. It defaults to
	// DefaultIdleTimeout.
	IdleTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait between dial attempts after
	// failures: the wait starts at MinBackoff, doubles with every
	// consecutive failure and stops at MaxBackoff. They default to
	// DefaultMinBackoff and DefaultMaxBackoff.
	MinBackoff, MaxBackoff time.Duration
	// OnError is called when the Pool has something to report. It runs on
	// the goroutine that noticed, outside the Pool's lock, and must not
	// block.
	OnError func(ServerError)
	// Clock defaults to the system clock.
	Clock Clock
}

// PoolStats is a snapshot of a Pool.
type PoolStats struct {
	// InUse and Idle count open connections that are leased and parked.
	InUse, Idle int
	// Dials counts connection attempts and Failures the failed ones.
	Dials, Failures int64
	// Disabled reports that the Server rejected the credentials.
	Disabled bool
}

// maxPoolConnections is the largest MaxConnections of a Server.
const maxPoolConnections = 100

// idleConn is a parked connection.
type idleConn struct {
	conn  ArticleConn
	since time.Time
}

// Pool keeps up to MaxConnections connections to one Server.
type Pool struct {
	name  string
	clock Clock

	mu          sync.Mutex
	max         int
	factory     ConnFactory
	idleTimeout time.Duration
	idle        []idleConn // oldest first
	inUse       int
	dialing     int
	dials       int64
	closed      bool
	minBackoff  time.Duration
	maxBackoff  time.Duration
	onError     func(ServerError)
	failures    int64 // consecutive dial failures
	// proven is set by a successful dial and cleared by a failure or a new
	// factory. Until it is set, only one dial runs at a time, so wrong
	// credentials are tried once, not once per connection.
	proven     bool
	totalFails int64
	retryAt    time.Time
	lastErr    error
	disabled   error // the auth error, while disabled
	// gen counts factory swaps; connections of an older generation are
	// closed instead of being reused.
	gen int
	// changed is closed and replaced whenever something a waiting Acquire
	// cares about changes.
	changed chan struct{}

	done chan struct{} // closed by Close
	kick chan struct{} // tells the janitor the idle list changed
	wg   sync.WaitGroup
}

// NewPool returns a Pool that opens no connection until the first Acquire.
// Close it to stop its background goroutine.
func NewPool(cfg PoolConfig) *Pool {
	p := &Pool{
		name:        cfg.Name,
		clock:       cfg.Clock,
		max:         min(max(cfg.MaxConnections, 1), maxPoolConnections),
		factory:     cfg.Factory,
		idleTimeout: cfg.IdleTimeout,
		minBackoff:  cfg.MinBackoff,
		maxBackoff:  cfg.MaxBackoff,
		onError:     cfg.OnError,
		changed:     make(chan struct{}),
		done:        make(chan struct{}),
		kick:        make(chan struct{}, 1),
	}
	if p.clock == nil {
		p.clock = systemClock{}
	}
	if p.idleTimeout <= 0 {
		p.idleTimeout = DefaultIdleTimeout
	}
	if p.minBackoff <= 0 {
		p.minBackoff = DefaultMinBackoff
	}
	if p.maxBackoff <= 0 {
		p.maxBackoff = DefaultMaxBackoff
	}
	p.maxBackoff = max(p.maxBackoff, p.minBackoff)
	p.wg.Add(1)
	go p.janitor()
	return p
}

// Lease is one connection checked out of a Pool.
type Lease struct {
	p        *Pool
	conn     ArticleConn
	gen      int
	released atomic.Bool
}

// Conn returns the leased connection. It must not be used after Release.
func (l *Lease) Conn() ArticleConn { return l.conn }

// Release returns the connection to the Pool. It is safe to call twice.
func (l *Lease) Release() {
	if l.released.Swap(true) {
		return
	}
	l.p.release(l.conn, l.gen, false)
}

// Discard closes the connection instead of returning it, for a connection
// that is suspect even though it does not report Dead. It is a no-op after
// Release or Discard.
func (l *Lease) Discard() {
	if l.released.Swap(true) {
		return
	}
	l.p.release(l.conn, l.gen, true)
}

// broadcast wakes every waiting Acquire. The caller holds p.mu.
func (p *Pool) broadcast() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Acquire returns a connection, opening one when none is idle and the limit
// allows. At the limit it waits for a Release, until ctx is done.
//
// After a failed dial the Pool backs off: the caller whose dial failed gets
// that error, and later callers wait out the backoff before dialing again.
// If ctx ends during that wait, the error wraps both ctx.Err() and the last
// dial error. While the Pool is disabled every call fails with
// ErrServerDisabled.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
		if p.disabled != nil {
			err := p.disabledError()
			p.mu.Unlock()
			return nil, err
		}
		var stale []ArticleConn
		for n := len(p.idle); n > 0; n = len(p.idle) {
			c := p.idle[n-1].conn
			p.idle = p.idle[:n-1]
			if c.Dead() {
				stale = append(stale, c)
				continue
			}
			p.inUse++
			p.mu.Unlock()
			closeAll(stale)
			return &Lease{p: p, conn: c, gen: p.gen}, nil
		}

		changed := p.changed
		var backoff Timer
		var backoffC <-chan time.Time
		if p.inUse+p.dialing < p.max {
			switch wait := p.retryAt.Sub(p.clock.Now()); {
			case (p.failures > 0 || !p.proven) && p.dialing > 0:
				// One probe at a time until a dial succeeds.
			case p.failures > 0 && wait > 0:
				backoff = p.clock.NewTimer(wait)
				backoffC = backoff.C()
			default:
				p.dialing++
				p.dials++
				factory, gen := p.factory, p.gen
				p.mu.Unlock()
				closeAll(stale)
				return p.dial(ctx, factory, gen)
			}
		}
		lastErr := p.lastErr
		p.mu.Unlock()
		closeAll(stale)

		select {
		case <-changed:
		case <-backoffC:
		case <-ctx.Done():
		}
		if backoff != nil {
			backoff.Stop()
		}
		if err := ctx.Err(); err != nil {
			if lastErr != nil && backoff != nil {
				return nil, fmt.Errorf("%w: waiting to reconnect after: %w", err, lastErr)
			}
			return nil, err
		}
	}
}

// disabledError builds the error for a disabled Pool. The caller holds p.mu.
func (p *Pool) disabledError() error {
	return fmt.Errorf("%w (%s): %w", ErrServerDisabled, p.name, p.disabled)
}

// dial opens one connection in a slot that Acquire reserved.
func (p *Pool) dial(ctx context.Context, factory ConnFactory, gen int) (*Lease, error) {
	var c ArticleConn
	var err error
	if factory == nil {
		err = fmt.Errorf("%w: pool %q has no connection factory", ErrConfig, p.name)
	} else {
		c, err = factory(ctx)
	}

	var event *ServerError
	p.mu.Lock()
	p.dialing--
	switch {
	case err != nil && (ctx.Err() != nil || gen != p.gen):
		// The caller gave up, or the factory was replaced meanwhile; that
		// says nothing about the current Server config.
	case err != nil && errors.Is(err, ErrAuth):
		if p.disabled == nil {
			p.disabled = err
			event = &ServerError{Server: p.name, Kind: ServerErrorAuth, Err: err}
		}
		err = p.disabledError()
	case err != nil:
		p.failures++
		p.proven = false
		p.totalFails++
		p.lastErr = err
		p.retryAt = p.clock.Now().Add(p.backoffFor(p.failures))
		if p.failures == 1 {
			event = &ServerError{Server: p.name, Kind: ServerErrorConnect, Err: err}
		}
	case p.closed:
		err = ErrPoolClosed
	case p.disabled != nil:
		err = p.disabledError()
	default:
		p.failures, p.lastErr, p.retryAt = 0, nil, time.Time{}
		p.proven = true
		p.inUse++
	}
	p.broadcast()
	p.mu.Unlock()

	if event != nil && p.onError != nil {
		p.onError(*event)
	}
	if err != nil {
		if c != nil {
			_ = c.Close()
		}
		return nil, err
	}
	return &Lease{p: p, conn: c, gen: gen}, nil
}

// backoffFor is the wait after n consecutive failures.
func (p *Pool) backoffFor(n int64) time.Duration {
	d := p.minBackoff
	for ; n > 1 && d < p.maxBackoff; n-- {
		d *= 2
	}
	return min(d, p.maxBackoff)
}

func (p *Pool) release(c ArticleConn, gen int, discard bool) {
	p.mu.Lock()
	p.inUse--
	// Keep the connection only if it is healthy, current and within the
	// (possibly reduced) limit.
	drop := discard || c.Dead() || p.closed || gen != p.gen ||
		p.inUse+len(p.idle)+p.dialing >= p.max
	if !drop {
		p.idle = append(p.idle, idleConn{conn: c, since: p.clock.Now()})
		p.wakeJanitor()
	}
	p.broadcast()
	p.mu.Unlock()
	if drop {
		_ = c.Close()
	}
}

// wakeJanitor tells the janitor the idle list changed.
func (p *Pool) wakeJanitor() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Reconfigure applies new settings without a restart. Zero fields keep their
// current value; Name, Clock and OnError cannot change.
//
//   - MaxConnections: a lower limit closes excess idle connections at once
//     and excess leased ones when they are released; a higher one lets
//     waiting Acquire calls dial more.
//   - IdleTimeout, MinBackoff, MaxBackoff take effect immediately.
//   - Factory (new credentials or endpoint): applies to every later dial.
//     Idle connections of the old factory are closed, leased ones on release,
//     and a Pool disabled by an auth failure is enabled again with a clean
//     backoff. Functions cannot be compared, so passing any Factory counts
//     as a change.
//
// It does nothing on a closed Pool.
func (p *Pool) Reconfigure(cfg PoolConfig) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if cfg.MaxConnections != 0 {
		p.max = min(max(cfg.MaxConnections, 1), maxPoolConnections)
	}
	if cfg.IdleTimeout > 0 {
		p.idleTimeout = cfg.IdleTimeout
	}
	if cfg.MinBackoff > 0 {
		p.minBackoff = cfg.MinBackoff
	}
	if cfg.MaxBackoff > 0 {
		p.maxBackoff = cfg.MaxBackoff
	}
	p.maxBackoff = max(p.maxBackoff, p.minBackoff)

	var closing []ArticleConn
	if cfg.Factory != nil {
		p.factory = cfg.Factory
		p.gen++
		p.disabled, p.failures, p.lastErr, p.retryAt = nil, 0, nil, time.Time{}
		p.proven = false
		for _, ic := range p.idle {
			closing = append(closing, ic.conn)
		}
		p.idle = nil
	}
	// Close the oldest idle connections beyond the limit.
	excess := p.inUse + len(p.idle) + p.dialing - p.max
	for excess > 0 && len(p.idle) > 0 {
		closing = append(closing, p.idle[0].conn)
		p.idle = p.idle[1:]
		excess--
	}
	p.wakeJanitor()
	p.broadcast()
	p.mu.Unlock()
	closeAll(closing)
}

// janitor closes connections that were idle for IdleTimeout. It sleeps until
// the oldest idle connection expires, or until the idle list changes.
func (p *Pool) janitor() {
	defer p.wg.Done()
	for {
		var expiry <-chan time.Time
		var timer Timer
		p.mu.Lock()
		if len(p.idle) > 0 {
			wait := p.idle[0].since.Add(p.idleTimeout).Sub(p.clock.Now())
			timer = p.clock.NewTimer(max(wait, 0))
			expiry = timer.C()
		}
		p.mu.Unlock()

		select {
		case <-p.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-p.kick:
			if timer != nil {
				timer.Stop()
			}
		case <-expiry:
			p.sweep()
		}
	}
}

// sweep closes the connections idle for at least IdleTimeout.
func (p *Pool) sweep() {
	p.mu.Lock()
	now := p.clock.Now()
	n := 0
	for n < len(p.idle) && now.Sub(p.idle[n].since) >= p.idleTimeout {
		n++
	}
	expired := make([]ArticleConn, n)
	for i := range n {
		expired[i] = p.idle[i].conn
	}
	p.idle = p.idle[n:]
	p.mu.Unlock()
	closeAll(expired)
}

// closeAll closes connections outside any lock, because Close may block on
// the network.
func closeAll(conns []ArticleConn) {
	for _, c := range conns {
		_ = c.Close()
	}
}

// Body leases a connection, fetches the body of messageID into sink and
// releases the connection. A connection that failed (ErrConnection,
// ErrProtocol, a failing sink) or died is closed instead of being reused.
// It does not retry; callers decide that.
func (p *Pool) Body(ctx context.Context, messageID string, sink func(line []byte) error) error {
	l, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	err = l.Conn().Body(ctx, messageID, sink)
	if err != nil && (errors.Is(err, ErrConnection) || errors.Is(err, ErrProtocol)) {
		l.Discard()
	} else {
		l.Release()
	}
	return err
}

// Stats returns a snapshot of the Pool.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{InUse: p.inUse, Idle: len(p.idle), Dials: p.dials, Failures: p.totalFails, Disabled: p.disabled != nil}
}

// Close closes the idle connections, wakes every waiting Acquire with
// ErrPoolClosed and stops the background goroutine. Leased connections are
// closed when released. It is safe to call more than once.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	idle := p.idle
	p.idle = nil
	p.broadcast()
	p.mu.Unlock()

	for _, ic := range idle {
		_ = ic.conn.Close()
	}
	p.wg.Wait()
	return nil
}
