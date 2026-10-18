package nntptest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
)

// fetch returns the body lines of id, copied out of the sink.
func fetch(t *testing.T, c nntp.ArticleConn, id string) ([]string, error) {
	t.Helper()
	var lines []string
	err := c.Body(context.Background(), id, func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	})
	return lines, err
}

func dial(t *testing.T, s *nntptest.Server) nntp.ArticleConn {
	t.Helper()
	c, err := s.Factory()(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestServerServesScriptedArticleLines(t *testing.T) {
	s := nntptest.NewServer("main")
	s.AddArticle("a@x", []string{"=ybegin line=128 size=0 name=f", "data", "=yend size=0"})

	lines, err := fetch(t, dial(t, s), "a@x")

	require.NoError(t, err)
	assert.Equal(t, []string{"=ybegin line=128 size=0 name=f", "data", "=yend size=0"}, lines)
}

func TestUnknownArticleIsNotFoundAndKeepsConnAlive(t *testing.T) {
	s := nntptest.NewServer("main")
	c := dial(t, s)

	_, err := fetch(t, c, "nope@x")

	assert.ErrorIs(t, err, nntp.ErrArticleNotFound)
	assert.False(t, c.Dead())
	var ne *nntp.Error
	require.True(t, errors.As(err, &ne))
	assert.Equal(t, 430, ne.Code)
}

func TestMissingArticlesAnswer430EvenWhenStored(t *testing.T) {
	s := nntptest.NewServer("main")
	s.AddArticle("a@x", []string{"x"})
	s.AddArticle("b@x", []string{"y"})
	s.Missing("a@x")
	c := dial(t, s)

	_, errA := fetch(t, c, "a@x")
	linesB, errB := fetch(t, c, "b@x")

	assert.ErrorIs(t, errA, nntp.ErrArticleNotFound)
	require.NoError(t, errB)
	assert.Equal(t, []string{"y"}, linesB)
}

func TestCountsRequestsPerMessageIDAndPerConnection(t *testing.T) {
	s := nntptest.NewServer("fill")
	s.AddArticle("a@x", []string{"x"})
	c1, c2 := dial(t, s), dial(t, s)

	_, _ = fetch(t, c1, "a@x")
	_, _ = fetch(t, c1, "a@x")
	_, _ = fetch(t, c2, "gone@x")

	assert.Equal(t, 2, s.Requests("a@x"))
	assert.Equal(t, 1, s.Requests("gone@x"))
	assert.Equal(t, 0, s.Requests("never@x"))
	assert.Equal(t, 3, s.TotalRequests())
	assert.ElementsMatch(t, []string{"a@x", "gone@x"}, s.RequestedIDs())
	assert.Equal(t, []int{2, 1}, s.ConnRequests())
	assert.Equal(t, 2, s.Dials())
}

func TestSinkErrorIsReturnedAndKillsConn(t *testing.T) {
	s := nntptest.NewServer("main")
	s.AddArticle("a@x", []string{"1", "2", "3"})
	c := dial(t, s)
	boom := errors.New("boom")
	n := 0

	err := c.Body(context.Background(), "a@x", func([]byte) error {
		n++
		return boom
	})

	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, nntp.ErrConnection)
	assert.Equal(t, 1, n)
	assert.True(t, c.Dead())
}

func TestSinkLineIsOnlyValidDuringTheCall(t *testing.T) {
	// Like the real client, the harness reuses its buffer, so a sink that
	// keeps the slice sees it overwritten.
	s := nntptest.NewServer("main")
	s.AddArticle("a@x", []string{"first", "other"})
	c := dial(t, s)
	var kept []byte

	require.NoError(t, c.Body(context.Background(), "a@x", func(l []byte) error {
		if kept == nil {
			kept = l
		}
		return nil
	}))

	assert.NotEqual(t, "first", string(kept))
}

func TestDeadConnRefusesCommands(t *testing.T) {
	s := nntptest.NewServer("main")
	c := dial(t, s)
	require.NoError(t, c.Close())

	_, err := fetch(t, c, "a@x")
	_, derr := c.Date(context.Background())

	assert.ErrorIs(t, err, nntp.ErrConnection)
	assert.ErrorIs(t, derr, nntp.ErrConnection)
	assert.Equal(t, 0, s.Requests("a@x"))
	require.NoError(t, c.Close(), "Close is idempotent")
}

func TestCancelledContextFailsBeforeCounting(t *testing.T) {
	s := nntptest.NewServer("main")
	s.AddArticle("a@x", []string{"x"})
	c := dial(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Body(ctx, "a@x", func([]byte) error { return nil })

	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, c.Dead())
	assert.Equal(t, 0, s.Requests("a@x"))
}

func TestInvalidMessageIDIsRejectedLikeTheRealClient(t *testing.T) {
	s := nntptest.NewServer("main")
	c := dial(t, s)

	_, err := fetch(t, c, "bad id>")

	assert.ErrorIs(t, err, nntp.ErrInvalidMessageID)
	assert.Equal(t, 0, s.TotalRequests())
}

func TestLatencyDelaysBodyAndDate(t *testing.T) {
	s := nntptest.NewServer("slow")
	s.AddArticle("a@x", []string{"x"})
	s.SetLatency(40 * time.Millisecond)
	c := dial(t, s)

	start := time.Now()
	_, err := fetch(t, c, "a@x")
	require.NoError(t, err)
	_, err = c.Date(context.Background())
	require.NoError(t, err)

	assert.GreaterOrEqual(t, time.Since(start), 80*time.Millisecond)
}

func TestLatencyHonorsContext(t *testing.T) {
	s := nntptest.NewServer("slow")
	s.AddArticle("a@x", []string{"x"})
	s.SetLatency(time.Hour)
	c := dial(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Body(ctx, "a@x", func([]byte) error { return nil })

	assert.Less(t, time.Since(start), 5*time.Second)
	assert.ErrorIs(t, err, nntp.ErrConnection)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, c.Dead(), "an aborted command leaves the real client dead")
}

func TestDisconnectCutsBodyAfterNLines(t *testing.T) {
	s := nntptest.NewServer("flaky")
	s.AddArticle("a@x", []string{"1", "2", "3", "4"})
	s.Disconnect("a@x", 2, 0)
	c := dial(t, s)

	lines, err := fetch(t, c, "a@x")

	assert.ErrorIs(t, err, nntp.ErrConnection)
	assert.Equal(t, []string{"1", "2"}, lines)
	assert.True(t, c.Dead())
}

func TestDisconnectTimesLimitsHowOftenItHappens(t *testing.T) {
	s := nntptest.NewServer("flaky")
	s.AddArticle("a@x", []string{"1", "2", "3"})
	s.Disconnect("a@x", 1, 2)

	for range 2 {
		_, err := fetch(t, dial(t, s), "a@x")
		assert.ErrorIs(t, err, nntp.ErrConnection)
	}
	lines, err := fetch(t, dial(t, s), "a@x")

	require.NoError(t, err)
	assert.Equal(t, []string{"1", "2", "3"}, lines)
}

func TestAuthFailureRejectsEveryDialAndCountsAttempts(t *testing.T) {
	s := nntptest.NewServer("main")
	s.FailAuth(true)

	for range 2 {
		c, err := s.Factory()(context.Background())
		assert.Nil(t, c)
		assert.ErrorIs(t, err, nntp.ErrAuth)
		assert.NotErrorIs(t, err, nntp.ErrConnection)
	}

	assert.Equal(t, 2, s.AuthAttempts())
	assert.Equal(t, 0, s.OpenConns())

	s.FailAuth(false)
	_ = dial(t, s)
	assert.Equal(t, 3, s.AuthAttempts())
}

func TestMaxConnectionsRefusesLikeA502UntilOneCloses(t *testing.T) {
	s := nntptest.NewServer("main")
	s.SetMaxConnections(2)
	c1, _ := dial(t, s), dial(t, s)

	c, err := s.Factory()(context.Background())
	assert.Nil(t, c)
	assert.ErrorIs(t, err, nntp.ErrConnection)
	var ne *nntp.Error
	require.True(t, errors.As(err, &ne))
	assert.Equal(t, 502, ne.Code)
	assert.Equal(t, 2, s.OpenConns())

	require.NoError(t, c1.Close())
	require.NoError(t, c1.Close(), "closing twice frees one slot only")
	assert.Equal(t, 1, s.OpenConns())
	_ = dial(t, s)
	assert.Equal(t, 2, s.OpenConns())
	assert.Equal(t, 2, s.PeakConns())
}

func TestConnectionThatDiedFreesItsSlot(t *testing.T) {
	s := nntptest.NewServer("main")
	s.SetMaxConnections(1)
	s.AddArticle("a@x", []string{"1", "2"})
	s.Disconnect("a@x", 1, 0)
	c := dial(t, s)

	_, err := fetch(t, c, "a@x")

	require.ErrorIs(t, err, nntp.ErrConnection)
	assert.Equal(t, 0, s.OpenConns())
	_ = dial(t, s)
}

func TestFailNextDialsRefusesThatManyConnects(t *testing.T) {
	s := nntptest.NewServer("main")
	s.FailNextDials(2)

	for range 2 {
		_, err := s.Factory()(context.Background())
		assert.ErrorIs(t, err, nntp.ErrConnection)
	}
	_ = dial(t, s)

	assert.Equal(t, 3, s.Dials())
}

func TestRetentionDaysIsPlainMetadata(t *testing.T) {
	s := nntptest.NewServer("main")
	assert.Equal(t, 0, s.RetentionDays())

	s.SetRetentionDays(30)

	assert.Equal(t, 30, s.RetentionDays())
}
