package scheduler_test

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/scheduler"
)

func TestMainServerHasEverythingFillServerIsNeverAsked(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(3, 5000), 1000)
	main := newTestServer(t, "main", 0, 4)
	fill := newTestServer(t, "fill", 1, 4)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)

	s, col := run(t, []*testServer{main, fill}, arts)

	for _, a := range arts {
		r := col.get(a.MessageID)
		assert.Equal(t, scheduler.Done, r.Status)
		assert.False(t, r.Damaged)
		assert.Equal(t, "main", r.Server)
		require.NotNil(t, r.Part)
		assert.Equal(t, 1, main.Requests(a.MessageID))
	}
	assert.Zero(t, fill.TotalRequests(), "fill Server is never asked")
	st := s.Stats()
	assert.EqualValues(t, len(arts), st.Done)
	assert.Zero(t, st.Missing)
	assert.EqualValues(t, len(arts), st.Servers[0].Requests)
	assert.Zero(t, st.Servers[1].Requests)
	assert.Positive(t, st.Servers[0].Bytes)
}

// pickIDs returns every nth message ID starting at offset.
func pickIDs(arts []scheduler.Article, every, offset int) []string {
	var ids []string
	for i := offset; i < len(arts); i += every {
		ids = append(ids, arts[i].MessageID)
	}
	return ids
}

func TestFillServerIsAskedExactlyForWhatMainLacks(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(4, 20000), 500)
	main := newTestServer(t, "main", 0, 4)
	fill := newTestServer(t, "fill", 1, 4)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	lacking := pickIDs(arts, 20, 3) // 5%
	main.Missing(lacking...)
	isLacking := map[string]bool{}
	for _, id := range lacking {
		isLacking[id] = true
	}

	s, col := run(t, []*testServer{main, fill}, arts)

	for _, a := range arts {
		r := col.get(a.MessageID)
		assert.Equal(t, scheduler.Done, r.Status, a.MessageID)
		assert.Equal(t, 1, main.Requests(a.MessageID))
		if isLacking[a.MessageID] {
			assert.Equal(t, "fill", r.Server)
			assert.Equal(t, 1, fill.Requests(a.MessageID))
		} else {
			assert.Equal(t, "main", r.Server)
			assert.Zero(t, fill.Requests(a.MessageID))
		}
	}
	assert.Equal(t, len(lacking), fill.TotalRequests())
	assert.EqualValues(t, len(arts), s.Stats().Done)
}

func TestArticleAbsentEverywhereIsMissing(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(2, 4000), 1000)
	main := newTestServer(t, "main", 0, 2)
	fill := newTestServer(t, "fill", 1, 2)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	gone := arts[2].MessageID
	main.Missing(gone)
	fill.Missing(gone)

	s, col := run(t, []*testServer{main, fill}, arts)

	r := col.get(gone)
	assert.Equal(t, scheduler.Missing, r.Status)
	assert.Nil(t, r.Part)
	assert.Empty(t, r.Server)
	assert.ErrorIs(t, r.Err, nntp.ErrArticleNotFound)
	assert.Equal(t, 1, main.Requests(gone))
	assert.Equal(t, 1, fill.Requests(gone))
	st := s.Stats()
	assert.EqualValues(t, 1, st.Missing)
	assert.EqualValues(t, len(arts)-1, st.Done)
}

func TestRetentionShorterThanArticleAgeSkipsTheServer(t *testing.T) {
	now := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	posted := now.AddDate(0, 0, -100)
	fx := nntptest.BuildFixture(t, fixtureFiles(2, 4000), 1000, nntptest.WithDate(posted))
	main := newTestServer(t, "main", 0, 2)
	fill := newTestServer(t, "fill", 1, 2)
	main.cfg.RetentionDays = 30
	fill.cfg.RetentionDays = 0
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)

	_, col := run(t, []*testServer{main, fill}, arts, func(c *scheduler.Config) {
		c.Now = func() time.Time { return now }
	})

	assert.Zero(t, main.TotalRequests(), "main retention is shorter than the Article age")
	assert.Equal(t, len(arts), fill.TotalRequests())
	for _, a := range arts {
		assert.Equal(t, "fill", col.get(a.MessageID).Server)
	}
}

func TestRetentionSkipOnTheOnlyServerMakesArticlesMissing(t *testing.T) {
	now := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000, nntptest.WithDate(now.AddDate(0, 0, -10)))
	main := newTestServer(t, "main", 0, 2)
	main.cfg.RetentionDays = 5
	fx.Install(main.Server)
	arts := articlesOf(t, fx)

	s, _ := run(t, []*testServer{main}, arts, func(c *scheduler.Config) {
		c.Now = func() time.Time { return now }
	})

	assert.Zero(t, main.TotalRequests())
	assert.EqualValues(t, len(arts), s.Stats().Missing)
}

