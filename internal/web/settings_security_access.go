package web

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// accessView is the "Sign-in" card: disable auth and LAN bypass.
type accessView struct {
	CSRF        string
	DisableAuth bool
	// LAN is the textarea: one network per line.
	LAN string
	// Errors is keyed by form field name; Error belongs to no field.
	Errors map[string]string
	Error  string
	Dirty  bool
	Saved  bool
	// NoHosts says no allowed host is stored yet, which blocks both options.
	NoHosts bool
	// Confirm is set instead of the form while a risky change waits for a yes.
	Confirm *confirmView
}

// hostsView is the "Allowed hosts and trusted proxies" card.
type hostsView struct {
	CSRF                         string
	AllowedHosts, TrustedProxies string
	Errors                       map[string]string
	Error                        string
	Dirty                        bool
	Saved                        bool
	// BypassOn says sign-in is skipped for someone, so the allowed hosts
	// are enforced.
	BypassOn bool
	// YourHost is the address this browser used, to suggest.
	YourHost string
	Confirm  *confirmView
}

func bypassOn(a core.Auth) bool { return a.DisableAuth || len(a.LANBypassCIDRs) > 0 }

func (s *Server) accessViewFor(r *http.Request) accessView {
	a := s.Settings.Current().Auth
	return accessView{
		CSRF: settingsCSRF(r), DisableAuth: a.DisableAuth, LAN: lines(a.LANBypassCIDRs),
		NoHosts: len(a.AllowedHosts) == 0,
	}
}

func (s *Server) hostsViewFor(r *http.Request) hostsView {
	a := s.Settings.Current().Auth
	return hostsView{
		CSRF: settingsCSRF(r), AllowedHosts: lines(a.AllowedHosts), TrustedProxies: lines(a.TrustedProxies),
		BypassOn: bypassOn(a), YourHost: r.Host,
	}
}

func (s *Server) registerAccessRoutes() {
	s.handleAuthed("GET /settings/security/access", s.getAccess)
	s.handleAuthed("POST /settings/security/access", s.postAccess)
	s.handleAuthed("GET /settings/security/hosts", s.getHosts)
	s.handleAuthed("POST /settings/security/hosts", s.postHosts)
}

func (s *Server) getAccess(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/security")
		return
	}
	s.renderSettingsPart(w, http.StatusOK, settingsSecurityPage, "access_card", s.accessViewFor(r))
}

func (s *Server) getHosts(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/security")
		return
	}
	s.renderSettingsPart(w, http.StatusOK, settingsSecurityPage, "hosts_card", s.hostsViewFor(r))
}

// Messages next to the fields.
const (
	needHostsMsg = "Add at least one allowed host in “Allowed hosts” below first. While sign-in is skipped, bitnews only answers requests for the host names you list there."
	noHostsLeft  = "Keep at least one host: sign-in is skipped for some devices, and that needs a list of allowed hosts. Turn those options off first if you want to empty it."
)

func lockoutMsg(host string) string {
	return "You reached bitnews as “" + shorten(host) + "”, which is not in this list. Once sign-in is skipped for some devices, bitnews would refuse this browser. Add it first."
}

// ---- sign-in options ---------------------------------------------------------

