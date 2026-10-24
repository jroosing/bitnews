package netbind_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/netbind"
)

type switchCalls struct {
	mu    sync.Mutex
	calls []string
	ch    chan string
}

func (s *switchCalls) pause(reason string) { s.record("pause:" + reason) }
func (s *switchCalls) resume()             { s.record("resume") }
func (s *switchCalls) record(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	s.ch <- c
}

func (s *switchCalls) next(t *testing.T) string {
	t.Helper()
	select {
	case c := <-s.ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no pause or resume call")
		return ""
	}
}

func (s *switchCalls) none(t *testing.T) {
	t.Helper()
	select {
	case c := <-s.ch:
		t.Fatalf("unexpected call %q", c)
	case <-time.After(50 * time.Millisecond):
	}
}

func startKillswitch(t *testing.T, b netbind.Binding, initial ...netbind.Iface) (*fakeHost, *fakeClock, *switchCalls, *netbind.Killswitch) {
	t.Helper()
	host, clock := &fakeHost{}, newFakeClock()
	host.set(initial...)
	calls := &switchCalls{ch: make(chan string, 16)}
	k := netbind.NewKillswitch(netbind.KillswitchConfig{
		Binding: b, Clock: clock, Lookup: host.lookup, Pause: calls.pause, Resume: calls.resume,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-clock.made:
	case <-time.After(5 * time.Second):
		t.Fatal("killswitch never started its ticker")
	}
	return host, clock, calls, k
}

func TestKillswitchStartingUnavailablePausesOnce(t *testing.T) {
	_, clock, calls, k := startKillswitch(t, netbind.Binding{Interface: "wg0"})

	assert.Equal(t, `pause:interface "wg0" does not exist`, calls.next(t))
	assert.True(t, k.Paused())

	clock.tick(t)
	clock.tick(t)
	calls.none(t)
}

func TestKillswitchStartingAvailableDoesNothing(t *testing.T) {
	_, clock, calls, k := startKillswitch(t, netbind.Binding{Interface: "wg0"}, netbind.Iface{Name: "wg0", Up: true})

	clock.tick(t)

	calls.none(t)
	assert.False(t, k.Paused())
}

func TestKillswitchResumesOnceWhenInterfaceReturns(t *testing.T) {
	host, clock, calls, k := startKillswitch(t, netbind.Binding{Interface: "wg0"})
	require.Contains(t, calls.next(t), "pause:")

	host.set(netbind.Iface{Name: "wg0", Up: true})
	clock.tick(t)

	assert.Equal(t, "resume", calls.next(t))
	assert.False(t, k.Paused())
	clock.tick(t)
	calls.none(t)
}

func TestKillswitchFlappingPausesAndResumesEachTime(t *testing.T) {
	up := netbind.Iface{Name: "wg0", Up: true}
	host, clock, calls, _ := startKillswitch(t, netbind.Binding{Interface: "wg0"}, up)

	var got []string
	for range 3 {
		host.set()
		clock.tick(t)
		got = append(got, calls.next(t))
		host.set(up)
		clock.tick(t)
		got = append(got, calls.next(t))
	}

	want := []string{`pause:interface "wg0" does not exist`, "resume"}
	assert.Equal(t, append(append(append([]string{}, want...), want...), want...), got)
}

func TestKillswitchReasonChangeWhileDownDoesNotPauseAgain(t *testing.T) {
	host, clock, calls, _ := startKillswitch(t, netbind.Binding{Interface: "wg0"})
	require.Contains(t, calls.next(t), "does not exist")

	host.set(netbind.Iface{Name: "wg0", Up: false})
	clock.tick(t)

	calls.none(t)
}

func TestKillswitchWithoutBindingNeverCallsAnything(t *testing.T) {
	_, clock, calls, k := startKillswitch(t, netbind.Binding{})

	clock.tick(t)

	calls.none(t)
	assert.False(t, k.Paused())
}
