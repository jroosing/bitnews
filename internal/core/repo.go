package core

import (
	"context"
	"errors"
	"time"
)

// Errors the repositories return. Implementations wrap them so callers can
// use errors.Is.
var (
	// ErrNotFound means no row matches the given ID or key.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a unique value, such as a Category name, is taken.
	ErrConflict = errors.New("conflict")
)

// DownloadQuery selects one page of Downloads, ordered by AddedAt, then ID.
// Zero-valued filter fields match everything.
type DownloadQuery struct {
	State      State
	CategoryID ID
	SourceKind SourceKind
	// After is the opaque cursor returned with the previous page; empty
	// starts at the first page.
	After string
	// Limit caps the page size; the repository applies its own maximum.
	Limit int
}

// DownloadRepo persists Downloads.
type DownloadRepo interface {
	Create(ctx context.Context, d Download) error
	Get(ctx context.Context, id ID) (Download, error)
	// List returns one page and the cursor of the next one, which is empty
	// on the last page.
	List(ctx context.Context, q DownloadQuery) (page []Download, next string, err error)
	// Update stores every field except ID, AddedAt (both immutable, so
	// paging on (AddedAt, ID) stays stable), State, and the fields only a
	// StateChange sets (FetchedAt, ReadyAt, OutputPath, FailureReason,
	// FailureDetail).
	Update(ctx context.Context, d Download) error
	// UpdateState applies c to the stored Download with
	// Download.ChangeState, so illegal moves are rejected by core, and
	// returns the result.
	UpdateState(ctx context.Context, id ID, c StateChange) (Download, error)
	// UpdateProgress stores DoneBytes and, when totalBytes is positive,
	// TotalBytes, and refreshes UpdatedAt. It writes nothing else, so an
	// engine reporting progress cannot overwrite a concurrent edit of the
	// Priority, Category or Paused flag the way a Get followed by Update
	// would. An unknown ID is ErrNotFound.
	UpdateProgress(ctx context.Context, id ID, doneBytes, totalBytes int64) error
	// UpdateDisplay stores the name (when it is not empty) and TotalBytes
	// (when totalBytes is positive) and refreshes UpdatedAt. A magnet
	// Download learns both when its metadata arrives. Like UpdateProgress it
	// writes nothing else, so it cannot overwrite a concurrent edit. An
	// unknown ID is ErrNotFound.
	UpdateDisplay(ctx context.Context, id ID, name string, totalBytes int64) error
	Delete(ctx context.Context, id ID) error
}

// CategoryRepo persists Categories. Names are unique (ErrConflict).
// Deleting a Category clears it from the Downloads that use it.
type CategoryRepo interface {
	Create(ctx context.Context, c Category) error
	Get(ctx context.Context, id ID) (Category, error)
	List(ctx context.Context) ([]Category, error)
	Update(ctx context.Context, c Category) error
	Delete(ctx context.Context, id ID) error
}

// SettingsRepo persists the Settings. Save validates them first.
type SettingsRepo interface {
	// Load returns the stored Settings, with DefaultSettings for anything
	// never saved.
	Load(ctx context.Context) (Settings, error)
	Save(ctx context.Context, s Settings) error
}

// User is a person who logs in to the UI. v1 has a single admin.
type User struct {
	ID       ID
	Username string
	// PasswordHash is the argon2id hash in PHC string format.
	PasswordHash []byte
	CreatedAt    time.Time
}

// UserRepo persists Users.
type UserRepo interface {
	Create(ctx context.Context, u User) error
	Get(ctx context.Context, id ID) (User, error)
	GetByUsername(ctx context.Context, username string) (User, error)
	// Count reports how many Users exist; zero means setup is required.
	Count(ctx context.Context) (int, error)
	// UpdatePasswordHash replaces the stored hash, for example to upgrade
	// it to the current argon2id parameters after a login. An unknown ID
	// is ErrNotFound.
	UpdatePasswordHash(ctx context.Context, id ID, hash []byte) error
}

// TokenHash is the SHA-256 hash of a generated token. Only the hash of a
// session token or API key is ever stored.
type TokenHash [32]byte

// Session is a logged-in UI session. Expiry is computed from CreatedAt
// (absolute) and LastSeenAt (idle).
type Session struct {
	TokenHash  TokenHash
	UserID     ID
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// SessionRepo persists Sessions, keyed by their TokenHash.
type SessionRepo interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, h TokenHash) (Session, error)
	Touch(ctx context.Context, h TokenHash, lastSeen time.Time) error
	// Delete removes the Session; an unknown hash is ErrNotFound.
	Delete(ctx context.Context, h TokenHash) error
	// DeleteExpired removes every Session created before createdBefore
	// (absolute expiry) or last seen before seenBefore (idle expiry) and
	// reports how many it removed.
	DeleteExpired(ctx context.Context, createdBefore, seenBefore time.Time) (int, error)
}

// APIKeyScope limits what an API key may do.
type APIKeyScope string

// The API key scopes: read allows GET requests, write allows everything.
const (
	APIKeyScopeRead  APIKeyScope = "read"
	APIKeyScopeWrite APIKeyScope = "write"
)

// APIKey is a named key for scripts and the own API.
type APIKey struct {
	ID         ID
	Name       string
	Scope      APIKeyScope
	Hash       TokenHash
	CreatedAt  time.Time
	LastUsedAt time.Time // zero until first use
}

// APIKeyRepo persists API keys.
type APIKeyRepo interface {
	Create(ctx context.Context, k APIKey) error
	List(ctx context.Context) ([]APIKey, error)
	GetByHash(ctx context.Context, h TokenHash) (APIKey, error)
	TouchLastUsed(ctx context.Context, id ID, at time.Time) error
	Delete(ctx context.Context, id ID) error
}
