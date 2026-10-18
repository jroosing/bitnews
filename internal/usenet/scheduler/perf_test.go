package scheduler_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/scheduler"
)

// runLarge runs n synthetic Articles over a main and a fill Server where main
// lacks 10% and returns the Scheduler's duration.
func runLarge(tb testing.TB, n int) time.Duration {
	tb.Helper()
	arts := syntheticArticles(n)
	main := newFake(func(id string, _ int) behavior {
		if hashN(id, 1)%10 == 0 {
			return bAbsent
		}
		return bOK
	})
	fill := newFake(func(string, int) behavior { return bOK })
	var done atomic.Int64
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("main", 0, 8, main), fakeServer("fill", 1, 8, fill)},
		Articles: arts,
		OnResult: func(scheduler.Result) { done.Add(1) },
	})
	require.NoError(tb, err)
	start := time.Now()
	require.NoError(tb, s.Run(context.Background()))
	d := time.Since(start)
	assert.EqualValues(tb, n, done.Load())
	assert.EqualValues(tb, n, s.Stats().Done)
	return d
}

func TestLargeNZBDispatchIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("large run")
	}
	small := runLarge(t, 25_000)
	large := runLarge(t, 200_000)
	t.Logf("25k: %v, 200k: %v", small, large)
	assert.Less(t, large, 60*time.Second)
	// 8x the Articles: linear work is about 8x, quadratic about 64x.
	assert.Less(t, float64(large)/float64(max(small, time.Millisecond)), 30.0)
}

func BenchmarkDispatch200kArticles(b *testing.B) {
	for b.Loop() {
		runLarge(b, 200_000)
	}
}
