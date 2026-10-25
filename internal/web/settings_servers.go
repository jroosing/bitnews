package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

// settingsServersComponent is the Component of the Usenet servers section.
func settingsServersComponent(v *View, p settingsPage) templ.Component {
	d, _ := p.Data.(serversPageData)
	return SettingsServersPage(v, p, d)
}

// serversPageData is the .Page.Data of the Usenet servers section.
type serversPageData struct {
	List serversView
}

// serversView is the card with the table of servers.
type serversView struct {
	CSRF  string
	Items []serverRow
}

// serverRow is one server in the table. It never holds the password.
type serverRow struct {
	CSRF                string
	ID                  string
	Name                string
	Host                string
	Port                int
	MaxConnections      int
	RetentionDays       int
	Retention           string // "Unlimited", "30 days"
	Priority            int
	Enabled             bool
	PasswordUnavailable bool
	// First and Last disable the move arrows.
	First, Last bool
}

// serverFormView is the add / edit dialog.
type serverFormView struct {
	CSRF string
	// ID is empty when adding.
	ID                                                                  string
	Name, Host, Port, Username, MaxConnections, RetentionDays, Priority string
	Enabled                                                             bool
	// HasPassword says a password is stored (edit); PasswordUnavailable that
	// it can no longer be decrypted and has to be entered again.
	HasPassword, PasswordUnavailable bool
	Errors                           map[string]string
	// Error is a problem that belongs to no single field.
	Error string
}

// action is where the dialog form posts: the server, or the collection when
// adding.
func (v serverFormView) action() string {
	if v.ID != "" {
		return "/settings/servers/" + v.ID
	}
	return "/settings/servers"
}

func (v serverFormView) passwordPlaceholder() string {
	if v.HasPassword && !v.PasswordUnavailable {
		return "Leave empty to keep it"
	}
	return ""
}

func (v serverFormView) passwordHelp() string {
	switch {
	case v.PasswordUnavailable:
		return "The old password is lost, so type it again."
	case v.HasPassword:
		return "Write-only. Leave empty to keep the stored password."
	case v.Error != "":
		return "For safety the password is not kept: type it again."
	}
	return "Stored encrypted and never shown again."
}

func settingsServersData(s *Server, r *http.Request) (any, error) {
	if s.Servers == nil {
		return serversPageData{}, nil
	}
	v, err := s.serversViewFor(r)
	if err != nil {
		return nil, err
	}
	return serversPageData{List: v}, nil
}

func (s *Server) serversViewFor(r *http.Request) (serversView, error) {
	servers, err := s.Servers.List(r.Context())
	if err != nil {
		return serversView{}, err
	}
	v := serversView{CSRF: settingsCSRF(r), Items: make([]serverRow, 0, len(servers))}
	for i, srv := range servers {
		v.Items = append(v.Items, s.serverRowOf(v.CSRF, srv, i == 0, i == len(servers)-1))
	}
	return v, nil
}

func (s *Server) serverRowOf(csrf string, srv core.Server, first, last bool) serverRow {
	return serverRow{
		CSRF: csrf, ID: string(srv.ID), Name: srv.Name, Host: srv.Host, Port: srv.Port,
		MaxConnections: srv.MaxConnections, RetentionDays: srv.RetentionDays, Retention: retentionText(srv.RetentionDays),
		Priority: srv.Priority, Enabled: srv.Enabled, PasswordUnavailable: srv.PasswordUnavailable, First: first, Last: last,
	}
}

// limitsText is the connections and retention line of a row.
func (r serverRow) limitsText() string {
	conns := strconv.Itoa(r.MaxConnections) + " connection"
	if r.MaxConnections != 1 {
		conns += "s"
	}
	return conns + " · Retention: " + r.Retention
}

func retentionText(days int) string {
	switch days {
	case 0:
		return "Unlimited"
	case 1:
		return "1 day"
	}
	return strconv.Itoa(days) + " days"
}

