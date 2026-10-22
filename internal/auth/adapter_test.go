package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

type adapterEnv struct {
	svc   *auth.AdapterCredentialService
	repo  *fakeAdapterCreds
	clock *fakeClock
	log   *bytes.Buffer
}

func newAdapterEnv(t *testing.T) adapterEnv {
	t.Helper()
	e := adapterEnv{repo: &fakeAdapterCreds{}, clock: newClock(), log: &bytes.Buffer{}}
	e.svc = auth.NewAdapterCredentialService(e.repo,
		auth.WithAdapterClock(e.clock.Now),
		auth.WithAdapterLogger(slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	return e
}

func TestAdapterCreateSABReturnsPrefixedKeyAndStoresOnlyItsHash(t *testing.T) {
	e := newAdapterEnv(t)

	plain, info, err := e.svc.Create(context.Background(), auth.KindAdapterSAB, "Sonarr", "")

	require.NoError(t, err)
	assert.Regexp(t, `^bns_[a-z2-7]{52}$`, plain)
	assert.Equal(t, auth.KindAdapterSAB, info.Kind)
	assert.Equal(t, "Sonarr", info.Name)
	assert.Empty(t, info.Username)
	assert.Equal(t, e.clock.Now(), info.CreatedAt)
	assert.Nil(t, info.LastUsedAt)
	require.Len(t, e.repo.rows, 1)
	assert.Equal(t, auth.HashToken(plain), e.repo.rows[0].SecretHash)
	assert.Equal(t, core.AdapterSAB, e.repo.rows[0].Kind)
}

func TestAdapterCreateSABIgnoresAUsername(t *testing.T) {
	e := newAdapterEnv(t)

	_, info, err := e.svc.Create(context.Background(), auth.KindAdapterSAB, "Sonarr", "sonarr")

	require.NoError(t, err)
	assert.Empty(t, info.Username)
}

func TestAdapterCreateQBitHasAUsernameFromTheNameByDefault(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()

	plain, info, err := e.svc.Create(ctx, auth.KindAdapterQBit, "My Radarr (4K)", "")
	_, explicit, err2 := e.svc.Create(ctx, auth.KindAdapterQBit, "Sonarr", "tv_manager-1")

	require.NoError(t, err)
	require.NoError(t, err2)
	assert.Regexp(t, `^bnq_[a-z2-7]{52}$`, plain)
	assert.Equal(t, "my-radarr-4k", info.Username)
	assert.Equal(t, "tv_manager-1", explicit.Username)
}

func TestAdapterCreateRejectsBadInput(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()

	for _, name := range []string{"", "   ", "bad\nname", strings.Repeat("x", 101)} {
		_, _, err := e.svc.Create(ctx, auth.KindAdapterSAB, name, "")
		assert.ErrorIs(t, err, auth.ErrInvalidAdapterName, "name %q", name)
	}
	for _, user := range []string{"UPPER", "has space", "dot.dot", strings.Repeat("a", 33), "ünï"} {
		_, _, err := e.svc.Create(ctx, auth.KindAdapterQBit, "ok", user)
		assert.ErrorIs(t, err, auth.ErrInvalidAdapterUsername, "username %q", user)
	}
	for _, kind := range []auth.TokenKind{auth.KindAPI, auth.KindSession, "bogus"} {
		_, _, err := e.svc.Create(ctx, kind, "ok", "")
		assert.ErrorIs(t, err, auth.ErrInvalidAdapterKind, "kind %q", kind)
	}
	assert.Empty(t, e.repo.rows)
}

func TestAdapterCreateQBitWithATakenUsernameConflicts(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	_, _, err := e.svc.Create(ctx, auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)

	_, _, err = e.svc.Create(ctx, auth.KindAdapterQBit, "Other", "radarr")

	assert.ErrorIs(t, err, auth.ErrAdapterUsernameTaken)
}

func TestAdapterListNeverExposesHashesOrPlaintext(t *testing.T) {
	e := newAdapterEnv(t)
	plain, _, err := e.svc.Create(context.Background(), auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)

	list, err := e.svc.List(context.Background())
	require.NoError(t, err)
	raw, err := json.Marshal(list)
	require.NoError(t, err)

	require.Len(t, list, 1)
	assert.NotContains(t, string(raw), plain)
	assert.NotContains(t, strings.ToLower(string(raw)), "hash")
	assert.JSONEq(t, `[{"id":"`+string(list[0].ID)+`","kind":"adapter_qbit","name":"Radarr","username":"radarr","created_at":"`+
		e.clock.Now().Format(time.RFC3339Nano)+`","last_used_at":null}]`, string(raw))
}

func TestAdapterSABKeyAuthenticatesThenRevocationIsImmediate(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	plain, info, err := e.svc.Create(ctx, auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)

	got, err := e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain)
	require.NoError(t, err)
	assert.Equal(t, info.ID, got.ID)

	require.NoError(t, e.svc.Revoke(ctx, info.ID))
	_, err = e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain)

	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential)
	assert.ErrorIs(t, e.svc.Revoke(ctx, info.ID), core.ErrNotFound)
}

