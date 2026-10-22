package web

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// errorPage is the data of the standalone error page.
type errorPage struct {
	Status    int
	Heading   string
	Message   string
	Href      string
	HrefLabel string
}

// renderError answers with an error page, or for htmx with the status, an
// empty body and a toast (HX-Trigger), because htmx does not swap errors.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	if isHTMX(r) {
		trigger, _ := json.Marshal(map[string]any{"toast": map[string]string{"level": "danger", "message": heading + ". " + message}})
		w.Header().Set("HX-Trigger", string(trigger))
		w.WriteHeader(status)
		return
	}
	v := s.newView(r, strconv.Itoa(status)+" "+heading)
	v.Page = errorPage{Status: status, Heading: heading, Message: message, Href: "/", HrefLabel: "Back to bitnews"}
	// An error page is anonymous: it shows no account menu, whatever the
	// request's principal.
	v.CSRF, v.AuthMode = "", ""
	s.render(w, r, status, "error.html", "layout_bare", v)
}
