package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/usenet/yenc"
)

// MaxAttempts is how many times one Server is asked for one Article when it
// keeps failing with transient errors.
const MaxAttempts = 3

// maxServers is the most Servers one Scheduler supports (one bit each in the
// per-Article tried mask).
const maxServers = 64

// Fetcher fetches an Article body. *nntp.Pool satisfies it. The sink gets
// dot-unstuffed lines and the slice is only valid during the call.
type Fetcher interface {
	Body(ctx context.Context, messageID string, sink func(line []byte) error) error
}

// Server is one Usenet Server as seen by the scheduler.
type Server struct {
	// Name identifies the Server in Results, Stats and OnServerDown.
	Name string
	// Priority is the Server Priority: lower values are tried first, equal
	// values share load.
	Priority int
	// RetentionDays is how many days back the Server keeps Articles; 0
	// means unlimited.
	RetentionDays int
	// Workers is the number of concurrent requests for this Server,
	// normally its connection limit. Values below 1 mean 1.
	Workers int
	// Pool performs the requests.
	Pool Fetcher
}

// Article is one unit of work.
type Article struct {
	// FileKey and SegmentNumber define the dispatch order: ascending by
	// FileKey, then SegmentNumber. The caller chooses what FileKey means,
	// for instance the file index in the NZB.
	FileKey       int
	SegmentNumber int
	MessageID     string
	// ExpectedBytes is the encoded size announced by the NZB. It is carried
	// to the Result and not interpreted.
	ExpectedBytes int64
	// Date is the NZB file date. The zero value means unknown age, which
	// never triggers a retention skip.
	Date time.Time
}

// Status is the outcome of an Article.
type Status int

const (
	// Done means a Part was delivered. Check Result.Damaged.
	Done Status = iota + 1
	// Missing means no Server could supply the Article, not even a damaged
	// copy.
	Missing
)

