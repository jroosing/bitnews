package app_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// clock is a manually advanced clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: epoch.Add(time.Hour)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stack is the real store with a bus and a subscriber that has seen every
// event since the stack was built.
type stack struct {
	db        *store.DB
	downloads *store.DownloadRepo
	cats      *store.CategoryRepo
	settings  *store.SettingsRepo
	bus       *core.Bus
	sub       *core.Subscription
	clock     *clock
}

func newStack(t *testing.T) *stack {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	bus := core.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &stack{
		db:        db,
		downloads: store.NewDownloadRepo(db),
		cats:      store.NewCategoryRepo(db),
		settings:  store.NewSettingsRepo(db),
		bus:       bus,
		sub:       bus.Subscribe(ctx),
		clock:     newClock(),
	}
}

// events drains the events published so far.
func (s *stack) events() []core.Event {
	var out []core.Event
	for {
		select {
		case ev := <-s.sub.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// addDownload stores a Queued NZB Download added at epoch+offset.
func (s *stack) addDownload(t *testing.T, name string, offset time.Duration) core.Download {
	t.Helper()
	at := epoch.Add(offset)
	d := core.Download{
		ID: core.NewID(), Name: name, SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at,
	}
	require.NoError(t, s.downloads.Create(context.Background(), d))
	return d
}
