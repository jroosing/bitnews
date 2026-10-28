package web

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/a-h/templ"
)

// Hx is the htmx wiring of one element. It replaces free-form attribute
// lists (ADR-0011): only these attributes can be expressed, so an event
// handler, style or srcdoc can never reach the page, which the CSP would
// block anyway. The URL fields only take same-origin references and Vals only
// takes server-side IDs and fixed words, never user text (download names).
// Free-text fields (Target, Swap, ...) are written by template authors and
// are escaped on output.
type Hx struct {
	Get, Post, Put, Patch, Delete string // same-origin URL

	PushURL    string // "true", "false" or a same-origin URL
	ReplaceURL string // "true", "false" or a same-origin URL

	Target, Swap, SwapOOB, Select, SelectOOB string
	Trigger, Encoding, DisabledElt, Confirm  string
	Include                                  string // CSS selector of the elements whose values join the request
	Ext, SSEConnect, SSESwap                 string // SSEConnect is a same-origin URL

	Vals map[string]string // hx-vals: names and ID-shaped values
}

var (
	hxValName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	// idValue is what a server-validated ID or fixed word looks like
	// (core.ID is lowercase base32); hx-vals only ever take such values.
	idValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// hxError is the panic value of MustAttrs, so render can turn it into a 500.
type hxError struct{ msg string }

func (e hxError) Error() string { return e.msg }

// Attrs returns the attributes to spread on an element: {{ h.MustAttrs()... }}
// in a .templ file. Only the fields that are set appear.
func (h Hx) Attrs() (templ.Attributes, error) {
	out := templ.Attributes{}
	for _, f := range []struct{ attr, name, value string }{
		{"hx-get", "get", h.Get},
		{"hx-post", "post", h.Post},
		{"hx-put", "put", h.Put},
		{"hx-patch", "patch", h.Patch},
		{"hx-delete", "delete", h.Delete},
		{"sse-connect", "sse-connect", h.SSEConnect},
	} {
		if f.value == "" {
			continue
		}
		if !relativeURL(f.value) {
			return nil, hxError{fmt.Sprintf("web: hx %s needs a relative URL, got %q", f.name, f.value)}
		}
		out[f.attr] = f.value
	}
	for attr, v := range map[string]string{"hx-push-url": h.PushURL, "hx-replace-url": h.ReplaceURL} {
		if v == "" {
			continue
		}
		if v != "true" && v != "false" && !relativeURL(v) {
			return nil, hxError{fmt.Sprintf("web: %s needs true, false or a relative URL, got %q", attr, v)}
		}
		out[attr] = v
	}
	for attr, v := range map[string]string{
		"hx-target": h.Target, "hx-swap": h.Swap, "hx-swap-oob": h.SwapOOB,
		"hx-select": h.Select, "hx-select-oob": h.SelectOOB,
		"hx-trigger": h.Trigger, "hx-encoding": h.Encoding,
		"hx-disabled-elt": h.DisabledElt, "hx-confirm": h.Confirm,
		"hx-ext": h.Ext, "sse-swap": h.SSESwap, "hx-include": h.Include,
	} {
		if v != "" {
			out[attr] = v
		}
	}
	if len(h.Vals) > 0 {
		for k, v := range h.Vals {
			if !hxValName.MatchString(k) || !idValue.MatchString(v) {
				return nil, hxError{fmt.Sprintf("web: hx vals take names and IDs, got %q=%q", k, v)}
			}
		}
		j, err := json.Marshal(h.Vals)
		if err != nil {
			return nil, err
		}
		out["hx-vals"] = string(j)
	}
	return out, nil
}

// MustAttrs is Attrs for templ expressions, which cannot return an error. An
// invalid value is a programmer error; render turns the panic into a 500.
func (h Hx) MustAttrs() templ.Attributes {
	a, err := h.Attrs()
	if err != nil {
		panic(err)
	}
	return a
}

// relativeURL reports whether s is a same-origin path, query or fragment
// reference: no scheme, no host, no backslash or control characters.
func relativeURL(s string) bool {
	if s == "" || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\x00\r\n\t") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil
}
