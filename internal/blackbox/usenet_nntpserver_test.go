package blackbox_test

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// nntpServer is a scriptable NNTP server that speaks TLS on a loopback port,
// for the daemon under test to fetch from. The in-memory nntptest.Server of
// the Go tests cannot be used: the daemon is a separate process.
//
// Scripting and observing may happen while connections are open.
type nntpServer struct {
	t  *testing.T
	ln net.Listener

	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]struct{}
	articles map[string][]string
	missing  map[string]bool
	corrupt  map[string]bool
	drops    map[string]*dropSpec
	latency  time.Duration

	// Behaviour of the connection set-up.
	user, pass string // required credentials; user "" means none
	greeting   string // replaces "200 ready"
	silent     bool   // accept and say nothing
	closeNow   bool   // close right after the TLS handshake
	maxConns   int
	dateReply  string // replaces the 111 answer to DATE
	authReply  string // replaces the answer to AUTHINFO USER

	// Observations.
	requests  map[string]int
	completed map[string]int
	total     int
	dials     int
	auths     int // AUTHINFO PASS commands received
	open      int
	peak      int
	commands  []string
}

type dropSpec struct{ after, remaining int }

type nntpOption func(*nntpConfig)

type nntpConfig struct {
	cert      *tls.Certificate
	plaintext bool
}

// withCert serves this certificate.
func withCert(c tls.Certificate) nntpOption { return func(o *nntpConfig) { o.cert = &c } }

// withPlaintext makes the server speak NNTP without TLS.
func withPlaintext() nntpOption { return func(o *nntpConfig) { o.plaintext = true } }

// startNNTP starts a server whose certificate is valid for 127.0.0.1 and
// localhost and signed by ca.
func startNNTP(t *testing.T, ca *testCA, opts ...nntpOption) *nntpServer {
	t.Helper()
	var cfg nntpConfig
	for _, f := range opts {
		f(&cfg)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &nntpServer{
		t: t, ln: ln, conns: map[net.Conn]struct{}{},
		articles: map[string][]string{}, missing: map[string]bool{}, corrupt: map[string]bool{},
		drops: map[string]*dropSpec{}, requests: map[string]int{}, completed: map[string]int{},
	}
	var tc *tls.Config
	if !cfg.plaintext {
		cert := ca.issue([]string{"127.0.0.1", "localhost"})
		if cfg.cert != nil {
			cert = *cfg.cert
		}
		tc = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, tc)
		}
	}()
	t.Cleanup(s.close)
	return s
}

