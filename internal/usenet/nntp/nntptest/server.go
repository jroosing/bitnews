package nntptest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
)

// Server is a scripted in-memory Server. All methods are safe for concurrent
// use, and scripting may change while connections are open.
type Server struct {
	name string

	mu       sync.Mutex
	articles map[string][]string
	missing  map[string]bool
	corrupt  map[string]bool

	retentionDays int
	failAuth      bool
	maxConns      int // 0 means unlimited
	failDials     int
	open, peak    int
	authAttempts  int
	latency       time.Duration
	disconnects   map[string]*disconnect

	requests map[string]int
	total    int
	connReqs []int // by connection number, in dial order
	dials    int
}

// NewServer returns an empty Server. The name only shows up in error text.
func NewServer(name string) *Server {
	return &Server{
		name:        name,
		articles:    map[string][]string{},
		missing:     map[string]bool{},
		corrupt:     map[string]bool{},
		disconnects: map[string]*disconnect{},
		requests:    map[string]int{},
	}
}

// AddArticle stores the body lines of the Article with the given message ID
// (no angle brackets). The lines are dot-unstuffed and have no line
// terminator, exactly as nntp.Conn.Body hands them to its sink.
func (s *Server) AddArticle(id string, lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.articles[id] = lines
}

// Missing makes the Server answer 430 for the given message IDs, whether or
// not they were added.
func (s *Server) Missing(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.missing[id] = true
	}
}

// Corrupt makes the Server serve the given Articles with one data byte
// changed, so that the CRC32 check of the yEnc decoder fails while the part
// still decodes to its declared length. The stored lines are not modified.
func (s *Server) Corrupt(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.corrupt[id] = true
	}
}

// SetLatency delays every BODY and DATE answer by d. The wait ends early
// when the command's context is done.
func (s *Server) SetLatency(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latency = d
}

// disconnect scripts a connection drop in the middle of a body.
type disconnect struct {
	after     int // lines delivered before the drop
	remaining int // drops left; negative means every time
}

// Disconnect makes the connection drop after afterLines lines of the body of
// id were delivered. The command then fails with nntp.ErrConnection and the
// connection is dead. It happens for the next times requests of id, or for
// every request when times is 0.
func (s *Server) Disconnect(id string, afterLines, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &disconnect{after: afterLines, remaining: times}
	if times <= 0 {
		d.remaining = -1
	}
	s.disconnects[id] = d
}

// Requests is how many BODY commands asked for id, whatever the answer.
func (s *Server) Requests(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[id]
}

// TotalRequests is how many BODY commands the Server has received.
func (s *Server) TotalRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// RequestedIDs lists every message ID that was asked for at least once.
func (s *Server) RequestedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.requests))
	for id := range s.requests {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// ConnRequests lists the BODY commands received per connection, in the order
// the connections were opened.
func (s *Server) ConnRequests() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.connReqs)
}

// Dials is how many connections have been opened, including refused ones.
func (s *Server) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// FailAuth makes every new connection fail authentication (nntp.ErrAuth, as
// a 481 does) until it is called again with false.
func (s *Server) FailAuth(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAuth = fail
}

// AuthAttempts is how many connections got as far as authenticating.
// Connections refused earlier (see SetMaxConnections, FailNextDials) do not
// count.
func (s *Server) AuthAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authAttempts
}

// SetMaxConnections limits concurrently open connections. A connect beyond
// the limit fails with nntp.ErrConnection and code 502, as providers do. Zero
// means no limit.
func (s *Server) SetMaxConnections(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxConns = n
}

// OpenConns is the number of connections that are open right now. A
// connection that died or was closed no longer counts.
func (s *Server) OpenConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// PeakConns is the highest OpenConns has been.
func (s *Server) PeakConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// FailNextDials makes the next n connects fail with nntp.ErrConnection, as
// if the host were unreachable.
func (s *Server) FailNextDials(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failDials = n
}

// SetRetentionDays records the Server's retention (0 is unlimited). The
// Server does not enforce it; scheduler tests read it back to build their
// Server configuration.
func (s *Server) SetRetentionDays(days int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retentionDays = days
}

// RetentionDays returns what SetRetentionDays stored.
func (s *Server) RetentionDays() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retentionDays
}

// Factory returns an nntp.ConnFactory whose connections talk to s. It
// behaves like nntp.Dial against a Server with the scripted behavior.
func (s *Server) Factory() nntp.ConnFactory {
	return func(ctx context.Context) (nntp.ArticleConn, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.dials++
		if err := ctx.Err(); err != nil {
			return nil, &nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: err}
		}
		if s.failDials > 0 {
			s.failDials--
			return nil, &nntp.Error{Kind: nntp.ErrConnection, Op: "dial", Err: errors.New("connection refused")}
		}
		if s.maxConns > 0 && s.open >= s.maxConns {
			return nil, &nntp.Error{Kind: nntp.ErrConnection, Op: "greeting", Code: 502, Text: "Too many connections"}
		}
		s.authAttempts++
		if s.failAuth {
			return nil, &nntp.Error{Kind: nntp.ErrAuth, Op: "auth", Code: 481, Text: "Authentication failed"}
		}
		s.open++
		s.peak = max(s.peak, s.open)
		s.connReqs = append(s.connReqs, 0)
		return &conn{s: s, num: len(s.connReqs) - 1}, nil
	}
}

type conn struct {
	s    *Server
	num  int
	dead atomic.Bool
	free sync.Once
}

func (c *conn) Dead() bool { return c.dead.Load() }

// kill marks the connection dead and gives its slot back to the Server.
func (c *conn) kill() {
	c.dead.Store(true)
	c.free.Do(func() {
		c.s.mu.Lock()
		defer c.s.mu.Unlock()
		c.s.open--
	})
}

func (c *conn) Close() error {
	c.kill()
	return nil
}