func TestUnknownArticleAgeIsNeverSkipped(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000)
	main := newTestServer(t, "main", 0, 2)
	main.cfg.RetentionDays = 1
	fx.Install(main.Server)
	arts := articlesOf(t, fx)
	for i := range arts {
		arts[i].Date = time.Time{}
	}

	s, _ := run(t, []*testServer{main}, arts)

	assert.EqualValues(t, len(arts), s.Stats().Done)
}

func TestTransientFailuresAreRetriedOnTheSameServer(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000)
	main := newTestServer(t, "main", 0, 2)
	fill := newTestServer(t, "fill", 1, 2)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	flaky := arts[1].MessageID
	main.Disconnect(flaky, 2, 2) // drops twice, third attempt works

	_, col := run(t, []*testServer{main, fill}, arts)

	assert.Equal(t, "main", col.get(flaky).Server)
	assert.Equal(t, 3, main.Requests(flaky))
	assert.Zero(t, fill.TotalRequests())
}

func TestServerThatKeepsFailingIsGivenUpAfterThreeAttempts(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000)
	main := newTestServer(t, "main", 0, 2)
	fill := newTestServer(t, "fill", 1, 2)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	flaky := arts[1].MessageID
	main.Disconnect(flaky, 2, 0) // drops every time

	_, col := run(t, []*testServer{main, fill}, arts)

	r := col.get(flaky)
	assert.Equal(t, scheduler.Done, r.Status)
	assert.Equal(t, "fill", r.Server)
	assert.Equal(t, scheduler.MaxAttempts, main.Requests(flaky))
	assert.Equal(t, 1, fill.Requests(flaky))
}

func TestTransientFailureOnTheLastServerLeavesTheArticleMissing(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000)
	main := newTestServer(t, "main", 0, 1)
	fx.Install(main.Server)
	arts := articlesOf(t, fx)
	flaky := arts[0].MessageID
	main.Disconnect(flaky, 1, 0)

	_, col := run(t, []*testServer{main}, arts)

	r := col.get(flaky)
	assert.Equal(t, scheduler.Missing, r.Status)
	assert.ErrorIs(t, r.Err, nntp.ErrConnection)
	assert.Equal(t, scheduler.MaxAttempts, main.Requests(flaky))
}

func TestCRCCorruptMainWithCleanFillGivesUndamagedPartFromFill(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(2, 5000), 1000)
	main := newTestServer(t, "main", 0, 3)
	fill := newTestServer(t, "fill", 1, 3)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	bad := pickIDs(arts, 4, 1)
	main.Corrupt(bad...)
	isBad := map[string]bool{}
	for _, id := range bad {
		isBad[id] = true
	}

	s, col := run(t, []*testServer{main, fill}, arts)

	for _, a := range arts {
		r := col.get(a.MessageID)
		assert.Equal(t, scheduler.Done, r.Status)
		assert.False(t, r.Damaged)
		require.NotNil(t, r.Part)
		assert.True(t, r.Part.CRCMatch())
		if isBad[a.MessageID] {
			assert.Equal(t, "fill", r.Server)
		} else {
			assert.Equal(t, "main", r.Server)
		}
	}
	assert.Zero(t, s.Stats().Damaged)
	assert.Equal(t, len(bad), fill.TotalRequests())
}

func TestCRCCorruptEverywhereGivesDoneDamagedWithTheBestPart(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000)
	main := newTestServer(t, "main", 0, 2)
	fill := newTestServer(t, "fill", 1, 2)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	bad := arts[1].MessageID
	main.Corrupt(bad)
	fill.Corrupt(bad)

	s, col := run(t, []*testServer{main, fill}, arts)

	r := col.get(bad)
	assert.Equal(t, scheduler.Done, r.Status)
	assert.True(t, r.Damaged)
	require.NotNil(t, r.Part)
	assert.False(t, r.Part.CRCMatch())
	assert.Len(t, r.Part.Data, 1000)
	assert.Equal(t, "main", r.Server, "the first of equally good copies is kept")
	assert.Equal(t, 1, main.Requests(bad))
	assert.Equal(t, 1, fill.Requests(bad))
	st := s.Stats()
	assert.EqualValues(t, 1, st.Damaged)
	assert.EqualValues(t, len(arts), st.Done)
	assert.Zero(t, st.Missing)
}