func (s *nntpServer) close() {
	s.mu.Lock()
	s.closed = true
	conns := slices.Collect(func(yield func(net.Conn) bool) {
		for c := range s.conns {
			if !yield(c) {
				return
			}
		}
	})
	s.mu.Unlock()
	_ = s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Port is the TCP port the server listens on.
func (s *nntpServer) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Host is the address the daemon is configured to dial; the certificate is
// valid for it.
func (s *nntpServer) Host() string { return "127.0.0.1" }

// withLock runs f with the lock held.
func (s *nntpServer) withLock(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// Install makes the server serve every article of rel.
func (s *nntpServer) Install(rel release) *nntpServer {
	s.withLock(func() {
		for id, lines := range rel.articles {
			s.articles[id] = lines
		}
	})
	return s
}

// Missing makes the server answer 430 for ids.
func (s *nntpServer) Missing(ids ...string) *nntpServer {
	s.withLock(func() {
		for _, id := range ids {
			s.missing[id] = true
		}
	})
	return s
}

// Corrupt serves ids with one data character changed, so the yEnc CRC32 no
// longer matches while the part still decodes to its declared length.
func (s *nntpServer) Corrupt(ids ...string) *nntpServer {
	s.withLock(func() {
		for _, id := range ids {
			s.corrupt[id] = true
		}
	})
	return s
}

// SetLatency delays every BODY answer.
func (s *nntpServer) SetLatency(d time.Duration) *nntpServer {
	s.withLock(func() { s.latency = d })
	return s
}

// Drop closes the connection after afterLines lines of the body of id, the
// next times requests of it (0 means every time).
func (s *nntpServer) Drop(id string, afterLines, times int) *nntpServer {
	s.withLock(func() {
		d := &dropSpec{after: afterLines, remaining: times}
		if times <= 0 {
			d.remaining = -1
		}
		s.drops[id] = d
	})
	return s
}

// Credentials makes the server demand AUTHINFO with this user and password.
func (s *nntpServer) Credentials(user, pass string) *nntpServer {
	s.withLock(func() { s.user, s.pass = user, pass })
	return s
}

// Greeting replaces the 200 greeting line, for instance with a refusal.
func (s *nntpServer) Greeting(line string) *nntpServer {
	s.withLock(func() { s.greeting = line })
	return s
}

// Silent makes the server accept connections and never say anything.
func (s *nntpServer) Silent() *nntpServer {
	s.withLock(func() { s.silent = true })
	return s
}

// CloseAtOnce makes the server hang up as soon as the connection is made.
func (s *nntpServer) CloseAtOnce() *nntpServer {
	s.withLock(func() { s.closeNow = true })
	return s
}

// MaxConnections refuses connections beyond n with a 502 greeting.
func (s *nntpServer) MaxConnections(n int) *nntpServer {
	s.withLock(func() { s.maxConns = n })
	return s
}

// DateReply replaces the answer to DATE.
func (s *nntpServer) DateReply(line string) *nntpServer {
	s.withLock(func() { s.dateReply = line })
	return s
}

// AuthReply replaces the answer to AUTHINFO USER.
func (s *nntpServer) AuthReply(line string) *nntpServer {
	s.withLock(func() { s.authReply = line })
	return s
}

// Requests is how many BODY commands asked for id.
func (s *nntpServer) Requests(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[id]
}

// Completed is how many times the whole body of id was sent.
func (s *nntpServer) Completed(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completed[id]
}

// Total is how many BODY commands the server has received.
func (s *nntpServer) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// Dials is how many connections were accepted.
func (s *nntpServer) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// Auths is how many AUTHINFO PASS commands arrived.
func (s *nntpServer) Auths() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auths
}

// Peak is the highest number of connections that were open at once.
func (s *nntpServer) Peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// Commands lists the commands received, in order, across connections.
func (s *nntpServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// Asked lists the message IDs that were asked for at least once.
func (s *nntpServer) Asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.requests))
	for id := range s.requests {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *nntpServer) serve(raw net.Conn, tc *tls.Config) {
	defer func() { _ = raw.Close() }()
	var c net.Conn = raw
	if tc != nil {
		tconn := tls.Server(raw, tc)
		_ = tconn.SetDeadline(time.Now().Add(30 * time.Second))
		if err := tconn.Handshake(); err != nil {
			return
		}
		_ = tconn.SetDeadline(time.Time{})
		c = tconn
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.dials++
	s.conns[raw] = struct{}{}
	refuse := s.maxConns > 0 && s.open >= s.maxConns
	if !refuse {
		s.open++
		s.peak = max(s.peak, s.open)
	}
	silent, closeNow, greeting := s.silent, s.closeNow, s.greeting
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, raw)
		if !refuse {
			s.open--
		}
		s.mu.Unlock()
	}()

	switch {
	case closeNow:
		return
	case silent:
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	case refuse:
		_, _ = fmt.Fprint(c, "502 Too many connections\r\n")
		return
	}
	if greeting == "" {
		greeting = "200 bitnews test server ready"
	}
	_, _ = fmt.Fprint(c, greeting+"\r\n")
	if strings.HasPrefix(greeting, "4") || strings.HasPrefix(greeting, "5") {
		return
	}

	br := bufio.NewReaderSize(c, 4096)
	authed := s.user == ""
	var gotUser string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.withLock(func() { s.commands = append(s.commands, line) })
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "AUTHINFO":
			sub, val, _ := strings.Cut(arg, " ")
			s.mu.Lock()
			user, pass, reply := s.user, s.pass, s.authReply
			s.mu.Unlock()
			switch strings.ToUpper(sub) {
			case "USER":
				gotUser = val
				switch {
				case reply != "":
					_, _ = fmt.Fprint(c, reply+"\r\n")
				case user == "":
					_, _ = fmt.Fprint(c, "281 no authentication needed\r\n")
				default:
					_, _ = fmt.Fprint(c, "381 password required\r\n")
				}
			case "PASS":
				s.withLock(func() { s.auths++ })
				if gotUser == user && val == pass {
					authed = true
					_, _ = fmt.Fprint(c, "281 authentication accepted\r\n")
				} else {
					_, _ = fmt.Fprint(c, "481 Authentication failed\r\n")
					return
				}
			default:
				_, _ = fmt.Fprint(c, "501 syntax error\r\n")
			}
		case "DATE":
			s.mu.Lock()
			reply := s.dateReply
			s.mu.Unlock()
			if reply == "" {
				reply = "111 " + time.Now().UTC().Format("20060102150405")
			}
			_, _ = fmt.Fprint(c, reply+"\r\n")
		case "BODY":
			if !authed {
				_, _ = fmt.Fprint(c, "480 authentication required\r\n")
				continue
			}
			if !s.body(c, strings.Trim(arg, "<>")) {
				return
			}
		case "QUIT":
			_, _ = fmt.Fprint(c, "205 bye\r\n")
			return
		default:
			_, _ = fmt.Fprint(c, "500 unknown command\r\n")
		}
	}
}

// body answers one BODY command. It reports false when the connection must
// be closed.
func (s *nntpServer) body(c net.Conn, id string) bool {
	s.mu.Lock()
	s.total++
	s.requests[id]++
	lines, ok := s.articles[id]
	ok = ok && !s.missing[id]
	if ok && s.corrupt[id] {
		lines = corruptBody(lines)
	}
	latency := s.latency
	cut := -1
	if d := s.drops[id]; d != nil && d.remaining != 0 {
		cut = d.after
		if d.remaining > 0 {
			d.remaining--
		}
	}
	s.mu.Unlock()

	if latency > 0 {
		time.Sleep(latency)
	}
	if !ok {
		_, err := fmt.Fprint(c, "430 No such article\r\n")
		return err == nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "222 0 <%s> body follows\r\n", id)
	for i, l := range lines {
		if i == cut {
			_, _ = fmt.Fprint(c, b.String())
			return false
		}
		if strings.HasPrefix(l, ".") {
			b.WriteByte('.')
		}
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	if cut >= 0 {
		_, _ = fmt.Fprint(c, b.String())
		return false
	}
	b.WriteString(".\r\n")
	if _, err := fmt.Fprint(c, b.String()); err != nil {
		return false
	}
	s.withLock(func() { s.completed[id]++ })
	return true
}

// corruptBody returns a copy of a body in which one data character was
// replaced by another plain one, so the CRC32 no longer matches and the
// decoded length stays the same.
func corruptBody(lines []string) []string {
	out := slices.Clone(lines)
	for i, l := range out {
		if strings.HasPrefix(l, "=y") {
			continue
		}
		for j := range len(l) {
			if l[j] == '=' || (j > 0 && l[j-1] == '=') {
				continue
			}
			for bit := range 8 {
				r := l[j] ^ (1 << bit)
				switch r {
				case 0, '\n', '\r', '=', ' ', '\t', '.':
					continue
				}
				out[i] = l[:j] + string([]byte{r}) + l[j+1:]
				return out
			}
		}
	}
	return out
}
