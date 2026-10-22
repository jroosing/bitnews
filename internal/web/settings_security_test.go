package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// setAuth stores auth settings the way the API would, bypassing the page.
func setAuth(t *testing.T, e *env, mutate func(*core.Auth)) {
	t.Helper()
	next := e.settings.Current()
	mutate(&next.Auth)
	require.NoError(t, e.settings.Update(t.Context(), next))
}

func TestSecurityPageShowsOneCardPerGroupOfSettings(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.get("/settings/security")

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	for _, id := range []string{"access-card", "hosts-card", "password-card", "apikeys-card", "adapters-card"} {
		assert.NotNil(t, byID(doc, id), id)
	}
	assert.NotContains(t, rec.Body.String(), "Coming next")
}

func TestSecurityPageWarnsAboutEachRiskyOption(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/security").Body.String())

	access := text(byID(doc, "access-card"))
	assert.Contains(t, access, "Anyone who can reach")
	assert.Contains(t, access, "skip sign-in")
	hosts := text(byID(doc, "hosts-card"))
	assert.Contains(t, hosts, "Allowed hosts")
	assert.Contains(t, hosts, "Trusted proxies")
}

func accessForm(disable bool, lan string) url.Values {
	v := url.Values{"lan_bypass": {lan}}
	if disable {
		v.Set("disable_auth", "on")
	}
	return v
}

func TestTurningOnLANBypassWithoutAllowedHostsIsBlockedWithAnExplanation(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/access", accessForm(false, "192.168.1.0/24"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "allowed host")
	assert.Empty(t, e.settings.Current().Auth.LANBypassCIDRs)
	doc := parseHTML(t, rec.Body.String())
	ta := byID(doc, "access-lan-bypass")
	require.NotNil(t, ta)
	assert.Equal(t, "true", attr(ta, "aria-invalid"))
	assert.Contains(t, text(ta), "192.168.1.0/24", "what was typed stays")
}

func TestTurningOffSignInWithoutAllowedHostsIsBlockedWithAnExplanation(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/access", accessForm(true, ""))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "allowed host")
	assert.False(t, e.settings.Current().Auth.DisableAuth)
}

func TestRiskyAccessChangeAsksForConfirmationBeforeSaving(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })

	rec := b.settingsPost("/settings/security/access", accessForm(false, "192.168.1.0/24"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, e.settings.Current().Auth.LANBypassCIDRs, "nothing saved yet")
	doc := parseHTML(t, rec.Body.String())
	confirm := byID(doc, "access-confirm")
	require.NotNil(t, confirm, "the confirm step")
	assert.Contains(t, text(confirm), "192.168.1.0/24")
	form := find(confirm, func(n *html.Node) bool { return n.Data == "form" })
	assert.Equal(t, "/settings/security/access", attr(form, "action"))
	hidden := map[string]string{}
	for _, in := range findAll(doc, func(n *html.Node) bool { return n.Data == "input" && attr(n, "type") == "hidden" }) {
		hidden[attr(in, "name")] = attr(in, "value")
	}
	assert.Equal(t, "1", hidden["confirmed"])
	assert.Equal(t, "192.168.1.0/24", hidden["lan_bypass"])
}

func TestConfirmedLANBypassIsSavedAndOnlyTouchesTheBypassList(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.TrustedProxies = []string{"10.0.0.0/8"}
	})
	form := accessForm(false, "192.168.1.0/24\n10.1.2.3")
	form.Set("confirmed", "1")

	rec := b.settingsPost("/settings/security/access", form)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.settings.Current().Auth
	assert.Equal(t, []string{"192.168.1.0/24", "10.1.2.3/32"}, got.LANBypassCIDRs, "a bare address becomes a /32")
	assert.Equal(t, []string{"example.com"}, got.AllowedHosts)
	assert.Equal(t, []string{"10.0.0.0/8"}, got.TrustedProxies)
	assert.False(t, got.DisableAuth)
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "toast")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "access-card"))
}

func TestConfirmedDisableAuthIsSaved(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })
	form := accessForm(true, "")
	form.Set("confirmed", "1")

	rec := b.settingsPost("/settings/security/access", form)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, e.settings.Current().Auth.DisableAuth)
}

