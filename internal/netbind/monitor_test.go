package netbind_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/netbind"
)

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestCheckInterfaceMustExistAndBeUp(t *testing.T) {
	b := netbind.Binding{Interface: "wg0"}

	assert.True(t, b.Check([]netbind.Iface{{Name: "wg0", Up: true}}).Available)

	missing := b.Check([]netbind.Iface{{Name: "eth0", Up: true}})
	assert.False(t, missing.Available)
	assert.Contains(t, missing.Reason, `"wg0"`)
	assert.Contains(t, missing.Reason, "does not exist")

	down := b.Check([]netbind.Iface{{Name: "wg0", Up: false}})
	assert.False(t, down.Available)
	assert.Contains(t, down.Reason, "down")
}

func TestCheckAddressMustBeAssignedToTheInterface(t *testing.T) {
	b := netbind.Binding{Interface: "wg0", Address: "10.2.0.2"}
	ifs := []netbind.Iface{
		{Name: "wg0", Up: true, Addrs: addrs("10.2.0.2")},
		{Name: "eth0", Up: true, Addrs: addrs("192.168.1.5")},
	}
	assert.True(t, b.Check(ifs).Available)

	other := netbind.Binding{Interface: "wg0", Address: "192.168.1.5"}.Check(ifs)
	assert.False(t, other.Available, "an address of another interface does not count")
	assert.Contains(t, other.Reason, "192.168.1.5")
}

func TestCheckAddressOnlyAcceptsAnyInterface(t *testing.T) {
	b := netbind.Binding{Address: "192.168.1.5"}
	ifs := []netbind.Iface{{Name: "eth0", Up: true, Addrs: addrs("192.168.1.5")}}
	assert.True(t, b.Check(ifs).Available)

	gone := b.Check([]netbind.Iface{{Name: "eth0", Up: true}})
	assert.False(t, gone.Available)

	downIf := b.Check([]netbind.Iface{{Name: "eth0", Up: false, Addrs: addrs("192.168.1.5")}})
	assert.False(t, downIf.Available, "the address of a down interface is not usable")
}

func TestCheckComparesMappedAndZonedAddresses(t *testing.T) {
	b := netbind.Binding{Address: "fd00::2"}
	z := netip.MustParseAddr("fd00::2").WithZone("wg0")
	assert.True(t, b.Check([]netbind.Iface{{Name: "wg0", Up: true, Addrs: []netip.Addr{z}}}).Available)

	b4 := netbind.Binding{Address: "10.0.0.2"}
	mapped := netip.MustParseAddr("::ffff:10.0.0.2")
	assert.True(t, b4.Check([]netbind.Iface{{Name: "x", Up: true, Addrs: []netip.Addr{mapped}}}).Available)
}

func TestCheckWithoutBindingIsAlwaysAvailable(t *testing.T) {
	assert.True(t, netbind.Binding{}.Check(nil).Available)
}

// fakeClock hands out tickers that the test fires by hand.
type fakeClock struct {
	mu      sync.Mutex
	tickers []*fakeTicker
	made    chan struct{}
}

type fakeTicker struct {
	c       chan time.Time
	stopped bool
	d       time.Duration
}

func (f *fakeTicker) C() <-chan time.Time { return f.c }
func (f *fakeTicker) Stop()               { f.stopped = true }

func newFakeClock() *fakeClock { return &fakeClock{made: make(chan struct{}, 8)} }

func (c *fakeClock) NewTicker(d time.Duration) netbind.Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTicker{c: make(chan time.Time), d: d}
	c.tickers = append(c.tickers, t)
	c.made <- struct{}{}
	return t
}

func (c *fakeClock) tick(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	tk := c.tickers[len(c.tickers)-1]
	c.mu.Unlock()
	select {
	case tk.c <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("monitor is not waiting for a tick")
	}
}

// fakeHost is a mutable interface list.
type fakeHost struct {
	mu     sync.Mutex
	ifaces []netbind.Iface
	err    error
}

