package core

import "context"

// AppKeyRepo persists the app key: the one secret that authenticates every
// client of bitnews (SABnzbd, qBittorrent and the own API, see ADR-0006). It
// is stored as plain text, like the key of Sonarr and Radarr, so that
// Settings can show it.
type AppKeyRepo interface {
	// Get returns the key. None stored yet is ErrNotFound.
	Get(ctx context.Context) (Secret, error)
	// Set stores key, replacing any previous one.
	Set(ctx context.Context, key Secret) error
}
