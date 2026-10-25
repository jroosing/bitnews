package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
)

// templateFuncs are the helpers templates can call.
func templateFuncs(a *assets) template.FuncMap {
	return template.FuncMap{
		"asset":  a.Path,
		"icon":   icon,
		"dict":   dict,
		"cond":   func(c bool, yes, no string) string { return map[bool]string{true: yes, false: no}[c] },
		"attrs":  attrs,
		"hxVals": hxVals,
	}
}

// icon renders an inline Heroicons outline SVG. The name must be one of the
// embedded icons. classes are written by template authors (never user
// data); they are escaped anyway.
func icon(name string, classes ...string) (template.HTML, error) {
	inner, ok := iconPaths[IconName(name)]
	if !ok {
		return "", fmt.Errorf("web: unknown icon %q", name)
	}
	class := "size-5"
	if len(classes) > 0 {
		class = strings.Join(classes, " ")
	}
	// inner comes from icons_gen.go, a constant generated from Heroicons.
	return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true" focusable="false" class="` + //nolint:gosec // constant markup
		html.EscapeString(class) + `">` + inner + `</svg>`), nil
}

// dict builds a map from alternating keys and values, to pass named
// parameters to a component: {{template "badge" dict "State" "ready"}}.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("web: dict needs an even number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("web: dict key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

var (
	attrName = regexp.MustCompile(`^[a-z][a-z0-9:_.-]*$`)
	// idValue is what a server-validated ID looks like (core.ID is lowercase
	// base32); hx-vals and similar attributes only ever take such values.
	idValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// urlAttrs take a URL; their value must be a relative reference so a
// javascript: or other scheme can never get in.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "formaction": true, "poster": true,
	"hx-get": true, "hx-post": true, "hx-put": true, "hx-patch": true, "hx-delete": true,
	"hx-push-url": true, "hx-replace-url": true, "sse-connect": true,
}

// attrs builds an attribute list from alternating names and values, for the
// Attrs key of components: {{attrs "hx-post" "/x" "hx-target" "#row"}}.
// Names are validated and values are escaped. Event handlers (on*, hx-on*),
// style and srcdoc are refused, because the CSP forbids them, and URL
// attributes only take relative references.
func attrs(kv ...string) (template.HTMLAttr, error) {
	if len(kv)%2 != 0 {
		return "", errors.New("web: attrs needs an even number of arguments")
	}
	var b strings.Builder
	for i := 0; i < len(kv); i += 2 {
		name, value := kv[i], kv[i+1]
		switch {
		case !attrName.MatchString(name):
			return "", fmt.Errorf("web: invalid attribute name %q", name)
		case strings.HasPrefix(name, "on"), strings.HasPrefix(name, "hx-on"), strings.HasPrefix(name, "x-on"), name == "style", name == "srcdoc":
			return "", fmt.Errorf("web: attribute %q is not allowed (CSP)", name)
		case urlAttrs[name] && !relativeURL(value):
			return "", fmt.Errorf("web: attribute %q needs a relative URL, got %q", name, value)
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(name + `="` + html.EscapeString(value) + `"`)
	}
	return template.HTMLAttr(b.String()), nil //nolint:gosec // names validated, values escaped
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

// hxVals builds an hx-vals attribute from alternating names and server-side
// IDs. Values must look like IDs: user-controlled text (download names)
// never goes into hx-vals.
func hxVals(kv ...string) (template.HTMLAttr, error) {
	if len(kv)%2 != 0 {
		return "", errors.New("web: hxVals needs an even number of arguments")
	}
	m := make(map[string]string, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		if !attrName.MatchString(kv[i]) || !idValue.MatchString(kv[i+1]) {
			return "", fmt.Errorf("web: hxVals takes names and IDs, got %q=%q", kv[i], kv[i+1])
		}
		m[kv[i]] = kv[i+1]
	}
	j, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return template.HTMLAttr(`hx-vals="` + html.EscapeString(string(j)) + `"`), nil //nolint:gosec // JSON of validated values, escaped
}