func (s Status) String() string {
	switch s {
	case Done:
		return "done"
	case Missing:
		return "missing"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// Result is the final outcome of one Article.
type Result struct {
	Article Article
	Status  Status
	// Part is the decoded data: nil when Missing. When Damaged it is the
	// best copy that failed its CRC check.
	Part *yenc.Part
	// Damaged is set on a Done Result when no Server delivered a copy
	// without a CRC mismatch (a part announcing no CRC is not damaged).
	Damaged bool
	// Server names the Server that supplied Part; empty when Missing.
	Server string
	// Err is the last error seen for a Missing Article, if any.
	Err error
}

// Config configures a Scheduler.
type Config struct {
	Servers  []Server
	Articles []Article
	// OnResult receives each Article's Result exactly once. It is called
	// concurrently from worker goroutines and must be safe for concurrent
	// use. It may block to apply back-pressure.
	OnResult func(Result)
	// OnServerDown is called once per Server declared dead, with the error
	// that killed it. It may be called concurrently with OnResult.
	OnServerDown func(name string, err error)
	// Now is the clock used to age Articles against Server retention. It is
	// read once when Run starts. Defaults to time.Now.
	Now func() time.Time
	// Backoff is the wait before retry number attempt (1 for the first
	// retry) after a transient error. Defaults to 200ms times attempt.
	Backoff func(attempt int) time.Duration
}

// ServerStats counts what one Server did.
type ServerStats struct {
	Name string
	// Requests counts BODY requests, retries included.
	Requests int64
	// Bytes counts encoded bytes received, including failed fetches.
	Bytes int64
	// Down reports that the Server was declared dead.
	Down bool
}

// Stats is a snapshot of progress.
type Stats struct {
	// Done counts delivered Articles, Damaged of which have no valid copy.
	Done, Missing, Damaged int64
	// Servers is in the order of Config.Servers.
	Servers []ServerStats
}

// Article scheduling states.
const (
	statePending uint8 = iota
	stateInflight
	stateResolved
)

// art is the scheduler's private state for one Article.
type art struct {
	tried uint64 // bit per server index that is done with this Article
	level int16
	state uint8
}

// srv is the private state of one Server.
type srv struct {
	idx     int
	cfg     Server
	bit     uint64
	level   int16
	retain  time.Duration // 0 = unlimited
	workers int

	cond   *sync.Cond // signalled when this Server may have work
	cursor int32      // next index of the Article list for the lowest level
	heap   minHeap    // promoted or handed-over Articles
	dead   bool

	requests atomic.Int64
	bytes    atomic.Int64
}

// Scheduler dispatches one set of Articles. Create it with New and call Run
// once.
type Scheduler struct {
	cfg      Config
	articles []Article // sorted by (FileKey, SegmentNumber); index = identity

	mu        sync.Mutex // guards everything below except atomics
	arts      []art
	servers   []*srv
	levels    [][]*srv
	remaining int
	started   bool
	now       time.Time
	damaged   map[int32]*damagedCopy
	lastErr   map[int32]error

	done, missing, damagedN atomic.Int64
	wg                      sync.WaitGroup
}

// damagedCopy is the best CRC-failing copy of an Article seen so far.
type damagedCopy struct {
	part   yenc.Part
	server string
}

// New validates cfg and prepares a Scheduler.
func New(cfg Config) (*Scheduler, error) {
	if len(cfg.Servers) == 0 {
		return nil, errors.New("scheduler: no servers")
	}
	if len(cfg.Servers) > maxServers {
		return nil, fmt.Errorf("scheduler: %d servers, at most %d supported", len(cfg.Servers), maxServers)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(attempt int) time.Duration { return 200 * time.Millisecond * time.Duration(attempt) }
	}
	if cfg.OnResult == nil {
		cfg.OnResult = func(Result) {}
	}
	if cfg.OnServerDown == nil {
		cfg.OnServerDown = func(string, error) {}
	}

	s := &Scheduler{cfg: cfg, damaged: map[int32]*damagedCopy{}, lastErr: map[int32]error{}}
	s.articles = slices.Clone(cfg.Articles)
	slices.SortStableFunc(s.articles, func(a, b Article) int {
		if a.FileKey != b.FileKey {
			return a.FileKey - b.FileKey
		}
		return a.SegmentNumber - b.SegmentNumber
	})
	s.arts = make([]art, len(s.articles))
	s.remaining = len(s.articles)

	var prios []int
	for _, c := range cfg.Servers {
		if c.Pool == nil {
			return nil, fmt.Errorf("scheduler: server %q has no pool", c.Name)
		}
		prios = append(prios, c.Priority)
	}
	slices.Sort(prios)
	prios = slices.Compact(prios)
	s.levels = make([][]*srv, len(prios))
	for i, c := range cfg.Servers {
		sv := &srv{idx: i, cfg: c, bit: 1 << uint(i), workers: max(c.Workers, 1)}
		sv.level = int16(slices.Index(prios, c.Priority))
		if c.RetentionDays > 0 {
			sv.retain = time.Duration(c.RetentionDays) * 24 * time.Hour
		}
		sv.cond = sync.NewCond(&s.mu)
		s.servers = append(s.servers, sv)
		s.levels[sv.level] = append(s.levels[sv.level], sv)
	}
	return s, nil
}

// Run starts the workers and blocks until every Article is resolved, or ctx
// ends. It returns nil when all Articles are resolved and ctx.Err()
// otherwise. All goroutines have exited when it returns. Run may be called
// once.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("scheduler: Run called twice")
	}
	s.started = true
	s.now = s.cfg.Now()
	remaining := s.remaining
	s.mu.Unlock()
	if remaining == 0 {
		return nil
	}

	stop := context.AfterFunc(ctx, s.wakeAll)
	defer stop()
	for _, sv := range s.servers {
		for range sv.workers {
			s.wg.Add(1)
			go s.worker(ctx, sv)
		}
	}
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remaining == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("scheduler: stalled with Articles unresolved")
}

func (s *Scheduler) wakeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.servers {
		sv.cond.Broadcast()
	}
}

// Stats returns a snapshot of the counters. It is safe to call at any time.
func (s *Scheduler) Stats() Stats {
	st := Stats{Done: s.done.Load(), Missing: s.missing.Load(), Damaged: s.damagedN.Load()}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.servers {
		st.Servers = append(st.Servers, ServerStats{
			Name: sv.cfg.Name, Requests: sv.requests.Load(), Bytes: sv.bytes.Load(), Down: sv.dead,
		})
	}
	return st
}