func TestDamagedCopyIsKeptWhenTheOtherServerLacksTheArticle(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(1, 3000), 1000)
	main := newTestServer(t, "main", 0, 2)
	fill := newTestServer(t, "fill", 1, 2)
	fx.Install(main.Server)
	arts := articlesOf(t, fx)
	bad := arts[0].MessageID
	main.Corrupt(bad)

	_, col := run(t, []*testServer{main, fill}, arts)

	r := col.get(bad)
	assert.Equal(t, scheduler.Done, r.Status)
	assert.True(t, r.Damaged)
	assert.Equal(t, "main", r.Server)
}

func TestSameLevelServersShareLoadAndTryEachArticleAtMostOnce(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(4, 30000), 500)
	a := newTestServer(t, "a", 0, 3)
	b := newTestServer(t, "b", 0, 3)
	fill := newTestServer(t, "fill", 1, 3)
	for _, s := range []*testServer{a, b, fill} {
		fx.Install(s.Server)
		s.SetLatency(time.Millisecond)
	}
	arts := articlesOf(t, fx)
	// Neither level-0 Server has these, so both must try them exactly once.
	both := pickIDs(arts, 10, 0)
	a.Missing(both...)
	b.Missing(both...)

	_, col := run(t, []*testServer{a, b, fill}, arts)

	isBoth := map[string]bool{}
	for _, id := range both {
		isBoth[id] = true
	}
	for _, art := range arts {
		id := art.MessageID
		assert.LessOrEqual(t, a.Requests(id), 1, id)
		assert.LessOrEqual(t, b.Requests(id), 1, id)
		r := col.get(id)
		assert.Equal(t, scheduler.Done, r.Status)
		if isBoth[id] {
			assert.Equal(t, 1, a.Requests(id))
			assert.Equal(t, 1, b.Requests(id))
			assert.Equal(t, "fill", r.Server)
		} else {
			assert.Equal(t, 1, a.Requests(id)+b.Requests(id), "exactly one of the siblings serves it")
			assert.Zero(t, fill.Requests(id))
		}
	}
	assert.Positive(t, a.TotalRequests())
	assert.Positive(t, b.TotalRequests())
}

func TestAuthFailureOnTheFillServerMarksItDeadAndTheRunStillCompletes(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(2, 8000), 500)
	main := newTestServer(t, "main", 0, 3)
	fill := newTestServer(t, "fill", 1, 3)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	fill.FailAuth(true)
	arts := articlesOf(t, fx)
	lacking := pickIDs(arts, 5, 0)
	main.Missing(lacking...)

	var mu sync.Mutex
	var downs []string
	s, col := run(t, []*testServer{main, fill}, arts, func(c *scheduler.Config) {
		c.OnServerDown = func(name string, err error) {
			mu.Lock()
			defer mu.Unlock()
			downs = append(downs, name)
			assert.ErrorIs(t, err, nntp.ErrServerDisabled)
		}
	})

	assert.Equal(t, []string{"fill"}, downs, "ServerDown fires once")
	for _, id := range lacking {
		assert.Equal(t, scheduler.Missing, col.get(id).Status)
	}
	st := s.Stats()
	assert.EqualValues(t, len(lacking), st.Missing)
	assert.EqualValues(t, len(arts)-len(lacking), st.Done)
	assert.True(t, st.Servers[1].Down)
	assert.False(t, st.Servers[0].Down)
	assert.Equal(t, 1, fill.AuthAttempts(), "the pool stops after one wrong password")
}

func TestDeadMainServerLetsTheFillServerTakeOver(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(2, 4000), 500)
	main := newTestServer(t, "main", 0, 3)
	fill := newTestServer(t, "fill", 1, 3)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	main.FailAuth(true)
	arts := articlesOf(t, fx)

	downs := 0
	var mu sync.Mutex
	s, col := run(t, []*testServer{main, fill}, arts, func(c *scheduler.Config) {
		c.OnServerDown = func(string, error) { mu.Lock(); downs++; mu.Unlock() }
	})

	assert.Equal(t, 1, downs)
	for _, a := range arts {
		assert.Equal(t, "fill", col.get(a.MessageID).Server)
	}
	assert.EqualValues(t, len(arts), s.Stats().Done)
}

func TestEveryServerDeadMakesRemainingArticlesMissing(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(2, 4000), 500)
	main := newTestServer(t, "main", 0, 3)
	fill := newTestServer(t, "fill", 1, 3)
	main.FailAuth(true)
	fill.FailAuth(true)
	arts := articlesOf(t, fx)

	s, _ := run(t, []*testServer{main, fill}, arts)

	assert.EqualValues(t, len(arts), s.Stats().Missing)
}

