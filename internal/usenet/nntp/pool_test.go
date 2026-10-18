package nntp_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

// fakeClock is a manually advanced nntp.Clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
	// created counts every timer ever made, so tests can tell that the pool
	// re-armed one.
	created int
}

type fakeTimer struct {
	clock *fakeClock
	when  time.Time
	ch    chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), timers: map[*fakeTimer]struct{}{}}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) nntp.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	t := &fakeTimer{clock: c, when: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		return t
	}
	c.timers[t] = struct{}{}
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	_, active := t.clock.timers[t]
	delete(t.clock.timers, t)
	return active
}

// Advance moves time forward and fires the timers that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for t := range c.timers {
		if !t.when.After(c.now) {
			t.ch <- c.now
			delete(c.timers, t)
		}
	}
}

// pending is the number of timers waiting to fire.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// waitCreated blocks until n timers have been created in total.
func (c *fakeClock) waitCreated(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.created >= n
	}, 5*time.Second, time.Millisecond, "want %d timers created", n)
}

// waitTimers blocks until exactly n timers are pending.
func (c *fakeClock) waitTimers(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return c.pending() == n }, 5*time.Second, time.Millisecond,
		"want %d pending timers, have %d", n, c.pending())
}

func newPool(t *testing.T, cfg nntp.PoolConfig) *nntp.Pool {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "test"
	}
	p := nntp.NewPool(cfg)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func acquire(t *testing.T, p *nntp.Pool) *nntp.Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx)
	require.NoError(t, err)
	return l
}

func TestAcquireConnectsLazilyAndReusesReleasedConnections(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 3, Factory: srv.Factory()})
	assert.Equal(t, 0, srv.Dials(), "nothing dials before the first Acquire")

	l1 := acquire(t, p)
	c1 := l1.Conn()
	require.NotNil(t, c1)
	l1.Release()
	l2 := acquire(t, p)

	assert.Same(t, c1, l2.Conn())
	assert.Equal(t, 1, srv.Dials())
	assert.Equal(t, nntp.PoolStats{InUse: 1, Dials: 1}, p.Stats())
	l2.Release()
	assert.Equal(t, nntp.PoolStats{Idle: 1, Dials: 1}, p.Stats())
}

func TestReleaseTwiceIsHarmless(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	l := acquire(t, p)

	l.Release()
	l.Release()

	assert.Equal(t, 1, p.Stats().Idle)
	assert.Equal(t, 0, p.Stats().InUse)
}

