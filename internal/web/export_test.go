package web

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
