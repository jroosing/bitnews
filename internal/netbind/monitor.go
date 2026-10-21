package netbind

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// DefaultInterval is how often a Monitor checks the binding.
const DefaultInterval = 5 * time.Second

// Iface describes one network interface at one moment.
type Iface struct {
	Name  string
	Up    bool
	Addrs []netip.Addr
}

// Lookup lists the host's network interfaces.
type Lookup func() ([]Iface, error)

// SystemLookup lists the interfaces of this host.
func SystemLookup() ([]Iface, error) {
	sys, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(sys))
	for _, s := range sys {
		i := Iface{Name: s.Name, Up: s.Flags&net.FlagUp != 0}
		// Addrs fails for an interface that vanished between the two calls;
		// it then has no addresses, which reads as "not available".
		if addrs, err := s.Addrs(); err == nil {
			for _, a := range addrs {
				if p, err := netip.ParsePrefix(a.String()); err == nil {
					i.Addrs = append(i.Addrs, p.Addr())
				}
			}
		}
		out = append(out, i)
	}
	return out, nil
}

// Status says whether a binding can currently carry traffic.
type Status struct {
	Available bool
	// Reason explains an unavailable binding (the bind_unavailable detail).
	Reason string
}

// Check evaluates the binding against a snapshot of the interfaces: the
// interface must exist and be up, and the address, if any, must be assigned
// to that interface (to any interface that is up when none is bound). A
// Binding that binds nothing is always available.
func (b Binding) Check(ifaces []Iface) Status {
	if !b.Enabled() {
		return Status{Available: true}
	}
	want, err := b.ip()
	if err != nil {
		return Status{Reason: err.Error()}
	}
	var candidates []Iface
	if b.Interface != "" {
		for _, i := range ifaces {
			if i.Name == b.Interface {
				if !i.Up {
					return Status{Reason: fmt.Sprintf("interface %q is down", b.Interface)}
				}
				candidates = append(candidates, i)
			}
		}
		if len(candidates) == 0 {
			return Status{Reason: fmt.Sprintf("interface %q does not exist", b.Interface)}
		}
		if !want.IsValid() {
			return Status{Available: true}
		}
	} else {
		for _, i := range ifaces {
			if i.Up {
				candidates = append(candidates, i)
			}
		}
	}
	for _, i := range candidates {
		for _, a := range i.Addrs {
			if a.WithZone("").Unmap() == want {
				return Status{Available: true}
			}
		}
	}
	if b.Interface != "" {
		return Status{Reason: fmt.Sprintf("address %s is not assigned to interface %q", want, b.Interface)}
	}
	return Status{Reason: fmt.Sprintf("address %s is not assigned to any interface that is up", want)}
}

// CheckNow is Check against this host's interfaces. A failed lookup counts
// as unavailable.
func (b Binding) CheckNow() Status { return b.checkWith(SystemLookup) }

func (b Binding) checkWith(l Lookup) Status {
	ifaces, err := l()
	if err != nil {
		return Status{Reason: fmt.Sprintf("cannot list network interfaces: %v", err)}
	}
	return b.Check(ifaces)
}

// Ticker is the part of time.Ticker a Monitor needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock creates tickers; tests replace it.
type Clock interface {
	NewTicker(d time.Duration) Ticker
}

type systemClock struct{}

func (systemClock) NewTicker(d time.Duration) Ticker { return sysTicker{time.NewTicker(d)} }

type sysTicker struct{ t *time.Ticker }

func (s sysTicker) C() <-chan time.Time { return s.t.C }
func (s sysTicker) Stop()               { s.t.Stop() }

// MonitorOption customises NewMonitor.
type MonitorOption func(*Monitor)

// WithLookup replaces the interface lookup (tests).
func WithLookup(l Lookup) MonitorOption { return func(m *Monitor) { m.lookup = l } }

// Monitor polls a binding and reports changes of its Status.
type Monitor struct {
	b        Binding
	interval time.Duration
	onChange func(Status)
	clock    Clock
	lookup   Lookup

	mu   sync.Mutex
	last Status
	seen bool
}

// NewMonitor returns a Monitor that checks b every interval (zero means
// DefaultInterval) and calls onChange once for the first check and then only
// when the Status changes. A nil clock is the system clock. onChange runs on
// the Run goroutine and must not block for long.
func NewMonitor(b Binding, interval time.Duration, onChange func(Status), clock Clock, opts ...MonitorOption) *Monitor {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if clock == nil {
		clock = systemClock{}
	}
	m := &Monitor{b: b, interval: interval, onChange: onChange, clock: clock, lookup: SystemLookup}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Status is the last Status the Monitor saw (the zero Status, unavailable
// with no reason, before the first check).
func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Run checks at once and then on every tick until ctx is done. It returns
// ctx.Err().
func (m *Monitor) Run(ctx context.Context) error {
	t := m.clock.NewTicker(m.interval)
	defer t.Stop()
	m.check()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C():
			m.check()
		}
	}
}

func (m *Monitor) check() {
	s := m.b.checkWith(m.lookup)
	m.mu.Lock()
	changed := !m.seen || s != m.last
	m.last, m.seen = s, true
	m.mu.Unlock()
	if changed && m.onChange != nil {
		m.onChange(s)
	}
}
