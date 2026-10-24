package web

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/core"
)

// The sections of Speed & queue and Torrent network are made of cards that
// each edit one slice of core.Settings and save on their own. A settingsCard
// says how to turn that slice into form values and back; the shared code here
// does the rest of the settings form pattern (see README.md): validation per
// field, a 422 with the card, a toast, the plain-POST fallback and the
// read-modify-write of the Settings.

// ConfigSource is what the settings pages ask the running daemon. The daemon
// implements it; it may be nil.
type ConfigSource interface {
	// Started returns the Settings the daemon was built with. Some settings
	// (the torrent engine's network options, post-processing concurrency and
	// the network bindings) are read once, at start-up; comparing them with
	// the saved ones tells whether a restart is pending.
	Started() core.Settings
	// Binding reports the live state of the network binding the daemon runs
	// with for the Usenet or torrent protocol.
	Binding(kind core.SourceKind) BindingStatus
	// TorrentPortOverride is the listen port the operator fixed with
	// BITNEWS_TORRENT_PORT, or 0. It wins over the setting.
	TorrentPortOverride() int
	// ListenPort is the torrent engine's live listen port and whether a peer
	// has connected in on it; ok is false when torrents are off.
	ListenPort() (st core.ListenPortStatus, ok bool)
}

// BindingStatus is the live state of one protocol's network binding.
type BindingStatus struct {
	// Enabled is whether the daemon runs with a binding at all. Interface and
	// Address are what it runs with.
	Enabled            bool
	Interface, Address string
	// Available says whether the binding can carry traffic right now; if not,
	// Reason says why (for example that the interface does not exist) and the
	// protocol's Queue is paused.
	Available bool
	Reason    string
}

// cardView is what every card template sees, alone or embedded in a larger
// view.
type cardView struct {
	CSRF string
	// Error is a problem that belongs to no single field.
	Error string
	// Dirty starts the form unsaved (after a failed save); Saved is the
	// plain-POST success note (htmx shows a toast instead).
	Dirty, Saved bool
	// V holds the value of each form field by name (a checkbox is "on" when
	// ticked) and E the message of each bad field.
	V, E map[string]string
	// Restart is set when the card's settings differ from the ones the
	// daemon runs with: they are saved, and a restart applies them.
	Restart bool
}

// settingsCard is one card of a section.
type settingsCard struct {
	// key names the card in the page data; route is its POST path.
	key, route string
	// slug is the section, partial the card's template define.
	slug, partial string
	// saved is the toast after a save.
	saved string
	// values turns the Settings into form values.
	values func(core.Settings) map[string]string
	// read parses the form into next, which starts as the current Settings,
	// and returns one message per bad field (by form field name).
	read func(s *Server, f map[string]string, next *core.Settings) map[string]string
	// view wraps the card's view with whatever else its template shows.
	view func(s *Server, v cardView, cur core.Settings) any
	// restart reports whether the saved Settings differ from the started
	// ones in what this card edits. Nil means the card applies live.
	restart func(s *Server, started, saved core.Settings) bool
	// restartNote is added to the toast when restart reports true.
	restartNote string
	// invalid replaces the card's note when fields need fixing.
	invalid string
	// page builds the whole section page around the card views, for a
	// section that shows more than cards. Nil means the views are the data.
	page func(s *Server, r *http.Request, views cardViews) (any, error)
}

// errFieldsInvalid stops a Settings change whose form has bad fields.
var errFieldsInvalid = errors.New("web: form fields invalid")

// cardViews is the page data of a section made of cards, by card key.
type cardViews map[string]any

// cardViewFor builds the view of a card from the Settings.
func (s *Server) cardViewFor(r *http.Request, c *settingsCard, cur core.Settings) any {
	v := cardView{CSRF: settingsCSRF(r), V: c.values(cur), E: map[string]string{}}
	return s.finishCard(c, v, cur)
}

func (s *Server) finishCard(c *settingsCard, v cardView, cur core.Settings) any {
	v.Restart = s.restartPending(c, cur)
	if c.view != nil {
		return c.view(s, v, cur)
	}
	return v
}

// cardPage is the data of a whole section page. over replaces the view of
// one card (the one that was just posted).
func (s *Server) cardPage(r *http.Request, cards []*settingsCard, over map[string]any) cardViews {
	cur := s.Settings.Current()
	out := cardViews{}
	for _, c := range cards {
		if v, ok := over[c.key]; ok {
			out[c.key] = v
			continue
		}
		out[c.key] = s.cardViewFor(r, c, cur)
	}
	return out
}