func (s *Server) registerServerRoutes() {
	s.handleAuthed("GET /settings/servers/new", s.getServerNew)
	s.handleAuthed("POST /settings/servers", s.postServerCreate)
	s.handleAuthed("POST /settings/servers/test", s.postServerTest)
	s.handleAuthed("GET /settings/servers/{id}/edit", s.getServerEdit)
	s.handleAuthed("POST /settings/servers/{id}", s.postServerUpdate)
	s.handleAuthed("PATCH /settings/servers/{id}", s.patchServerEnabled)
	s.handleAuthed("DELETE /settings/servers/{id}", s.deleteServer)
	s.handleAuthed("POST /settings/servers/{id}/delete", s.deleteServer)
	s.handleAuthed("POST /settings/servers/{id}/move/{dir}", s.postServerMove)
	s.handleAuthed("POST /settings/servers/{id}/test", s.postServerRowTest)
}

// ---- parsing and validating the form ------------------------------------

// serverInput is the form as typed.
type serverInput struct {
	Name, Host, Port, Username, Password, MaxConnections, RetentionDays, Priority string
	Enabled                                                                       bool
}

func serverInputOf(r *http.Request) serverInput {
	f := r.PostForm
	return serverInput{
		Name: strings.TrimSpace(f.Get("name")), Host: strings.TrimSpace(f.Get("host")),
		Port: strings.TrimSpace(f.Get("port")), Username: f.Get("username"), Password: f.Get("password"),
		MaxConnections: strings.TrimSpace(f.Get("max_connections")), RetentionDays: strings.TrimSpace(f.Get("retention_days")),
		Priority: strings.TrimSpace(f.Get("priority")), Enabled: f.Has("enabled"),
	}
}

// Messages next to the fields.
const (
	serverNameMsg      = "Enter a name of 1 to 64 characters, such as the provider's name. It must be unique."
	serverHostMsg      = "Enter only the host name or IP address, such as news.example.com: no https://, port or path."
	serverPortMsg      = "Use a port from 1 to 65535. 563 is the usual port for TLS."
	serverConnsMsg     = "Use 1 to 100 connections. Your provider's plan says how many you may open."
	serverRetentionMsg = "Use 0 for unlimited, or the number of days the provider keeps articles."
	serverPriorityMsg  = "Use 0 or higher. Lower numbers are tried first."
	serverCredsMsg     = "At most 256 characters, without control characters."
	serverWholeMsg     = "Enter a whole number."
)

// parse converts the form into a spec and a message per bad field. The spec
// is valid only when no message is returned.
func (in serverInput) parse() (app.ServerSpec, map[string]string) {
	errs := map[string]string{}
	num := func(field, value string, blankOK bool) int {
		if value == "" {
			if !blankOK {
				errs[field] = serverWholeMsg
			}
			return 0
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			errs[field] = serverWholeMsg
		}
		return n
	}
	spec := app.ServerSpec{
		Name: in.Name, Host: in.Host, Username: in.Username, Password: core.Secret(in.Password),
		Port: num("port", in.Port, false), MaxConnections: num("max_connections", in.MaxConnections, false),
		RetentionDays: num("retention_days", in.RetentionDays, true), Priority: num("priority", in.Priority, true),
	}
	enabled := in.Enabled
	spec.Enabled = &enabled

	// Check the same rules as the service, but all at once and per field. The
	// core model is the single source of the rules.
	probe := core.Server{
		Name: spec.Name, Host: spec.Host, Port: spec.Port, MaxConnections: spec.MaxConnections,
		RetentionDays: spec.RetentionDays, Priority: spec.Priority, Username: spec.Username, Password: spec.Password,
	}
	if err := probe.Validate(); err != nil {
		for _, p := range []struct {
			err   error
			field string
			msg   string
		}{
			{core.ErrInvalidServerName, "name", serverNameMsg},
			{core.ErrInvalidServerHost, "host", serverHostMsg},
			{core.ErrInvalidServerPort, "port", serverPortMsg},
			{core.ErrInvalidServerMaxConnections, "max_connections", serverConnsMsg},
			{core.ErrInvalidServerRetention, "retention_days", serverRetentionMsg},
			{core.ErrInvalidServerPriority, "priority", serverPriorityMsg},
		} {
			if _, bad := errs[p.field]; !bad && errors.Is(err, p.err) {
				errs[p.field] = p.msg
			}
		}
		if errors.Is(err, core.ErrInvalidServerCredentials) {
			if (core.Server{Name: "x", Host: "x", Port: 1, MaxConnections: 1, Username: spec.Username}).Validate() != nil {
				errs["username"] = serverCredsMsg
			}
			if (core.Server{Name: "x", Host: "x", Port: 1, MaxConnections: 1, Password: spec.Password}).Validate() != nil {
				errs["password"] = serverCredsMsg
			}
		}
	}
	return spec, errs
}