func (s *Server) postAccess(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	f := r.PostForm
	disable := f.Has("disable_auth")
	typed := f.Get("lan_bypass")
	confirmed := f.Get("confirmed") == "1"

	v := s.accessViewFor(r)
	v.DisableAuth, v.LAN, v.Errors = disable, typed, map[string]string{}
	fail := func() {
		v.Dirty, v.Error = true, "Nothing was saved. Fix the marked fields and try again."
		s.respondAccess(w, r, http.StatusUnprocessableEntity, v)
	}

	nets, bad := parseNetworks(typed)
	if len(bad) > 0 {
		v.Errors["lan_bypass"] = badNetworksMsg(bad)
		fail()
		return
	}
	v.LAN = lines(nets)

	// The lockout guard and the confirmation depend on the hosts that are
	// stored, so they are decided under the Settings lock, on the Settings
	// the change is applied to.
	// Nothing in the callback may read the Settings again (that would wait
	// for the lock it holds): the confirmation is built after it.
	var (
		next     core.Settings
		newlyOff bool
		newNets  []string
	)
	err := s.Settings.Change(r.Context(), func(n *core.Settings) error {
		if disable || len(nets) > 0 {
			switch {
			case len(n.Auth.AllowedHosts) == 0:
				if disable {
					v.Errors["disable_auth"] = needHostsMsg
				}
				if len(nets) > 0 {
					v.Errors["lan_bypass"] = needHostsMsg
				}
			case !auth.HostAllowed(r.Host, n.Auth.AllowedHosts):
				field := "lan_bypass"
				if disable {
					field = "disable_auth"
				}
				v.Errors[field] = lockoutMsg(r.Host)
			}
		}
		if len(v.Errors) > 0 {
			return errFieldsInvalid
		}
		newlyOff = disable && !n.Auth.DisableAuth
		newNets = addedEntries(nets, n.Auth.LANBypassCIDRs)
		if (newlyOff || len(newNets) > 0) && !confirmed {
			return errNeedsConfirm
		}
		n.Auth.DisableAuth = disable
		n.Auth.LANBypassCIDRs = nets
		next = *n
		return nil
	})
	switch ae, ok := settingsAppError(err); {
	case err == nil:
	case errors.Is(err, errFieldsInvalid):
		fail()
		return
	case errors.Is(err, errNeedsConfirm):
		v.Confirm = s.accessConfirm(r, disable, newlyOff, nets, newNets)
		s.respondAccess(w, r, http.StatusOK, v)
		return
	case ok:
		v.Dirty, v.Error = true, ae.Message
		s.respondAccess(w, r, http.StatusUnprocessableEntity, v)
		return
	default:
		s.settingsFail(w, r, "settings", err)
		return
	}

	v = s.accessViewFor(r)
	level, msg := "success", "Sign-in options saved."
	if bypassOn(next.Auth) {
		level, msg = "warn", "Saved. Sign-in is skipped for some devices now."
	}
	if isHTMX(r) {
		settingsTrigger(w, level, msg, nil)
	} else {
		v.Saved = true
	}
	s.respondAccess(w, r, http.StatusOK, v)
}

// errNeedsConfirm stops a Settings change that the user has to confirm
// first.
var errNeedsConfirm = errors.New("web: change needs confirmation")

// accessConfirm words what the change does, in plain terms.
func (s *Server) accessConfirm(r *http.Request, disable, newlyOff bool, nets, newNets []string) *confirmView {
	c := &confirmView{
		ID: "access", Action: "/settings/security/access", Title: "Sign-in",
		Heading: "Check this before you turn it on", Label: "Yes, apply this change",
		CSRF: settingsCSRF(r), Cancel: "/settings/security",
	}
	if newlyOff {
		c.Bullets = append(c.Bullets,
			"Sign-in will be off for everyone. Anyone who can reach bitnews in a browser or over the network can see and control it and everything it downloads, with no password.",
			"The API, Sonarr and Radarr credentials keep working as before.")
	}
	if len(newNets) > 0 {
		c.Bullets = append(c.Bullets, "Devices on these networks will skip sign-in and get full control of bitnews: "+strings.Join(newNets, ", ")+".")
		if anyWide(newNets) {
			c.Bullets = append(c.Bullets, "One of them covers every address on the internet. If bitnews can be reached from outside your home, anyone can use it.")
		}
	}
	if ip := s.clientIP(r); covers(newNets, ip) {
		c.Bullets = append(c.Bullets, "Your own device ("+ip.String()+") is in that range, so you will no longer be asked to sign in from here.")
	}
	if len(nets) > 0 && !disable {
		c.Bullets = append(c.Bullets, "A warning banner stays on every page while this is on. You can switch it off again here.")
	}
	c.Fields = []confirmField{{"lan_bypass", lines(nets)}, {"confirmed", "1"}}
	if disable {
		c.Fields = append(c.Fields, confirmField{"disable_auth", "on"})
	}
	return c
}

func (s *Server) respondAccess(w http.ResponseWriter, r *http.Request, status int, v accessView) {
	s.respondSecurityCard(w, r, status, "access_card", v, func(d *securityPageData) { d.Access = v })
}

// ---- allowed hosts and trusted proxies ------------------------------------

