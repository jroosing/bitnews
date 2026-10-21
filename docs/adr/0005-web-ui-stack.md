# The web UI is server-rendered htmx with Tailwind and Alpine, all vendored

The UI is HTML rendered by `html/template` in `internal/web`, enhanced with htmx (and its SSE extension), styled with Tailwind CSS and given small client-side state by Alpine.js. It is embedded in the binary and served by the daemon under a strict CSP: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`. There is no third-party origin at runtime and no JavaScript build step.

## Decisions

- **Tailwind CSS v4.3.3 through the standalone CLI** (no node, no npm). `scripts/get-tailwind.sh` downloads the pinned binary into `bin/tools/` and verifies its SHA-256 against checksums committed in the script. The generated `internal/web/assets/static/app.css` is **committed**, so `go build` and CI need no Tailwind step. `make css` rebuilds it, `make css-watch` watches, `make css-check` diffs a fresh build against the committed file (not part of CI, which stays lean). Design tokens (colours, one accent) are CSS variables in `internal/web/src/app.css`, mapped to Tailwind colours, with light and dark values.
- **Alpine.js CSP build** (`@alpinejs/csp` 3.17.4). The normal build uses `new Function` and would need `'unsafe-eval'`. Components are registered with `Alpine.data` in the first-party `js/app.js` (which loads before Alpine and registers in `alpine:init`). Alpine holds only small client state (menus, dialogs, toasts, theme, tabs, clipboard); fetching and swapping stay with htmx.
- **htmx 2.0.11 and htmx-ext-sse 2.2.4**, configured through `<meta name="htmx-config">`: `allowEval` and `allowScriptTags` off, `selfRequestsOnly` on, `includeIndicatorStyles` off (the indicator rules live in our CSS, because an inline `<style>` is blocked), `historyCacheSize` 0. No `hx-on`, inline script, `style=""` attribute or `javascript:` URL anywhere. Progress bars are `<progress>` elements.
- **Vendored, pinned and verified.** `scripts/vendor-web.sh` downloads each package from the npm registry and checks the sha512 integrity string npm publishes; versions and integrity are recorded in `internal/web/assets/static/VENDOR.md`.
- **Fonts and icons are self-hosted:** Inter (variable woff2, OFL, licence shipped) and inline Heroicons SVGs (MIT) through a `{{icon "name"}}` template function backed by a generated map.
- **Assets live inside the package** (`internal/web/assets/{static,templates}`), because `go:embed` cannot reach outside a package directory. They are loaded into memory at startup; `/static/` answers by exact map lookup (no file system access, no traversal, no directory listing). Links use content-hash names (`app.<hash8>.css`) served with `Cache-Control: public, max-age=31536000, immutable`; plain names revalidate. `url()` references inside CSS (fonts) are rewritten to hashed names.
- **Page and component structure.** One `layout_app` (shell) and one `layout_bare` (login, setup, errors); one parsed template set per page file; components are template partials taking a `dict` of named parameters, plus CSS component classes (`btn`, `card`, `badge-*`, ...). A living style guide at `/ui/styleguide` renders every component and is the visual test page.
- **Security wiring.** The UI sets its own CSP on its responses (the API's `default-src 'self'; frame-ancestors 'none'` is unchanged), `no-store` on HTML, CSRF through the existing `auth.CSRF` (htmx sends `X-CSRF-Token` from the meta tag; plain form posts carry a hidden `_csrf` field that a small wrapper copies into the header before the middleware runs), and `auth.UIAuth` for sessions, disable-auth and LAN bypass. While setup is open, `auth.Gate` also lets `GET /`, `GET /login`, `GET /setup` and `POST /setup` through. Errors from the auth middleware (JSON) are turned into a login redirect, an error page or, for htmx, a header or toast.

## Considered options

- **A SPA (React and similar).** Needs a build step and a large dependency tree, and duplicates logic the services already hold. Rejected.
- **Plain htmx and hand-written CSS.** No build step at all, but a modern, consistent look costs far more hand-written CSS than utility classes plus a token layer. Kept as the fallback if the Tailwind binary ever becomes a problem: the generated CSS is plain CSS.
- **Alpine's normal build with `'unsafe-eval'`.** Weakens the CSP the UI exists to keep. Rejected.
- **A CDN for htmx, Alpine, fonts.** Violates the same-origin rule and the offline homelab case. Rejected.
- **Tailwind through npm.** Adds node to the toolchain for one binary. Rejected.

## Consequences

- Changing a template, `src/app.css` or `js/*.js` needs `make css` before committing; the Tailwind binary is downloaded on first use (needs network once).
- Templates may only use what the CSP allows. Tests render every page and fail on inline script or style, event handlers and `hx-on`, and on any `javascript:` URL.
- Bumping htmx, Alpine, Inter or Heroicons is a script edit plus an integrity update, never a hand-copied file.