func TestConnectionLimitIsNeverExceededUnderConcurrentLoad(t *testing.T) {
	const limit = 5
	srv := nntptest.NewServer("main")
	srv.SetMaxConnections(limit) // a pool that overshoots gets 502s
	srv.SetLatency(time.Millisecond)
	srv.AddArticle("a@x", []string{"line"})
	p := newPool(t, nntp.PoolConfig{MaxConnections: limit, Factory: srv.Factory()})

	var wg sync.WaitGroup
	errs := make(chan error, 50*20)
	for range 50 {
		wg.Go(func() {
			for range 20 {
				l, err := p.Acquire(context.Background())
				if err != nil {
					errs <- err
					return
				}
				err = l.Conn().Body(context.Background(), "a@x", func([]byte) error { return nil })
				l.Release()
				if err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	assert.LessOrEqual(t, srv.PeakConns(), limit)
	assert.Equal(t, 1000, srv.Requests("a@x"))
	assert.LessOrEqual(t, srv.Dials(), limit, "idle connections are reused")
}

func TestAcquireBlocksAtTheLimitUntilALeaseIsReleased(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	first := acquire(t, p)
	got := make(chan *nntp.Lease, 1)
	go func() {
		l, err := p.Acquire(context.Background())
		assert.NoError(t, err)
		got <- l
	}()

	select {
	case <-got:
		t.Fatal("Acquire returned while the only connection was leased")
	case <-time.After(50 * time.Millisecond):
	}
	first.Release()

	select {
	case l := <-got:
		l.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not wake up after Release")
	}
	assert.Equal(t, 1, srv.Dials())
}

func TestCancellingTheContextEndsAWaitingAcquire(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	_ = acquire(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.Acquire(ctx)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire ignored the cancelled context")
	}
	assert.Equal(t, 1, p.Stats().InUse, "a cancelled waiter takes no slot")
}

func TestAcquireWithADoneContextFailsWithoutDialing(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Acquire(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, srv.Dials())
}

func TestDeadConnectionIsDroppedOnReleaseAndReplacedOnNextAcquire(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.AddArticle("a@x", []string{"1", "2"})
	srv.Disconnect("a@x", 1, 1)
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	l := acquire(t, p)
	dead := l.Conn()
	err := dead.Body(context.Background(), "a@x", func([]byte) error { return nil })
	require.ErrorIs(t, err, nntp.ErrConnection)
	require.True(t, dead.Dead())

	l.Release()
	assert.Equal(t, 0, p.Stats().Idle)
	l2 := acquire(t, p)

	assert.NotSame(t, dead, l2.Conn())
	assert.False(t, l2.Conn().Dead())
	assert.Equal(t, 2, srv.Dials())
	assert.Equal(t, 1, srv.OpenConns(), "the dead connection's slot was freed")
}

func TestConnectionThatDiesWhileIdleIsNotHandedOut(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	l := acquire(t, p)
	idleConn := l.Conn()
	l.Release()
	require.NoError(t, idleConn.Close()) // the server hung up while idle

	l2 := acquire(t, p)

	assert.NotSame(t, idleConn, l2.Conn())
	assert.Equal(t, 2, srv.Dials())
}

func TestCloseClosesIdleConnectionsAndRejectsLaterAcquires(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := nntp.NewPool(nntp.PoolConfig{MaxConnections: 2, Factory: srv.Factory()})
	a, b := acquire(t, p), acquire(t, p)
	a.Release()
	b.Release()
	require.Equal(t, 2, srv.OpenConns())

	require.NoError(t, p.Close())

	assert.Equal(t, 0, srv.OpenConns())
	_, err := p.Acquire(context.Background())
	assert.ErrorIs(t, err, nntp.ErrPoolClosed)
}

func TestCloseIsIdempotent(t *testing.T) {
	p := nntp.NewPool(nntp.PoolConfig{MaxConnections: 1, Factory: nntptest.NewServer("main").Factory()})

	assert.NoError(t, p.Close())
	assert.NoError(t, p.Close())
}

func TestCloseWakesWaitingAcquires(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := nntp.NewPool(nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	held := acquire(t, p)
	errc := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := p.Acquire(context.Background())
			errc <- err
		}()
	}
	time.Sleep(20 * time.Millisecond)

	require.NoError(t, p.Close())

	for range 3 {
		select {
		case err := <-errc:
			assert.ErrorIs(t, err, nntp.ErrPoolClosed)
		case <-time.After(5 * time.Second):
			t.Fatal("waiter was not woken by Close")
		}
	}
	held.Release()
	assert.Equal(t, 0, srv.OpenConns(), "a lease released after Close closes its connection")
}

func TestIdleConnectionsAreClosedAfterTheIdleTimeout(t *testing.T) {
	clock := newFakeClock()
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 2, Factory: srv.Factory(), Clock: clock})
	l := acquire(t, p)
	l.Release()
	clock.waitTimers(t, 1)

	clock.Advance(59 * time.Second)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 1, srv.OpenConns(), "still within the default 60 s")

	clock.Advance(time.Second)
	require.Eventually(t, func() bool { return srv.OpenConns() == 0 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, 0, p.Stats().Idle)

	l2 := acquire(t, p)
	assert.Equal(t, 2, srv.Dials(), "the next Acquire connects again")
	l2.Release()
}

func TestOnlyTheConnectionsIdleLongEnoughAreClosed(t *testing.T) {
	clock := newFakeClock()
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 2, Factory: srv.Factory(), Clock: clock, IdleTimeout: 10 * time.Second})
	a, b := acquire(t, p), acquire(t, p)
	a.Release()
	clock.waitTimers(t, 1)
	clock.Advance(6 * time.Second)
	b.Release()
	clock.waitCreated(t, 2) // the janitor re-armed for the oldest connection
	clock.waitTimers(t, 1)

	clock.Advance(4 * time.Second) // a has idled 10 s, b only 4 s

	require.Eventually(t, func() bool { return srv.OpenConns() == 1 }, 5*time.Second, time.Millisecond)
	clock.waitTimers(t, 1)
	clock.Advance(6 * time.Second)
	require.Eventually(t, func() bool { return srv.OpenConns() == 0 }, 5*time.Second, time.Millisecond)
}

