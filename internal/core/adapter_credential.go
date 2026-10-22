package core

import (
	"context"
	"time"
)

// AdapterKind says which media-manager adapter an AdapterCredential is valid
// on. The values are the kind names of the matching token kinds.
type AdapterKind string

// The adapter kinds.
const (
	// AdapterSAB is the SABnzbd API adapter at /sabnzbd/api. Its credential
	// is an API key.
	AdapterSAB AdapterKind = "adapter_sab"
	// AdapterQBit is the qBittorrent WebUI API adapter. Its credential is a
	// username plus a generated key that acts as the password.
	AdapterQBit AdapterKind = "adapter_qbit"
)

// Valid reports whether k is a known adapter kind.
func (k AdapterKind) Valid() bool { return k == AdapterSAB || k == AdapterQBit }

// AdapterCredential authenticates one media manager (Sonarr, Radarr,
// Prowlarr, ...) on one adapter. There is one per media manager, so each can
// be revoked on its own. It is valid on its adapter's routes only.
type AdapterCredential struct {
	ID   ID
	Kind AdapterKind
	// Name is the label shown to the admin, such as "Sonarr".
	Name string
	// Username is set for AdapterQBit only, and is unique per kind.
	Username string
	// SecretHash is the SHA-256 hash of the key. The key itself is shown
	// once, when the credential is created, and never stored.
	SecretHash TokenHash
	CreatedAt  time.Time
	LastUsedAt time.Time // zero until first use
}

// AdapterCredentialRepo persists AdapterCredentials.
type AdapterCredentialRepo interface {
	// Create stores c. A taken secret hash, or a taken username for the
	// same kind, is ErrConflict.
	Create(ctx context.Context, c AdapterCredential) error
	// List returns every credential, oldest first.
	List(ctx context.Context) ([]AdapterCredential, error)
	// GetByHash returns the credential whose key hashes to h, of any kind;
	// the caller checks the kind. An unknown hash is ErrNotFound.
	GetByHash(ctx context.Context, h TokenHash) (AdapterCredential, error)
	// GetByUsername returns the credential of the kind with the username.
	// An unknown one is ErrNotFound.
	GetByUsername(ctx context.Context, kind AdapterKind, username string) (AdapterCredential, error)
	TouchLastUsed(ctx context.Context, id ID, at time.Time) error
	// Delete revokes the credential. An unknown ID is ErrNotFound.
	Delete(ctx context.Context, id ID) error
}
