package netbind

import (
	"context"
	"sync"
	"time"
)

// KillswitchConfig configures a Killswitch.
type KillswitchConfig struct {
	Binding Binding
	// Interval is the Monitor interval; zero means DefaultInterval.
	Interval time.Duration
	// Pause is called when the binding becomes unavailable, with the detail
	// for the bind_unavailable pause reason. The daemon pauses the
	// protocol's Queue and closes its sockets. Required.
	Pause func(reason string)
	// Resume is called when the binding is available again after a Pause.
	Resume func()
	// Clock and Lookup replace the system clock and interface lookup in
	// tests.
	Clock  Clock
	Lookup Lookup
}

// Killswitch turns the transitions of a Monitor into Pause and Resume
// calls. Pause runs once per outage (a change of reason while the binding
// stays unavailable does not call it again) and Resume once when the outage
// ends. A binding that was available from the start never calls Resume.
type Killswitch struct {
	cfg KillswitchConfig
	mon *Monitor

	mu     sync.Mutex
	paused bool
}

// NewKillswitch returns a Killswitch; call Run to start it.
func NewKillswitch(cfg KillswitchConfig) *Killswitch {
	k := &Killswitch{cfg: cfg}
	var opts []MonitorOption
	if cfg.Lookup != nil {
		opts = append(opts, WithLookup(cfg.Lookup))
	}
	k.mon = NewMonitor(cfg.Binding, cfg.Interval, k.onChange, cfg.Clock, opts...)
	return k
}

// Paused reports whether Pause was called and Resume has not been since.
func (k *Killswitch) Paused() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.paused
}

// Status is the last binding Status seen.
func (k *Killswitch) Status() Status { return k.mon.Status() }

// Run monitors the binding until ctx is done and returns ctx.Err(). With no
// binding enabled it only waits.
func (k *Killswitch) Run(ctx context.Context) error { return k.mon.Run(ctx) }

func (k *Killswitch) onChange(s Status) {
	k.mu.Lock()
	switch {
	case !s.Available && !k.paused:
		k.paused = true
		k.mu.Unlock()
		if k.cfg.Pause != nil {
			k.cfg.Pause(s.Reason)
		}
	case s.Available && k.paused:
		k.paused = false
		k.mu.Unlock()
		if k.cfg.Resume != nil {
			k.cfg.Resume()
		}
	default:
		k.mu.Unlock()
	}
}