func (h *fakeHost) set(i ...netbind.Iface) { h.mu.Lock(); h.ifaces, h.err = i, nil; h.mu.Unlock() }
func (h *fakeHost) lookup() ([]netbind.Iface, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ifaces, h.err
}

type monitorHarness struct {
	host    *fakeHost
	clock   *fakeClock
	changes chan netbind.Status
	done    chan error
	cancel  context.CancelFunc
	mon     *netbind.Monitor
}

func startMonitor(t *testing.T, b netbind.Binding, initial ...netbind.Iface) *monitorHarness {
	t.Helper()
	h := &monitorHarness{host: &fakeHost{}, clock: newFakeClock(), changes: make(chan netbind.Status, 16), done: make(chan error, 1)}
	h.host.set(initial...)
	h.mon = netbind.NewMonitor(b, 0, func(s netbind.Status) { h.changes <- s }, h.clock, netbind.WithLookup(h.host.lookup))
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.mon.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-h.done })
	select {
	case <-h.clock.made:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor never started its ticker")
	}
	return h
}

func (h *monitorHarness) next(t *testing.T) netbind.Status {
	t.Helper()
	select {
	case s := <-h.changes:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no status change")
		return netbind.Status{}
	}
}

func (h *monitorHarness) none(t *testing.T) {
	t.Helper()
	select {
	case s := <-h.changes:
		t.Fatalf("unexpected status change %+v", s)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMonitorReportsInitialStatusOnceAndThenOnlyTransitions(t *testing.T) {
	h := startMonitor(t, netbind.Binding{Interface: "wg0"}, netbind.Iface{Name: "wg0", Up: true})

	assert.True(t, h.next(t).Available, "initial status")

	h.clock.tick(t)
	h.none(t) // unchanged: no call

	h.host.set() // interface vanishes
	h.clock.tick(t)
	gone := h.next(t)
	assert.False(t, gone.Available)
	assert.Contains(t, gone.Reason, "does not exist")

	h.clock.tick(t)
	h.none(t)

	h.host.set(netbind.Iface{Name: "wg0", Up: true})
	h.clock.tick(t)
	assert.True(t, h.next(t).Available)
	assert.Equal(t, netbind.Status{Available: true}, h.mon.Status())
}

func TestMonitorStartingUnavailableReportsItAtOnce(t *testing.T) {
	h := startMonitor(t, netbind.Binding{Interface: "wg0"})

	s := h.next(t)

	assert.False(t, s.Available)
	assert.NotEmpty(t, s.Reason)
}

func TestMonitorTreatsLookupFailureAsUnavailable(t *testing.T) {
	h := startMonitor(t, netbind.Binding{Interface: "wg0"}, netbind.Iface{Name: "wg0", Up: true})
	require.True(t, h.next(t).Available)

	h.host.mu.Lock()
	h.host.err = errors.New("netlink busy")
	h.host.mu.Unlock()
	h.clock.tick(t)

	s := h.next(t)
	assert.False(t, s.Available)
	assert.Contains(t, s.Reason, "netlink busy")
}

func TestMonitorDefaultsToFiveSecondsAndStopsItsTickerOnCancel(t *testing.T) {
	h := startMonitor(t, netbind.Binding{Interface: "wg0"}, netbind.Iface{Name: "wg0", Up: true})
	h.clock.mu.Lock()
	tk := h.clock.tickers[0]
	h.clock.mu.Unlock()
	assert.Equal(t, 5*time.Second, tk.d)

	h.cancel()
	assert.ErrorIs(t, <-h.done, context.Canceled)
	h.done <- nil // the cleanup waits on done again
	assert.True(t, tk.stopped)
}

func TestSystemLookupSeesTheLoopbackInterface(t *testing.T) {
	ifs, err := netbind.SystemLookup()
	require.NoError(t, err)

	var found bool
	for _, i := range ifs {
		if i.Up && len(i.Addrs) > 0 && (i.Name == "lo" || i.Name == "lo0") {
			found = true
		}
	}
	assert.True(t, found, "%+v", ifs)
	assert.True(t, netbind.Binding{Address: "127.0.0.1"}.Check(ifs).Available)
}