func TestClosedPoolLeavesNoGoroutinesBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	srv := nntptest.NewServer("main")
	srv.AddArticle("a@x", []string{"x"})
	p := nntp.NewPool(nntp.PoolConfig{MaxConnections: 4, Factory: srv.Factory()})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_ = p.Body(ctx, "a@x", func([]byte) error { return nil })
		})
	}
	wg.Wait()
	require.NoError(t, p.Close())

	// testify's Eventually runs goroutines of its own, so settle by hand.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), before, "goroutines leaked by the Pool")
}

// flakyFactory fails with err while failing is set, otherwise delegates.
type flakyFactory struct {
	inner   nntp.ConnFactory
	failing atomic.Bool
	err     error
	calls   atomic.Int64
}

func (f *flakyFactory) factory(ctx context.Context) (nntp.ArticleConn, error) {
	f.calls.Add(1)
	if f.failing.Load() {
		return nil, f.err
	}
	return f.inner(ctx)
}

func newFlaky(srv *nntptest.Server) *flakyFactory {
	f := &flakyFactory{inner: srv.Factory(), err: &nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: errors.New("refused")}}
	f.failing.Store(true)
	return f
}

type acquireResult struct {
	lease *nntp.Lease
	err   error
}

func acquireAsync(p *nntp.Pool, ctx context.Context) <-chan acquireResult {
	ch := make(chan acquireResult, 1)
	go func() {
		l, err := p.Acquire(ctx)
		ch <- acquireResult{l, err}
	}()
	return ch
}

// expectBlocked asserts that the Acquire has not returned.
func expectBlocked(t *testing.T, ch <-chan acquireResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("Acquire returned early: lease=%v err=%v", r.lease, r.err)
	case <-time.After(15 * time.Millisecond):
	}
}

func expectResult(t *testing.T, ch <-chan acquireResult) acquireResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return")
		return acquireResult{}
	}
}

func TestDialFailuresBackOffExponentiallyFromOneToSixtySeconds(t *testing.T) {
	clock := newFakeClock()
	flaky := newFlaky(nntptest.NewServer("main"))
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: flaky.factory, Clock: clock})

	_, err := p.Acquire(context.Background())
	require.ErrorIs(t, err, nntp.ErrConnection, "the caller whose dial failed gets the error")
	require.EqualValues(t, 1, flaky.calls.Load())

	for i, wait := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
		wait *= time.Second
		res := acquireAsync(p, context.Background())
		clock.waitTimers(t, 1)
		clock.Advance(wait - time.Millisecond)
		expectBlocked(t, res)
		require.EqualValues(t, i+1, flaky.calls.Load(), "no dial before the backoff of %v is over", wait)

		clock.Advance(time.Millisecond)
		r := expectResult(t, res)
		assert.ErrorIs(t, r.err, nntp.ErrConnection)
		require.EqualValues(t, i+2, flaky.calls.Load())
	}
	assert.EqualValues(t, 9, p.Stats().Failures)
}

func TestSuccessfulDialResetsTheBackoff(t *testing.T) {
	clock := newFakeClock()
	flaky := newFlaky(nntptest.NewServer("main"))
	p := newPool(t, nntp.PoolConfig{MaxConnections: 2, Factory: flaky.factory, Clock: clock})
	for _, wait := range []time.Duration{0, time.Second, 2 * time.Second} {
		res := acquireAsync(p, context.Background())
		if wait > 0 {
			clock.waitTimers(t, 1)
			clock.Advance(wait)
		}
		require.Error(t, expectResult(t, res).err)
	}
	flaky.failing.Store(false)
	res := acquireAsync(p, context.Background())
	clock.waitTimers(t, 1)
	clock.Advance(4 * time.Second)
	good := expectResult(t, res)
	require.NoError(t, good.err)

	flaky.failing.Store(true)
	require.Error(t, expectResult(t, acquireAsync(p, context.Background())).err)
	res = acquireAsync(p, context.Background())
	clock.waitTimers(t, 1)
	clock.Advance(time.Second) // 1 s again, not 8 s

	assert.Error(t, expectResult(t, res).err)
	good.lease.Release()
}

