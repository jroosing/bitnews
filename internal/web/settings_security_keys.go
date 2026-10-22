package web

import (
	"errors"
	"net/http"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// keysView is the "API keys" card.
type keysView struct {
	CSRF string
	// Unavailable is set when the build has no API key service.
	Unavailable bool
	Items       []keyRow
	// Name and Scope are the add form as typed.
	Name, Scope string
	// Scopes are the options of the access select.
	Scopes []option
	Errors map[string]string
	Error  string
	// Secret is set only in the answer to a create: the plaintext key.
	Secret *shownSecret
}

// keyRow is one key. It never holds the hash or the plaintext.
type keyRow struct {
	CSRF, ID, Name    string
	Scope             string
	ScopeLabel        string
	Created, LastUsed whenView
}

// shownSecret is a credential shown once, in the answer to its creation.
type shownSecret struct {
	// ID prefixes the element IDs; Heading says what was made.
	ID, Heading, Intro string
	Fields             []secretField
	// Done reloads the card without the secret.
	Done string
	// Hints are extra lines below the values.
	Hints []string
}

// secretField is one value to copy: a label, the value and whether it is a
// secret (secrets are shown in full, but the screen reader hears the label).
type secretField struct {
	Label, Value string
	Secret       bool
}

var scopeLabels = map[core.APIKeyScope]string{
	core.APIKeyScopeRead:  "Read only",
	core.APIKeyScopeWrite: "Read and change",
}

func (s *Server) keysViewFor(r *http.Request) (keysView, error) {
	v := keysView{
		CSRF: settingsCSRF(r), Scope: string(core.APIKeyScopeRead),
		Scopes: []option{
			{Value: string(core.APIKeyScopeRead), Label: scopeLabels[core.APIKeyScopeRead]},
			{Value: string(core.APIKeyScopeWrite), Label: scopeLabels[core.APIKeyScopeWrite]},
		},
	}
	if s.APIKeys == nil {
		v.Unavailable = true
		return v, nil
	}
	items, err := s.APIKeys.List(r.Context())
	if err != nil {
		return keysView{}, err
	}
	now := time.Now()
	for _, k := range items {
		row := keyRow{
			CSRF: v.CSRF, ID: string(k.ID), Name: k.Name, Scope: string(k.Scope), ScopeLabel: scopeLabels[k.Scope],
			Created: histWhen(now, k.CreatedAt),
		}
		if k.LastUsedAt != nil {
			row.LastUsed = histWhen(now, *k.LastUsedAt)
		}
		v.Items = append(v.Items, row)
	}
	return v, nil
}

func (s *Server) registerKeyRoutes() {
	s.handleAuthed("GET /settings/security/api-keys", s.getKeys)
	s.handleAuthed("POST /settings/security/api-keys", s.postKeyCreate)
	s.handleAuthed("POST /settings/security/api-keys/{id}/revoke", s.postKeyRevoke)
}

func (s *Server) getKeys(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/security")
		return
	}
	v, err := s.keysViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "API key", err)
		return
	}
	s.renderSettingsPart(w, http.StatusOK, settingsSecurityPage, "apikeys_card", v)
}

func (s *Server) postKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.APIKeys != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	name, scope := r.PostForm.Get("name"), core.APIKeyScope(r.PostForm.Get("scope"))
	v, err := s.keysViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "API key", err)
		return
	}
	v.Name, v.Scope, v.Errors = name, string(scope), map[string]string{}

	plain, _, err := s.APIKeys.Create(r.Context(), name, scope)
	switch {
	case errors.Is(err, auth.ErrInvalidAPIKeyName):
		v.Errors["name"] = "Enter a name of 1 to 100 characters, such as where the key will be used."
	case errors.Is(err, auth.ErrInvalidAPIKeyScope):
		v.Errors["scope"] = "Choose read only or read and change."
	case err != nil:
		s.settingsFail(w, r, "API key", err)
		return
	}
	if len(v.Errors) > 0 {
		v.Error = "The key was not created. Fix the marked fields and try again."
		s.respondKeys(w, r, http.StatusUnprocessableEntity, v)
		return
	}

	// The list now includes the new key; the form starts empty again.
	if v, err = s.keysViewFor(r); err != nil {
		s.settingsFail(w, r, "API key", err)
		return
	}
	v.Secret = &shownSecret{
		ID: "apikeys", Heading: "Your new API key",
		Intro:  "Copy it now and keep it somewhere safe. It is shown only once: bitnews stores a fingerprint, not the key, so it cannot show it again.",
		Fields: []secretField{{Label: "API key", Value: plain, Secret: true}},
		Done:   "/settings/security/api-keys",
		Hints:  []string{"Send it as the X-Api-Key header, or as “Authorization: Bearer” followed by the key."},
	}
	if isHTMX(r) {
		settingsTrigger(w, "success", "API key created. Copy it now: it is shown only once.", nil)
	}
	s.respondKeys(w, r, http.StatusOK, v)
}

func (s *Server) postKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.APIKeys != nil) {
		return
	}
	if err := s.APIKeys.Revoke(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.settingsFail(w, r, "API key", err)
		return
	}
	v, err := s.keysViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "API key", err)
		return
	}
	if isHTMX(r) {
		settingsTrigger(w, "success", "API key revoked. It stops working now.", nil)
	}
	s.respondKeys(w, r, http.StatusOK, v)
}

func (s *Server) respondKeys(w http.ResponseWriter, r *http.Request, status int, v keysView) {
	if v.Secret != nil {
		// The page holds a plaintext secret: never keep it.
		w.Header().Set("Cache-Control", "no-store")
	}
	s.respondSecurityCard(w, r, status, "apikeys_card", v, func(d *securityPageData) { d.Keys = v })
}
