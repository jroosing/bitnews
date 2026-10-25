package web

// headAssets are the hashed URLs of the files the document head links.
// They are resolved once at startup, so an asset that is missing from the
// binary stops bitnews from starting instead of failing every page.
type headAssets struct {
	Favicon, CSS, ThemeJS, AppJS, QueueJS, DetailJS, SettingsJS string
	HTMX, HTMXSSE, Alpine                                       string
}

func newHeadAssets(a *assets) (headAssets, error) {
	var h headAssets
	for _, f := range []struct {
		dst  *string
		name string
	}{
		{&h.Favicon, "favicon.svg"},
		{&h.CSS, "app.css"},
		{&h.ThemeJS, "js/theme.js"},
		{&h.AppJS, "js/app.js"},
		{&h.QueueJS, "js/queue.js"},
		{&h.DetailJS, "js/detail.js"},
		{&h.SettingsJS, "js/settings.js"},
		{&h.HTMX, "vendor/htmx-2.0.11.min.js"},
		{&h.HTMXSSE, "vendor/htmx-ext-sse-2.2.4.min.js"},
		{&h.Alpine, "vendor/alpine-csp-3.17.4.min.js"},
	} {
		p, err := a.Path(f.name)
		if err != nil {
			return headAssets{}, err
		}
		*f.dst = p
	}
	return h, nil
}