func TestTurningBypassOffNeedsNoConfirmation(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.DisableAuth = true
		a.LANBypassCIDRs = []string{"192.168.1.0/24"}
	})

	rec := b.settingsPost("/settings/security/access", accessForm(false, ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.settings.Current().Auth
	assert.False(t, got.DisableAuth)
	assert.Empty(t, got.LANBypassCIDRs)
}

func TestKeepingTheSameBypassNeedsNoConfirmation(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.LANBypassCIDRs = []string{"192.168.1.0/24"}
	})

	rec := b.settingsPost("/settings/security/access", accessForm(false, "192.168.1.0/24"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, byID(parseHTML(t, rec.Body.String()), "access-confirm"))
}

func TestBadNetworksAreReportedPerLine(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })

	rec := b.settingsPost("/settings/security/access", accessForm(false, "192.168.1.0/24\n192.168.1.0/33\nnot-a-network"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "192.168.1.0/33")
	assert.Contains(t, body, "not-a-network")
	assert.Empty(t, e.settings.Current().Auth.LANBypassCIDRs)
}

func TestAnAddressRangeThatCoversTheWholeInternetGetsAStrongerWarning(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })

	rec := b.settingsPost("/settings/security/access", accessForm(false, "0.0.0.0/0"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "access-confirm")), "every address on the internet")
}

func TestConfirmationNamesTheDeviceTheUserIsOn(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })

	// httptest requests come from 192.0.2.1.
	rec := b.settingsPost("/settings/security/access", accessForm(false, "192.0.2.0/24"))

	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "access-confirm")), "192.0.2.1")
}

func TestAccessFormWorksWithoutJavaScript(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })
	form := accessForm(false, "192.168.1.0/24")
	form.Set("confirmed", "1")
	form.Set("_csrf", b.csrf())

	rec := b.post("/settings/security/access", form)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"192.168.1.0/24"}, e.settings.Current().Auth.LANBypassCIDRs)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, byID(doc, "main"), "a whole page")
	assert.Contains(t, text(byID(doc, "access-card")), "Saved")
}

func hostsForm(hosts, proxies string) url.Values {
	return url.Values{"allowed_hosts": {hosts}, "trusted_proxies": {proxies}}
}

func TestAllowedHostsAndTrustedProxiesAreSavedWithoutTouchingTheRestOfAuth(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.LANBypassCIDRs = nil; a.DisableAuth = false })

	rec := b.settingsPost("/settings/security/hosts", hostsForm("Bitnews.LAN\n192.168.1.10:9494, bitnews.lan", "172.18.0.5/16"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.settings.Current().Auth
	assert.Equal(t, []string{"bitnews.lan", "192.168.1.10:9494"}, got.AllowedHosts, "lower-cased, no duplicates")
	assert.Equal(t, []string{"172.18.0.0/16"}, got.TrustedProxies, "masked")
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "toast")
}

func TestHostsKeepTheBypassSettingsWhenSaved(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.LANBypassCIDRs = []string{"192.168.1.0/24"}
		a.DisableAuth = true
	})

	rec := b.settingsPost("/settings/security/hosts", hostsForm("example.com\nother.lan", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := e.settings.Current().Auth
	assert.True(t, got.DisableAuth)
	assert.Equal(t, []string{"192.168.1.0/24"}, got.LANBypassCIDRs)
	assert.Equal(t, []string{"example.com", "other.lan"}, got.AllowedHosts)
}

func TestBadHostsAndProxiesAreRejectedPerField(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/hosts", hostsForm("http://bitnews.lan/path\nok.lan", "10.0.0.0/40\n172.16.0.0/12"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Equal(t, "true", attr(byID(doc, "hosts-allowed"), "aria-invalid"))
	assert.Equal(t, "true", attr(byID(doc, "hosts-proxies"), "aria-invalid"))
	assert.Contains(t, text(byID(doc, "hosts-allowed-error")), "http://bitnews.lan/path")
	assert.Contains(t, text(byID(doc, "hosts-proxies-error")), "10.0.0.0/40")
	assert.Empty(t, e.settings.Current().Auth.AllowedHosts)
}

func TestAllowedHostsCannotBeEmptiedWhileSignInIsSkipped(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.LANBypassCIDRs = []string{"192.168.1.0/24"}
	})

	rec := b.settingsPost("/settings/security/hosts", hostsForm("", ""))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "hosts-allowed-error")), "at least one host")
	assert.Equal(t, []string{"example.com"}, e.settings.Current().Auth.AllowedHosts)
}

