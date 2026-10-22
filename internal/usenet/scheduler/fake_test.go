package scheduler_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp/nntptest"
	"gitlab.com/kyura4k/gitnews/internal/usenet/scheduler"
)

// behavior is what a fakeFetcher does for one request.
type behavior int

const (
	bOK        behavior = iota // valid article
	bAbsent                    // 430
	bCorrupt                   // CRC mismatch
	bFlaky                     // connection error
	bDisabled                  // pool disabled
	bMalformed                 // not yEnc at all
	bNoCRC                     // valid article whose =yend announces no CRC
)

var (
	goodLines  = nntptest.EncodeArticles("f.bin", []byte("hello, scheduler"), 1000, 128)[0]
	badLines   = corruptTrailer(goodLines)
	noCRCLines = dropCRC(goodLines)
)

func dropCRC(lines []string) []string {
	out := append([]string(nil), lines...)
	last := out[len(out)-1]
	out[len(out)-1] = last[:strings.Index(last, " crc32=")]
	return out
}

func corruptTrailer(lines []string) []string {
	out := append([]string(nil), lines...)
	last := out[len(out)-1]
	d := last[len(last)-1]
	repl := byte('0')
	if d == '0' {
		repl = '1'
	}
	out[len(out)-1] = last[:len(last)-1] + string(repl)
	return out
}

// fakeFetcher is an in-memory Fetcher with scripted behavior and counters.
type fakeFetcher struct {
	script func(id string, call int) behavior

	mu    sync.Mutex
	calls map[string]int
	order []string
	total atomic.Int64
}

func newFake(script func(id string, call int) behavior) *fakeFetcher {
	return &fakeFetcher{script: script, calls: map[string]int{}}
}

func (f *fakeFetcher) Body(ctx context.Context, id string, sink func([]byte) error) error {
	if err := ctx.Err(); err != nil {
		return &nntp.Error{Kind: nntp.ErrConnection, Op: "body", Err: err}
	}
	f.total.Add(1)
	f.mu.Lock()
	f.calls[id]++
	n := f.calls[id]
	f.order = append(f.order, id)
	f.mu.Unlock()
	var lines []string
	switch f.script(id, n) {
	case bAbsent:
		return &nntp.Error{Kind: nntp.ErrArticleNotFound, Op: "body", Code: 430}
	case bFlaky:
		return &nntp.Error{Kind: nntp.ErrConnection, Op: "body"}
	case bDisabled:
		return fmt.Errorf("%w: test", nntp.ErrServerDisabled)
	case bMalformed:
		lines = []string{"=ybegin garbage", "no part"}
	case bCorrupt:
		lines = badLines
	case bNoCRC:
		lines = noCRCLines
	default:
		lines = goodLines
	}
	for _, l := range lines {
		if err := sink([]byte(l)); err != nil {
			return fmt.Errorf("nntp: sink: %w", err)
		}
	}
	return nil
}

func (f *fakeFetcher) callsFor(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *fakeFetcher) idsInOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func fakeServer(name string, priority, workers int, f *fakeFetcher) scheduler.Server {
	return scheduler.Server{Name: name, Priority: priority, Workers: workers, Pool: f}
}

func syntheticArticles(n int) []scheduler.Article {
	arts := make([]scheduler.Article, n)
	for i := range arts {
		arts[i] = scheduler.Article{FileKey: i / 100, SegmentNumber: i%100 + 1, MessageID: fmt.Sprintf("a%07d@x", i)}
	}
	return arts
}

func hashN(s string, salt int) int {
	h := 14695981039346656037 + uint64(salt)*1099511628211
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return int(h >> 33)
}
