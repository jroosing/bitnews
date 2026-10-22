package core

import (
	"context"
	"errors"
)

// ErrAppKeyUnreadable means the stored app key cannot be decrypted: it was
// sealed with another master key, or the value is damaged. The caller makes a
// new key.
var ErrAppKeyUnreadable = errors.New("core: stored app key is unreadable")

// AppKeyRepo persists the app key: the one secret that authenticates every
// client of bitnews (SABnzbd, qBittorrent and the own API, see ADR-0006). It
// is stored sealed with the master key, so Settings can show it again.
type AppKeyRepo interface {
	// Get returns the key. None stored yet is ErrNotFound; a stored key that
	// cannot be decrypted is ErrAppKeyUnreadable.
	Get(ctx context.Context) (Secret, error)
	// Set stores key, replacing any previous one.
	Set(ctx context.Context, key Secret) error
}