func TestAllowedHostsCannotDropTheAddressTheUserIsOnWhileSignInIsSkipped(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.LANBypassCIDRs = []string{"192.168.1.0/24"}
	})

	rec := b.settingsPost("/settings/security/hosts", hostsForm("other.lan", ""))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "hosts-allowed-error")), "example.com")
	assert.Equal(t, []string{"example.com"}, e.settings.Current().Auth.AllowedHosts)
}

func TestTurningOnBypassFromAnAddressOutsideTheAllowedHostsIsBlocked(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"other.lan"} })

	rec := b.settingsPost("/settings/security/access", accessForm(true, ""))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "access-disable-auth-error")), "example.com")
	assert.False(t, e.settings.Current().Auth.DisableAuth)
}

func TestTrustingAProxyWhileBypassIsOnAsksForConfirmation(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.LANBypassCIDRs = []string{"192.168.1.0/24"}
	})

	rec := b.settingsPost("/settings/security/hosts", hostsForm("example.com", "172.18.0.0/16"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, e.settings.Current().Auth.TrustedProxies)
	confirm := byID(parseHTML(t, rec.Body.String()), "hosts-confirm")
	require.NotNil(t, confirm)
	assert.Contains(t, text(confirm), "172.18.0.0/16")

	form := hostsForm("example.com", "172.18.0.0/16")
	form.Set("confirmed", "1")
	rec = b.settingsPost("/settings/security/hosts", form)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"172.18.0.0/16"}, e.settings.Current().Auth.TrustedProxies)
}

func TestTrustingAProxyWithoutBypassNeedsNoConfirmationUnlessItIsEveryAddress(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/hosts", hostsForm("", "172.18.0.0/16"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"172.18.0.0/16"}, e.settings.Current().Auth.TrustedProxies)

	rec = b.settingsPost("/settings/security/hosts", hostsForm("", "172.18.0.0/16\n0.0.0.0/0"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"172.18.0.0/16"}, e.settings.Current().Auth.TrustedProxies, "waiting for a yes")
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "hosts-confirm")), "every address on the internet")
}

func TestCancellingAConfirmationReloadsTheCardWithoutChanges(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })

	rec := b.settingsDo(http.MethodGet, "/settings/security/access", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, byID(doc, "access"))
	assert.Nil(t, byID(doc, "access-confirm"))
}

func TestSecurityCardsRejectAPostWithoutTheCSRFToken(t *testing.T) {
	_, b := settingsSignedIn(t)

	for _, path := range []string{"/settings/security/access", "/settings/security/hosts"} {
		rec := b.post(path, url.Values{}, htmx())

		assert.Equal(t, http.StatusForbidden, rec.Code, path)
	}
}

// ---- password -------------------------------------------------------------

const changedPassword = "an entirely different passphrase"

func passwordForm(current, next, again string) url.Values {
	return url.Values{"current_password": {current}, "new_password": {next}, "confirm_password": {again}}
}

func TestPasswordCardAsksForTheCurrentAndTheNewPasswordTwice(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/security").Body.String())

	card := byID(doc, "password-card")
	for _, f := range []struct{ id, autocomplete string }{
		{"password-current", "current-password"}, {"password-new", "new-password"}, {"password-confirm", "new-password"},
	} {
		in := byID(card, f.id)
		require.NotNil(t, in, f.id)
		assert.Equal(t, "password", attr(in, "type"), f.id)
		assert.Equal(t, f.autocomplete, attr(in, "autocomplete"), f.id)
		assert.Empty(t, attr(in, "value"), f.id)
	}
}

func TestChangingThePasswordSwitchesLoginsAndSignsOutOtherBrowsers(t *testing.T) {
	e, b := settingsSignedIn(t)
	other := e.browser()
	other.login()
	require.Equal(t, http.StatusOK, other.get("/queue").Code)

	rec := b.settingsPost("/settings/security/password", passwordForm(adminPassword, changedPassword, changedPassword))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "Password changed")
	assert.Equal(t, http.StatusOK, b.get("/queue").Code, "this browser stays signed in")
	assert.Equal(t, http.StatusSeeOther, other.get("/queue").Code, "the other browser is signed out")
	fresh := e.browser()
	assert.Equal(t, http.StatusUnprocessableEntity, fresh.post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}}).Code, "old password no longer works")
	assert.Equal(t, http.StatusSeeOther, fresh.post("/login", url.Values{"username": {"admin"}, "password": {changedPassword}}).Code)
}

