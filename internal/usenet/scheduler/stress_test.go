package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/usenet/scheduler"
)

// stressScript decides per (server, article) what happens. Server 3 dies
// after a while.
func stressScript(server int) func(id string, call int) behavior {
	return func(id string, call int) behavior {
		switch h := hashN(id, server) % 100; {
		case h < 30:
			return bAbsent
		case h < 40:
			return bCorrupt
		case h < 47:
			if call < 3 {
				return bFlaky
			}
			return bOK
		case h < 50:
			return bFlaky // never recovers
		default:
			return bOK
		}
	}
}

func TestStressFourServersManyWorkers(t *testing.T) {
	const n = 2000
	arts := syntheticArticles(n)
	fakes := make([]*fakeFetcher, 4)
	for i := range fakes {
		fakes[i] = newFake(stressScript(i))
	}
	var killed sync.Once
	died := make(chan struct{})
	var served int
	var smu sync.Mutex
	dying := newFake(func(id string, call int) behavior {
		smu.Lock()
		served++
		dead := served > 60
		smu.Unlock()
		if dead {
			killed.Do(func() { close(died) })
			return bDisabled
		}
		return stressScript(3)(id, call)
	})
	fakes[3] = dying

	servers := []scheduler.Server{
		fakeServer("a0", 0, 4, fakes[0]),
		fakeServer("a1", 0, 4, fakes[1]),
		fakeServer("b", 1, 6, fakes[2]),
		fakeServer("c", 2, 6, fakes[3]),
	}
	col := newCollector()
	var downs sync.Map
	s, err := scheduler.New(scheduler.Config{
		Servers: servers, Articles: arts, OnResult: col.add, Backoff: noBackoff,
		OnServerDown: func(name string, _ error) {
			_, dup := downs.LoadOrStore(name, true)
			assert.False(t, dup, "ServerDown fires once")
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, s.Run(ctx))

	require.Equal(t, n, col.n())
	for _, a := range arts {
		id := a.MessageID
		for si, f := range fakes {
			assert.LessOrEqual(t, f.callsFor(id), scheduler.MaxAttempts, "server %d article %s", si, id)
		}
		r := col.get(id)
		switch r.Status {
		case scheduler.Done:
			require.NotNil(t, r.Part)
			assert.Equal(t, r.Damaged, !r.Part.CRCMatch())
		case scheduler.Missing:
			assert.Nil(t, r.Part)
		default:
			t.Fatalf("article %s unresolved: %v", id, r.Status)
		}
		// Expected outcome among the Servers that cannot die: the first
		// level with a good copy wins.
		good := func(i int) bool { h := hashN(id, i) % 100; return h >= 50 || (h >= 40 && h < 47) }
		if good(0) || good(1) {
			assert.Equal(t, scheduler.Done, r.Status)
			assert.False(t, r.Damaged)
			assert.Contains(t, []string{"a0", "a1"}, r.Server)
			assert.Zero(t, fakes[2].callsFor(id))
			assert.Zero(t, fakes[3].callsFor(id))
		} else if good(2) {
			assert.False(t, r.Damaged)
			assert.Equal(t, "b", r.Server)
			assert.Zero(t, fakes[3].callsFor(id))
		}
	}
	st := s.Stats()
	assert.EqualValues(t, n, st.Done+st.Missing)
	var total int64
	for _, f := range fakes {
		total += f.total.Load()
	}
	var reqs int64
	for _, sv := range st.Servers {
		reqs += sv.Requests
	}
	assert.Equal(t, total, reqs, "Stats counts every request")
	select {
	case <-died:
		assert.True(t, st.Servers[3].Down)
	default:
		t.Fatal("the dying Server was never exercised")
	}
}
