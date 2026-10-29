package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

// probeView is the result of a connection test as the page shows it.
type probeView struct {
	OK bool
	// Title says what happened, Body what it means and what to do.
	Title, Body string
	// Detail is the short technical message of the prober (never secrets).
	Detail string
	// Timing is set for a success.
	Timing string
	Steps  []probeStep
	// Compact is the small variant used inside a table row.
	Compact bool
}

// probeStep is one stage of a connection, with how far the test got.
type probeStep struct {
	Label string
	State probeState
}

// probeState is how far a connection got at one stage.
type probeState string

const (
	probeOK      probeState = "ok"
	probeFail    probeState = "fail"
	probeSkipped probeState = "skipped"
)

// srText is what a screen reader hears after the stage label.
func (st probeStep) srText() string {
	switch st.State {
	case probeOK:
		return "worked"
	case probeFail:
		return "failed"
	}
	return "not reached"
}

// alertClass is the colour of the full result: green, red when a stage
// failed, amber when the test did not run.
func (v probeView) alertClass() string {
	switch {
	case v.OK:
		return "alert-success"
	case len(v.Steps) > 0:
		return "alert-danger"
	}
	return "alert-warn"
}

func (v probeView) role() string {
	if v.OK {
		return "status"
	}
	return "alert"
}

func (v probeView) probe() string {
	if v.OK {
		return "ok"
	}
	return "failed"
}

var probeStepLabels = []string{"Host name", "Connection", "TLS", "Login", "Server reply"}

// probeStepIndex is the first stage that failed for a class.
var probeStepIndex = map[app.ProbeClass]int{
	app.ProbeDNS: 0, app.ProbeTCP: 1, app.ProbeTLS: 2, app.ProbeAuth: 3, app.ProbeProtocol: 4,
}

// probeViewOf explains a ProbeResult in plain language. host is the host
// that was tested, for the messages.
func probeViewOf(res app.ProbeResult, host string, port int) probeView {
	v := probeView{OK: res.OK}
	// The prober's own words add something only when the server said them.
	if res.Class == app.ProbeAuth || res.Class == app.ProbeProtocol {
		v.Detail = res.Message
	}
	failed := len(probeStepLabels)
	if !res.OK {
		failed = probeStepIndex[res.Class]
	}
	for i, label := range probeStepLabels {
		st := probeOK
		switch {
		case i == failed:
			st = probeFail
		case i > failed:
			st = probeSkipped
		}
		v.Steps = append(v.Steps, probeStep{Label: label, State: st})
	}
	if res.OK {
		v.Title = "Connection works"
		v.Body = "bitnews connected over TLS and signed in."
		v.Detail = ""
		v.Timing = fmt.Sprintf("Connected in %d ms, server round trip %d ms.", ms(res.ConnectTime), ms(res.Latency))
		return v
	}
	v.Title, v.Body = probeExplanation(res, host, port)
	return v
}

func ms(d time.Duration) int64 {
	if d > 0 && d < time.Millisecond {
		return 1
	}
	return d.Milliseconds()
}

const neverSkips = " bitnews never skips certificate checks, so it will not connect until this is fixed."

// probeExplanation turns a class and detail into a title and advice.
func probeExplanation(res app.ProbeResult, host string, port int) (title, body string) {
	hostPort := fmt.Sprintf("%s:%d", host, port)
	switch res.Class {
	case app.ProbeDNS:
		switch res.Detail {
		case "not_found":
			return "Host name not found", fmt.Sprintf("%q does not exist in DNS. Check the spelling of the host name your provider gave you.", host)
		case "timeout":
			return "Host name lookup timed out", "The DNS server did not answer in time. Check that this machine has internet access and a working DNS server."
		}
		return "Host name lookup failed", fmt.Sprintf("%q could not be resolved to an address. Check the spelling and this machine's DNS settings.", host)
	case app.ProbeTCP:
		switch res.Detail {
		case "refused":
			return "Connection refused", fmt.Sprintf("Nothing accepted the connection at %s. Check the port: 563 (or 443) is the usual port for a TLS connection.", hostPort)
		case "timeout":
			return "Connection timed out", fmt.Sprintf("%s did not answer. A firewall, VPN or wrong host or port are the usual causes.", hostPort)
		case "unreachable":
			return "Host unreachable", fmt.Sprintf("There is no network route to %s. Check this machine's network, VPN and firewall.", host)
		}
		return "Could not connect", fmt.Sprintf("bitnews could not open a connection to %s. Check the host, the port and your firewall.", hostPort)
	case app.ProbeTLS:
		switch res.Detail {
		case "certificate_untrusted":
			return "Certificate not trusted", "The server's certificate was not issued by an authority this system trusts (it may be self-signed, or this system's trusted certificates may be out of date)." + neverSkips
		case "certificate_hostname":
			return "Certificate is for a different host name", fmt.Sprintf("The server's certificate is not valid for %q. Use the exact host name your provider documents, not an IP address or an alias.", host) + neverSkips
		case "certificate_expired":
			return "Certificate expired or not valid yet", "The certificate is outside its validity dates. If the provider's certificate is current, check this machine's clock." + neverSkips
		case "certificate_invalid":
			return "Certificate not valid", "The server presented a certificate that failed verification." + neverSkips
		case "not_tls":
			return "That port does not speak TLS", fmt.Sprintf("%s answered, but not with TLS. bitnews only connects over TLS: use your provider's TLS port, usually 563 or 443, not the plain port 119.", hostPort)
		case "timeout":
			return "TLS handshake timed out", fmt.Sprintf("%s accepted the connection but did not finish the secure handshake in time.", hostPort)
		}
		return "Secure connection failed", "The TLS handshake did not complete. The port may not be a TLS port, or the server may not support modern TLS."
	case app.ProbeAuth:
		return "Login rejected", "The server refused the user name or password. Check both, and that your plan is active. Some providers need an app password or a different login for newsgroup access."
	case app.ProbeProtocol:
		return "Unexpected reply from the server", "The server answered, but not like a Usenet (NNTP) server. Check that the host and port are the ones your provider lists for Usenet."
	}
	return "The test failed", "The connection test did not succeed."
}