// nameInvalid reports whether the name breaks the rules of a Server name.
func (in serverInput) nameInvalid() bool {
	_, errs := in.parse()
	_, bad := errs["name"]
	return bad
}

// formViewOf rebuilds the dialog from what was typed.
func (in serverInput) formView(csrf, id string, errs map[string]string) serverFormView {
	return serverFormView{
		CSRF: csrf, ID: id, Name: in.Name, Host: in.Host, Port: in.Port, Username: in.Username,
		MaxConnections: in.MaxConnections, RetentionDays: in.RetentionDays, Priority: in.Priority,
		Enabled: in.Enabled, Errors: errs,
	}
}

// fieldForCode maps the error codes of the service to the form field.
var serverCodeFields = map[string]struct{ field, msg string }{
	"invalid_name":            {"name", serverNameMsg},
	"name_taken":              {"name", "A server with this name already exists."},
	"invalid_host":            {"host", serverHostMsg},
	"invalid_port":            {"port", serverPortMsg},
	"invalid_max_connections": {"max_connections", serverConnsMsg},
	"invalid_retention_days":  {"retention_days", serverRetentionMsg},
	"invalid_priority":        {"priority", serverPriorityMsg},
	"invalid_credentials":     {"username", serverCredsMsg},
}

// ---- the dialog -----------------------------------------------------------

func (s *Server) renderServerForm(w http.ResponseWriter, r *http.Request, status int, v serverFormView) {
	if v.Errors == nil {
		v.Errors = map[string]string{}
	}
	s.renderComponent(w, r, status, ServerForm(v))
}

func (s *Server) getServerNew(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	servers, err := s.Servers.List(r.Context())
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	priority := 0
	for _, srv := range servers {
		priority = max(priority, srv.Priority+1)
	}
	s.renderServerForm(w, r, http.StatusOK, serverFormView{
		CSRF: settingsCSRF(r), Port: strconv.Itoa(core.DefaultServerPort), MaxConnections: "8",
		RetentionDays: "0", Priority: strconv.Itoa(priority), Enabled: true,
	})
}

func (s *Server) getServerEdit(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	srv, err := s.Servers.Get(r.Context(), core.ID(r.PathValue("id")))
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	v := serverFormViewOf(settingsCSRF(r), srv)
	if r.URL.Query().Get("reenter") == "1" && srv.PasswordUnavailable {
		v.Errors = map[string]string{"password": "Enter the password again."}
	}
	s.renderServerForm(w, r, http.StatusOK, v)
}

func serverFormViewOf(csrf string, srv core.Server) serverFormView {
	return serverFormView{
		CSRF: csrf, ID: string(srv.ID), Name: srv.Name, Host: srv.Host, Port: strconv.Itoa(srv.Port), Username: srv.Username,
		MaxConnections: strconv.Itoa(srv.MaxConnections), RetentionDays: strconv.Itoa(srv.RetentionDays),
		Priority: strconv.Itoa(srv.Priority), Enabled: srv.Enabled,
		HasPassword: srv.Password.Reveal() != "" || srv.PasswordUnavailable, PasswordUnavailable: srv.PasswordUnavailable,
	}
}