func TestReassemblyFromDeliveredPartsIsByteExact(t *testing.T) {
	files := fixtureFiles(5, 25000)
	fx := nntptest.BuildFixture(t, files, 700)
	main := newTestServer(t, "main", 0, 4)
	fill := newTestServer(t, "fill", 1, 4)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	arts := articlesOf(t, fx)
	main.Missing(pickIDs(arts, 7, 2)...)
	main.Corrupt(pickIDs(arts, 11, 5)...)

	_, col := run(t, []*testServer{main, fill}, arts)

	got := assemble(t, col)
	require.Len(t, got, len(files))
	for name, want := range files {
		assert.True(t, bytes.Equal(want, got[name]), "file %s differs", name)
	}
}

func TestDispatchFollowsFileAndSegmentOrderAtEveryLevel(t *testing.T) {
	arts := []scheduler.Article{
		{FileKey: 1, SegmentNumber: 2, MessageID: "b2@x"},
		{FileKey: 0, SegmentNumber: 2, MessageID: "a2@x"},
		{FileKey: 1, SegmentNumber: 1, MessageID: "b1@x"},
		{FileKey: 0, SegmentNumber: 1, MessageID: "a1@x"},
	}
	main := newFake(func(id string, _ int) behavior {
		if strings.HasSuffix(id, "2@x") {
			return bAbsent
		}
		return bOK
	})
	fill := newFake(func(string, int) behavior { return bOK })
	col := newCollector()
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("main", 0, 1, main), fakeServer("fill", 5, 1, fill)},
		Articles: arts, OnResult: col.add,
	})
	require.NoError(t, err)

	require.NoError(t, s.Run(context.Background()))

	assert.Equal(t, []string{"a1@x", "a2@x", "b1@x", "b2@x"}, main.idsInOrder())
	assert.Equal(t, []string{"a2@x", "b2@x"}, fill.idsInOrder())
}

func TestHigherLevelWorkersOnlyTakeArticlesPromotedToThem(t *testing.T) {
	main := newFake(func(string, int) behavior { return bOK })
	fill := newFake(func(string, int) behavior { return bOK })
	col := newCollector()
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("main", 0, 2, main), fakeServer("fill", 1, 8, fill)},
		Articles: syntheticArticles(500), OnResult: col.add,
	})
	require.NoError(t, err)
	require.NoError(t, s.Run(context.Background()))
	assert.EqualValues(t, 500, main.total.Load())
	assert.Zero(t, fill.total.Load())
}

func TestMalformedPartCountsAsAbsentForThatServer(t *testing.T) {
	main := newFake(func(string, int) behavior { return bMalformed })
	fill := newFake(func(string, int) behavior { return bOK })
	col := newCollector()
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("main", 0, 1, main), fakeServer("fill", 1, 1, fill)},
		Articles: syntheticArticles(3), OnResult: col.add,
	})
	require.NoError(t, err)
	require.NoError(t, s.Run(context.Background()))
	for _, a := range syntheticArticles(3) {
		assert.Equal(t, "fill", col.get(a.MessageID).Server)
		assert.Equal(t, 1, main.callsFor(a.MessageID), "garbage is not retried")
	}
}

func TestContextCancelStopsTheRunWithoutLeakingGoroutines(t *testing.T) {
	fx := nntptest.BuildFixture(t, fixtureFiles(4, 50000), 500)
	main := newTestServer(t, "main", 0, 4)
	fill := newTestServer(t, "fill", 1, 4)
	fx.Install(main.Server)
	fx.Install(fill.Server)
	main.SetLatency(5 * time.Millisecond)
	arts := articlesOf(t, fx)
	// Warm the pools so their background goroutines are in the baseline.
	_ = main.cfg.Pool.Body(context.Background(), arts[0].MessageID, func([]byte) error { return nil })
	_ = fill.cfg.Pool.Body(context.Background(), arts[0].MessageID, func([]byte) error { return nil })
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	col := newCollector()
	var once sync.Once
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{main.cfg, fill.cfg},
		Articles: arts,
		OnResult: func(r scheduler.Result) {
			col.add(r)
			if col.n() >= 20 {
				once.Do(cancel)
			}
		},
		Backoff: noBackoff,
	})
	require.NoError(t, err)

	err = s.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	delivered := col.n()
	assert.GreaterOrEqual(t, delivered, 20)
	assert.Less(t, delivered, len(arts), "the run stopped early")
	settleGoroutines(t, before)
	assert.Equal(t, delivered, col.n(), "nothing is delivered after Run returned")
}

func settleGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	buf = buf[:runtime.Stack(buf, true)]
	t.Fatalf("goroutines did not settle: %d > %d\n%s", runtime.NumGoroutine(), want, buf)
}

