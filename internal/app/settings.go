package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/jroosing/bitnews/internal/core"
)

// SettingsService reads and writes the Settings. It keeps them in memory, so
// the authentication middleware can read them on every request without
// touching the database.
type SettingsService struct {
	repo   core.SettingsRepo
	events Publisher

	mu      sync.RWMutex
	current core.Settings
}

// NewSettingsService loads the stored Settings.
func NewSettingsService(ctx context.Context, repo core.SettingsRepo, events Publisher) (*SettingsService, error) {
	s, err := repo.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: load settings: %w", err)
	}
	return &SettingsService{repo: repo, events: events, current: s}, nil
}

// Current returns the Settings in effect. The caller may modify the result:
// it shares nothing with the service.
func (s *SettingsService) Current() core.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.current)
}

// Update validates and stores next, replacing every setting, and publishes
// core.SettingsChanged. Invalid Settings are rejected with code
// "invalid_settings" and a message that lists every problem.
func (s *SettingsService) Update(ctx context.Context, next core.Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store(ctx, next)
}

// Change applies change to a copy of the current Settings and stores the
// result like Update, all under one lock, so two changes to different
// settings never undo each other. When change returns an error nothing is
// stored and that error is returned. change runs under the lock: it must not
// call back into the service (Current would wait forever).
func (s *SettingsService) Change(ctx context.Context, change func(*core.Settings) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.current)
	if err := change(&next); err != nil {
		return err
	}
	return s.store(ctx, next)
}

// store is Update without the lock.
func (s *SettingsService) store(ctx context.Context, next core.Settings) error {
	if err := next.Validate(); err != nil {
		return invalid("invalid_settings", "%s", err.Error())
	}
	if err := s.repo.Save(ctx, next); err != nil {
		if errors.Is(err, core.ErrInvalidSettings) {
			return invalid("invalid_settings", "%s", err.Error())
		}
		return fmt.Errorf("app: save settings: %w", err)
	}
	s.current = clone(next)
	s.events.Publish(core.SettingsChanged{})
	return nil
}

func clone(s core.Settings) core.Settings {
	s.Auth.LANBypassCIDRs = slices.Clone(s.Auth.LANBypassCIDRs)
	s.Auth.TrustedProxies = slices.Clone(s.Auth.TrustedProxies)
	s.Auth.AllowedHosts = slices.Clone(s.Auth.AllowedHosts)
	return s
}