func TestPasswordChangeNeverEchoesOrLogsAPassword(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/password", passwordForm(adminPassword, changedPassword, changedPassword))
	bad := b.settingsPost("/settings/security/password", passwordForm("wrong wrong wrong", "short", "different"))

	for _, body := range []string{rec.Body.String(), bad.Body.String(), rec.Header().Get("HX-Trigger"), e.log.String()} {
		assert.NotContains(t, body, adminPassword)
		assert.NotContains(t, body, changedPassword)
		assert.NotContains(t, body, "wrong wrong wrong")
	}
}

func TestPasswordChangeWithAWrongCurrentPasswordChangesNothing(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/password", passwordForm("not my password!!", changedPassword, changedPassword))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Contains(t, text(byID(doc, "password-current-error")), "current password")
	fresh := e.browser()
	assert.Equal(t, http.StatusUnprocessableEntity, fresh.post("/login", url.Values{"username": {"admin"}, "password": {changedPassword}}).Code, "new password was not set")
	assert.Equal(t, http.StatusSeeOther, fresh.post("/login", url.Values{"username": {"admin"}, "password": {adminPassword}}).Code, "old password still works")
}

func TestPasswordChangeChecksLengthAndConfirmationPerField(t *testing.T) {
	_, b := settingsSignedIn(t)

	short := b.settingsPost("/settings/security/password", passwordForm(adminPassword, "too short", "too short"))
	mismatch := b.settingsPost("/settings/security/password", passwordForm(adminPassword, changedPassword, changedPassword+"x"))
	blank := b.settingsPost("/settings/security/password", passwordForm("", "", ""))

	assert.Equal(t, http.StatusUnprocessableEntity, short.Code)
	assert.Contains(t, text(byID(parseHTML(t, short.Body.String()), "password-new-error")), "12 characters")
	assert.Equal(t, http.StatusUnprocessableEntity, mismatch.Code)
	assert.Contains(t, text(byID(parseHTML(t, mismatch.Body.String()), "password-confirm-error")), "do not match")
	assert.Equal(t, http.StatusUnprocessableEntity, blank.Code)
	assert.NotNil(t, byID(parseHTML(t, blank.Body.String()), "password-current-error"))
}

func TestPasswordChangeFailuresAreRateLimited(t *testing.T) {
	_, b := settingsSignedIn(t)
	for range 5 {
		b.settingsPost("/settings/security/password", passwordForm("not my password!!", changedPassword, changedPassword))
	}

	rec := b.settingsPost("/settings/security/password", passwordForm(adminPassword, changedPassword, changedPassword))

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "Too many")
}

func TestPasswordCardIsNotAvailableWithoutASignedInUser(t *testing.T) {
	e, b := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.DisableAuth = true
	})
	anon := e.browser()

	rec := anon.get("/settings/security")

	require.Equal(t, http.StatusOK, rec.Code)
	card := byID(parseHTML(t, rec.Body.String()), "password-card")
	assert.Contains(t, text(card), "signed in")
	assert.Nil(t, byID(card, "password-current"))
	post := anon.settingsPost("/settings/security/password", passwordForm(adminPassword, changedPassword, changedPassword))
	assert.Equal(t, http.StatusUnprocessableEntity, post.Code)
	_ = b
}

// ---- API keys -------------------------------------------------------------

func TestAPIKeysCardListsKeysWithScopeCreatedAndLastUsedButNeverASecret(t *testing.T) {
	e, b := settingsSignedIn(t)
	plain, info, err := e.apiKeys.Create(t.Context(), "Home Assistant", core.APIKeyScopeRead)
	require.NoError(t, err)
	_, err = e.apiKeys.Authenticate(t.Context(), plain)
	require.NoError(t, err)
	_, unused, err := e.apiKeys.Create(t.Context(), "Backup script", core.APIKeyScopeWrite)
	require.NoError(t, err)

	rec := b.get("/settings/security")

	body := rec.Body.String()
	assert.NotContains(t, body, plain)
	card := byID(parseHTML(t, body), "apikeys-card")
	rows := findAll(card, func(n *html.Node) bool { return n.Data == "tr" && attr(n, "id") != "" })
	require.Len(t, rows, 2)
	first := text(byID(card, "apikey-"+string(info.ID)))
	assert.Contains(t, first, "Home Assistant")
	assert.Contains(t, first, "Read only")
	assert.NotContains(t, first, "Never")
	second := text(byID(card, "apikey-"+string(unused.ID)))
	assert.Contains(t, second, "Never", "a key that was not used yet")
	assert.Contains(t, second, "Read and change")
}

