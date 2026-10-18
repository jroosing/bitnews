package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func setupBody(token, username, password string) string {
	b, _ := json.Marshal(map[string]string{"token": token, "username": username, "password": password})
	return string(b)
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string }
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.NotEmpty(t, body.Error.Message)
	return body.Error.Code
}

func TestSetupHandlerCreatesAdminAndReturns201WithoutEchoingSecrets(t *testing.T) {
	users := &fakeUsers{}
	s, tok, logs := newSetup(t, users)
	h := auth.SetupHandler(s)

	rec := post(h, setupBody(tok, "admin", goodPassword.Reveal()))

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.NotContains(t, rec.Body.String(), goodPassword.Reveal())
	assert.NotContains(t, rec.Body.String(), tok)
	assert.NotContains(t, rec.Body.String(), "argon2")
	require.Len(t, users.users, 1)
	assert.Equal(t, 1, strings.Count(logs.String(), tok))
}

func TestSetupHandlerMapsErrorsToStatusCodes(t *testing.T) {
	cases := []struct {
		name   string
		body   func(tok string) string
		status int
		code   string
	}{
		{"wrong token", func(string) string { return setupBody("nope", "admin", goodPassword.Reveal()) }, 403, "invalid_setup_token"},
		{"missing token", func(string) string { return `{"username":"admin","password":"` + goodPassword.Reveal() + `"}` }, 403, "invalid_setup_token"},
		{"short password", func(tok string) string { return setupBody(tok, "admin", "short") }, 400, "invalid_password"},
		{"bad username", func(tok string) string { return setupBody(tok, " ", goodPassword.Reveal()) }, 400, "invalid_username"},
		{"malformed json", func(string) string { return `{not json` }, 400, "invalid_request"},
		{"empty body", func(string) string { return `` }, 400, "invalid_request"},
		{"unknown field", func(tok string) string {
			return `{"token":"` + tok + `","username":"admin","password":"` + goodPassword.Reveal() + `","admin":true}`
		}, 400, "invalid_request"},
		{"trailing data", func(tok string) string { return setupBody(tok, "admin", goodPassword.Reveal()) + `{}` }, 400, "invalid_request"},
		{"wrong types", func(string) string { return `{"token":1,"username":2,"password":3}` }, 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			users := &fakeUsers{}
			s, tok, _ := newSetup(t, users)

			rec := post(auth.SetupHandler(s), tc.body(tok))

			assert.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errCode(t, rec))
			assert.Empty(t, users.users)
			assert.NotContains(t, rec.Body.String(), goodPassword.Reveal())
		})
	}
}

func TestSetupHandlerSecondSetupIsConflict(t *testing.T) {
	s, tok, _ := newSetup(t, &fakeUsers{})
	h := auth.SetupHandler(s)
	require.Equal(t, http.StatusCreated, post(h, setupBody(tok, "admin", goodPassword.Reveal())).Code)

	rec := post(h, setupBody(tok, "other", goodPassword.Reveal()))

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "setup_closed", errCode(t, rec))
}

func TestSetupHandlerRejectsBodiesOver1MiB(t *testing.T) {
	users := &fakeUsers{}
	s, tok, _ := newSetup(t, users)
	huge := setupBody(tok, "admin", strings.Repeat("a", 2<<20))

	rec := post(auth.SetupHandler(s), huge)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "request_too_large", errCode(t, rec))
	assert.Empty(t, users.users)
}

func TestSetupHandlerOnlyAcceptsPost(t *testing.T) {
	s, _, _ := newSetup(t, &fakeUsers{})
	h := auth.SetupHandler(s)

	for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		rec := do(h, m, "/api/v1/setup")

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, m)
		assert.Equal(t, "POST", rec.Header().Get("Allow"))
		assert.Equal(t, "method_not_allowed", errCode(t, rec))
	}
}

func TestSetupHandlerReportsUnexpectedFailuresAsGeneric500(t *testing.T) {
	auth.UseFastParams(t)
	users := &failingUsers{}
	logger, logs := newLogger()
	s, err := auth.NewSetup(context.Background(), users, logger)
	require.NoError(t, err)
	tok := setupToken(t, logs)
	users.failCreate = true

	rec := post(auth.SetupHandler(s), setupBody(tok, "admin", goodPassword.Reveal()))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal", errCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "disk on fire")
}

type failingUsers struct {
	fakeUsers
	failCreate bool
}

func (f *failingUsers) Create(ctx context.Context, u core.User) error {
	if f.failCreate {
		return errors.New("disk on fire")
	}
	return f.fakeUsers.Create(ctx, u)
}
