package web

import (
	"context"
	"fmt"
	"html"
	"io"

	"github.com/a-h/templ"
)

// Icon renders an inline Heroicons outline SVG. class holds the sizing and
// colour utilities, written by template authors (never user data); they are
// escaped anyway.
func Icon(name IconName, class string) templ.Component {
	inner, ok := iconPaths[name]
	if !ok {
		// Only reachable by converting a string to IconName.
		return templ.ComponentFunc(func(context.Context, io.Writer) error {
			return fmt.Errorf("web: unknown icon %q", string(name))
		})
	}
	// inner comes from icons_gen.go, a constant generated from Heroicons.
	return templ.Raw(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true" focusable="false" class="` +
		html.EscapeString(class) + `">` + inner + `</svg>`) //nolint:gosec // constant markup, escaped class
}