func TestCancelBeforeRunReturnsImmediately(t *testing.T) {
	main := newFake(func(string, int) behavior { return bOK })
	s, err := scheduler.New(scheduler.Config{
		Servers: []scheduler.Server{fakeServer("main", 0, 2, main)}, Articles: syntheticArticles(10),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, s.Run(ctx), context.Canceled)
}

func TestNewRejectsBadConfigs(t *testing.T) {
	ok := fakeServer("a", 0, 1, newFake(func(string, int) behavior { return bOK }))
	_, err := scheduler.New(scheduler.Config{})
	assert.Error(t, err, "no servers")
	_, err = scheduler.New(scheduler.Config{Servers: []scheduler.Server{{Name: "x"}}})
	assert.Error(t, err, "no pool")
	var many []scheduler.Server
	for range 65 {
		many = append(many, ok)
	}
	_, err = scheduler.New(scheduler.Config{Servers: many})
	assert.Error(t, err, "too many servers")
}

func TestRunTwiceIsAnError(t *testing.T) {
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("a", 0, 1, newFake(func(string, int) behavior { return bOK }))},
		Articles: syntheticArticles(2),
	})
	require.NoError(t, err)
	require.NoError(t, s.Run(context.Background()))
	assert.Error(t, s.Run(context.Background()))
}

func TestNoArticlesFinishesImmediately(t *testing.T) {
	s, err := scheduler.New(scheduler.Config{
		Servers: []scheduler.Server{fakeServer("a", 0, 1, newFake(func(string, int) behavior { return bOK }))},
	})
	require.NoError(t, err)
	assert.NoError(t, s.Run(context.Background()))
}

func TestTwoSchedulersRunConcurrentlyWithoutSharedState(t *testing.T) {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			f := newFake(func(string, int) behavior { return bOK })
			col := newCollector()
			s, err := scheduler.New(scheduler.Config{
				Servers: []scheduler.Server{fakeServer("a", 0, 3, f)}, Articles: syntheticArticles(300), OnResult: col.add,
			})
			assert.NoError(t, err)
			assert.NoError(t, s.Run(context.Background()))
			assert.Equal(t, 300, col.n())
		})
	}
	wg.Wait()
}

func TestBackoffGrowsWithTheAttemptNumber(t *testing.T) {
	f := newFake(func(string, int) behavior { return bFlaky })
	var mu sync.Mutex
	var seen []int
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("a", 0, 1, f)},
		Articles: syntheticArticles(1),
		Backoff: func(attempt int) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, attempt)
			return 0
		},
	})
	require.NoError(t, err)
	require.NoError(t, s.Run(context.Background()))
	assert.Equal(t, []int{1, 2}, seen, "waits between the three attempts")
}

func TestPartWithoutAnnouncedCRCIsGoodNotDamaged(t *testing.T) {
	main := newFake(func(string, int) behavior { return bNoCRC })
	fill := newFake(func(string, int) behavior { return bOK })
	col := newCollector()
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("main", 0, 2, main), fakeServer("fill", 1, 2, fill)},
		Articles: syntheticArticles(5), OnResult: col.add,
	})
	require.NoError(t, err)
	require.NoError(t, s.Run(context.Background()))
	for _, a := range syntheticArticles(5) {
		r := col.get(a.MessageID)
		assert.Equal(t, scheduler.Done, r.Status)
		assert.False(t, r.Damaged)
		assert.Equal(t, "main", r.Server)
		require.NotNil(t, r.Part)
		assert.False(t, r.Part.HasExpectedCRC)
	}
	assert.Zero(t, fill.total.Load(), "no fallback for a part that announces no CRC")
	assert.Zero(t, s.Stats().Damaged)
}

func TestPartWithWrongAnnouncedCRCStillFallsBackAndIsDamagedIfAlone(t *testing.T) {
	main := newFake(func(string, int) behavior { return bCorrupt })
	fill := newFake(func(string, int) behavior { return bAbsent })
	col := newCollector()
	s, err := scheduler.New(scheduler.Config{
		Servers:  []scheduler.Server{fakeServer("main", 0, 1, main), fakeServer("fill", 1, 1, fill)},
		Articles: syntheticArticles(2), OnResult: col.add,
	})
	require.NoError(t, err)
	require.NoError(t, s.Run(context.Background()))
	for _, a := range syntheticArticles(2) {
		r := col.get(a.MessageID)
		assert.True(t, r.Damaged)
		assert.Equal(t, 1, fill.callsFor(a.MessageID))
	}
	assert.EqualValues(t, 2, s.Stats().Damaged)
}