func createKeyForm(name, scope string) url.Values {
	return url.Values{"name": {name}, "scope": {scope}}
}

func TestCreatingAnAPIKeyShowsTheSecretOnceAndTheKeyWorks(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/api-keys", createKeyForm("Home Assistant", "read"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	doc := parseHTML(t, rec.Body.String())
	secret := byID(doc, "apikeys-secret")
	require.NotNil(t, secret, "the show-once block")
	plain := text(find(secret, func(n *html.Node) bool { return n.Data == "code" }))
	assert.True(t, strings.HasPrefix(plain, "bn_"), plain)
	copyBtn := find(secret, func(n *html.Node) bool { return attr(n, "data-copy") != "" })
	require.NotNil(t, copyBtn, "copy button")
	assert.Equal(t, plain, attr(copyBtn, "data-copy"))
	assert.NotContains(t, rec.Header().Get("HX-Trigger"), plain)
	assert.NotContains(t, e.log.String(), plain)

	api := e.browser().get("/api/v1/downloads", header("Authorization", "Bearer "+plain))
	assert.Equal(t, http.StatusOK, api.Code, "the key works on the API")

	later := b.get("/settings/security")
	assert.NotContains(t, later.Body.String(), plain, "never on a later load")
	assert.Contains(t, later.Body.String(), "Home Assistant")
	assert.NotContains(t, b.settingsDo(http.MethodGet, "/settings/security/api-keys", nil).Body.String(), plain)
}

func TestCreatingAnAPIKeyWithAPlainFormPostShowsTheSecretOnceOnTheWholePage(t *testing.T) {
	_, b := settingsSignedIn(t)
	form := createKeyForm("Cron job", "write")
	form.Set("_csrf", b.csrf())

	rec := b.post("/settings/security/api-keys", form)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, byID(doc, "main"))
	assert.NotNil(t, byID(doc, "apikeys-secret"))
}

func TestAPIKeyNameAndScopeAreChecked(t *testing.T) {
	e, b := settingsSignedIn(t)

	blank := b.settingsPost("/settings/security/api-keys", createKeyForm("  ", "read"))
	badScope := b.settingsPost("/settings/security/api-keys", createKeyForm("ok", "admin"))
	long := b.settingsPost("/settings/security/api-keys", createKeyForm(strings.Repeat("x", 101), "read"))

	assert.Equal(t, http.StatusUnprocessableEntity, blank.Code)
	assert.NotNil(t, byID(parseHTML(t, blank.Body.String()), "apikeys-name-error"))
	assert.Equal(t, http.StatusUnprocessableEntity, badScope.Code)
	assert.NotNil(t, byID(parseHTML(t, badScope.Body.String()), "apikeys-scope-error"))
	assert.Equal(t, http.StatusUnprocessableEntity, long.Code)
	keys, _ := e.apiKeys.List(t.Context())
	assert.Empty(t, keys)
	assert.Contains(t, text(byID(parseHTML(t, blank.Body.String()), "apikeys-card")), "No API keys", "the list stays")
}

func TestRevokingAnAPIKeyStopsItAtOnce(t *testing.T) {
	e, b := settingsSignedIn(t)
	plain, info, err := e.apiKeys.Create(t.Context(), "Old script", core.APIKeyScopeWrite)
	require.NoError(t, err)
	api := func() int {
		return e.browser().get("/api/v1/downloads", header("Authorization", "Bearer "+plain)).Code
	}
	require.Equal(t, http.StatusOK, api())

	rec := b.settingsPost("/settings/security/api-keys/"+string(info.ID)+"/revoke", url.Values{})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, http.StatusUnauthorized, api())
	assert.NotContains(t, rec.Body.String(), "Old script", "gone from the list")
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "revoked")
}

