package web

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Security section (M6-08) is five independent cards, each swapped on
// its own by htmx: sign-in options (access), allowed hosts and trusted
// proxies (hosts), the password, API keys and media manager credentials.
// The files settings_security_*.go hold one card each; this file holds the
// registry entry, the page data and the parsing the cards share.

const settingsSecurityPage = "settings_security.html"

// securitySection is the "Security" entry of the settings registry.
func securitySection() SettingsSection {
	return SettingsSection{
		Slug: "security", Title: "Security", Icon: "shield", Order: 50,
		Description: "Sign-in options, API keys and media manager credentials.",
		Template:    settingsSecurityPage, Data: settingsSecurityData, Routes: (*Server).registerSecurityRoutes,
	}
}

// securityPageData is the .Page.Data of the section.
type securityPageData struct {
	Access   accessView
	Hosts    hostsView
	Password passwordView
	Keys     keysView
	Adapters adaptersView
}

func settingsSecurityData(s *Server, r *http.Request) (any, error) {
	d := securityPageData{
		Access:   s.accessViewFor(r),
		Hosts:    s.hostsViewFor(r),
		Password: s.passwordViewFor(r),
	}
	var err error
	if d.Keys, err = s.keysViewFor(r); err != nil {
		return nil, err
	}
	if d.Adapters, err = s.adaptersViewFor(r); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Server) registerSecurityRoutes() {
	s.registerAccessRoutes()
	s.registerPasswordRoutes()
	s.registerKeyRoutes()
	s.registerAdapterRoutes()
}

// respondSecurityCard answers a card post: the card for htmx, the whole
// page (with the same card state) for a plain form post.
func (s *Server) respondSecurityCard(w http.ResponseWriter, r *http.Request, status int, card string, v any, page func(*securityPageData)) {
	if isHTMX(r) {
		s.renderSettingsPart(w, status, settingsSecurityPage, card, v)
		return
	}
	d, err := settingsSecurityData(s, r)
	if err != nil {
		s.settingsFail(w, r, "security settings", err)
		return
	}
	data := d.(securityPageData)
	page(&data)
	s.renderSettingsPage(w, r, status, "security", data)
}

// ---- shared parsing -------------------------------------------------------

const (
	maxListEntries = 64
	maxEntryRunes  = 253
)

// splitList splits a textarea into entries: one per line, commas, semicolons
// and spaces also separate. Blank entries are dropped.
func splitList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' ' || r == '\t'
	})
}

// shorten cuts a hostile string for display in a message.
func shorten(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > 40 {
		return string([]rune(s)[:40]) + "…"
	}
	return s
}

// parseNetworks reads a list of networks. A bare address is written as a
// /32 or /128, a network is masked ("192.168.1.5/24" is "192.168.1.0/24"),
// and duplicates are dropped. bad holds the entries that are not networks.
func parseNetworks(raw string) (nets, bad []string) {
	seen := map[string]bool{}
	for _, tok := range splitList(raw) {
		p, ok := parseNetwork(tok)
		if !ok {
			bad = append(bad, shorten(tok))
			continue
		}
		if k := p.String(); !seen[k] {
			seen[k] = true
			nets = append(nets, k)
		}
	}
	if len(nets) > maxListEntries {
		bad = append(bad, "more than 64 entries")
	}
	return nets, bad
}

func parseNetwork(tok string) (netip.Prefix, bool) {
	if strings.Contains(tok, "/") {
		p, err := netip.ParsePrefix(tok)
		if err != nil {
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(tok)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, a.BitLen()), true
}

// badNetworksMsg words the error of a network list.
func badNetworksMsg(bad []string) string {
	return "Not a network: " + strings.Join(bad, ", ") + ". Write a network such as 192.168.1.0/24, or a single address such as 192.168.1.20."
}

// addedEntries returns the entries of next that are not in old.
func addedEntries(next, old []string) []string {
	oldSet := map[string]bool{}
	for _, o := range old {
		if p, ok := parseNetwork(o); ok {
			oldSet[p.String()] = true
		}
	}
	var out []string
	for _, n := range next {
		if !oldSet[n] {
			out = append(out, n)
		}
	}
	return out
}

// anyWide reports whether a network has no prefix at all (0.0.0.0/0, ::/0).
func anyWide(nets []string) bool {
	return slices.ContainsFunc(nets, func(n string) bool {
		p, ok := parseNetwork(n)
		return ok && p.Bits() == 0
	})
}

// covers reports whether any of the networks contains ip.
func covers(nets []string, ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	return slices.ContainsFunc(nets, func(n string) bool {
		p, ok := parseNetwork(n)
		return ok && p.Contains(ip)
	})
}

// lines is the text of a textarea for a list.
func lines(list []string) string { return strings.Join(list, "\n") }

// confirmView is the "are you sure" step of a risky change. The card holds
// the values again as hidden fields plus confirmed=1, so confirming is a
// plain form post with nothing to remember on the server.
type confirmView struct {
	ID, Action, Title, Heading string
	// Bullets say what the change does.
	Bullets []string
	Fields  []confirmField
	Label   string
	CSRF    string
	// Cancel is where the Cancel link leads without JavaScript.
	Cancel string
}

type confirmField struct{ Name, Value string }
