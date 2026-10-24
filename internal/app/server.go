package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jroosing/bitnews/internal/core"
)

// Defaults for the fields a client may leave out when creating a Server.
const (
	defaultServerMaxConnections = 8
)

// ServerService manages the Usenet Servers: CRUD with validation, and
// connection tests. Every change publishes core.ServersChanged so the runner
// can reconfigure its connection pools.
type ServerService struct {
	repo   core.ServerRepo
	events Publisher
	prober Prober
	opts   options

	// mu serializes the read-modify-write cycle of Update.
	mu sync.Mutex
}

// NewServerService returns a ServerService. prober runs the connection
// tests; use NewNNTPProber for the real one.
func NewServerService(repo core.ServerRepo, events Publisher, prober Prober, opts ...Option) *ServerService {
	return &ServerService{repo: repo, events: events, prober: prober, opts: newOptions(opts)}
}

// ServerSpec describes a Server to create or to test. Zero values take the
// defaults: port 563, 8 connections, enabled.
type ServerSpec struct {
	Name           string
	Host           string
	Port           int
	Username       string
	Password       core.Secret
	MaxConnections int
	RetentionDays  int
	Priority       int
	// Enabled defaults to true when nil.
	Enabled *bool
}

func (sp ServerSpec) server() core.Server {
	s := core.Server{
		Name: strings.TrimSpace(sp.Name), Host: strings.TrimSpace(sp.Host), Port: sp.Port,
		Username: sp.Username, Password: sp.Password, MaxConnections: sp.MaxConnections,
		RetentionDays: sp.RetentionDays, Priority: sp.Priority, Enabled: true,
	}
	if sp.Port == 0 {
		s.Port = core.DefaultServerPort
	}
	if sp.MaxConnections == 0 {
		s.MaxConnections = defaultServerMaxConnections
	}
	if sp.Enabled != nil {
		s.Enabled = *sp.Enabled
	}
	return s
}

// ServerPatch lists the changes Update applies; nil fields are left alone. A
// nil Password keeps the stored one.
type ServerPatch struct {
	Name           *string
	Host           *string
	Port           *int
	Username       *string
	Password       *core.Secret
	MaxConnections *int
	RetentionDays  *int
	Priority       *int
	Enabled        *bool
}

func (p ServerPatch) empty() bool { return p == ServerPatch{} }

// List returns every Server ordered by Server Priority, then name. The slice
// is never nil. Servers whose password cannot be decrypted are included with
// PasswordUnavailable set.
func (s *ServerService) List(ctx context.Context) ([]core.Server, error) {
	servers, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: list servers: %w", err)
	}
	if servers == nil {
		servers = []core.Server{}
	}
	return servers, nil
}

// Get returns the Server with the given ID.
func (s *ServerService) Get(ctx context.Context, id core.ID) (core.Server, error) {
	srv, err := s.repo.Get(ctx, id)
	if err != nil {
		return core.Server{}, fmt.Errorf("app: get server: %w", err)
	}
	return srv, nil
}

// Create validates and stores a new Server. A name that is already taken is
// a conflict (code "name_taken").
func (s *ServerService) Create(ctx context.Context, sp ServerSpec) (core.Server, error) {
	srv := sp.server()
	srv.ID = core.NewID()
	srv.CreatedAt = s.opts.now()
	if err := validateServer(srv); err != nil {
		return core.Server{}, err
	}
	if err := s.repo.Create(ctx, srv); err != nil {
		return core.Server{}, serverWriteError("create", err)
	}
	s.events.Publish(core.ServersChanged{})
	return srv, nil
}

// Update validates and stores the changes to a Server. Without a Password
// in the patch the stored password stays, even one that cannot be decrypted.
func (s *ServerService) Update(ctx context.Context, id core.ID, p ServerPatch) (core.Server, error) {
	if p.empty() {
		return core.Server{}, invalid("empty_patch", "nothing to change: give at least one field")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	srv, err := s.repo.Get(ctx, id)
	if err != nil {
		return core.Server{}, fmt.Errorf("app: update server: %w", err)
	}
	if p.Name != nil {
		srv.Name = strings.TrimSpace(*p.Name)
	}
	if p.Host != nil {
		srv.Host = strings.TrimSpace(*p.Host)
	}
	setIf(&srv.Port, p.Port)
	setIf(&srv.Username, p.Username)
	setIf(&srv.MaxConnections, p.MaxConnections)
	setIf(&srv.RetentionDays, p.RetentionDays)
	setIf(&srv.Priority, p.Priority)
	setIf(&srv.Enabled, p.Enabled)
	if p.Password != nil {
		srv.Password, srv.PasswordUnavailable = *p.Password, false
	}
	if err := validateServer(srv); err != nil {
		return core.Server{}, err
	}
	if err := s.repo.Update(ctx, srv); err != nil {
		return core.Server{}, serverWriteError("update", err)
	}
	s.events.Publish(core.ServersChanged{})
	return srv, nil
}

func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// Delete removes a Server.
func (s *ServerService) Delete(ctx context.Context, id core.ID) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("app: delete server: %w", err)
	}
	s.events.Publish(core.ServersChanged{})
	return nil
}

// Test connects to the stored Server, authenticates and asks for its clock.
// A failed test is not an error: the ProbeResult says which step failed. A
// Server whose password cannot be decrypted cannot be tested (conflict, code
// "password_unavailable").
func (s *ServerService) Test(ctx context.Context, id core.ID) (ProbeResult, error) {
	srv, err := s.repo.Get(ctx, id)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("app: test server: %w", err)
	}
	if srv.PasswordUnavailable {
		return ProbeResult{}, conflict("password_unavailable", "the stored password cannot be decrypted: enter the password again")
	}
	return s.prober.Probe(ctx, srv), nil
}

// TestSpec runs the same test for a Server that is not saved. Name, Priority
// and the limits do not matter for the test but are validated like any
// other Server.
func (s *ServerService) TestSpec(ctx context.Context, sp ServerSpec) (ProbeResult, error) {
	srv := sp.server()
	if srv.Name == "" {
		srv.Name = "test"
	}
	if err := validateServer(srv); err != nil {
		return ProbeResult{}, err
	}
	return s.prober.Probe(ctx, srv), nil
}

// serverProblems maps each validation error to the API code, in the order
// the fields are reported.
var serverProblems = []struct {
	err  error
	code string
}{
	{core.ErrInvalidServerName, "invalid_name"},
	{core.ErrInvalidServerHost, "invalid_host"},
	{core.ErrInvalidServerPort, "invalid_port"},
	{core.ErrInvalidServerMaxConnections, "invalid_max_connections"},
	{core.ErrInvalidServerRetention, "invalid_retention_days"},
	{core.ErrInvalidServerPriority, "invalid_priority"},
	{core.ErrInvalidServerCredentials, "invalid_credentials"},
}

// validateServer turns core's validation into an app error. The code is the
// first problem's; the message lists all of them. Validation errors never
// contain credentials.
func validateServer(srv core.Server) error {
	err := srv.Validate()
	if err == nil {
		return nil
	}
	for _, p := range serverProblems {
		if errors.Is(err, p.err) {
			return invalid(p.code, "%s", strings.ReplaceAll(err.Error(), "\n", "; "))
		}
	}
	return fmt.Errorf("app: validate server: %w", err)
}

func serverWriteError(op string, err error) error {
	if errors.Is(err, core.ErrConflict) {
		return &Error{Kind: core.ErrConflict, Code: "name_taken", Message: "a server with this name already exists"}
	}
	return fmt.Errorf("app: %s server: %w", op, err)
}