// parseHosts reads the allowed hosts: host names, IP addresses, each with an
// optional port. They are lower-cased; bad holds the entries that are not.
func parseHosts(raw string) (hosts, bad []string) {
	seen := map[string]bool{}
	for _, tok := range splitList(raw) {
		h := strings.ToLower(tok)
		if !validHostEntry(h) {
			bad = append(bad, shorten(tok))
			continue
		}
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	if len(hosts) > maxListEntries {
		bad = append(bad, "more than 64 entries")
	}
	return hosts, bad
}

func validHostEntry(h string) bool {
	if h == "" || len(h) > maxEntryRunes {
		return false
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	if ap, err := netip.ParseAddrPort(h); err == nil {
		return ap.Addr().Zone() == "" && ap.Port() != 0
	}
	name := h
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		port, err := strconv.Atoi(h[i+1:])
		if err != nil || port < 1 || port > 65535 {
			return false
		}
		name = h[:i]
	}
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func (s *Server) postHosts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	f := r.PostForm
	confirmed := f.Get("confirmed") == "1"

	v := s.hostsViewFor(r)
	v.AllowedHosts, v.TrustedProxies, v.Errors = f.Get("allowed_hosts"), f.Get("trusted_proxies"), map[string]string{}
	fail := func() {
		v.Dirty, v.Error = true, "Nothing was saved. Fix the marked fields and try again."
		s.respondHosts(w, r, http.StatusUnprocessableEntity, v)
	}

	hosts, badHosts := parseHosts(f.Get("allowed_hosts"))
	proxies, badProxies := parseNetworks(f.Get("trusted_proxies"))
	if len(badHosts) > 0 {
		v.Errors["allowed_hosts"] = "Not a host: " + strings.Join(badHosts, ", ") + ". Use only a name or address such as bitnews.lan or 192.168.1.10, optionally with a port (:9494). No http://, no path."
	}
	if len(badProxies) > 0 {
		v.Errors["trusted_proxies"] = badNetworksMsg(badProxies)
	}
	if len(v.Errors) > 0 {
		fail()
		return
	}
	v.AllowedHosts, v.TrustedProxies = lines(hosts), lines(proxies)

	// As for the access card, the guards read the stored Settings under the
	// lock, and the confirmation is built after it.
	var (
		newProxies []string
		bypass     bool
	)
	err := s.Settings.Change(r.Context(), func(n *core.Settings) error {
		if bypassOn(n.Auth) {
			switch {
			case len(hosts) == 0:
				v.Errors["allowed_hosts"] = noHostsLeft
			case !auth.HostAllowed(r.Host, hosts):
				v.Errors["allowed_hosts"] = lockoutMsg(r.Host)
			}
		}
		if len(v.Errors) > 0 {
			return errFieldsInvalid
		}
		newProxies, bypass = addedEntries(proxies, n.Auth.TrustedProxies), bypassOn(n.Auth)
		if len(newProxies) > 0 && (bypass || anyWide(newProxies)) && !confirmed {
			return errNeedsConfirm
		}
		n.Auth.AllowedHosts = hosts
		n.Auth.TrustedProxies = proxies
		return nil
	})
	switch ae, ok := settingsAppError(err); {
	case err == nil:
	case errors.Is(err, errFieldsInvalid):
		fail()
		return
	case errors.Is(err, errNeedsConfirm):
		v.Confirm = s.hostsConfirm(r, hosts, proxies, newProxies, bypass)
		s.respondHosts(w, r, http.StatusOK, v)
		return
	case ok:
		v.Dirty, v.Error = true, ae.Message
		s.respondHosts(w, r, http.StatusUnprocessableEntity, v)
		return
	default:
		s.settingsFail(w, r, "settings", err)
		return
	}
	v = s.hostsViewFor(r)
	if isHTMX(r) {
		settingsTrigger(w, "success", "Hosts and proxies saved.", nil)
	} else {
		v.Saved = true
	}
	s.respondHosts(w, r, http.StatusOK, v)
}

func (s *Server) hostsConfirm(r *http.Request, hosts, proxies, newProxies []string, bypass bool) *confirmView {
	c := &confirmView{
		ID: "hosts", Action: "/settings/security/hosts", Title: "Allowed hosts and trusted proxies",
		Heading: "Check this before you trust a proxy", Label: "Yes, trust these proxies",
		CSRF: settingsCSRF(r), Cancel: "/settings/security",
	}
	c.Bullets = append(c.Bullets, "bitnews will believe the X-Forwarded-For header of requests that come from "+strings.Join(newProxies, ", ")+" and treat the address in it as the visitor's address.")
	if bypass {
		c.Bullets = append(c.Bullets, "Sign-in is skipped for some devices, so anything that can send that header from those addresses can pretend to be one of them and get in without a password.")
	}
	if anyWide(newProxies) {
		c.Bullets = append(c.Bullets, "One of them covers every address on the internet. Only do this if bitnews can be reached through your proxy alone.")
	}
	c.Fields = []confirmField{{"allowed_hosts", lines(hosts)}, {"trusted_proxies", lines(proxies)}, {"confirmed", "1"}}
	return c
}

func (s *Server) respondHosts(w http.ResponseWriter, r *http.Request, status int, v hostsView) {
	s.respondSecurityCard(w, r, status, "hosts_card", v, func(d *securityPageData) { d.Hosts = v })
}
