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

	"github.com/jroosing/bitnews/internal/core"
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
	for _, id := range []string{"access-card", "hosts-card", "password-card", "appkey-card"} {
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

// ---- the app key ----------------------------------------------------------

const hostileHost = `<script>alert(1)</script>"><img src=x onerror=alert(1)>`

func TestKeyCardShowsTheKeyAsPlainTextWithACopyButtonAndNeverElsewhere(t *testing.T) {
	e, b := settingsSignedIn(t)
	key := e.appKey.Current().Reveal()

	rec := b.get("/settings/security")

	body := rec.Body.String()
	doc := parseHTML(t, body)
	card := byID(doc, "appkey-card")
	require.NotNil(t, card)
	assert.Equal(t, strings.Count(renderNode(t, card), key), strings.Count(body, key), "the key is in the card and nowhere else on the page")
	assert.Equal(t, key, text(byID(card, "appkey-value")), "visible, no masking")
	assert.Nil(t, find(card, func(n *html.Node) bool { return n.Data == "input" && attr(n, "type") == "password" }))
	assert.Nil(t, find(card, func(n *html.Node) bool { return n.Data == "button" && strings.Contains(text(n), "Reveal") }), "nothing to reveal")
	assert.NotNil(t, find(card, func(n *html.Node) bool { return attr(n, "data-copy") == key }), "a Copy button")
	assert.Nil(t, byID(doc, "apikeys-card"))
	assert.Nil(t, byID(doc, "adapters-card"))
	assert.NotContains(t, e.log.String(), key)
}

func TestTheSameKeyIsShownOnEveryLoad(t *testing.T) {
	e, b := settingsSignedIn(t)
	key := e.appKey.Current().Reveal()

	first := text(byID(parseHTML(t, b.get("/settings/security").Body.String()), "appkey-value"))
	second := text(byID(parseHTML(t, b.get("/settings/security").Body.String()), "appkey-value"))
	card := text(byID(parseHTML(t, b.settingsDo(http.MethodGet, "/settings/security/app-key", nil).Body.String()), "appkey-value"))

	assert.Equal(t, key, first)
	assert.Equal(t, key, second)
	assert.Equal(t, key, card)
}

func TestKeyCardGivesCopyReadyValuesForSonarrAndRadarr(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.get("http://nas.lan:9494/settings/security")

	card := byID(parseHTML(t, rec.Body.String()), "appkey-card")
	vals := map[string]string{}
	for _, dt := range findAll(card, func(n *html.Node) bool { return n.Data == "dt" }) {
		dd := dt.NextSibling
		for dd != nil && dd.Data != "dd" {
			dd = dd.NextSibling
		}
		require.NotNil(t, dd)
		code := find(dd, func(n *html.Node) bool { return n.Data == "code" })
		require.NotNil(t, code, text(dt))
		vals[text(dt)] = text(code)
		btn := find(dd, func(n *html.Node) bool { return attr(n, "data-copy") != "" })
		require.NotNil(t, btn, "a copy button for %s", text(dt))
		assert.Equal(t, text(code), attr(btn, "data-copy"))
	}
	assert.Equal(t, "nas.lan", vals["Host"])
	assert.Equal(t, "9494", vals["Port"])
	assert.Equal(t, "sabnzbd", vals["URL base"])
	assert.Equal(t, "bitnews", vals["qBittorrent username"])
}

func TestKeyCardRendersAHostileHostHeaderInert(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.get("/settings/security", func(r *http.Request) { r.Host = hostileHost })

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "<script>alert(1)")
	assert.NotContains(t, body, "<img src=x")
	doc := parseHTML(t, body)
	assert.Nil(t, find(doc, func(n *html.Node) bool { return n.Data == "img" && attr(n, "onerror") != "" }))
	assert.Contains(t, text(byID(doc, "appkey-card")), "<script>alert(1)</script>")
}

func TestRotateAsksForConfirmationAndChangesNothingUntilConfirmed(t *testing.T) {
	e, b := settingsSignedIn(t)
	key := e.appKey.Current()

	rec := b.settingsPost("/settings/security/app-key", url.Values{})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	doc := parseHTML(t, rec.Body.String())
	assert.Contains(t, text(byID(doc, "appkey-card")), "every client (Sonarr, Radarr, scripts) stops working until updated")
	confirm := byID(doc, "appkey-confirm-form")
	require.NotNil(t, confirm)
	assert.Equal(t, "1", inputValue(confirm, "confirmed"))
	assert.Equal(t, key, e.appKey.Current(), "nothing changed yet")
	assert.Contains(t, sabCall(e, key.Reveal()), "categories")
}

func TestRotateButtonDoesNotNeedJavaScriptToAskForConfirmation(t *testing.T) {
	_, b := settingsSignedIn(t)
	card := byID(parseHTML(t, b.get("/settings/security").Body.String()), "appkey-card")

	form := find(card, func(n *html.Node) bool { return n.Data == "form" && attr(n, "action") == "/settings/security/app-key" })

	require.NotNil(t, form, "a plain form post reaches the confirm step")
	assert.Equal(t, "post", strings.ToLower(attr(form, "method")))
}