func TestAdapterAuthenticateRejectsEveryOtherKindOfToken(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	sab, _, err := e.svc.Create(ctx, auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)
	qbit, _, err := e.svc.Create(ctx, auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)
	session, _, _ := auth.NewToken()
	apiKey, _, _ := auth.NewKindToken(auth.KindAPI)

	for _, k := range []string{"", "bns_", "bns_short", session, apiKey, qbit, strings.Repeat("a", 100_000)} {
		_, err := e.svc.Authenticate(ctx, auth.KindAdapterSAB, k)
		assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential)
	}
	_, err = e.svc.Authenticate(ctx, auth.KindAdapterQBit, sab)
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential, "a SAB key is not a qBit key")
	_, err = e.svc.Authenticate(ctx, auth.KindAPI, sab)
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterKind, "only adapter kinds can be asked for")
}

func TestAdapterAuthenticateComparesTheStoredHashAgain(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	plain, _, err := e.svc.Create(ctx, auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)
	other, _, _ := auth.NewKindToken(auth.KindAdapterSAB)
	e.repo.rows[0].SecretHash = auth.HashToken(other) // a buggy repo returning a wrong row

	repo := &lyingAdapterRepo{fakeAdapterCreds: e.repo}
	svc := auth.NewAdapterCredentialService(repo, auth.WithAdapterClock(e.clock.Now))
	_, err = svc.Authenticate(ctx, auth.KindAdapterSAB, plain)

	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential)
}

// lyingAdapterRepo returns its first row for any hash.
type lyingAdapterRepo struct{ *fakeAdapterCreds }

func (l *lyingAdapterRepo) GetByHash(context.Context, core.TokenHash) (core.AdapterCredential, error) {
	return l.rows[0], nil
}

func TestAdapterAuthenticateTouchesLastUsedAtMostOncePerMinute(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	plain, _, err := e.svc.Create(ctx, auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)

	info, err := e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain)
	require.NoError(t, err)
	require.NotNil(t, info.LastUsedAt)
	assert.Equal(t, e.clock.Now(), *info.LastUsedAt)
	e.clock.Advance(30 * time.Second)
	_, err = e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain)
	require.NoError(t, err)
	assert.Equal(t, 1, e.repo.touchCount(), "within a minute: no second write")

	e.clock.Advance(31 * time.Second)
	_, err = e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain)
	require.NoError(t, err)
	assert.Equal(t, 2, e.repo.touchCount())
}

func TestAdapterAuthenticateQBitChecksUsernameAndKey(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	plain, info, err := e.svc.Create(ctx, auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)
	_, _, err = e.svc.Create(ctx, auth.KindAdapterQBit, "Sonarr", "sonarr")
	require.NoError(t, err)

	got, err := e.svc.AuthenticateQBit(ctx, "radarr", plain)
	require.NoError(t, err)
	assert.Equal(t, info.ID, got.ID)

	_, err = e.svc.AuthenticateQBit(ctx, "sonarr", plain)
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential, "the key of another credential")
	_, err = e.svc.AuthenticateQBit(ctx, "nobody", plain)
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential)
	_, err = e.svc.AuthenticateQBit(ctx, "radarr", "")
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential)
	sab, _, err := e.svc.Create(ctx, auth.KindAdapterSAB, "Prowlarr", "")
	require.NoError(t, err)
	_, err = e.svc.AuthenticateQBit(ctx, "radarr", sab)
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential, "a SAB key is not a qBit password")
}

func TestAdapterServiceNeverLogsKeys(t *testing.T) {
	e := newAdapterEnv(t)
	ctx := context.Background()
	plain, _, err := e.svc.Create(ctx, auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)
	e.repo.rows[0].LastUsedAt = time.Time{}

	_, _ = e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain)
	_, _ = e.svc.Authenticate(ctx, auth.KindAdapterSAB, plain+"x")

	assert.NotContains(t, e.log.String(), plain)
}