func TestRevokingAnUnknownAPIKeyIsNotFound(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/api-keys/"+string(core.NewID())+"/revoke", url.Values{})

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRevokeButtonsAskForConfirmation(t *testing.T) {
	e, b := settingsSignedIn(t)
	_, info, _ := e.apiKeys.Create(t.Context(), "Old script", core.APIKeyScopeRead)

	doc := parseHTML(t, b.get("/settings/security").Body.String())

	row := byID(doc, "apikey-"+string(info.ID))
	btn := find(row, func(n *html.Node) bool { return n.Data == "button" && attr(n, "hx-post") != "" })
	require.NotNil(t, btn)
	assert.Equal(t, "/settings/security/api-keys/"+string(info.ID)+"/revoke", attr(btn, "hx-post"))
	assert.Contains(t, attr(btn, "hx-confirm"), "Old script")
	assert.Equal(t, "danger", attr(btn, "data-confirm-tone"))
}

func TestHostileAPIKeyNamesRenderInert(t *testing.T) {
	_, b := settingsSignedIn(t)
	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(1)>`

	rec := b.settingsPost("/settings/security/api-keys", createKeyForm(hostile, "read"))
	page := b.get("/settings/security")

	for _, body := range []string{rec.Body.String(), page.Body.String()} {
		assert.NotContains(t, body, "<script>alert(1)")
		assert.NotContains(t, body, "<img src=x")
		doc := parseHTML(t, body)
		assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "img" && attr(n, "onerror") != "" }))
		assert.Contains(t, text(byID(doc, "apikeys-card")), "<script>alert(1)</script>")
	}
}

// ---- media manager credentials --------------------------------------------

func adapterForm(kind, name, username string) url.Values {
	return url.Values{"kind": {kind}, "name": {name}, "username": {username}}
}

// shown reads the label and value pairs of the show-once block.
func shown(t *testing.T, body, prefix string) map[string]string {
	t.Helper()
	block := byID(parseHTML(t, body), prefix+"-secret")
	require.NotNil(t, block, "the show-once block")
	out := map[string]string{}
	for _, dt := range findAll(block, func(n *html.Node) bool { return n.Data == "dt" }) {
		dd := dt.NextSibling
		for dd != nil && dd.Data != "dd" {
			dd = dd.NextSibling
		}
		require.NotNil(t, dd)
		code := find(dd, func(n *html.Node) bool { return n.Data == "code" })
		require.NotNil(t, code)
		out[text(dt)] = text(code)
		btn := find(dd, func(n *html.Node) bool { return attr(n, "data-copy") != "" })
		require.NotNil(t, btn, "a copy button for %s", text(dt))
		assert.Equal(t, text(code), attr(btn, "data-copy"))
	}
	return out
}

func sabCall(e *env, key string) string {
	return e.browser().get("/sabnzbd/api?mode=get_cats&output=json&apikey=" + url.QueryEscape(key)).Body.String()
}

func TestCreatingASABnzbdCredentialShowsWhatSonarrNeedsOnceAndItWorksUntilRevoked(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/adapters", adapterForm("sab", "Sonarr", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	vals := shown(t, rec.Body.String(), "adapters")
	assert.Equal(t, "example.com", vals["Host"])
	assert.Equal(t, "80", vals["Port"])
	assert.Equal(t, "sabnzbd", vals["URL base"])
	key := vals["API key"]
	assert.True(t, strings.HasPrefix(key, "bns_"), key)
	assert.NotContains(t, e.log.String(), key)
	assert.NotContains(t, rec.Header().Get("HX-Trigger"), key)

	assert.Contains(t, sabCall(e, key), "categories", "Sonarr can use it")
	assert.NotContains(t, b.get("/settings/security").Body.String(), key, "never on a later load")

	creds, err := e.adapters.List(t.Context())
	require.NoError(t, err)
	require.Len(t, creds, 1)
	revoke := b.settingsPost("/settings/security/adapters/"+string(creds[0].ID)+"/revoke", url.Values{})
	require.Equal(t, http.StatusOK, revoke.Code)
	assert.Contains(t, sabCall(e, key), "API Key Incorrect", "the very next request fails")
	assert.Contains(t, revoke.Body.String(), "No media managers connected", "gone from the list")
}

func TestCreatingAQBittorrentCredentialShowsUsernameAndPasswordOnceAndItWorksUntilRevoked(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/adapters", adapterForm("qbit", "My Radarr", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	vals := shown(t, rec.Body.String(), "adapters")
	assert.Equal(t, "example.com", vals["Host"])
	assert.Equal(t, "80", vals["Port"])
	assert.Equal(t, "my-radarr", vals["Username"], "derived from the name")
	password := vals["Password"]
	assert.True(t, strings.HasPrefix(password, "bnq_"), password)
	assert.NotContains(t, e.log.String(), password)
	assert.NotContains(t, b.get("/settings/security").Body.String(), password)

	info, err := e.adapters.AuthenticateQBit(t.Context(), "my-radarr", password)
	require.NoError(t, err)
	assert.Equal(t, "My Radarr", info.Name)

	revoke := b.settingsPost("/settings/security/adapters/"+string(info.ID)+"/revoke", url.Values{})
	require.Equal(t, http.StatusOK, revoke.Code)
	_, err = e.adapters.AuthenticateQBit(t.Context(), "my-radarr", password)
	assert.ErrorIs(t, err, auth.ErrInvalidAdapterCredential, "revoked at once")
}

func TestAdapterCredentialShowsTheHostAndPortTheBrowserUsed(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.settingsPost("http://nas.lan:9494/settings/security/adapters", adapterForm("sab", "Sonarr", ""), header("Origin", "http://nas.lan:9494"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	vals := shown(t, rec.Body.String(), "adapters")
	assert.Equal(t, "nas.lan", vals["Host"])
	assert.Equal(t, "9494", vals["Port"])
}

func TestAdapterCredentialsListLastUsedAndKind(t *testing.T) {
	e, b := settingsSignedIn(t)
	key, sab, err := e.adapters.Create(t.Context(), auth.KindAdapterSAB, "Sonarr", "")
	require.NoError(t, err)
	_, qbit, err := e.adapters.Create(t.Context(), auth.KindAdapterQBit, "Radarr", "radarr")
	require.NoError(t, err)
	require.Contains(t, sabCall(e, key), "categories")

	body := b.get("/settings/security").Body.String()

	card := byID(parseHTML(t, body), "adapters-card")
	used := text(byID(card, "adapter-"+string(sab.ID)))
	assert.Contains(t, used, "Sonarr")
	assert.Contains(t, used, "SABnzbd")
	assert.NotContains(t, used, "Never")
	fresh := text(byID(card, "adapter-"+string(qbit.ID)))
	assert.Contains(t, fresh, "qBittorrent")
	assert.Contains(t, fresh, "radarr")
	assert.Contains(t, fresh, "Never")
	assert.NotContains(t, body, key)
}

func TestAdapterCredentialInputIsCheckedPerField(t *testing.T) {
	e, b := settingsSignedIn(t)
	_, _, err := e.adapters.Create(t.Context(), auth.KindAdapterQBit, "Existing", "taken")
	require.NoError(t, err)

	blank := b.settingsPost("/settings/security/adapters", adapterForm("sab", " ", ""))
	kind := b.settingsPost("/settings/security/adapters", adapterForm("plex", "x", ""))
	badUser := b.settingsPost("/settings/security/adapters", adapterForm("qbit", "x", "Not Valid!"))
	taken := b.settingsPost("/settings/security/adapters", adapterForm("qbit", "x", "taken"))

	for name, tc := range map[string]struct {
		rec *httptest.ResponseRecorder
		id  string
	}{
		"name": {blank, "adapters-name-error"}, "kind": {kind, "adapters-kind-error"},
		"username": {badUser, "adapters-username-error"}, "taken": {taken, "adapters-username-error"},
	} {
		assert.Equal(t, http.StatusUnprocessableEntity, tc.rec.Code, name)
		assert.NotNil(t, byID(parseHTML(t, tc.rec.Body.String()), tc.id), name)
		assert.Nil(t, byID(parseHTML(t, tc.rec.Body.String()), "adapters-secret"), name)
	}
	creds, _ := e.adapters.List(t.Context())
	assert.Len(t, creds, 1)
}

func TestRevokingAnUnknownAdapterCredentialIsNotFound(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/security/adapters/"+string(core.NewID())+"/revoke", url.Values{})

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdapterRevokeButtonsAskForConfirmation(t *testing.T) {
	e, b := settingsSignedIn(t)
	_, info, _ := e.adapters.Create(t.Context(), auth.KindAdapterSAB, "Sonarr", "")

	doc := parseHTML(t, b.get("/settings/security").Body.String())

	btn := find(byID(doc, "adapter-"+string(info.ID)), func(n *html.Node) bool { return n.Data == "button" && attr(n, "hx-post") != "" })
	require.NotNil(t, btn)
	assert.Contains(t, attr(btn, "hx-confirm"), "Sonarr")
	assert.Equal(t, "danger", attr(btn, "data-confirm-tone"))
}

func TestHostileAdapterNamesRenderInert(t *testing.T) {
	_, b := settingsSignedIn(t)
	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(1)>`

	rec := b.settingsPost("/settings/security/adapters", adapterForm("sab", hostile, ""))
	page := b.get("/settings/security")

	for _, body := range []string{rec.Body.String(), page.Body.String()} {
		assert.NotContains(t, body, "<script>alert(1)")
		doc := parseHTML(t, body)
		assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "img" && attr(n, "onerror") != "" }))
		assert.Contains(t, text(byID(doc, "adapters-card")), "<script>alert(1)</script>")
	}
}

