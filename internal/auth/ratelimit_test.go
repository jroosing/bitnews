package auth_test

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/auth"
)

var (
	ip1 = netip.MustParseAddr("192.0.2.1")
	ip2 = netip.MustParseAddr("192.0.2.2")
)

func newLimiter(opts ...auth.LimiterOption) (*auth.LoginLimiter, *fakeClock) {
	clock := newClock()
	opts = append([]auth.LimiterOption{auth.WithLimiterClock(clock.Now)}, opts...)
	return auth.NewLoginLimiter(opts...), clock
}

func failN(l *auth.LoginLimiter, ip netip.Addr, user string, n int) {
	for range n {
		l.Fail(ip, user)
	}
}

func TestLimiterAllowsFirstFourFailuresWithoutDelay(t *testing.T) {
	l, _ := newLimiter()

	failN(l, ip1, "admin", 4)

	assert.Zero(t, l.Check(ip1, "admin"))
}

func TestLimiterFifthFailureBlocksForOneSecond(t *testing.T) {
	l, clock := newLimiter()

	failN(l, ip1, "admin", 5)

	assert.Equal(t, time.Second, l.Check(ip1, "admin"))
	clock.Advance(400 * time.Millisecond)
	assert.Equal(t, 600*time.Millisecond, l.Check(ip1, "admin"), "reports the remaining wait")
	clock.Advance(600 * time.Millisecond)
	assert.Zero(t, l.Check(ip1, "admin"))
}

func TestLimiterBackoffDoublesAndCapsAtFifteenMinutes(t *testing.T) {
	l, clock := newLimiter()
	failN(l, ip1, "admin", 4)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i := range 20 {
		l.Fail(ip1, "admin")
		got := l.Check(ip1, "admin")
		if i < len(want) {
			assert.Equal(t, want[i], got, "failure %d", i+5)
		}
		assert.LessOrEqual(t, got, 15*time.Minute)
		clock.Advance(got) // wait out the block, then fail again
	}
	l.Fail(ip1, "admin")
	assert.Equal(t, 15*time.Minute, l.Check(ip1, "admin"))
}

func TestLimiterBlocksAnotherIPForTheSameUsername(t *testing.T) {
	l, _ := newLimiter()
	failN(l, ip1, "admin", 5)

	assert.NotZero(t, l.Check(ip2, "admin"), "username is blocked from every IP")
}

func TestLimiterBlocksAnotherUsernameFromTheSameIP(t *testing.T) {
	l, _ := newLimiter()
	failN(l, ip1, "admin", 5)

	assert.NotZero(t, l.Check(ip1, "other"), "IP is blocked for every username")
	assert.Zero(t, l.Check(ip2, "other"))
}

func TestLimiterReportsTheLongerOfTheTwoWaits(t *testing.T) {
	l, _ := newLimiter()
	failN(l, ip1, "a", 5) // ip1 at 1s
	failN(l, ip2, "b", 7) // ip2 at 4s, b at 4s

	assert.Equal(t, 4*time.Second, l.Check(ip1, "b"))
}

func TestLimiterSuccessResetsBothCounters(t *testing.T) {
	l, _ := newLimiter()
	failN(l, ip1, "admin", 5)

	l.Success(ip1, "admin")

	assert.Zero(t, l.Check(ip1, "admin"))
	failN(l, ip1, "admin", 4)
	assert.Zero(t, l.Check(ip1, "admin"), "the count starts over")
}

func TestLimiterTreatsUsernamesCaseInsensitively(t *testing.T) {
	l, _ := newLimiter()
	failN(l, ip1, "Admin", 3)
	failN(l, ip2, "ADMIN", 2)

	assert.NotZero(t, l.Check(netip.MustParseAddr("192.0.2.3"), "admin"))
}

func TestLimiterGroupsIPv6AddressesByTheir64BitPrefix(t *testing.T) {
	l, _ := newLimiter()
	for i := range 5 {
		l.Fail(netip.MustParseAddr("2001:db8::"+string(rune('1'+i))), "x"+string(rune('a'+i)))
	}

	assert.NotZero(t, l.Check(netip.MustParseAddr("2001:db8::ffff"), "fresh"))
	assert.Zero(t, l.Check(netip.MustParseAddr("2001:db8:0:1::1"), "fresh"), "another /64")
}

func TestLimiterForgetsAFailureHistoryAfterItGoesQuiet(t *testing.T) {
	l, clock := newLimiter()
	failN(l, ip1, "admin", 4)

	clock.Advance(16 * time.Minute)
	l.Fail(ip1, "admin") // would be the 5th

	assert.Zero(t, l.Check(ip1, "admin"))
}

func TestLimiterKeepsABlockedKeyUntilItsBackoffAndGraceEnd(t *testing.T) {
	l, clock := newLimiter()
	failN(l, ip1, "admin", 30)
	clock.Advance(15*time.Minute + time.Second) // block over, key not yet forgotten

	l.Fail(ip1, "admin")

	assert.Equal(t, 15*time.Minute, l.Check(ip1, "admin"), "history survived the wait")
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	l, _ := newLimiter(auth.WithMaxTracked(8))

	for i := range 100 {
		a := netip.AddrFrom4([4]byte{10, 0, byte(i), 1})
		l.Fail(a, "user"+string(rune('A'+i%26))+string(rune('a'+i/26)))
	}

	ips, users := l.Tracked()
	assert.LessOrEqual(t, ips, 8)
	assert.LessOrEqual(t, users, 8)
}

func TestLimiterEvictsLeastRecentlyFailedKeyFirst(t *testing.T) {
	l, _ := newLimiter(auth.WithMaxTracked(2))
	failN(l, ip1, "a", 5)
	l.Fail(ip2, "b")
	l.Fail(netip.MustParseAddr("192.0.2.3"), "c") // evicts ip1/"a"

	assert.Zero(t, l.Check(ip1, "a"))
}

func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	l, _ := newLimiter(auth.WithMaxTracked(16))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ip := netip.AddrFrom4([4]byte{10, 0, 0, byte(i)})
			for range 200 {
				l.Fail(ip, "user")
				l.Check(ip, "user")
				l.Success(ip, "other")
			}
		})
	}
	wg.Wait()
}
