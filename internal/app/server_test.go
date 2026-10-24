package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// fakeProber records what it was asked to probe and answers with result.
type fakeProber struct {
	mu     sync.Mutex
	result app.ProbeResult
	probed []core.Server
}

func (p *fakeProber) Probe(_ context.Context, s core.Server) app.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, s)
	return p.result
}

type serverStack struct {
	*stack
	key    store.Key
	repo   *store.ServerRepo
	prober *fakeProber
}

func newServerStack(t *testing.T) *serverStack {
	t.Helper()
	s := newStack(t)
	key := store.GenerateKey()
	return &serverStack{stack: s, key: key, repo: store.NewServerRepo(s.db, key),
		prober: &fakeProber{result: app.ProbeResult{OK: true, Class: app.ProbeOK}}}
}

func (s *serverStack) service() *app.ServerService {
	return app.NewServerService(s.repo, s.bus, s.prober, app.WithClock(s.clock.Now))
}

func spec(name string) app.ServerSpec {
	return app.ServerSpec{Name: name, Host: name + ".example.com", Username: "joe", Password: "hunter2", MaxConnections: 10}
}

func TestCreateServerAppliesDefaultsAndStoresIt(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()

	got, err := svc.Create(ctx, app.ServerSpec{Name: "main", Host: "news.example.com", Username: "joe", Password: "hunter2"})

	require.NoError(t, err)
	assert.NotEmpty(t, got.ID)
	assert.Equal(t, 563, got.Port)
	assert.Equal(t, 8, got.MaxConnections)
	assert.True(t, got.Enabled)
	assert.Equal(t, s.clock.Now(), got.CreatedAt)
	stored, err := s.repo.Get(ctx, got.ID)
	require.NoError(t, err)
	assert.Equal(t, got, stored)
}

func TestCreateServerHonorsAnExplicitDisabled(t *testing.T) {
	s := newServerStack(t)
	sp := spec("main")
	off := false
	sp.Enabled = &off

	got, err := s.service().Create(context.Background(), sp)

	require.NoError(t, err)
	assert.False(t, got.Enabled)
}

func TestCreateServerRejectsInvalidInputWithFieldCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*app.ServerSpec)
		code string
	}{
		"empty name":    {func(s *app.ServerSpec) { s.Name = "" }, "invalid_name"},
		"url as host":   {func(s *app.ServerSpec) { s.Host = "nntps://news.example.com" }, "invalid_host"},
		"empty host":    {func(s *app.ServerSpec) { s.Host = "" }, "invalid_host"},
		"port":          {func(s *app.ServerSpec) { s.Port = 70000 }, "invalid_port"},
		"connections":   {func(s *app.ServerSpec) { s.MaxConnections = 101 }, "invalid_max_connections"},
		"retention":     {func(s *app.ServerSpec) { s.RetentionDays = -1 }, "invalid_retention_days"},
		"priority":      {func(s *app.ServerSpec) { s.Priority = -1 }, "invalid_priority"},
		"credentials":   {func(s *app.ServerSpec) { s.Username = "a\nb" }, "invalid_credentials"},
		"first problem": {func(s *app.ServerSpec) { s.Name, s.Host = "", "" }, "invalid_name"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServerStack(t)
			sp := spec("main")
			tc.mut(&sp)

			_, err := s.service().Create(context.Background(), sp)

			require.ErrorIs(t, err, app.ErrInvalidInput)
			assert.Equal(t, tc.code, errCode(t, err))
		})
	}
}

func TestInvalidServerErrorsNeverContainThePassword(t *testing.T) {
	s := newServerStack(t)
	sp := spec("main")
	sp.Host = ""
	sp.Password = "hunter2-secret"

	_, err := s.service().Create(context.Background(), sp)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2-secret")
}

func TestCreateServerWithTakenNameIsConflict(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	_, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)

	_, err = svc.Create(ctx, spec("main"))

	require.ErrorIs(t, err, core.ErrConflict)
	assert.Equal(t, "name_taken", errCode(t, err))
}

func TestServerChangesPublishServersChanged(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()

	created, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)
	name := "renamed"
	_, err = svc.Update(ctx, created.ID, app.ServerPatch{Name: &name})
	require.NoError(t, err)
	require.NoError(t, svc.Delete(ctx, created.ID))

	assert.Equal(t, []core.Event{core.ServersChanged{}, core.ServersChanged{}, core.ServersChanged{}}, s.events())
}

func TestFailedServerChangePublishesNothing(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	_, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)
	s.events()

	_, err = svc.Create(ctx, spec("main"))
	require.Error(t, err)
	assert.ErrorIs(t, svc.Delete(ctx, core.NewID()), core.ErrNotFound)

	assert.Empty(t, s.events())
}

func TestUpdateServerWithoutPasswordKeepsTheStoredOne(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	created, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)
	host, port := "other.example.org", 443

	got, err := svc.Update(ctx, created.ID, app.ServerPatch{Host: &host, Port: &port})

	require.NoError(t, err)
	assert.Equal(t, host, got.Host)
	assert.Equal(t, 443, got.Port)
	stored, err := s.repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", stored.Password.Reveal())
	assert.Equal(t, "joe", stored.Username)
}

