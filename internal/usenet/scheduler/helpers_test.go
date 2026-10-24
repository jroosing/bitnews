package scheduler_test

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
	"github.com/jroosing/bitnews/internal/usenet/scheduler"
)

// noBackoff makes retries immediate.
func noBackoff(int) time.Duration { return 0 }

// articlesOf parses the fixture NZB into scheduler Articles.
func articlesOf(t testing.TB, fx nntptest.Fixture) []scheduler.Article {
	t.Helper()
	n, err := nzb.Parse(bytes.NewReader(fx.NZB), nzb.DefaultLimits())
	require.NoError(t, err)
	var out []scheduler.Article
	for fi, f := range n.Files {
		for _, s := range f.Segments {
			out = append(out, scheduler.Article{
				FileKey: fi, SegmentNumber: s.Number, MessageID: s.MessageID,
				ExpectedBytes: s.Bytes, Date: f.Date,
			})
		}
	}
	return out
}

// testServer is a scripted Server with a real Pool in front of it.
type testServer struct {
	*nntptest.Server
	cfg scheduler.Server
}

func newTestServer(t testing.TB, name string, priority, workers int) *testServer {
	t.Helper()
	srv := nntptest.NewServer(name)
	pool := nntp.NewPool(nntp.PoolConfig{Name: name, MaxConnections: workers, Factory: srv.Factory()})
	t.Cleanup(func() { _ = pool.Close() })
	return &testServer{Server: srv, cfg: scheduler.Server{Name: name, Priority: priority, Workers: workers, Pool: pool}}
}

// collector gathers Results; it is safe for concurrent use.
type collector struct {
	mu      sync.Mutex
	results map[string]scheduler.Result
	count   int
}

func newCollector() *collector { return &collector{results: map[string]scheduler.Result{}} }

func (c *collector) add(r scheduler.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	c.results[r.Article.MessageID] = r
}

func (c *collector) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func (c *collector) get(id string) scheduler.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.results[id]
}

// run builds a Scheduler, runs it to completion and returns the collector.
func run(t testing.TB, servers []*testServer, arts []scheduler.Article, mod ...func(*scheduler.Config)) (*scheduler.Scheduler, *collector) {
	t.Helper()
	col := newCollector()
	cfg := scheduler.Config{Articles: arts, OnResult: col.add, Backoff: noBackoff}
	for _, s := range servers {
		cfg.Servers = append(cfg.Servers, s.cfg)
	}
	for _, m := range mod {
		m(&cfg)
	}
	s, err := scheduler.New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, s.Run(ctx))
	require.Equal(t, len(arts), col.n(), "every Article resolves exactly once")
	return s, col
}

func fixtureFiles(n, size int) map[string][]byte {
	files := map[string][]byte{}
	for i := range n {
		data := make([]byte, size+i*7)
		for j := range data {
			data[j] = byte((j*31 + i*17 + j>>8) % 251)
		}
		files[fmt.Sprintf("file%02d.bin", i)] = data
	}
	return files
}

// assemble rebuilds files from Done Parts, keyed by Part.Name.
func assemble(t testing.TB, col *collector) map[string][]byte {
	t.Helper()
	col.mu.Lock()
	defer col.mu.Unlock()
	files := map[string][]byte{}
	var ids []string
	for id := range col.results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := col.results[id]
		if r.Part == nil {
			continue
		}
		p := r.Part
		buf := files[p.Name]
		if buf == nil {
			buf = make([]byte, p.Size)
			files[p.Name] = buf
		}
		copy(buf[p.Begin-1:], p.Data)
	}
	return files
}
