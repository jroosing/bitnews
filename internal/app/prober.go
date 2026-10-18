package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nntp"
)

// ProbeClass says which step of a connection test failed.
type ProbeClass string

// The classes of a connection test result. ProbeTLS covers certificate
// problems too: ProbeResult.Detail tells them apart ("certificate_*").
const (
	ProbeOK       ProbeClass = "ok"
	ProbeDNS      ProbeClass = "dns"
	ProbeTCP      ProbeClass = "tcp"
	ProbeTLS      ProbeClass = "tls"
	ProbeAuth     ProbeClass = "auth"
	ProbeProtocol ProbeClass = "protocol"
)

// ProbeResult is the outcome of a connection test. It never contains
// credentials.
type ProbeResult struct {
	OK    bool
	Class ProbeClass
	// Detail is a stable machine-readable refinement of Class:
	//
	//	dns:      not_found, timeout, other
	//	tcp:      refused, timeout, unreachable, other
	//	tls:      certificate_untrusted, certificate_hostname,
	//	          certificate_expired, certificate_invalid, not_tls,
	//	          timeout, handshake
	//	auth:     rejected
	//	protocol: greeting, auth_response, date, other
	//
	// It is empty for ProbeOK.
	Detail string
	// Message is a short human-readable explanation.
	Message string
	// ConnectTime is how long dial, TLS handshake, greeting and
	// authentication took. Latency is the round trip of the DATE command.
	// Both are zero unless the connection worked.
	ConnectTime time.Duration
	Latency     time.Duration
}

// Prober tests a connection to a Server: connect, authenticate, run DATE.
type Prober interface {
	// Probe never fails: a problem is the returned result.
	Probe(ctx context.Context, s core.Server) ProbeResult
}

// probeConn is the part of an *nntp.Conn a test uses.
type probeConn interface {
	Date(ctx context.Context) (time.Time, error)
	Close() error
}

// probeTimeout bounds a whole connection test; the connect and read
// timeouts of the client are shorter.
const (
	probeTimeout        = 20 * time.Second
	probeConnectTimeout = 10 * time.Second
	probeReadTimeout    = 10 * time.Second
)

// NNTPProber is the Prober that dials with the nntp client: strict TLS, no
// way around it.
type NNTPProber struct {
	dial func(ctx context.Context, cfg nntp.Config) (probeConn, error)
	now  func() time.Time
}

// NewNNTPProber returns the Prober used by the daemon.
func NewNNTPProber() *NNTPProber {
	return &NNTPProber{
		dial: func(ctx context.Context, cfg nntp.Config) (probeConn, error) { return nntp.Dial(ctx, cfg) },
		now:  time.Now,
	}
}

// Probe implements Prober.
func (p *NNTPProber) Probe(ctx context.Context, s core.Server) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cfg := nntp.Config{
		Host: s.Host, Port: s.Port, User: s.Username, Pass: s.Password.Reveal(),
		ConnectTimeout: probeConnectTimeout, ReadTimeout: probeReadTimeout,
	}
	secrets := []string{s.Username, s.Password.Reveal()}

	start := p.now()
	conn, err := p.dial(ctx, cfg)
	if err != nil {
		return classifyProbeError(err, secrets)
	}
	defer func() { _ = conn.Close() }()
	connected := p.now()
	if _, err := conn.Date(ctx); err != nil {
		return classifyProbeError(err, secrets)
	}
	return ProbeResult{
		OK: true, Class: ProbeOK, Message: "connected and authenticated",
		ConnectTime: connected.Sub(start), Latency: p.now().Sub(connected),
	}
}

func fail(class ProbeClass, detail, message string) ProbeResult {
	return ProbeResult{Class: class, Detail: detail, Message: message}
}

// classifyProbeError maps a dial or DATE failure to a result. secrets are
// scrubbed from any server text it quotes.
func classifyProbeError(err error, secrets []string) ProbeResult {
	e, ok := errors.AsType[*nntp.Error](err)
	if !ok {
		if isTimeout(err) {
			return fail(ProbeTCP, "timeout", "the connection timed out")
		}
		return fail(ProbeProtocol, "other", "the connection test failed")
	}
	switch e.Op {
	case "dial":
		return classifyDial(e.Err)
	case "tls":
		return classifyTLS(e.Err)
	}
	if e.Kind == nntp.ErrAuth {
		return fail(ProbeAuth, "rejected", fmt.Sprintf("the server rejected the user name or password (%d)", e.Code))
	}
	detail := e.Op
	switch e.Op {
	case "greeting", "date":
	case "auth":
		detail = "auth_response"
	default:
		detail = "other"
	}
	msg := "unexpected response from the server"
	if e.Code != 0 {
		msg += fmt.Sprintf(" (%d", e.Code)
		if text := scrub(e.Text, secrets); text != "" {
			msg += " " + text
		}
		msg += ")"
	} else if isTimeout(e.Err) {
		msg = "the server did not answer in time"
	}
	return fail(ProbeProtocol, detail, msg)
}

func classifyDial(err error) ProbeResult {
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		switch {
		case dnsErr.IsNotFound:
			return fail(ProbeDNS, "not_found", "the host name does not exist")
		case dnsErr.IsTimeout:
			return fail(ProbeDNS, "timeout", "the host name lookup timed out")
		}
		return fail(ProbeDNS, "other", "the host name could not be resolved")
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return fail(ProbeTCP, "refused", "the connection was refused: check the host and port")
	case isTimeout(err):
		return fail(ProbeTCP, "timeout", "the connection timed out")
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return fail(ProbeTCP, "unreachable", "the host cannot be reached")
	}
	return fail(ProbeTCP, "other", "could not connect to the server")
}

func classifyTLS(err error) ProbeResult {
	if cve, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		inner := cve.Unwrap()
		switch {
		case isType[x509.UnknownAuthorityError](inner):
			return fail(ProbeTLS, "certificate_untrusted", "the server's certificate is not trusted")
		case isType[x509.HostnameError](inner):
			return fail(ProbeTLS, "certificate_hostname", "the server's certificate is not valid for this host name")
		}
		if inv, ok := errors.AsType[x509.CertificateInvalidError](inner); ok && inv.Reason == x509.Expired {
			return fail(ProbeTLS, "certificate_expired", "the server's certificate has expired or is not valid yet")
		}
		return fail(ProbeTLS, "certificate_invalid", "the server's certificate is not valid")
	}
	switch {
	case isType[tls.RecordHeaderError](err):
		return fail(ProbeTLS, "not_tls", "the server does not speak TLS on this port: bitnews only connects over TLS")
	case isTimeout(err):
		return fail(ProbeTLS, "timeout", "the TLS handshake timed out")
	}
	return fail(ProbeTLS, "handshake", "the TLS handshake failed")
}

func isType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

func scrub(text string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			text = strings.ReplaceAll(text, sec, "***")
		}
	}
	return text
}
