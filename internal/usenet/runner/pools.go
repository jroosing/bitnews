package runner

import (
	"log/slog"
	"slices"
	"strings"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
)

// poolEntry is one Server with its pool.
type poolEntry struct {
	spec ServerSpec
	pool *nntp.Pool
}

// poolSet holds one nntp.Pool per enabled Server, shared by all Downloads.
type poolSet struct {
	factory func(ServerSpec) nntp.ConnFactory
	log     *slog.Logger

	mu      sync.Mutex
	entries map[string]*poolEntry
	closed  bool
}

func newPoolSet(factory func(ServerSpec) nntp.ConnFactory, log *slog.Logger) *poolSet {
	return &poolSet{factory: factory, log: log, entries: map[string]*poolEntry{}}
}

// sync makes the pools match specs: pools of new enabled Servers are created,
// those of removed or disabled Servers closed, and a pool whose connection
// settings changed is reconfigured in place. It reports whether the set of
// Servers or anything the scheduler depends on (priority, retention,
// connection count) changed, so running fetches must restart their
// scheduler.
func (p *poolSet) sync(specs []ServerSpec) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	want := map[string]ServerSpec{}
	for _, s := range specs {
		if s.Enabled {
			want[s.Name] = s
		}
	}
	for name, e := range p.entries {
		if _, ok := want[name]; !ok {
			_ = e.pool.Close()
			delete(p.entries, name)
			changed = true
		}
	}
	for name, s := range want {
		e := p.entries[name]
		if e == nil {
			p.entries[name] = &poolEntry{spec: s, pool: p.newPool(s)}
			changed = true
			continue
		}
		if e.spec == s {
			continue
		}
		if e.spec.Host != s.Host || e.spec.Port != s.Port || e.spec.User != s.User || e.spec.Pass != s.Pass ||
			e.spec.MaxConnections != s.MaxConnections {
			e.pool.Reconfigure(nntp.PoolConfig{MaxConnections: s.MaxConnections, Factory: p.factory(s)})
		}
		if e.spec.Priority != s.Priority || e.spec.RetentionDays != s.RetentionDays ||
			e.spec.MaxConnections != s.MaxConnections {
			changed = true
		}
		e.spec = s
	}
	return changed
}

func (p *poolSet) newPool(s ServerSpec) *nntp.Pool {
	log := p.log
	return nntp.NewPool(nntp.PoolConfig{
		Name:           s.Name,
		MaxConnections: s.MaxConnections,
		Factory:        p.factory(s),
		OnError: func(e nntp.ServerError) {
			log.Warn("usenet server error", "server", e.Server, "kind", int(e.Kind), "err", e.Err)
		},
	})
}

// active returns the enabled Servers, ordered by priority and name.
func (p *poolSet) active() []poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b poolEntry) int {
		if a.spec.Priority != b.spec.Priority {
			return a.spec.Priority - b.spec.Priority
		}
		return strings.Compare(a.spec.Name, b.spec.Name)
	})
	return out
}

// revive re-enables pools that were disabled by an authentication failure,
// for a retry after the failure backoff. A pool is only re-enabled by a new
// factory, so every disabled pool gets one.
func (p *poolSet) revive() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.pool.Stats().Disabled {
			e.pool.Reconfigure(nntp.PoolConfig{Factory: p.factory(e.spec)})
		}
	}
}

func (p *poolSet) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for name, e := range p.entries {
		_ = e.pool.Close()
		delete(p.entries, name)
	}
}

// defaultFactory dials a Server with strict TLS.
func defaultFactory(s ServerSpec) nntp.ConnFactory {
	return nntp.DialFactory(nntp.Config{Host: s.Host, Port: s.Port, User: s.User, Pass: s.Pass})
}