func TestBackoffBoundsAreConfigurable(t *testing.T) {
	clock := newFakeClock()
	flaky := newFlaky(nntptest.NewServer("main"))
	p := newPool(t, nntp.PoolConfig{
		MaxConnections: 1, Factory: flaky.factory, Clock: clock,
		MinBackoff: 100 * time.Millisecond, MaxBackoff: 300 * time.Millisecond,
	})
	_, _ = p.Acquire(context.Background())

	for _, wait := range []time.Duration{100, 200, 300, 300} {
		res := acquireAsync(p, context.Background())
		clock.waitTimers(t, 1)
		clock.Advance(wait * time.Millisecond)
		assert.Error(t, expectResult(t, res).err)
	}
}

func TestAcquireWaitingInBackoffEndsWithTheContextAndReportsTheDialError(t *testing.T) {
	clock := newFakeClock()
	flaky := newFlaky(nntptest.NewServer("main"))
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: flaky.factory, Clock: clock})
	_, _ = p.Acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	res := acquireAsync(p, ctx)
	clock.waitTimers(t, 1)

	cancel()

	r := expectResult(t, res)
	assert.ErrorIs(t, r.err, context.Canceled)
	assert.ErrorIs(t, r.err, nntp.ErrConnection, "the last dial error is kept")
	assert.EqualValues(t, 1, flaky.calls.Load())
	clock.waitTimers(t, 0)
}

func TestDialAbortedByTheCallersContextIsNotAServerFailure(t *testing.T) {
	clock := newFakeClock()
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Clock: clock, Factory: func(ctx context.Context) (nntp.ArticleConn, error) {
		<-ctx.Done()
		return nil, &nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: ctx.Err()}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	res := acquireAsync(p, ctx)
	time.Sleep(10 * time.Millisecond)
	cancel()
	require.Error(t, expectResult(t, res).err)

	assert.Zero(t, p.Stats().Failures)
	clock.waitTimers(t, 0)
}

func TestAuthFailureDisablesThePoolAndStopsAllDials(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.FailAuth(true)
	var events []nntp.ServerError
	var mu sync.Mutex
	p := newPool(t, nntp.PoolConfig{Name: "news-1", MaxConnections: 4, Factory: srv.Factory(),
		OnError: func(e nntp.ServerError) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
		}})

	for range 5 {
		_, err := p.Acquire(context.Background())
		assert.ErrorIs(t, err, nntp.ErrServerDisabled)
		assert.ErrorIs(t, err, nntp.ErrAuth, "the cause stays reachable")
	}

	assert.Equal(t, 1, srv.AuthAttempts(), "the server saw exactly one auth attempt")
	assert.Equal(t, 1, srv.Dials())
	assert.True(t, p.Stats().Disabled)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 1)
	assert.Equal(t, "news-1", events[0].Server)
	assert.Equal(t, nntp.ServerErrorAuth, events[0].Kind)
	assert.ErrorIs(t, events[0].Err, nntp.ErrAuth)
}

func TestAuthFailureWakesWaitersWithTheDisabledError(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.FailAuth(true)
	srv.SetLatency(0)
	gate := make(chan struct{})
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: func(ctx context.Context) (nntp.ArticleConn, error) {
		<-gate
		return srv.Factory()(ctx)
	}})
	first := acquireAsync(p, context.Background())
	time.Sleep(10 * time.Millisecond)
	second := acquireAsync(p, context.Background())
	expectBlocked(t, second)

	close(gate)

	assert.ErrorIs(t, expectResult(t, first).err, nntp.ErrServerDisabled)
	assert.ErrorIs(t, expectResult(t, second).err, nntp.ErrServerDisabled)
	assert.Equal(t, 1, srv.AuthAttempts())
}