func TestUpdateServerWithPasswordReplacesIt(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	created, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)
	pw := core.Secret("new-pass")

	_, err = svc.Update(ctx, created.ID, app.ServerPatch{Password: &pw})

	require.NoError(t, err)
	stored, err := s.repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-pass", stored.Password.Reveal())
}

func TestUpdateServerRevalidatesAndRejectsEmptyPatch(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	created, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)
	badHost := "http://x"
	zero := 0

	_, empty := svc.Update(ctx, created.ID, app.ServerPatch{})
	_, bad := svc.Update(ctx, created.ID, app.ServerPatch{Host: &badHost})
	_, conns := svc.Update(ctx, created.ID, app.ServerPatch{MaxConnections: &zero})
	_, missing := svc.Update(ctx, core.NewID(), app.ServerPatch{Host: &badHost})

	assert.Equal(t, "empty_patch", errCode(t, empty))
	assert.Equal(t, "invalid_host", errCode(t, bad))
	assert.Equal(t, "invalid_max_connections", errCode(t, conns))
	assert.ErrorIs(t, missing, core.ErrNotFound)
}

func TestUpdateServerToATakenNameIsConflict(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	_, err := svc.Create(ctx, spec("one"))
	require.NoError(t, err)
	two, err := svc.Create(ctx, spec("two"))
	require.NoError(t, err)
	name := "one"

	_, err = svc.Update(ctx, two.ID, app.ServerPatch{Name: &name})

	assert.Equal(t, "name_taken", errCode(t, err))
}

func TestServerWithUndecryptablePasswordIsListedAndCanBeFixedByANewPassword(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	created, err := s.service().Create(ctx, spec("main"))
	require.NoError(t, err)
	// The daemon restarts with another key.
	svc := app.NewServerService(store.NewServerRepo(s.db, store.GenerateKey()), s.bus, s.prober)

	listed, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.True(t, listed[0].PasswordUnavailable)

	// A PATCH without a password keeps the (unreadable) stored one and is allowed.
	name := "renamed"
	patched, err := svc.Update(ctx, created.ID, app.ServerPatch{Name: &name})
	require.NoError(t, err)
	assert.True(t, patched.PasswordUnavailable)

	// Giving a password makes the Server usable again.
	pw := core.Secret("fresh")
	fixed, err := svc.Update(ctx, created.ID, app.ServerPatch{Password: &pw})
	require.NoError(t, err)
	assert.False(t, fixed.PasswordUnavailable)
	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh", got.Password.Reveal())
}

func TestListServersIsOrderedAndNeverNil(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	empty, err := svc.List(ctx)
	require.NoError(t, err)
	require.NotNil(t, empty)
	fill := spec("fill")
	fill.Priority = 1
	_, err = svc.Create(ctx, fill)
	require.NoError(t, err)
	_, err = svc.Create(ctx, spec("main"))
	require.NoError(t, err)

	got, err := svc.List(ctx)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "main", got[0].Name)
	assert.Equal(t, "fill", got[1].Name)
}

func TestGetUnknownServerIsNotFound(t *testing.T) {
	_, err := newServerStack(t).service().Get(context.Background(), core.NewID())
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTestServerProbesTheStoredServerWithItsPassword(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()
	created, err := svc.Create(ctx, spec("main"))
	require.NoError(t, err)
	s.prober.result = app.ProbeResult{OK: true, Class: app.ProbeOK, Latency: 12 * time.Millisecond}

	res, err := svc.Test(ctx, created.ID)

	require.NoError(t, err)
	assert.Equal(t, s.prober.result, res)
	require.Len(t, s.prober.probed, 1)
	assert.Equal(t, "main.example.com", s.prober.probed[0].Host)
	assert.Equal(t, "hunter2", s.prober.probed[0].Password.Reveal())
}

func TestTestServerWithUnreadablePasswordTellsTheUserToReenterIt(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	created, err := s.service().Create(ctx, spec("main"))
	require.NoError(t, err)
	svc := app.NewServerService(store.NewServerRepo(s.db, store.GenerateKey()), s.bus, s.prober)

	_, err = svc.Test(ctx, created.ID)

	require.ErrorIs(t, err, core.ErrConflict)
	assert.Equal(t, "password_unavailable", errCode(t, err))
	assert.Empty(t, s.prober.probed)
}

func TestTestServerUnknownIDIsNotFound(t *testing.T) {
	_, err := newServerStack(t).service().Test(context.Background(), core.NewID())
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTestSpecProbesAnUnsavedServerAfterValidating(t *testing.T) {
	ctx := context.Background()
	s := newServerStack(t)
	svc := s.service()

	res, err := svc.TestSpec(ctx, app.ServerSpec{Host: "news.example.com", Username: "joe", Password: "pw"})
	_, bad := svc.TestSpec(ctx, app.ServerSpec{Host: "bad host"})

	require.NoError(t, err)
	assert.True(t, res.OK)
	require.Len(t, s.prober.probed, 1)
	assert.Equal(t, 563, s.prober.probed[0].Port, "default port applies")
	assert.Equal(t, "invalid_host", errCode(t, bad))
	stored, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, stored, "nothing is saved")
}
