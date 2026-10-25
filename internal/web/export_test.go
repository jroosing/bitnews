package web

import (
	"bytes"
	"fmt"
	"html/template"
)

// RenderTemplate renders one named template of a page's template set with
// arbitrary data, for component tests.
func RenderTemplate(page, name string, data any) (string, error) {
	a, err := loadTestAssets()
	if err != nil {
		return "", err
	}
	fsys, err := subFS("assets/templates")
	if err != nil {
		return "", err
	}
	tpl, err := loadTemplates(fsys, templateFuncs(a))
	if err != nil {
		return "", err
	}
	set, ok := tpl.sets[page]
	if !ok {
		// Every set holds all components; pages move to templ one by one, so
		// a page that is gone falls back to any remaining one.
		for _, set = range tpl.sets {
			ok = true
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("no page %q", page)
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func loadTestAssets() (*assets, error) {
	fsys, err := subFS("assets/static")
	if err != nil {
		return nil, err
	}
	return loadAssets(fsys, ".")
}

// TemplateFuncs exposes the template helpers.
func TemplateFuncs() template.FuncMap {
	a, err := loadTestAssets()
	if err != nil {
		panic(err)
	}
	return templateFuncs(a)
}

// Icons lists the embedded icon names.
func Icons() []string {
	var out []string
	for name := range iconPaths {
		out = append(out, string(name))
	}
	return out
}

// ContentSecurityPolicy is the CSP of the UI.
const ContentSecurityPolicy = contentSecurityPolicy