func TestConnectErrorEventFiresOncePerFailureStreak(t *testing.T) {
	clock := newFakeClock()
	flaky := newFlaky(nntptest.NewServer("main"))
	var kinds []nntp.ServerErrorKind
	var mu sync.Mutex
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: flaky.factory, Clock: clock,
		OnError: func(e nntp.ServerError) {
			mu.Lock()
			defer mu.Unlock()
			kinds = append(kinds, e.Kind)
		}})
	for _, wait := range []time.Duration{0, time.Second, 2 * time.Second} {
		res := acquireAsync(p, context.Background())
		if wait > 0 {
			clock.waitTimers(t, 1)
			clock.Advance(wait)
		}
		require.Error(t, expectResult(t, res).err)
	}
	mu.Lock()
	assert.Equal(t, []nntp.ServerErrorKind{nntp.ServerErrorConnect}, kinds)
	mu.Unlock()

	flaky.failing.Store(false)
	res := acquireAsync(p, context.Background())
	clock.waitTimers(t, 1)
	clock.Advance(4 * time.Second)
	r := expectResult(t, res)
	require.NoError(t, r.err)
	r.lease.Release()
	flaky.failing.Store(true)
	// release the idle connection's slot by closing it, so a dial is needed
	require.NoError(t, r.lease.Conn().Close())
	require.Error(t, expectResult(t, acquireAsync(p, context.Background())).err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []nntp.ServerErrorKind{nntp.ServerErrorConnect, nntp.ServerErrorConnect}, kinds)
}

func TestReconfigureShrinkClosesIdleNowAndLeasedOnRelease(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 4, Factory: srv.Factory()})
	var leases []*nntp.Lease
	for range 4 {
		leases = append(leases, acquire(t, p))
	}
	leases[0].Release()
	leases[1].Release()

	p.Reconfigure(nntp.PoolConfig{MaxConnections: 1})

	assert.Equal(t, 2, srv.OpenConns(), "excess idle connections are closed at once")
	leases[2].Release()
	assert.Equal(t, 1, srv.OpenConns(), "a leased one beyond the new limit is closed on release")
	leases[3].Release()
	assert.Equal(t, 1, srv.OpenConns())
	assert.Equal(t, nntp.PoolStats{Idle: 1, Dials: 4}, p.Stats())
}

func TestReconfigureGrowLetsWaitersDialMore(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	_ = acquire(t, p)
	res := acquireAsync(p, context.Background())
	expectBlocked(t, res)

	p.Reconfigure(nntp.PoolConfig{MaxConnections: 2})

	require.NoError(t, expectResult(t, res).err)
	assert.Equal(t, 2, srv.OpenConns())
}

func TestReconfigureFactoryAffectsNewDialsAndRetiresOldConnections(t *testing.T) {
	oldSrv, newSrv := nntptest.NewServer("old"), nntptest.NewServer("new")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 2, Factory: oldSrv.Factory()})
	idle, leased := acquire(t, p), acquire(t, p)
	idle.Release()

	p.Reconfigure(nntp.PoolConfig{Factory: newSrv.Factory()})

	assert.Equal(t, 1, oldSrv.OpenConns(), "idle connections of the old factory are closed")
	leased.Release()
	assert.Equal(t, 0, oldSrv.OpenConns(), "so are leased ones, on release")
	l := acquire(t, p)
	assert.Equal(t, 1, newSrv.Dials())
	assert.Equal(t, 2, oldSrv.Dials())
	l.Release()
}

func TestReconfigureWithNewCredentialsReenablesADisabledPool(t *testing.T) {
	bad, good := nntptest.NewServer("bad"), nntptest.NewServer("good")
	bad.FailAuth(true)
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: bad.Factory()})
	_, err := p.Acquire(context.Background())
	require.ErrorIs(t, err, nntp.ErrServerDisabled)

	p.Reconfigure(nntp.PoolConfig{MaxConnections: 1, Factory: good.Factory()})

	assert.False(t, p.Stats().Disabled)
	l := acquire(t, p)
	l.Release()
	assert.Equal(t, 1, bad.AuthAttempts(), "the old credentials were not tried again")
}

func TestReconfigureWithoutFactoryKeepsADisabledPoolDisabled(t *testing.T) {
	bad := nntptest.NewServer("bad")
	bad.FailAuth(true)
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: bad.Factory()})
	_, _ = p.Acquire(context.Background())

	p.Reconfigure(nntp.PoolConfig{MaxConnections: 5})

	_, err := p.Acquire(context.Background())
	assert.ErrorIs(t, err, nntp.ErrServerDisabled)
	assert.Equal(t, 1, bad.AuthAttempts())
}

func TestReconfigureIdleTimeoutAppliesToParkedConnections(t *testing.T) {
	clock := newFakeClock()
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory(), Clock: clock})
	l := acquire(t, p)
	l.Release()
	clock.waitTimers(t, 1)

	p.Reconfigure(nntp.PoolConfig{IdleTimeout: 5 * time.Second})
	clock.waitCreated(t, 2) // the janitor re-armed
	clock.waitTimers(t, 1)
	clock.Advance(5 * time.Second)

	require.Eventually(t, func() bool { return srv.OpenConns() == 0 }, 5*time.Second, time.Millisecond)
}