// registerCards adds the POST route of each card.
func (s *Server) registerCards(cards []*settingsCard) {
	for _, c := range cards {
		s.handleAuthed("POST "+c.route, s.postCard(cards, c))
	}
}

func (s *Server) postCard(cards []*settingsCard, c *settingsCard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
			return
		}
		form := flatten(r.PostForm)
		// The form is read into the Settings under the service's lock, so a
		// save of another card at the same moment is never undone.
		var errs map[string]string
		var next core.Settings
		err := s.Settings.Change(r.Context(), func(n *core.Settings) error {
			if errs = c.read(s, form, n); len(errs) > 0 {
				return errFieldsInvalid
			}
			next = *n
			return nil
		})
		if err != nil {
			cur := s.Settings.Current()
			v := cardView{CSRF: settingsCSRF(r), V: form, E: map[string]string{}, Dirty: true}
			switch ae, ok := settingsAppError(err); {
			case errors.Is(err, errFieldsInvalid):
				v.E, v.Error = errs, cmp.Or(c.invalid, "Some values need fixing. Nothing was saved.")
			case ok:
				v.Error = ae.Message
			default:
				s.settingsFail(w, r, "settings", err)
				return
			}
			s.respondCard(w, r, http.StatusUnprocessableEntity, cards, c, s.finishCard(c, v, cur))
			return
		}
		v := cardView{CSRF: settingsCSRF(r), V: c.values(next), E: map[string]string{}}
		view := s.finishCard(c, v, next)
		if isHTMX(r) {
			level, msg := "success", c.saved
			if s.restartPending(c, next) {
				level, msg = "info", c.saved+" "+c.restartNote
			}
			settingsTrigger(w, level, msg, nil)
		} else {
			v.Saved = true
			view = s.finishCard(c, v, next)
		}
		s.respondCard(w, r, http.StatusOK, cards, c, view)
	}
}

// restartPending reports whether a restart is needed for the card's saved
// settings to take effect.
func (s *Server) restartPending(c *settingsCard, saved core.Settings) bool {
	return c.restart != nil && s.Config != nil && c.restart(s, s.Config.Started(), saved)
}

// respondCard answers with the card for htmx, or the whole page.
func (s *Server) respondCard(w http.ResponseWriter, r *http.Request, status int, cards []*settingsCard, c *settingsCard, view any) {
	if isHTMX(r) {
		sec, _ := settingsSectionBySlug(c.slug)
		s.renderSettingsPart(w, status, sec.Template, c.partial, view)
		return
	}
	views := s.cardPage(r, cards, map[string]any{c.key: view})
	if c.page == nil {
		s.renderSettingsPage(w, r, status, c.slug, views)
		return
	}
	data, err := c.page(s, r, views)
	if err != nil {
		s.settingsFail(w, r, "settings", err)
		return
	}
	s.renderSettingsPage(w, r, status, c.slug, data)
}

// flatten keeps the first value of each form field.
func flatten(form url.Values) map[string]string {
	out := make(map[string]string, len(form))
	for k, v := range form {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// ---- parsing ------------------------------------------------------------

// wholeNumber parses a whole number of 0..max from user input: digits only
// (no sign, exponent, hex, separators or non-ASCII digits). Blank is blank.
func wholeNumber(raw string, max int64) (n int64, blank bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true, true
	}
	if len(raw) > 18 || !allASCIIDigits(raw) {
		return 0, false, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n > max {
		return 0, false, false
	}
	return n, false, true
}

// decimalNumber parses a plain decimal such as "2" or "2.5" with at most
// three decimals: no sign, exponent, hex, NaN or Inf.
func decimalNumber(raw string, max float64) (f float64, blank bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true, true
	}
	whole, frac, hasDot := strings.Cut(raw, ".")
	if whole == "" || len(whole) > 12 || !allASCIIDigits(whole) || (hasDot && (frac == "" || len(frac) > 3 || !allASCIIDigits(frac))) {
		return 0, false, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f > max {
		return 0, false, false
	}
	return f, false, true
}

func allASCIIDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// on reports whether a checkbox was ticked.
func on(f map[string]string, name string) bool {
	_, ok := f[name]
	return ok
}

// optNumber renders a limit for a field: empty for zero (no limit).
func optNumber(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func checked(b bool) string {
	if b {
		return "on"
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
