package auth_test

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

func TestClientIPFromUsesTheTrustedProxiesOfTheSettings(t *testing.T) {
	r := httptest.NewRequest("GET", "http://bitnews.lan/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")

	assert.Equal(t, netip.MustParseAddr("10.0.0.1"), auth.ClientIPFrom(r, core.Auth{}))
	assert.Equal(t, netip.MustParseAddr("198.51.100.7"),
		auth.ClientIPFrom(r, core.Auth{TrustedProxies: []string{"10.0.0.0/8"}}))
}

func TestSignInSetsTheSessionCookieUntilTheAbsoluteTimeout(t *testing.T) {
	e := newUIEnv(t)
	rec := httptest.NewRecorder()

	token, user, err := e.ui.SignIn(rec, httptest.NewRequest("POST", "http://bitnews.lan/login", nil), "admin", goodPassword)

	require.NoError(t, err)
	assert.Equal(t, e.user.ID, user.ID)
	c := cookieOf(t, rec)
	assert.Equal(t, auth.SessionCookieName, c.Name)
	assert.Equal(t, token, c.Value)
	want := e.clock.Now().Add(auth.SessionAbsoluteTimeout).Truncate(time.Second)
	assert.True(t, c.Expires.Equal(want), "expires %v, want %v", c.Expires, want)
}

func TestSignInWithWrongPasswordSetsNoCookie(t *testing.T) {
	e := newUIEnv(t)
	rec := httptest.NewRecorder()

	_, _, err := e.ui.SignIn(rec, httptest.NewRequest("POST", "http://bitnews.lan/login", nil), "admin", core.Secret("wrong password!!"))

	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	assert.NotErrorIs(t, err, auth.ErrSessionStart)
	assert.Empty(t, rec.Result().Cookies())
}