func TestReconfigureAfterCloseDoesNothing(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := nntp.NewPool(nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	require.NoError(t, p.Close())

	p.Reconfigure(nntp.PoolConfig{MaxConnections: 3, Factory: srv.Factory()})

	_, err := p.Acquire(context.Background())
	assert.ErrorIs(t, err, nntp.ErrPoolClosed)
}

func TestMaxConnectionsIsClampedToOneThroughAHundred(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 500, Factory: srv.Factory()})
	for range 100 {
		_ = acquire(t, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.Acquire(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the 101st waits")

	zero := newPool(t, nntp.PoolConfig{Factory: srv.Factory()})
	_ = acquire(t, zero)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	_, err = zero.Acquire(ctx2)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "no limit means one connection")
}

// stubConn is an ArticleConn whose Body fails with a scripted error and
// whose Dead stays false.
type stubConn struct {
	err    error
	closed atomic.Bool
}

func (c *stubConn) Body(context.Context, string, func([]byte) error) error { return c.err }
func (c *stubConn) Date(context.Context) (time.Time, error)                { return time.Time{}, c.err }
func (c *stubConn) Close() error                                           { c.closed.Store(true); return nil }
func (c *stubConn) Dead() bool                                             { return false }

func TestBodyStreamsTheArticleAndKeepsTheConnectionForReuse(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.AddArticle("a@x", []string{"one", "two"})
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	var lines []string

	for range 2 {
		lines = nil
		err := p.Body(context.Background(), "a@x", func(l []byte) error {
			lines = append(lines, string(l))
			return nil
		})
		require.NoError(t, err)
	}

	assert.Equal(t, []string{"one", "two"}, lines)
	assert.Equal(t, 1, srv.Dials())
	assert.Equal(t, nntp.PoolStats{Idle: 1, Dials: 1}, p.Stats())
}

func TestBodyKeepsTheConnectionAfterArticleNotFound(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})

	err := p.Body(context.Background(), "gone@x", func([]byte) error { return nil })
	_ = p.Body(context.Background(), "gone@x", func([]byte) error { return nil })

	assert.ErrorIs(t, err, nntp.ErrArticleNotFound)
	assert.Equal(t, 1, srv.Dials())
	assert.Equal(t, 1, p.Stats().Idle)
}

func TestBodyDropsTheConnectionAfterAMidBodyDisconnect(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.AddArticle("a@x", []string{"1", "2", "3"})
	srv.Disconnect("a@x", 1, 1)
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})

	err := p.Body(context.Background(), "a@x", func([]byte) error { return nil })
	assert.ErrorIs(t, err, nntp.ErrConnection)
	assert.Equal(t, 0, srv.OpenConns())
	assert.Equal(t, nntp.PoolStats{Dials: 1}, p.Stats())

	require.NoError(t, p.Body(context.Background(), "a@x", func([]byte) error { return nil }), "the pool does not retry, the next call works")
	assert.Equal(t, 2, srv.Dials())
}

func TestBodyDropsConnectionsThatFailedEvenIfTheyDoNotReportDead(t *testing.T) {
	for name, kind := range map[string]error{"connection": nntp.ErrConnection, "protocol": nntp.ErrProtocol} {
		t.Run(name, func(t *testing.T) {
			stub := &stubConn{err: &nntp.Error{Kind: kind, Op: "body"}}
			p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: func(context.Context) (nntp.ArticleConn, error) { return stub, nil }})

			err := p.Body(context.Background(), "a@x", func([]byte) error { return nil })

			assert.ErrorIs(t, err, kind)
			assert.True(t, stub.closed.Load())
			assert.Equal(t, 0, p.Stats().Idle)
		})
	}
}

func TestBodyDropsTheConnectionWhenTheSinkFails(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.AddArticle("a@x", []string{"1", "2"})
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	boom := errors.New("disk full")

	err := p.Body(context.Background(), "a@x", func([]byte) error { return boom })

	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 0, srv.OpenConns())
}