// worker takes and processes Articles for one Server until the run ends.
func (s *Scheduler) worker(ctx context.Context, sv *srv) {
	defer s.wg.Done()
	var out []Result
	for {
		s.mu.Lock()
		if ctx.Err() != nil || s.remaining == 0 || sv.dead {
			s.mu.Unlock()
			return
		}
		i, ok := s.next(sv, &out)
		if !ok && len(out) == 0 {
			sv.cond.Wait()
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		s.emit(out)
		out = out[:0]
		if ok {
			s.process(ctx, sv, i)
		}
	}
}

func (s *Scheduler) emit(out []Result) {
	for _, r := range out {
		s.cfg.OnResult(r)
	}
}

// skipsByRetention reports whether sv's retention is shorter than Article
// i's age. The caller holds s.mu or s.now is not changing (it is set once).
func (s *Scheduler) skipsByRetention(sv *srv, i int32) bool {
	if sv.retain == 0 {
		return false
	}
	d := s.articles[i].Date
	if d.IsZero() {
		return false
	}
	return s.now.Sub(d) > sv.retain
}

// next returns the next Article sv should fetch and marks it in flight.
// Articles resolved on the way (retention skips that exhaust the last level)
// are appended to out. The caller holds s.mu.
func (s *Scheduler) next(sv *srv, out *[]Result) (int32, bool) {
	n := int32(len(s.articles))
	for {
		var i int32
		switch {
		case sv.cursor < n && (len(sv.heap) == 0 || sv.cursor < sv.heap[0]):
			i = sv.cursor
			sv.cursor++
		case len(sv.heap) > 0:
			i = sv.heap.pop()
		default:
			return 0, false
		}
		a := &s.arts[i]
		if a.state != statePending || a.level != sv.level || a.tried&sv.bit != 0 {
			continue
		}
		if s.skipsByRetention(sv, i) {
			a.tried |= sv.bit
			s.settle(i, true, out)
			continue
		}
		a.state = stateInflight
		return i, true
	}
}

// settle decides where a pending Article goes next: to the untried Servers of
// its level, to the next level, or to its final result. With push false it
// only promotes or resolves when no Server of the level can still take it
// (the Article is then known to be in every candidate's queue already). The
// caller holds s.mu and the Article is not in flight.
func (s *Scheduler) settle(i int32, push bool, out *[]Result) {
	a := &s.arts[i]
	for {
		var cands []*srv
		for _, sv := range s.levels[a.level] {
			if sv.dead || a.tried&sv.bit != 0 || s.skipsByRetention(sv, i) {
				continue
			}
			cands = append(cands, sv)
		}
		if len(cands) > 0 {
			a.state = statePending
			if push {
				for _, sv := range cands {
					sv.heap.push(i)
					sv.cond.Signal()
				}
			}
			return
		}
		if int(a.level)+1 < len(s.levels) {
			a.level++
			push = true // the new level's queues have never seen it
			continue
		}
		s.resolveMissing(i, out)
		return
	}
}

// resolveMissing finishes an Article no Server can supply. The caller holds
// s.mu.
func (s *Scheduler) resolveMissing(i int32, out *[]Result) {
	a := &s.arts[i]
	a.state = stateResolved
	r := Result{Article: s.articles[i], Err: s.lastErr[i]}
	delete(s.lastErr, i)
	if d := s.damaged[i]; d != nil {
		delete(s.damaged, i)
		part := d.part
		r.Status, r.Part, r.Damaged, r.Server = Done, &part, true, d.server
		s.done.Add(1)
		s.damagedN.Add(1)
	} else {
		r.Status = Missing
		s.missing.Add(1)
	}
	*out = append(*out, r)
	s.resolved()
}

// resolved is called when an Article reaches its final state. The caller
// holds s.mu.
func (s *Scheduler) resolved() {
	s.remaining--
	if s.remaining == 0 {
		for _, sv := range s.servers {
			sv.cond.Broadcast()
		}
	}
}
