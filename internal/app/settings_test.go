package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
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