func TestAdapterCredentialsStayManageableWhileSignInIsOff(t *testing.T) {
	e, _ := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.DisableAuth = true
	})
	anon := e.browser()

	rec := anon.settingsPost("/settings/security/adapters", adapterForm("sab", "Sonarr", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "adapters-secret"))
}

// addSecurityResponses renders the pages and fragments of the Security
// section into out for the shared HTML invariant tests.
func addSecurityResponses(t *testing.T, e *env, b *browser, out map[string]*httptest.ResponseRecorder) {
	t.Helper()
	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(1)>`
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })
	_, _, err := e.apiKeys.Create(t.Context(), hostile, core.APIKeyScopeRead)
	require.NoError(t, err)
	_, _, err = e.adapters.Create(t.Context(), auth.KindAdapterQBit, hostile, "")
	require.NoError(t, err)

	out["GET /settings/security (with data)"] = b.get("/settings/security")
	out["POST access (confirm)"] = b.settingsPost("/settings/security/access", accessForm(true, "192.168.1.0/24"))
	out["POST access (invalid)"] = b.settingsPost("/settings/security/access", accessForm(false, "nope"))
	out["POST hosts (invalid)"] = b.settingsPost("/settings/security/hosts", hostsForm("http://x/", "nope"))
	out["POST hosts (confirm)"] = b.settingsPost("/settings/security/hosts", hostsForm("", "0.0.0.0/0"))
	out["POST password (invalid)"] = b.settingsPost("/settings/security/password", passwordForm("", "short", "other"))
	out["POST api key (invalid)"] = b.settingsPost("/settings/security/api-keys", createKeyForm("", "x"))
	out["POST api key (created)"] = b.settingsPost("/settings/security/api-keys", createKeyForm(hostile, "write"))
	out["POST credential (invalid)"] = b.settingsPost("/settings/security/adapters", adapterForm("x", "", "Bad Name"))
	out["POST credential (sab created)"] = b.settingsPost("/settings/security/adapters", adapterForm("sab", hostile, ""))
	out["POST credential (qbit created)"] = b.settingsPost("/settings/security/adapters", adapterForm("qbit", "Radarr", ""))
	// Last, because turning sign-in off changes what every later request sees.
	form := accessForm(false, "192.168.1.0/24")
	form.Set("confirmed", "1")
	out["POST access (saved)"] = b.settingsPost("/settings/security/access", form)
}

func TestAuthBannerLinksToTheSecuritySettings(t *testing.T) {
	e, _ := settingsSignedIn(t)
	for name, mutate := range map[string]func(*core.Auth){
		"disabled": func(a *core.Auth) { a.DisableAuth = true },
		"bypass":   func(a *core.Auth) { a.LANBypassCIDRs = []string{"192.0.2.0/24"} },
	} {
		setAuth(t, e, func(a *core.Auth) { *a = core.Auth{AllowedHosts: []string{"example.com"}}; mutate(a) })

		doc := parseHTML(t, e.browser().get("/queue").Body.String())

		banner := find(doc, func(n *html.Node) bool { return hasAttr(n, "data-auth-banner") })
		require.NotNil(t, banner, name)
		link := find(banner, func(n *html.Node) bool { return n.Data == "a" })
		require.NotNil(t, link, name)
		assert.Equal(t, "/settings/security", attr(link, "href"), name)
	}
}
