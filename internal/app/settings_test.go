package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

func (s *stack) settingsService(t *testing.T) *app.SettingsService {
	t.Helper()
	svc, err := app.NewSettingsService(context.Background(), s.settings, s.bus)
	require.NoError(t, err)
	return svc
}

func TestSettingsStartAsDefaultsAndLoadWhatWasStored(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	assert.Equal(t, core.DefaultSettings(), s.settingsService(t).Current())
	stored := core.DefaultSettings()
	stored.Queues.Usenet = 3
	require.NoError(t, s.settings.Save(ctx, stored))

	assert.Equal(t, stored, s.settingsService(t).Current())
}

func TestUpdateSettingsStoresThemUpdatesCurrentAndPublishesSettingsChanged(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.settingsService(t)
	next := core.DefaultSettings()
	next.Queues.Torrent = 9

	err := svc.Update(ctx, next)

	require.NoError(t, err)
	assert.Equal(t, next, svc.Current())
	stored, err := s.settings.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, next, stored)
	assert.Equal(t, []core.Event{core.SettingsChanged{}}, s.events())
}

func TestUpdateSettingsRejectsInvalidSettingsWithoutChangingOrPublishing(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.settingsService(t)
	next := core.DefaultSettings()
	next.Queues.Usenet = -1

	err := svc.Update(ctx, next)

	require.ErrorIs(t, err, app.ErrInvalidInput)
	assert.Equal(t, "invalid_settings", errCode(t, err))
	assert.Contains(t, err.Error(), "usenet queue limit")
	assert.Equal(t, core.DefaultSettings(), svc.Current())
	assert.Empty(t, s.events())
}

func TestCurrentSettingsReturnsACopyCallersCannotMutate(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.settingsService(t)
	next := core.DefaultSettings()
	next.Auth.TrustedProxies = []string{"10.0.0.0/8"}
	require.NoError(t, svc.Update(ctx, next))

	svc.Current().Auth.TrustedProxies[0] = "0.0.0.0/0"

	assert.Equal(t, []string{"10.0.0.0/8"}, svc.Current().Auth.TrustedProxies)
}

func TestChangeSettingsAppliesConcurrentChangesWithoutLosingAny(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.settingsService(t)

	done := make(chan error, 2)
	go func() { done <- svc.Change(ctx, func(n *core.Settings) error { n.Queues.Usenet = 7; return nil }) }()
	go func() {
		done <- svc.Change(ctx, func(n *core.Settings) error { n.Torrent.ListenPort = 40000; return nil })
	}()
	require.NoError(t, <-done)
	require.NoError(t, <-done)

	cur := svc.Current()
	assert.Equal(t, 7, cur.Queues.Usenet)
	assert.Equal(t, 40000, cur.Torrent.ListenPort)
}

func TestChangeSettingsKeepsTheSettingsWhenTheChangeFails(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.settingsService(t)
	refused := errors.New("refused")

	err := svc.Change(ctx, func(n *core.Settings) error { n.Queues.Usenet = 7; return refused })

	require.ErrorIs(t, err, refused)
	assert.Equal(t, core.DefaultSettings(), svc.Current())
	assert.Empty(t, s.events())
}

func TestChangeSettingsValidatesTheResult(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	svc := s.settingsService(t)

	err := svc.Change(ctx, func(n *core.Settings) error { n.Torrent.ListenPort = 70000; return nil })

	require.ErrorIs(t, err, app.ErrInvalidInput)
	assert.Equal(t, core.DefaultSettings(), svc.Current())
}