// ---- routes --------------------------------------------------------------

func (s *Server) renderProbe(w http.ResponseWriter, r *http.Request, v probeView) {
	s.renderComponent(w, r, http.StatusOK, ProbeResult(v))
}

// postServerRowTest tests a saved server with its stored settings.
func (s *Server) postServerRowTest(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	id := core.ID(r.PathValue("id"))
	srv, err := s.Servers.Get(r.Context(), id)
	if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	res, err := s.Servers.Test(r.Context(), id)
	if ae, ok := errors.AsType[*app.Error](err); ok && ae.Code == "password_unavailable" {
		s.renderProbe(w, r, probeView{
			Compact: true,
			Title:   "Password needs to be entered again",
			Body:    "The stored password can no longer be read. Use Re-enter password to set it, then test again.",
		})
		return
	} else if err != nil {
		s.settingsFail(w, r, "server", err)
		return
	}
	v := probeViewOf(res, srv.Host, srv.Port)
	v.Compact = true
	s.renderProbe(w, r, v)
}

// postServerTest tests the values in the dialog, saved or not. An empty
// password field on a saved server tests with the stored password.
func (s *Server) postServerTest(w http.ResponseWriter, r *http.Request) {
	if !s.settingsServices(w, r, s.Servers != nil) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	in := serverInputOf(r)
	// The name, limits and priority do not matter for a connection test; fill
	// them so only the fields that do matter can fail.
	probeIn := in
	probeIn.Name, probeIn.MaxConnections, probeIn.RetentionDays, probeIn.Priority = "test", "1", "0", "0"
	spec, errs := probeIn.parse()
	// A good name is kept: a prober may need it to find the server.
	if in.Name != "" && !in.nameInvalid() {
		spec.Name = in.Name
	}
	var problems []string
	for _, f := range []struct{ field, label string }{{"host", "Host"}, {"port", "Port"}, {"username", "User name"}, {"password", "Password"}} {
		if msg, bad := errs[f.field]; bad {
			problems = append(problems, f.label+": "+msg)
		}
	}
	if len(problems) > 0 {
		s.renderProbe(w, r, probeView{Title: "Fix the connection details first", Body: strings.Join(problems, " ")})
		return
	}

	var res app.ProbeResult
	var err error
	if id := core.ID(r.PostForm.Get("server_id")); id != "" && in.Password == "" {
		stored, getErr := s.Servers.Get(r.Context(), id)
		if getErr != nil {
			s.settingsFail(w, r, "server", getErr)
			return
		}
		if stored.PasswordUnavailable {
			s.renderProbe(w, r, probeView{Title: "Enter the password to test", Body: "The stored password can no longer be read. Type it in the password field, then test again."})
			return
		}
		if stored.Host == spec.Host && stored.Port == spec.Port && stored.Username == spec.Username {
			res, err = s.Servers.Test(r.Context(), id)
		} else {
			spec.Password = stored.Password
			res, err = s.Servers.TestSpec(r.Context(), spec)
		}
	} else {
		res, err = s.Servers.TestSpec(r.Context(), spec)
	}
	if err != nil {
		if ae, ok := errors.AsType[*app.Error](err); ok {
			s.renderProbe(w, r, probeView{Title: "Could not test", Body: ae.Message})
			return
		}
		s.settingsFail(w, r, "server", err)
		return
	}
	s.renderProbe(w, r, probeViewOf(res, spec.Host, spec.Port))
}