// serverSaved answers a successful create or update: the dialog closes and
// the list is swapped in.
func (s *Server) serverSaved(w http.ResponseWriter, r *http.Request, message string) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/servers")
		return
	}
	v, err := s.serversViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	w.Header().Set("HX-Retarget", "#servers-list")
	w.Header().Set("HX-Reswap", "outerHTML")
	settingsTrigger(w, "success", message, map[string]any{"dialog-close": true})
	s.renderComponent(w, r, http.StatusOK, ServersList(v))
}

// serverRejected answers a form that did not pass.
func (s *Server) serverRejected(w http.ResponseWriter, r *http.Request, v serverFormView) {
	if !isHTMX(r) {
		msg := "Check the form and try again."
		for _, m := range v.Errors {
			msg = m
			break
		}
		if v.Error != "" {
			msg = v.Error
		}
		s.renderError(w, r, http.StatusUnprocessableEntity, "The server was not saved", msg)
		return
	}
	s.renderServerForm(w, r, http.StatusUnprocessableEntity, v)
}

func (s *Server) postServerCreate(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	in := serverInputOf(r)
	spec, errs := in.parse()
	v := in.formView(settingsCSRF(r), "", errs)
	if len(errs) > 0 {
		v.Error = "Some fields need fixing. Nothing was saved."
		s.serverRejected(w, r, v)
		return
	}
	if _, err := s.Servers.Create(r.Context(), spec); err != nil {
		if s.mapServerError(&v, err) {
			s.serverRejected(w, r, v)
			return
		}
		s.settingsFail(w, r, "server", err)
		return
	}
	s.serverSaved(w, r, "Server added.")
}

// mapServerError puts a service error next to its field. It reports false
// for errors that are not the user's input.
func (s *Server) mapServerError(v *serverFormView, err error) bool {
	ae, ok := settingsAppError(err)
	if !ok {
		return false
	}
	if v.Errors == nil {
		v.Errors = map[string]string{}
	}
	if f, known := serverCodeFields[ae.Code]; known {
		v.Errors[f.field] = f.msg
		v.Error = "Some fields need fixing. Nothing was saved."
		return true
	}
	v.Error = ae.Message
	return true
}

func (s *Server) postServerUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	id := core.ID(r.PathValue("id"))
	stored, err := s.Servers.Get(r.Context(), id)
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	in := serverInputOf(r)
	spec, errs := in.parse()
	if stored.PasswordUnavailable && in.Password == "" {
		errs["password"] = "Enter the password again. The stored one can no longer be read."
	}
	v := in.formView(settingsCSRF(r), string(id), errs)
	v.HasPassword, v.PasswordUnavailable = stored.Password.Reveal() != "" || stored.PasswordUnavailable, stored.PasswordUnavailable
	if len(errs) > 0 {
		v.Error = "Some fields need fixing. Nothing was saved."
		s.serverRejected(w, r, v)
		return
	}
	patch := app.ServerPatch{
		Name: &spec.Name, Host: &spec.Host, Port: &spec.Port, Username: &spec.Username,
		MaxConnections: &spec.MaxConnections, RetentionDays: &spec.RetentionDays, Priority: &spec.Priority,
		Enabled: spec.Enabled,
	}
	if in.Password != "" {
		patch.Password = &spec.Password
	}
	if _, err := s.Servers.Update(r.Context(), id, patch); err != nil {
		if s.mapServerError(&v, err) {
			s.serverRejected(w, r, v)
			return
		}
		s.settingsFail(w, r, "server", err)
		return
	}
	s.serverSaved(w, r, "Server saved.")
}

// ---- the list ---------------------------------------------------------------