func TestConfirmedRotateMakesANewKeyAndTheOldOneStopsEverywhereAtOnce(t *testing.T) {
	e, b := settingsSignedIn(t)
	old := e.appKey.Current().Reveal()
	require.Contains(t, sabCall(e, old), "categories")
	require.Equal(t, http.StatusOK, e.browser().get("/api/v1/downloads", header("Authorization", "Bearer "+old)).Code)

	rec := b.settingsPost("/settings/security/app-key", url.Values{"confirmed": {"1"}})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fresh := e.appKey.Current().Reveal()
	assert.NotEqual(t, old, fresh)
	assert.Contains(t, sabCall(e, old), "API Key Incorrect")
	assert.Contains(t, sabCall(e, fresh), "categories")
	assert.Equal(t, http.StatusUnauthorized, e.browser().get("/api/v1/downloads", header("Authorization", "Bearer "+old)).Code)
	assert.Equal(t, http.StatusOK, e.browser().get("/api/v1/downloads", header("Authorization", "Bearer "+fresh)).Code)
	card := byID(parseHTML(t, rec.Body.String()), "appkey-card")
	assert.Equal(t, fresh, text(byID(card, "appkey-value")), "the card shows the new key")
	assert.NotContains(t, rec.Body.String(), old)
	assert.Contains(t, rec.Header().Get("HX-Trigger"), "rotated")
	assert.NotContains(t, rec.Header().Get("HX-Trigger"), fresh)
	assert.NotContains(t, e.log.String(), old)
	assert.NotContains(t, e.log.String(), fresh)
}

func TestKeyCardAndRotateWorkWhileSignInIsOff(t *testing.T) {
	e, _ := settingsSignedIn(t)
	setAuth(t, e, func(a *core.Auth) {
		a.AllowedHosts = []string{"example.com"}
		a.DisableAuth = true
	})
	anon := e.browser()

	page := anon.get("/settings/security")
	rec := anon.settingsPost("/settings/security/app-key", url.Values{"confirmed": {"1"}})

	require.Equal(t, http.StatusOK, page.Code)
	assert.NotNil(t, byID(parseHTML(t, page.Body.String()), "appkey-card"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotEmpty(t, e.appKey.Current())
}

func TestKeyRoutesRejectAPostWithoutTheCSRFToken(t *testing.T) {
	e, b := settingsSignedIn(t)
	key := e.appKey.Current()

	rec := b.post("/settings/security/app-key", url.Values{"confirmed": {"1"}}, htmx())

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, key, e.appKey.Current())
}

func TestTheOldKeyAndCredentialRoutesAreGone(t *testing.T) {
	_, b := settingsSignedIn(t)

	for _, path := range []string{
		"/settings/security/api-keys", "/settings/security/api-keys/abc/revoke",
		"/settings/security/adapters", "/settings/security/adapters/abc/revoke",
	} {
		assert.Equal(t, http.StatusNotFound, b.settingsDo(http.MethodGet, path, nil).Code, "GET "+path)
		assert.Equal(t, http.StatusNotFound, b.settingsPost(path, url.Values{"name": {"x"}}).Code, "POST "+path)
	}
}

func TestSecurityPageStillHasTheOtherCardsUnchanged(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/security").Body.String())

	assert.Contains(t, text(byID(doc, "access-card")), "Turn sign-in off")
	assert.NotNil(t, byID(doc, "access"))
	assert.Contains(t, text(byID(doc, "hosts-card")), "Trusted proxies")
	assert.NotNil(t, byID(doc, "password-current"))
	assert.Contains(t, text(byID(doc, "appkey-card")), "API key")
}

// renderNode serialises a node back to HTML.
func renderNode(t *testing.T, n *html.Node) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, html.Render(&b, n))
	return b.String()
}

// inputValue is the value of the named input inside n.
func inputValue(n *html.Node, name string) string {
	in := find(n, func(c *html.Node) bool { return c.Data == "input" && attr(c, "name") == name })
	if in == nil {
		return ""
	}
	return attr(in, "value")
}

func sabCall(e *env, key string) string {
	return e.browser().get("/sabnzbd/api?mode=get_cats&output=json&apikey=" + url.QueryEscape(key)).Body.String()
}

// addSecurityResponses renders the pages and fragments of the Security
// section into out for the shared HTML invariant tests.
func addSecurityResponses(t *testing.T, e *env, b *browser, out map[string]*httptest.ResponseRecorder) {
	t.Helper()
	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(1)>`
	setAuth(t, e, func(a *core.Auth) { a.AllowedHosts = []string{"example.com"} })

	out["GET /settings/security (with data)"] = b.get("/settings/security")
	out["POST access (confirm)"] = b.settingsPost("/settings/security/access", accessForm(true, "192.168.1.0/24"))
	out["POST access (invalid)"] = b.settingsPost("/settings/security/access", accessForm(false, "nope"))
	out["POST hosts (invalid)"] = b.settingsPost("/settings/security/hosts", hostsForm("http://x/", "nope"))
	out["POST hosts (confirm)"] = b.settingsPost("/settings/security/hosts", hostsForm("", "0.0.0.0/0"))
	out["POST password (invalid)"] = b.settingsPost("/settings/security/password", passwordForm("", "short", "other"))
	out["GET /settings/security (hostile host)"] = b.get("/settings/security", func(r *http.Request) { r.Host = hostile })
	out["POST key rotate (confirm)"] = b.settingsPost("/settings/security/app-key", url.Values{})
	out["POST key rotate (done)"] = b.settingsPost("/settings/security/app-key", url.Values{"confirmed": {"1"}})
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
