package qbit

import (
	"mime"
	"net/http"
	"net/netip"
	"strings"

	"github.com/jroosing/bitnews/internal/auth"
)

// sidCookie is the name of the session cookie, as in qBittorrent.
const sidCookie = "SID"

const bannedText = "Your IP address has been banned after too many failed authentication attempts."

func isForm(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/x-www-form-urlencoded"
}

// authenticate checks the SID cookie. The session must be live and must have
// been opened with the current app key, so refreshing the key ends every
// session with the next request.
func (c *call) authenticate() bool {
	cookie, err := c.r.Cookie(sidCookie)
	if err != nil {
		return false
	}
	generation, ok := c.h.sessions.use(cookie.Value)
	if !ok {
		return false
	}
	if generation != c.h.AppKey.Generation() {
		c.h.sessions.drop(cookie.Value)
		return false
	}
	c.sid = cookie.Value
	return true
}

// trustedProxies are the networks whose X-Forwarded-* headers are believed.
func (h *handler) trustedProxies() []netip.Prefix {
	return auth.ParsePrefixes(h.Settings.Current().Auth.TrustedProxies)
}

// secure reports whether the client reached us over HTTPS: directly, or
// through a trusted proxy that says so.
func (h *handler) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return false
	}
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer := ap.Addr().Unmap()
	for _, p := range h.trustedProxies() {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

// login serves auth/login: the form fields username and password are checked
// against the fixed username and the app key. The answer is "Ok." and a SID cookie,
// or "Fails." with HTTP 200 like qBittorrent. Failures are counted per client
// IP only (the username is not a key: anyone could otherwise lock the media
// manager out by failing its username); a banned IP gets 403 whatever it
// sends.
func (c *call) login() {
	ip := auth.ClientIPFrom(c.r, c.h.Settings.Current().Auth)
	key := ip.String()
	if wait := c.h.Limiter.Check(ip, key); wait > 0 {
		c.h.log.Warn("qbit: login refused, too many failures", "ip", key)
		c.text(http.StatusForbidden, bannedText)
		return
	}
	c.r.Body = http.MaxBytesReader(c.w, c.r.Body, maxLoginBytes)
	var username, password string
	if isForm(c.r) {
		if err := c.r.ParseForm(); err != nil {
			c.bodyFailed(err)
			return
		}
		username, password = c.r.PostForm.Get("username"), c.r.PostForm.Get("password")
	}
	// The generation is read first: a refresh between the check and the
	// session would at worst make this session stale, never valid for a key
	// that is gone.
	generation := c.h.AppKey.Generation()
	if !c.h.AppKey.AuthenticateQBit(username, password) {
		c.h.Limiter.Fail(ip, key)
		c.h.log.Warn("qbit: login failed", "ip", key, "username", printable(username, 40))
		c.text(http.StatusOK, "Fails.")
		return
	}
	token, err := c.h.sessions.create(generation)
	if err != nil {
		c.h.log.Error("qbit: session failed", "err", err)
		c.text(http.StatusInternalServerError, "Internal Server Error")
		return
	}
	c.h.Limiter.Success(ip, key)
	http.SetCookie(c.w, &http.Cookie{
		Name: sidCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: c.h.secure(c.r), SameSite: http.SameSiteStrictMode,
	})
	c.h.log.Info("qbit: login", "username", username)
	c.text(http.StatusOK, "Ok.")
}

// logout serves auth/logout.
func (c *call) logout() {
	c.h.sessions.drop(c.sid)
	http.SetCookie(c.w, &http.Cookie{
		Name: sidCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: c.h.secure(c.r), SameSite: http.SameSiteStrictMode,
	})
	c.ok()
}

// sameOrigin is the CSRF rule of the adapter: a browser request must come
// from this origin or an allowed host (auth.CheckOrigin). Media managers send
// neither Origin nor Sec-Fetch-Site, so they pass.
func (h *handler) sameOrigin(r *http.Request) bool {
	return auth.CheckOrigin(r, h.Settings.Current().Auth.AllowedHosts) == nil
}