func (s *Server) respondServersList(w http.ResponseWriter, r *http.Request, message string) {
	if !isHTMX(r) {
		s.redirect(w, r, "/settings/servers")
		return
	}
	v, err := s.serversViewFor(r)
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	if message != "" {
		settingsTrigger(w, "success", message, nil)
	}
	s.renderComponent(w, r, http.StatusOK, ServersList(v))
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	if err := s.Servers.Delete(r.Context(), core.ID(r.PathValue("id"))); err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	s.respondServersList(w, r, "Server deleted.")
}

// patchServerEnabled turns a server on or off. The body holds "enabled" when
// the switch is on.
func (s *Server) patchServerEnabled(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	on := r.PostForm.Has("enabled")
	id := core.ID(r.PathValue("id"))
	if _, err := s.Servers.Update(r.Context(), id, app.ServerPatch{Enabled: &on}); err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	// The row's neighbours decide its arrows, so the whole list is
	// returned and swapped by id.
	msg := "Server turned off. Its connections close."
	if on {
		msg = "Server turned on."
	}
	s.respondServersList(w, r, msg)
}

// movePriorities returns the new Server Priority of the servers that change
// when id moves one place up (towards "tried first") or down. servers must
// be in list order (priority, then name). It returns nil when id cannot
// move.
func movePriorities(servers []core.Server, id core.ID, down bool) map[core.ID]int {
	i := slices.IndexFunc(servers, func(s core.Server) bool { return s.ID == id })
	if i < 0 {
		return nil
	}
	if down {
		i++
		if i >= len(servers) {
			return nil
		}
		id = servers[i].ID
	}
	if i == 0 {
		return nil
	}
	want := slices.Clone(servers)
	want[i-1], want[i] = want[i], want[i-1]

	// First try swapping the two priorities: that keeps every other server
	// where it is. It works when they differ and the names do not reorder
	// them again.
	if servers[i-1].Priority != servers[i].Priority {
		swapped := slices.Clone(servers)
		swapped[i-1].Priority, swapped[i].Priority = servers[i].Priority, servers[i-1].Priority
		if sameOrder(sortedServers(swapped), want) {
			return map[core.ID]int{servers[i-1].ID: servers[i].Priority, servers[i].ID: servers[i-1].Priority}
		}
	}

	// Otherwise (equal priorities, or names in the way) walk the wanted
	// order and raise only what has to be raised, so the list sorts the way
	// it should.
	out := map[core.ID]int{}
	prev := core.Server{Priority: -1}
	for k, srv := range want {
		p := srv.Priority
		if k > 0 && (p < prev.Priority || (p == prev.Priority && prev.Name > srv.Name)) {
			p = prev.Priority
			if prev.Name > srv.Name {
				p++
			}
		}
		if p != srv.Priority {
			out[srv.ID] = p
		}
		srv.Priority = p
		prev = srv
	}
	return out
}

func sortedServers(servers []core.Server) []core.Server {
	out := slices.Clone(servers)
	slices.SortStableFunc(out, func(a, b core.Server) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func sameOrder(a, b []core.Server) bool {
	return slices.EqualFunc(a, b, func(x, y core.Server) bool { return x.ID == y.ID })
}

func (s *Server) postServerMove(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	dir := r.PathValue("dir")
	if dir != "up" && dir != "down" {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "Move a server up or down.")
		return
	}
	id := core.ID(r.PathValue("id"))
	servers, err := s.Servers.List(r.Context())
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	if !slices.ContainsFunc(servers, func(srv core.Server) bool { return srv.ID == id }) {
		s.settingsFail(w, r, "server", core.ErrNotFound)
		return
	}
	for sid, p := range movePriorities(servers, id, dir == "down") {
		if _, err := s.Servers.Update(r.Context(), sid, app.ServerPatch{Priority: &p}); err != nil {
			s.settingsFail(w, r, "server", err)
			return
		}
	}
	s.respondServersList(w, r, "")
}
