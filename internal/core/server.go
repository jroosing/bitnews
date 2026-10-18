package core

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode"
)

// DefaultServerPort is the NNTP-over-TLS port (RFC 8143) that a Server gets
// when none is given.
const DefaultServerPort = 563

// Limits on Server fields.
const (
	maxServerNameBytes = 64
	maxHostBytes       = 253
	maxLabelBytes      = 63
	maxCredentialBytes = 256
	// MaxServerConnections is the largest MaxConnections a Server may have.
	MaxServerConnections = 100
)

// Server is one NNTP endpoint of a Usenet provider, with its own
// credentials, connection limit, retention and Server Priority.
//
// TLS is always on and certificates are always verified: the model has no
// field to change either, and that is on purpose.
type Server struct {
	ID ID
	// Name is a unique label shown to the user.
	Name string
	// Host is a bare host name or IP address: no scheme, port or path. It is
	// also the name the certificate must match.
	Host string
	// Port is 1 to 65535.
	Port int
	// Username is empty for a Server that needs no authentication.
	Username string
	Password Secret
	// MaxConnections is 1 to MaxServerConnections.
	MaxConnections int
	// RetentionDays is how many days back the Server keeps Articles; 0
	// means unlimited.
	RetentionDays int
	// Priority is the Server Priority: Servers with the same value share
	// load, and a higher-numbered one is used only for Articles the lower
	// ones don't have.
	Priority  int
	Enabled   bool
	CreatedAt time.Time

	// PasswordUnavailable is set by a ServerRepo when the stored password
	// could not be decrypted (the key changed or is missing). Password is
	// empty then, and the user has to enter it again. Saving the Server
	// with this flag set leaves the stored password untouched.
	PasswordUnavailable bool
}

// ErrSecretUnavailable means a stored secret could not be decrypted, so the
// user has to supply it again.
var ErrSecretUnavailable = errors.New("secret unavailable")

// ServerRepo persists Servers. Names are unique (ErrConflict).
type ServerRepo interface {
	// Create stores s, encrypting its password.
	Create(ctx context.Context, s Server) error
	// Get returns the Server. A password that cannot be decrypted does not
	// fail the call: see Server.PasswordUnavailable.
	Get(ctx context.Context, id ID) (Server, error)
	// List returns every Server ordered by Priority, then Name.
	List(ctx context.Context) ([]Server, error)
	// Update stores every field except ID and CreatedAt. When
	// s.PasswordUnavailable is set the stored password is left alone.
	Update(ctx context.Context, s Server) error
	Delete(ctx context.Context, id ID) error
}

// The errors Server.Validate returns, each wrapped with the offending value
// where that is safe to show.
var (
	ErrInvalidServerName           = errors.New("server name must be 1 to 64 characters without control characters")
	ErrInvalidServerHost           = errors.New("server host must be a host name or IP address, without scheme, port or path")
	ErrInvalidServerPort           = errors.New("server port must be 1 to 65535")
	ErrInvalidServerMaxConnections = errors.New("server max connections must be 1 to 100")
	ErrInvalidServerRetention      = errors.New("server retention days must not be negative")
	ErrInvalidServerPriority       = errors.New("server priority must not be negative")
	ErrInvalidServerCredentials    = errors.New("server username and password must be at most 256 bytes without control characters")
)

// Validate reports whether the Server may be stored. Credentials are never
// part of the error text.
func (s Server) Validate() error {
	var problems []error
	if name := strings.TrimSpace(s.Name); name == "" || len(s.Name) > maxServerNameBytes || hasControl(s.Name) {
		problems = append(problems, ErrInvalidServerName)
	}
	if !validHost(s.Host) {
		problems = append(problems, fmt.Errorf("%w: %q", ErrInvalidServerHost, s.Host))
	}
	if s.Port < 1 || s.Port > 65535 {
		problems = append(problems, fmt.Errorf("%w: %d", ErrInvalidServerPort, s.Port))
	}
	if s.MaxConnections < 1 || s.MaxConnections > MaxServerConnections {
		problems = append(problems, fmt.Errorf("%w: %d", ErrInvalidServerMaxConnections, s.MaxConnections))
	}
	if s.RetentionDays < 0 {
		problems = append(problems, ErrInvalidServerRetention)
	}
	if s.Priority < 0 {
		problems = append(problems, ErrInvalidServerPriority)
	}
	if !validCredential(s.Username) || !validCredential(s.Password.Reveal()) {
		problems = append(problems, ErrInvalidServerCredentials)
	}
	return errors.Join(problems...)
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

func validCredential(s string) bool {
	return len(s) <= maxCredentialBytes && !hasControl(s)
}

// validHost accepts an IP address (without brackets or zone) or a DNS name
// of ASCII letters, digits, '-' and '_' (Docker service names use it).
// Internationalized names must be given in their punycode form.
func validHost(h string) bool {
	if h == "" || len(h) > maxHostBytes {
		return false
	}
	if strings.Contains(h, ":") {
		addr, err := netip.ParseAddr(h)
		return err == nil && addr.Zone() == ""
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > maxLabelBytes || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			switch c := label[i]; {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
			default:
				return false
			}
		}
	}
	return true
}
