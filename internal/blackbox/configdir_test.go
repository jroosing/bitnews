package blackbox_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// What prepareConfigDir stores, for the tests to look for.
const (
	adminName         = "admin"
	adminPassword     = "correct horse battery staple"
	serverName        = "Provider One"
	serverHost        = "news.example.net"
	serverUsername    = "reader"
	serverPassword    = "s3cret-nntp-password"
	categoryName      = "tv"
	categoryUsenetDir = "/media/usenet/tv"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

var (
	adminHashOnce sync.Once
	adminHash     []byte
)

// sharedAdminHash hashes the admin password once for the whole run, with the
// production argon2id parameters, because every config dir uses the same one.
func sharedAdminHash(t *testing.T) []byte {
	t.Helper()
	adminHashOnce.Do(func() {
		var err error
		adminHash, err = auth.HashPassword(core.Secret(adminPassword))
		require.NoError(t, err)
	})
	return adminHash
}

// configDir is a config dir written by the Go packages, the way a previous
// Go run of bitnews would have left it.
type configDir struct {
	Dir     string
	KeyFile string
	DBPath  string
	// AppKey is the app key stored in the database.
	AppKey string

	ServerID   core.ID
	CategoryID core.ID
}

type prepOptions struct {
	noAdmin bool
}

type prepOption func(*prepOptions)

// withoutAdmin leaves the Users table empty, so the instance starts in
// first-run setup mode.
func withoutAdmin() prepOption { return func(o *prepOptions) { o.noAdmin = true } }

// prepareConfigDir writes a config dir with: the master key file, a database
// with every migration, an admin User (unless withoutAdmin), a known app key,
// a Server whose password is sealed with the master key and a Category.
func prepareConfigDir(t *testing.T, opts ...prepOption) configDir {
	t.Helper()
	var o prepOptions
	for _, f := range opts {
		f(&o)
	}
	ctx := t.Context()
	dir := t.TempDir()
	c := configDir{
		Dir:     dir,
		KeyFile: filepath.Join(dir, "bitnews.key"),
		DBPath:  filepath.Join(dir, "bitnews.db"),
	}
	key, err := store.LoadOrCreateKey(c.KeyFile, discard)
	require.NoError(t, err)
	db, err := store.Open(ctx, c.DBPath, store.WithLogger(discard))
	require.NoError(t, err)

	if !o.noAdmin {
		require.NoError(t, store.NewUserRepo(db).Create(ctx, core.User{
			ID: core.NewID(), Username: adminName, PasswordHash: sharedAdminHash(t), CreatedAt: time.Now(),
		}))
	}
	c.AppKey = newAppKey(t)
	require.NoError(t, store.NewAppKeyRepo(db).Set(ctx, core.Secret(c.AppKey)))

	c.ServerID = core.NewID()
	require.NoError(t, store.NewServerRepo(db, key).Create(ctx, core.Server{
		ID: c.ServerID, Name: serverName, Host: serverHost, Port: 563,
		Username: serverUsername, Password: core.Secret(serverPassword),
		MaxConnections: 8, RetentionDays: 3000, Priority: 1, Enabled: true,
		CreatedAt: time.Now(),
	}))
	c.CategoryID = core.NewID()
	require.NoError(t, store.NewCategoryRepo(db).Create(ctx, core.Category{
		ID: c.CategoryID, Name: categoryName, UsenetDir: categoryUsenetDir,
	}))
	require.NoError(t, db.Close())
	return c
}

// newAppKey returns a fresh, well-formed app key.
func newAppKey(t *testing.T) string {
	t.Helper()
	k, _, err := auth.NewKindToken(auth.KindAppKey)
	require.NoError(t, err)
	return k
}

// setAppKey replaces the app key stored in the database of c, the way a
// previous run of another version could have left it.
func setAppKey(t *testing.T, c configDir, key string) {
	t.Helper()
	db := openDB(t, c)
	require.NoError(t, store.NewAppKeyRepo(db).Set(t.Context(), core.Secret(key)))
	require.NoError(t, db.Close())
}

// storedAppKey reads the app key from the database of c with the Go store.
func storedAppKey(t *testing.T, c configDir) string {
	t.Helper()
	db := openDB(t, c)
	defer func() { require.NoError(t, db.Close()) }()
	key, err := store.NewAppKeyRepo(db).Get(t.Context())
	require.NoError(t, err)
	return key.Reveal()
}

// openDB opens the database of c with the Go store (which also runs the
// migrations, so it fails when the schema is newer than the Go code).
func openDB(t *testing.T, c configDir) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), c.DBPath, store.WithLogger(discard))
	require.NoError(t, err)
	return db
}