func TestBodyReportsAcquireFailures(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.FailAuth(true)
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})

	err := p.Body(context.Background(), "a@x", func([]byte) error { return nil })

	assert.ErrorIs(t, err, nntp.ErrServerDisabled)
}

func TestLeaseDiscardClosesTheConnectionAndFreesTheSlot(t *testing.T) {
	srv := nntptest.NewServer("main")
	p := newPool(t, nntp.PoolConfig{MaxConnections: 1, Factory: srv.Factory()})
	l := acquire(t, p)

	l.Discard()
	l.Release() // no effect after Discard

	assert.Equal(t, 0, srv.OpenConns())
	assert.Equal(t, nntp.PoolStats{Dials: 1}, p.Stats())
	_ = acquire(t, p)
}

func TestDialFactoryReportsDialErrorsAsNilConnections(t *testing.T) {
	c, err := nntp.DialFactory(nntp.Config{})(context.Background())

	assert.Nil(t, c, "a nil *Conn must not become a non-nil interface")
	assert.ErrorIs(t, err, nntp.ErrConfig)
}

func TestWrongPasswordIsTriedOnceNotOncePerConnection(t *testing.T) {
	srv := nntptest.NewServer("main")
	srv.FailAuth(true)
	p := newPool(t, nntp.PoolConfig{MaxConnections: 50, Factory: srv.Factory()})
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Go(func() {
			_, err := p.Acquire(context.Background())
			errs <- err
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.ErrorIs(t, err, nntp.ErrServerDisabled)
	}
	assert.Equal(t, 1, srv.AuthAttempts(), "providers ban accounts after repeated failed logins")
	assert.Equal(t, 1, srv.Dials())
}

func TestAfterTheFirstSuccessfulDialConnectionsOpenInParallel(t *testing.T) {
	srv := nntptest.NewServer("main")
	const n = 10
	p := newPool(t, nntp.PoolConfig{MaxConnections: n, Factory: srv.Factory()})
	var wg sync.WaitGroup
	leases := make(chan *nntp.Lease, n)
	for range n {
		wg.Go(func() {
			l, err := p.Acquire(context.Background())
			assert.NoError(t, err)
			leases <- l
		})
	}
	wg.Wait()
	close(leases)

	assert.Equal(t, n, srv.PeakConns())
	for l := range leases {
		l.Release()
	}
}

// probeDialer records how many dials had started when the first one
// finished.
type probeDialer struct {
	inner   nntp.ConnFactory
	delay   time.Duration
	started atomic.Int64
	once    sync.Once
	atFirst atomic.Int64
}

func (g *probeDialer) factory(ctx context.Context) (nntp.ArticleConn, error) {
	g.started.Add(1)
	time.Sleep(g.delay)
	g.once.Do(func() { g.atFirst.Store(g.started.Load()) })
	return g.inner(ctx)
}

func acquireMany(t *testing.T, p *nntp.Pool, n int) []*nntp.Lease {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var leases []*nntp.Lease
	for range n {
		wg.Go(func() {
			if l, err := p.Acquire(context.Background()); err == nil {
				mu.Lock()
				leases = append(leases, l)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return leases
}

func TestOnlyOneDialRunsAtATimeUntilTheFirstSuccess(t *testing.T) {
	g := &probeDialer{inner: nntptest.NewServer("main").Factory(), delay: 10 * time.Millisecond}
	p := newPool(t, nntp.PoolConfig{MaxConnections: 8, Factory: g.factory})

	leases := acquireMany(t, p, 8)

	assert.Len(t, leases, 8)
	assert.EqualValues(t, 1, g.atFirst.Load(), "no second dial starts before the first one succeeds")
	for _, l := range leases {
		l.Release()
	}
}

func TestReconfigureWithANewFactoryProbesWithOneDialAgain(t *testing.T) {
	g := &probeDialer{inner: nntptest.NewServer("new").Factory(), delay: 10 * time.Millisecond}
	p := newPool(t, nntp.PoolConfig{MaxConnections: 8, Factory: nntptest.NewServer("old").Factory()})
	l := acquire(t, p) // proven with the old factory
	l.Release()

	p.Reconfigure(nntp.PoolConfig{Factory: g.factory})
	leases := acquireMany(t, p, 8)

	assert.Len(t, leases, 8)
	assert.EqualValues(t, 1, g.atFirst.Load(), "new credentials are probed by one dial first")
}
